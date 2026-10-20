package server

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"fmt"
	"net"
	"net/url"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/baongo/relay/internal/config"
	"github.com/baongo/relay/internal/obs"
	"github.com/baongo/relay/internal/rotation"
	"github.com/baongo/relay/internal/script"
)

// Server is the relay: one fasthttp listener serving every configured route.
type Server struct {
	routes     []*compiledRoute
	verbose    bool
	clients    *clientPool
	log        *obs.Logger
	metrics    *obs.Metrics
	adminPath  string
	adminToken string
	isDraining atomic.Bool
}

type compiledRoute struct {
	cfg      *config.Route
	script   *script.Script
	headers  *headerSet
	upstream *upstream
	timeout  time.Duration
	forceSSE *bool
	params   map[string]any
	// rotator is nil when the route has no [route.rotation] block. A nil
	// rotator means one upstream request and no failure classification.
	rotator *rotation.Rotator
}

type upstream struct {
	scheme   string
	host     string
	hostPort string
	isTLS    bool
	basePath string
}

func parseUpstream(raw string) (*upstream, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	hp := u.Host
	if _, _, err := net.SplitHostPort(hp); err != nil {
		if u.Scheme == "https" {
			hp += ":443"
		} else {
			hp += ":80"
		}
	}
	return &upstream{
		scheme:   u.Scheme,
		host:     u.Host,
		hostPort: hp,
		isTLS:    u.Scheme == "https",
		basePath: strings.TrimSuffix(u.Path, "/"),
	}, nil
}

// New compiles every route. Any script syntax error or bad upstream aborts
// startup: a relay that starts with a broken route fails later, in production.
func New(cfg *config.Config, scripts map[string]*script.Script) (*Server, error) {
	return NewWithObservability(cfg, scripts, obs.NewStdout(cfg.Server.LogFormat == "json"), obs.NewMetrics())
}

// NewWithObservability is New with injected logger and metrics. Tests and
// embedders use it to capture output and read counters.
func NewWithObservability(cfg *config.Config, scripts map[string]*script.Script, logger *obs.Logger, metrics *obs.Metrics) (*Server, error) {
	if logger == nil {
		logger = obs.NewStdout(cfg.Server.LogFormat == "json")
	}
	if metrics == nil {
		metrics = obs.NewMetrics()
	}
	s := &Server{
		verbose:    cfg.Server.Verbose,
		clients:    newClientPool(cfg.Server.MaxConns),
		log:        logger,
		metrics:    metrics,
		adminPath:  cfg.Server.MetricsPath,
		adminToken: cfg.Server.AdminToken,
	}
	for i := range cfg.Routes {
		rc := &cfg.Routes[i]
		sc, ok := scripts[rc.Script]
		if !ok {
			return nil, fmt.Errorf("route %q: no compiled script for %q", rc.Name, rc.Script)
		}
		up, err := parseUpstream(rc.Upstream)
		if err != nil {
			return nil, fmt.Errorf("route %q: upstream: %w", rc.Name, err)
		}
		d, err := time.ParseDuration(rc.Timeout)
		if err != nil {
			return nil, fmt.Errorf("route %q: timeout: %w", rc.Name, err)
		}
		params := map[string]any{}
		for k, v := range rc.Params {
			params[k] = v
		}
		var rot *rotation.Rotator
		if rc.Rotation != nil {
			// Rotation sources carry file paths (file, cookie_jar, directory).
			// Resolve them against the config file so a mounted /etc/relay
			// behaves identically regardless of the process working directory.
			cfg.ResolveRotationPaths(rc.Rotation)
			rot, err = rotation.New(rc.Rotation)
			if err != nil {
				return nil, fmt.Errorf("route %q: %w", rc.Name, err)
			}
		}
		s.routes = append(s.routes, &compiledRoute{
			cfg:      rc,
			script:   sc,
			headers:  compileHeaders(rc.Headers),
			upstream: up,
			timeout:  d,
			forceSSE: rc.Response.StreamSSE,
			params:   params,
			rotator:  rot,
		})
	}
	// Longest prefix first so /opencode/v1 wins over /opencode.
	for i := 0; i < len(s.routes); i++ {
		for j := i + 1; j < len(s.routes); j++ {
			if len(s.routes[j].cfg.Prefix) > len(s.routes[i].cfg.Prefix) {
				s.routes[i], s.routes[j] = s.routes[j], s.routes[i]
			}
		}
	}
	return s, nil
}

// Handler returns the fasthttp request handler.
//
// Every request passes through three cross-cutting concerns before reaching a
// route: drain check, panic recovery, and metrics. They are applied here rather
// than inside serve() so that admin endpoints are covered too.
func (s *Server) Handler() fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		start := time.Now()
		path := string(ctx.Path())

		// A draining relay tells load balancers to stop sending new work while
		// requests already in flight are allowed to finish.
		if s.draining() && !isAdminPath(s.adminPath, path) {
			ctx.Response.Header.Set("Connection", "close")
			ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
			ctx.SetBodyString("relay is shutting down")
			return
		}

		s.metrics.InFlight.Add(1)
		defer s.metrics.InFlight.Add(-1)

		route := ""
		stream := false
		status := 0
		defer func() {
			if rec := recover(); rec != nil {
				// A panic inside a Lua bridge or the pipeline must not take the
				// whole process down: one bad request would kill every stream.
				status = fasthttp.StatusInternalServerError
				ctx.ResetBody()
				ctx.SetStatusCode(status)
				ctx.SetBodyString("internal error")
				s.log.Error("panic serving request",
					"route", route, "path", path, "panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()))
			}
			if route != "" {
				// A response that ended up as SSE is what matters for latency
				// interpretation, so prefer the observed value over the hint.
				streamed := stream
				if v, ok := ctx.UserValue(streamKey).(bool); ok && v {
					streamed = true
				}
				s.metrics.Observe(route, ctx.Response.StatusCode(), streamed, time.Since(start))
			}
		}()

		if path == "/healthz" || path == "/readyz" {
			ctx.SetStatusCode(200)
			ctx.SetBodyString("ok")
			return
		}
		if s.adminPath != "" && path == s.adminPath {
			s.serveMetrics(ctx)
			return
		}
		if path == "/routes" {
			s.serveRoutes(ctx)
			return
		}

		r := s.match(path)
		if r == nil {
			ctx.SetStatusCode(404)
			ctx.SetBodyString("no route for " + path)
			return
		}
		route = r.cfg.Name
		stream = r.responseStreams()
		s.serve(ctx, r)
	}
}

// streamKey is the RequestCtx user value set by serve() when the response was
// actually streamed. Metrics read it so the label reflects the response, not
// the route's static hint.
const streamKey = "relay.stream"

// isAdminPath reports whether path is an endpoint that must stay reachable
// while draining.
func isAdminPath(adminPath, path string) bool {
	switch path {
	case "/healthz", "/readyz", "/routes":
		return true
	}
	return adminPath != "" && path == adminPath
}

// draining reports whether shutdown has begun.
func (s *Server) draining() bool { return s.isDraining.Load() }

// PrepareShutdown marks the server as draining. New requests are refused from
// the next handler call; requests already running are unaffected.
func (s *Server) PrepareShutdown() { s.isDraining.Store(true) }

// responseStreams reports whether the route always answers with SSE. It is
// only a metrics label: the real decision is made per response in serve().
func (r *compiledRoute) responseStreams() bool { return r.forceSSE != nil && *r.forceSSE }

// serveMetrics writes the Prometheus exposition, optionally guarded by a token.
func (s *Server) serveMetrics(ctx *fasthttp.RequestCtx) {
	if s.adminToken != "" {
		got := string(ctx.Request.Header.Peek("Authorization"))
		want := "Bearer " + s.adminToken
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			ctx.SetStatusCode(fasthttp.StatusUnauthorized)
			ctx.SetBodyString("unauthorized")
			return
		}
	}
	ctx.Response.Header.SetContentType("text/plain; version=0.0.4; charset=utf-8")
	ctx.SetStatusCode(200)
	var buf bytes.Buffer
	if _, err := s.metrics.WriteTo(&buf); err != nil {
		ctx.SetStatusCode(500)
		return
	}
	ctx.SetBody(buf.Bytes())
}

func (s *Server) match(path string) *compiledRoute {
	for _, r := range s.routes {
		p := r.cfg.Prefix
		if path == p || strings.HasPrefix(path, p+"/") {
			return r
		}
	}
	return nil
}

func (s *Server) serveRoutes(ctx *fasthttp.RequestCtx) {
	var b bytes.Buffer
	for _, r := range s.routes {
		fmt.Fprintf(&b, "%-24s %-16s -> %s (%s)", r.cfg.Name, r.cfg.Prefix, r.cfg.Upstream, r.cfg.Script)
		if r.rotator != nil {
			c := r.rotator.Config()
			fmt.Fprintf(&b, " rotation=%s creds=%d attempts=%d", c.Strategy, len(r.rotator.Credentials()), c.MaxAttempts)
		}
		b.WriteByte('\n')
	}
	ctx.SetContentType("text/plain; charset=utf-8")
	ctx.SetBody(b.Bytes())
}

func (s *Server) serve(ctx *fasthttp.RequestCtx, r *compiledRoute) {
	method := string(ctx.Method())
	origPath := string(ctx.Path())
	relPath := strings.TrimPrefix(origPath, r.cfg.Prefix)
	if relPath == "" {
		relPath = "/"
	}
	query := string(ctx.URI().QueryString())

	// ── request headers ──
	reqHeaders := map[string]string{}
	ctx.Request.Header.VisitAll(func(k, v []byte) {
		name := string(k)
		if hopByHop(name) || r.headers.dropped(name) {
			return
		}
		if strings.EqualFold(name, "content-length") {
			return
		}
		reqHeaders[name] = string(v)
	})
	for k, v := range r.headers.set {
		reqHeaders[k] = v
	}
	for k, v := range r.headers.generate() {
		reqHeaders[k] = v
	}

	// ── request body ──
	body := ctx.PostBody()
	res, err := r.script.RunRequest(&script.Request{
		Body:    string(body),
		Method:  method,
		Path:    relPath,
		Headers: reqHeaders,
	}, r.params)
	if err != nil {
		s.logf("[%s] request script: %v", r.cfg.Name, err)
		ctx.SetStatusCode(500)
		ctx.SetBodyString("request script failed: " + err.Error())
		return
	}
	outBody := body
	// A script may return headers only. In that case the body must survive
	// untouched; replacing it with "" would silently strip the payload.
	if res.Changed && res.Body != "" {
		outBody = []byte(res.Body)
	}
	for k, v := range res.Headers {
		reqHeaders[k] = v
	}
	// ── upstream request ──
	fullPath := r.upstream.basePath + relPath
	uri := r.upstream.scheme + "://" + r.upstream.host + fullPath
	if query != "" {
		uri += "?" + query
	}

	start := time.Now()
	resp, err := s.dispatch(r, method, uri, reqHeaders, outBody)
	if err != nil {
		s.logf("[%s] upstream: %v", r.cfg.Name, err)
		ctx.SetStatusCode(502)
		ctx.SetBodyString("upstream error: " + err.Error())
		return
	}
	defer fasthttp.ReleaseResponse(resp)
	latency := time.Since(start)

	// ── response headers ──
	ctx.SetStatusCode(resp.StatusCode())
	resp.Header.VisitAll(func(k, v []byte) {
		name := string(k)
		if hopByHop(name) || strings.EqualFold(name, "content-length") {
			return
		}
		ctx.Response.Header.Set(name, string(v))
	})

	contentType := string(resp.Header.ContentType())
	isSSE := r.isSSEContent(contentType, outBody)
	if isSSE {
		// The handler reads this back for the metrics label; the route's static
		// forceSSE flag is only a hint, the real answer comes from the response.
		ctx.SetUserValue(streamKey, true)
	}

	if isSSE {
		s.serveSSE(ctx, r, resp)
	} else {
		s.serveBuffered(ctx, r, resp)
	}

	if s.verbose {
		s.logf("[%s] %d %s %s %v", r.cfg.Name, resp.StatusCode(), method, origPath, latency)
	}
}

// dispatch sends the request once, or once per attempt when the route has a
// rotator, and returns the response the client should see. The caller owns the
// returned response and must release it.
//
// Retrying is safe because the whole upstream body is buffered here: nothing
// has been handed to the client yet, so a retry replaces the response before
// serveSSE or serveBuffered parses it. Each attempt rebuilds the request from
// the same outBody slice, so the body is byte-identical across attempts.
func (s *Server) dispatch(r *compiledRoute, method, uri string, reqHeaders map[string]string, outBody []byte) (*fasthttp.Response, error) {
	send := func(credHeaders map[string]string) (*fasthttp.Response, error) {
		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)
		req.Header.SetMethod(method)
		// Host must be set AFTER SetRequestURI: the URI parser fills Header.Host
		// from the absolute URL, and HostClient dials upstream.hostPort while
		// forwarding whatever Host header it finds. Leaving it empty made the
		// origin answer with a cross-protocol redirect.
		req.SetRequestURI(uri)
		req.Header.SetHost(r.upstream.host)
		for k, v := range reqHeaders {
			req.Header.Set(k, v)
		}
		// Credential headers go last: a credential wins over a static route
		// header of the same name.
		for k, v := range credHeaders {
			req.Header.Set(k, v)
		}
		req.Header.Del("Accept-Encoding")
		req.Header.Set("Accept-Encoding", "identity")
		req.SetBody(outBody)

		resp := fasthttp.AcquireResponse()
		resp.SkipBody = false
		if err := s.clients.For(r.upstream.hostPort, r.upstream.isTLS).DoTimeout(req, resp, r.timeout); err != nil {
			fasthttp.ReleaseResponse(resp)
			return nil, err
		}
		return resp, nil
	}

	if r.rotator == nil {
		return send(nil)
	}

	var keeper *fasthttp.Response
	_, attempts, err := r.rotator.Do(func(cred *rotation.Credential) (*rotation.Result, error) {
		resp, err := send(cred.Headers)
		if err != nil {
			return nil, err
		}
		// Keep the latest response so the client can be answered with it when
		// every credential fails. Earlier attempts are released immediately.
		if keeper != nil {
			fasthttp.ReleaseResponse(keeper)
		}
		keeper = resp
		return &rotation.Result{
			Status:  resp.StatusCode(),
			Headers: &resp.Header,
			Body:    resp.Body(),
		}, nil
	})
	if err != nil {
		if keeper != nil {
			fasthttp.ReleaseResponse(keeper)
		}
		return nil, err
	}
	if s.verbose {
		for i, a := range attempts {
			s.logf("[%s] attempt %d cred=%s status=%d class=%s", r.cfg.Name, i+1, a.Credential, a.Status, a.Class)
		}
	}
	if keeper == nil {
		// Every credential is cooling or dead and none was tried. Degrade to a
		// single unauthenticated attempt rather than failing a request the
		// upstream might still accept; the route's own headers still apply.
		s.logf("[%s] rotation: no credential available, sending without one", r.cfg.Name)
		return send(nil)
	}
	return keeper, nil
}

// isSSEContent decides whether the response is a stream. An explicit
// stream_sse in the route wins; otherwise Content-Type decides, falling back
// to the request's own "stream":true when the upstream is vague.
func (r *compiledRoute) isSSEContent(contentType string, reqBody []byte) bool {
	if r.forceSSE != nil {
		return *r.forceSSE
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	if strings.Contains(strings.ToLower(contentType), "application/json") {
		return false
	}
	return bytes.Contains(reqBody, []byte(`"stream":true`))
}

func (s *Server) serveSSE(ctx *fasthttp.RequestCtx, r *compiledRoute, resp *fasthttp.Response) {
	br := bufio.NewReaderSize(bytes.NewReader(resp.Body()), 64<<10)
	ctx.Response.Header.Set("Content-Type", "text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("X-Accel-Buffering", "no")
	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		if err := (&sseWriter{r: r, params: r.params}).write(br, w, w.Flush); err != nil {
			s.logf("[%s] sse: %v", r.cfg.Name, err)
		}
	})
}

func (s *Server) serveBuffered(ctx *fasthttp.RequestCtx, r *compiledRoute, resp *fasthttp.Response) {
	payload := resp.Body()
	res, err := r.script.RunResponse(&script.Response{
		Status:  resp.StatusCode(),
		Headers: headerMap(&resp.Header),
		Body:    string(payload),
	}, r.params)
	if err != nil {
		s.logf("[%s] response script: %v", r.cfg.Name, err)
		ctx.SetBody(payload)
		return
	}
	if res.Changed && res.Body != "" {
		ctx.SetBodyString(res.Body)
		return
	}
	ctx.SetBody(payload)
}

// Close releases idle upstream connections and the Lua VM pools held by every
// route. Without the script teardown a long-lived process that reloads routes
// would leak one VM pool per route per generation.
func (s *Server) Close() {
	s.clients.Close()
	seen := make(map[*script.Script]bool, len(s.routes))
	for _, r := range s.routes {
		if r.script == nil || seen[r.script] {
			continue
		}
		seen[r.script] = true
		r.script.Close()
	}
}

func (s *Server) logf(format string, args ...any) {
	s.log.Info(fmt.Sprintf(format, args...))
}

func headerMap(h *fasthttp.ResponseHeader) map[string]string {
	out := map[string]string{}
	h.VisitAll(func(k, v []byte) { out[string(k)] = string(v) })
	return out
}

// hopByHop reports headers that must not be forwarded by a proxy.
func hopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "host",
		"content-length", "content-encoding":
		return true
	}
	return false
}
