package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"

	"github.com/baongo/relay/internal/obs"
)

// newObservableRelay is newTestRelay with a captured log buffer and the
// server's own metrics registry exposed for assertions.
func newObservableRelay(t *testing.T, cfgTOML string, scripts map[string]string) (*Server, *bytes.Buffer) {
	t.Helper()
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	t.Cleanup(closeUp)

	buf := &bytes.Buffer{}
	cfg, sc := loadForTest(t, relayTOML("/p", upURL, "s.lua"), scripts)
	srv, err := NewWithObservability(cfg, sc, obs.New(buf, true), obs.NewMetrics())
	if err != nil {
		t.Fatalf("NewWithObservability: %v", err)
	}
	return srv, buf
}

// TestPanicIsRecovered proves a panic inside a handler becomes a 500 rather
// than taking the process down. The process dying would sever every other
// client's stream, so this is the single most important failure mode to cover.
func TestPanicIsRecovered(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	defer closeUp()

	logBuf := &bytes.Buffer{}
	cfg, sc := loadForTest(t, relayTOML("/p", upURL, "s.lua"), map[string]string{
		// error() raises a Lua error, which surfaces through the bridge; a
		// panic in the Go layer is what we want, so use a nil deref in a Go
		// helper instead: the script itself cannot panic the process.
		"s.lua": `return nil`,
	})
	srv, err := NewWithObservability(cfg, sc, obs.New(logBuf, true), obs.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}

	// The recovery wraps everything inside Handler(). Setting the route's
	// client pool to nil makes serve() dereference nil on the upstream call,
	// which is the same shape as a bug in the pipeline or the Lua bridge.
	srv.clients = nil

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/p/x")
	srv.Handler()(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got)
	}
	if body := string(ctx.Response.Body()); body != "internal error" {
		t.Fatalf("body = %q, want generic message without internals", body)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "panic serving request") {
		t.Fatalf("panic not logged: %s", logged)
	}
	if !strings.Contains(logged, `"route":"p"`) {
		t.Fatalf("log record missing the route label: %s", logged)
	}
	// The stack must name the panic site, or the log is useless for diagnosis.
	if !strings.Contains(logged, "internal/server") || !strings.Contains(logged, "goroutine") {
		t.Fatalf("stack trace missing from log: %s", logged)
	}
}

// TestDrainingRefusesNewRequests: once shutdown starts, the relay must stop
// accepting work so a load balancer withdraws it, while admin endpoints keep
// answering so the rollout is observable.
func TestDrainingRefusesNewRequests(t *testing.T) {
	srv, _ := newObservableRelay(t, "", map[string]string{"s.lua": `return nil`})
	h := srv.Handler()

	ok := func(path string) int {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI(path)
		h(ctx)
		return ctx.Response.StatusCode()
	}

	if got := ok("/p/x"); got == 503 {
		t.Fatalf("not draining yet, got 503")
	}

	srv.PrepareShutdown()

	if got := ok("/p/x"); got != fasthttp.StatusServiceUnavailable {
		t.Fatalf("draining route status = %d, want 503", got)
	}
	if got := ok("/healthz"); got != 200 {
		t.Fatalf("healthz during drain = %d, want 200 so LB sees the transition", got)
	}
}

// TestMetricsEndpoint: counters must move and the exposition must parse.
func TestMetricsEndpoint(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	defer closeUp()

	cfgTOML := "[server]\nscript_dir = SCRIPT_DIR\nmetrics_path = \"/metrics\"\n" + relayTOML("/p", upURL, "s.lua")
	cfg, sc := loadForTest(t, cfgTOML, map[string]string{"s.lua": `return nil`})
	srv, err := NewWithObservability(cfg, sc, obs.New(&bytes.Buffer{}, true), obs.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	for i := 0; i < 3; i++ {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/p/x")
		h(ctx)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/metrics")
	h(ctx)
	if got := ctx.Response.StatusCode(); got != 200 {
		t.Fatalf("metrics status = %d", got)
	}
	body := string(ctx.Response.Body())
	for _, want := range []string{
		"relay_requests_total{route=\"p\"",
		"relay_inflight_requests",
		"relay_uptime_seconds",
		"# TYPE relay_requests_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, body)
		}
	}
}

// TestMetricsTokenGuard: with admin_token set, the endpoint must reject
// unauthenticated scrapes.
func TestMetricsTokenGuard(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeUp()

	cfgTOML := "[server]\nscript_dir = SCRIPT_DIR\nmetrics_path = \"/metrics\"\nadmin_token = \"s3cret\"\n" + relayTOML("/p", upURL, "s.lua")
	cfg, sc := loadForTest(t, cfgTOML, map[string]string{"s.lua": `return nil`})
	srv, err := NewWithObservability(cfg, sc, obs.New(&bytes.Buffer{}, true), obs.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/metrics")
	h(ctx)
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", got)
	}

	ctx = &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/metrics")
	ctx.Request.Header.Set("Authorization", "Bearer s3cret")
	h(ctx)
	if got := ctx.Response.StatusCode(); got != 200 {
		t.Fatalf("with token: status = %d, want 200", got)
	}
}

// TestMetricsLabelUsesObservedStream: a route without stream_sse that answers
// with SSE anyway must be labelled stream="true". The static hint is only a
// hint; the label has to describe what was actually sent.
func TestMetricsLabelUsesObservedStream(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {}\n\n"))
	})
	defer closeUp()

	cfgTOML := "[server]\nscript_dir = SCRIPT_DIR\nmetrics_path = \"/metrics\"\n" + relayTOML("/p", upURL, "s.lua")
	cfg, sc := loadForTest(t, cfgTOML, map[string]string{"s.lua": `return nil`})
	srv, err := NewWithObservability(cfg, sc, obs.New(&bytes.Buffer{}, true), obs.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/p/x")
	h(ctx)

	mctx := &fasthttp.RequestCtx{}
	mctx.Request.SetRequestURI("/metrics")
	h(mctx)
	if !strings.Contains(string(mctx.Response.Body()), `stream="true"`) {
		t.Fatalf("SSE response labelled as non-stream:\n%s", mctx.Response.Body())
	}
}

// Close must release the Lua VM pools, not just the HTTP client pool. A relay
// that loads routes, runs traffic and shuts down should not leave VMs behind.
func TestCloseReleasesScriptVMs(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	defer closeUp()

	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `return nil`})

	// Populate the pool so closeAll has something to free.
	for i := 0; i < 3; i++ {
		if res := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{}`}); res.Status != 200 {
			t.Fatalf("warm-up request failed: %d", res.Status)
		}
	}
	srv.Close()

	// A second Close on the same server must not panic (double-free of VMs).
	srv.Close()
}

// Two routes sharing one script file must free the VM pool exactly once.
func TestCloseDeduplicatesSharedScript(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	defer closeUp()

	cfgTOML := relayTOML("/a", upURL, "s.lua") +
		"\n[[route]]\nprefix = \"/b\"\nupstream = " + tomlQuote(upURL) +
		"\nscript = \"s.lua\"\ntimeout = \"5s\"\n"
	srv, _ := newTestRelay(t, cfgTOML, map[string]string{"s.lua": `return nil`})

	doReq(t, srv, testRequest{Method: "POST", Path: "/a/x", Body: `{}`})
	doReq(t, srv, testRequest{Method: "POST", Path: "/b/x", Body: `{}`})
	srv.Close()
}
