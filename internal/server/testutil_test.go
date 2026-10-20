package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"

	"github.com/baongo/relay/internal/config"
	"github.com/baongo/relay/internal/script"
)

// newTestUpstream starts an httptest server for the given handler and returns
// its base URL plus an idempotent close function.
func newTestUpstream(t *testing.T, h http.HandlerFunc) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	var once sync.Once
	closeFn := func() { once.Do(srv.Close) }
	t.Cleanup(closeFn)
	return srv.URL, closeFn
}

// loadTestRelay writes cfgTOML to a temp file, writes scripts into the same
// directory, loads the config, compiles the scripts and builds the Server.
// The scripts map is keyed by the `script` value as written in TOML, matching
// what main.go does.
func newTestRelay(t *testing.T, cfgTOML string, scripts map[string]string) (*Server, func()) {
	t.Helper()
	cfg, compiled := loadForTest(t, cfgTOML, scripts)
	srv, err := New(cfg, compiled)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv, func() {}
}

// loadForTest writes the config and scripts, then returns the parsed config and
// the compiled scripts without constructing a Server. Tests that need to inject
// a logger or metrics use this and call NewWithObservability themselves.
func loadForTest(t *testing.T, cfgTOML string, scripts map[string]string) (*config.Config, map[string]*script.Script) {
	t.Helper()
	dir := t.TempDir()

	for name, src := range scripts {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for script %s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatalf("write script %s: %v", name, err)
		}
	}

	cfgPath := filepath.Join(dir, "relay.toml")
	if !strings.Contains(cfgTOML, "script_dir") {
		cfgTOML = "[server]\nscript_dir = " + tomlQuote(dir) + "\n" + cfgTOML
	} else {
		cfgTOML = strings.Replace(cfgTOML, "SCRIPT_DIR", tomlQuote(dir), 1)
	}
	if err := os.WriteFile(cfgPath, []byte(cfgTOML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	paths, err := cfg.ScriptPaths()
	if err != nil {
		t.Fatalf("ScriptPaths: %v", err)
	}
	compiled := make(map[string]*script.Script, len(paths))
	for i, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read script %s: %v", p, err)
		}
		sc, err := script.Compile(cfg.Routes[i].Name, string(src))
		if err != nil {
			t.Fatalf("compile script %s: %v", p, err)
		}
		compiled[cfg.Routes[i].Script] = sc
	}
	return cfg, compiled
}

// tomlQuote renders s as a TOML basic string.
func tomlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// testResponse is what a client sees back through the relay.
type testResponse struct {
	Status  int
	Body    string
	Headers map[string]string
}

// Header returns a response header by name (case-insensitive), "" if absent.
func (r *testResponse) Header(name string) string {
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// testRequest describes one call into the relay handler.
type testRequest struct {
	Method string
	Path   string // path plus optional "?query"
	Body   string
	Header map[string]string
	Host   string // overrides the Host header
}

// doReq runs one request against srv.Handler() over an in-memory listener and
// returns what the client received.
func doReq(t *testing.T, srv *Server, r testRequest) *testResponse {
	t.Helper()
	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	fs := &fasthttp.Server{
		Handler:                       srv.Handler(),
		NoDefaultServerHeader:         true,
		NoDefaultDate:                 true,
		DisableHeaderNamesNormalizing: true,
	}
	done := make(chan error, 1)
	go func() { done <- fs.Serve(ln) }()
	defer fs.Shutdown()

	client := &fasthttp.Client{
		Dial:                     func(addr string) (net.Conn, error) { return ln.Dial() },
		NoDefaultUserAgentHeader: true,
	}

	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethod(method)
	req.SetRequestURI(r.Path)
	host := r.Host
	if host == "" {
		host = ln.Addr().String()
	}
	req.Header.SetHost(host)
	for k, v := range r.Header {
		switch strings.ToLower(k) {
		case "host":
			req.Header.SetHost(v)
		case "content-type":
			req.Header.SetContentType(v)
		case "connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade":
			req.Header.Add(k, v)
		default:
			req.Header.Set(k, v)
		}
	}
	if r.Body != "" {
		req.SetBodyString(r.Body)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		err := client.DoTimeout(req, resp, time.Until(deadline))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s: %v", method, r.Path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	out := &testResponse{
		Status:  resp.StatusCode(),
		Body:    string(resp.Body()),
		Headers: map[string]string{},
	}
	resp.Header.VisitAll(func(k, v []byte) {
		out.Headers[string(k)] = string(v)
	})
	return out
}

// mustDoReq is doReq with the status assertion folded in.
func mustDoReq(t *testing.T, srv *Server, r testRequest) *testResponse {
	t.Helper()
	resp := doReq(t, srv, r)
	return resp
}

// recordUpstream returns an httptest server that records the last request it
// saw and replies with a fixed body.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   []byte
	Header headerView
	Host   string
}

// headerView stores upstream headers under lowercased names plus the raw map.
type headerView struct {
	values map[string]string
}

func newHeaderView(h http.Header) headerView {
	v := headerView{values: make(map[string]string, len(h))}
	for k, vals := range h {
		v.values[strings.ToLower(k)] = strings.Join(vals, ", ")
	}
	return v
}

// Get returns a header value by name, case-insensitively.
func (v headerView) Get(name string) string { return v.values[strings.ToLower(name)] }

// Has reports whether the header was present.
func (v headerView) Has(name string) bool { _, ok := v.values[strings.ToLower(name)]; return ok }

type recordingUpstream struct {
	mu   sync.Mutex
	reqs []recordedRequest
	URL  string
}

func (u *recordingUpstream) last() recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		return recordedRequest{}
	}
	return u.reqs[len(u.reqs)-1]
}

func (u *recordingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

// newRecordingUpstream records every request and answers 200 with body.
func newRecordingUpstream(t *testing.T, body string) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	url, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := readAll(r)
		u.mu.Lock()
		u.reqs = append(u.reqs, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Body:   b,
			Header: newHeaderView(r.Header),
			Host:   r.Host,
		})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	u.URL = url
	return u
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, nil
		}
	}
}

// freePort returns a localhost address that nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// relayTOML builds a one-route config against upstream with the given prefix.
func relayTOML(prefix, upstream, scriptName string) string {
	return fmt.Sprintf(`
[[route]]
prefix = %s
upstream = %s
script = %s
timeout = "5s"
`, tomlQuote(prefix), tomlQuote(upstream), tomlQuote(scriptName))
}
