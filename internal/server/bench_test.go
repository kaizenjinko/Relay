package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"

	"github.com/baongo/relay/internal/config"
	"github.com/baongo/relay/internal/script"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// benchPayload returns a JSON body of approximately n bytes shaped like a
// chat-completions request, so scripts that decode and re-encode it do real
// work rather than walking a single key.
func benchPayload(n int) string {
	filler := strings.Repeat("x", 900)
	msg := map[string]string{"role": "user", "content": filler}
	var msgs []map[string]string
	body := map[string]any{
		"model":    "bench-model",
		"messages": msgs,
		"stream":   true,
	}
	for {
		msgs = append(msgs, msg)
		body["messages"] = msgs
		b, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		if len(b) >= n {
			return string(b)
		}
	}
}

// mustCompile compiles src under name, failing the benchmark otherwise.
func mustCompile(b *testing.B, name, src string) *script.Script {
	b.Helper()
	sc, err := script.Compile(name, src)
	if err != nil {
		b.Fatalf("compile %s: %v", name, err)
	}
	return sc
}

// benchServer builds a Server with a single route from literal TOML. It skips
// the file system round trip newTestRelay does, which is irrelevant to the
// per-request paths being measured.
// benchUpstream starts an httptest server and closes it when the benchmark
// ends; newTestUpstream takes *testing.T, which *testing.B is not.
func benchUpstream(b *testing.B, h http.HandlerFunc) string {
	b.Helper()
	srv := httptest.NewServer(h)
	b.Cleanup(srv.Close)
	return srv.URL
}

// serveBenchRelay starts srv on an in-memory listener and returns a
// keep-alive client pointing at it. The relay side is in-memory so the
// benchmark does not pay for loopback TCP; the upstream side is a real
// httptest server, which is where connection reuse is being measured.
func serveBenchRelay(b *testing.B, srv *Server) *fasthttp.Client {
	b.Helper()
	ln := fasthttputil.NewInmemoryListener()
	b.Cleanup(func() { _ = ln.Close() })
	fs := &fasthttp.Server{Handler: srv.Handler(), NoDefaultServerHeader: true, NoDefaultDate: true}
	go func() { _ = fs.Serve(ln) }()
	b.Cleanup(func() { _ = fs.Shutdown() })
	return &fasthttp.Client{
		Dial:                     func(string) (net.Conn, error) { return ln.Dial() },
		NoDefaultUserAgentHeader: true,
		MaxConnsPerHost:          512,
	}
}

func benchServer(b *testing.B, cfgTOML string, scripts map[string]*script.Script) *Server {
	b.Helper()
	dir := b.TempDir()
	cfgPath := filepath.Join(dir, "relay.toml")
	full := fmt.Sprintf("[server]\nscript_dir = %s\nmax_conns = 512\n", tomlQuote(dir)) + cfgTOML
	if err := os.WriteFile(cfgPath, []byte(full), 0o644); err != nil {
		b.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		b.Fatalf("config.Load: %v", err)
	}
	srv, err := New(cfg, scripts)
	if err != nil {
		b.Fatalf("server.New: %v", err)
	}
	return srv
}

// ── BenchmarkServe_NoOpScript ───────────────────────────────────────────────

// BenchmarkServe_NoOpScript measures the full relay hop — header pipeline,
// request script, upstream round trip, response script — for a 2KB JSON body
// and a script that returns nil on both sides. The upstream is an httptest
// server, so the connection is a real TCP connection and keep-alive reuse is
// what the client pool is supposed to provide.
func BenchmarkServe_NoOpScript(b *testing.B) {
	payload := benchPayload(2048)
	upURL := benchUpstream(b, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bench","choices":[{"message":{"content":"ok"}}]}`))
	})
	srv := benchServer(b, relayTOML("/bench", upURL, "nop.lua"), map[string]*script.Script{
		"nop.lua": mustCompile(b, "nop.lua", "return nil"),
	})

	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()
	fs := &fasthttp.Server{Handler: srv.Handler(), NoDefaultServerHeader: true, NoDefaultDate: true}
	go fs.Serve(ln)
	defer fs.Shutdown()

	client := &fasthttp.Client{
		Dial:                     func(string) (net.Conn, error) { return ln.Dial() },
		NoDefaultUserAgentHeader: true,
		MaxConnsPerHost:          512,
	}
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI("http://relay/bench/v1/chat/completions")
	req.Header.SetContentType("application/json")
	req.Header.SetHost(ln.Addr().String())
	req.SetBodyString(payload)
	if err := client.DoTimeout(req, resp, 10*time.Second); err != nil {
		b.Fatalf("warmup: %v", err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.DoTimeout(req, resp, 10*time.Second); err != nil {
			b.Fatalf("request %d: %v", i, err)
		}
		if resp.StatusCode() != 200 {
			b.Fatalf("request %d: status %d", i, resp.StatusCode())
		}
	}
}

// ── BenchmarkServe_SSE_20Chunks ─────────────────────────────────────────────

// sseStream builds an SSE body of n data lines plus the [DONE] terminator.
func sseStream(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, `data: {"id":"c","choices":[{"delta":{"content":"chunk-%02d"}}]}`+"\n\n", i)
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// BenchmarkServe_SSE_20Chunks measures the stream path: 20 data lines, each
// run through the response script, then the [DONE] terminator passed through
// untouched.
func BenchmarkServe_SSE_20Chunks(b *testing.B) {
	stream := sseStream(20)
	upURL := benchUpstream(b, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	})
	srv := benchServer(b, relayTOML("/bench", upURL, "sse.lua"), map[string]*script.Script{
		"sse.lua": mustCompile(b, "sse.lua", "return nil"),
	})

	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()
	fs := &fasthttp.Server{Handler: srv.Handler(), NoDefaultServerHeader: true, NoDefaultDate: true}
	go fs.Serve(ln)
	defer fs.Shutdown()

	client := &fasthttp.Client{
		Dial:                     func(string) (net.Conn, error) { return ln.Dial() },
		NoDefaultUserAgentHeader: true,
		MaxConnsPerHost:          512,
	}
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI("http://relay/bench/v1/chat/completions")
	req.Header.SetHost(ln.Addr().String())
	req.SetBodyString(`{"model":"m","stream":true}`)
	if err := client.DoTimeout(req, resp, 10*time.Second); err != nil {
		b.Fatalf("warmup: %v", err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.DoTimeout(req, resp, 10*time.Second); err != nil {
			b.Fatalf("request %d: %v", i, err)
		}
		if got := len(resp.Body()); got != len(stream) {
			b.Fatalf("request %d: %d body bytes, want %d", i, got, len(stream))
		}
	}
}

// ── BenchmarkHeadersGenerate ────────────────────────────────────────────────

// BenchmarkHeadersGenerate measures the header pipeline alone: compile once,
// then render a mixed set (static overrides, dynamic hex/uuid/static kinds)
// per iteration against a real RequestHeader.
func BenchmarkHeadersGenerate(b *testing.B) {
	cfgTOML := relayTOML("/bench", "http://127.0.0.1:1", "s.lua") + `
[route.headers.set]
X-Api-Key = "server-key"
X-Another = "fixed"
[route.headers]
drop = ["X-Drop-Exact", "x-secret-*"]
[route.headers.dynamic.X-Hex]
kind = "hex"
bytes = 8
[route.headers.dynamic.X-Uuid]
kind = "uuid"
[route.headers.dynamic.X-Static]
kind = "static"
value = "v"
`
	srv := benchServer(b, cfgTOML, map[string]*script.Script{
		"s.lua": mustCompile(b, "s.lua", "return nil"),
	})
	hs := srv.routes[0].headers

	var clientReq fasthttp.RequestHeader
	clientReq.SetMethod("POST")
	clientReq.SetContentType("application/json")
	clientReq.Set("Authorization", "Bearer abc")
	clientReq.Set("X-Drop-Exact", "gone")
	clientReq.Set("X-Secret-One", "gone")
	clientReq.Set("X-Kept", "yes")
	clientReq.Set("User-Agent", "bench/1")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := make(map[string]string, 20)
		clientReq.VisitAll(func(k, v []byte) {
			name := string(k)
			if hopByHop(name) || hs.dropped(name) || strings.EqualFold(name, "content-length") {
				return
			}
			m[name] = string(v)
		})
		for k, v := range hs.set {
			m[k] = v
		}
		for k, v := range hs.generate() {
			m[k] = v
		}
	}
}

// ── BenchmarkLuaRun_NoChange ────────────────────────────────────────────────

// BenchmarkLuaRun_NoChange measures the cost floor of the script API: build a
// fresh VM, install the globals, run a chunk that returns nil.
func BenchmarkLuaRun_NoChange(b *testing.B) {
	sc := mustCompile(b, "nop.lua", "return nil")
	payload := benchPayload(2048)
	req := &script.Request{Body: payload, Method: "POST", Path: "/v1/chat", Headers: benchHeaders()}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := sc.RunRequest(req, nil)
		if err != nil {
			b.Fatal(err)
		}
		if res.Changed {
			b.Fatal("no-change script reported a change")
		}
	}
}

// ── BenchmarkLuaRun_Transform ───────────────────────────────────────────────

// BenchmarkLuaRun_Transform measures a script that decodes the request body,
// mutates one key and re-encodes it: the realistic transform, not the floor.
func BenchmarkLuaRun_Transform(b *testing.B) {
	sc := mustCompile(b, "tx.lua", `
local d = json.decode(req.body)
d.temperature = 0.2
return json.encode(d)
`)
	payload := benchPayload(2048)
	r := &script.Request{Body: payload, Method: "POST", Path: "/v1/chat", Headers: benchHeaders()}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := sc.RunRequest(r, nil)
		if err != nil {
			b.Fatal(err)
		}
		if !res.Changed || len(res.Body) < len(payload)/2 {
			b.Fatalf("transform did not produce a body (%d bytes)", len(res.Body))
		}
	}
}

func benchHeaders() map[string]string {
	return map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer bench",
		"User-Agent":    "bench/1",
	}
}

// ── BenchmarkServe_Parallel ─────────────────────────────────────────────────

// BenchmarkServe_Parallel drives the relay from many goroutines at once to show
// what the client pool buys under contention: shared connections instead of
// one handshake per request per goroutine.
func BenchmarkServe_Parallel(b *testing.B) {
	payload := benchPayload(2048)
	upURL := benchUpstream(b, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bench","choices":[{"message":{"content":"ok"}}]}`))
	})
	srv := benchServer(b, relayTOML("/bench", upURL, "nop.lua"), map[string]*script.Script{
		"nop.lua": mustCompile(b, "nop.lua", "return nil"),
	})

	ln := fasthttputil.NewInmemoryListener()
	b.Cleanup(func() { _ = ln.Close() })
	fs := &fasthttp.Server{Handler: srv.Handler(), NoDefaultServerHeader: true, NoDefaultDate: true}
	go func() { _ = fs.Serve(ln) }()
	b.Cleanup(func() { _ = fs.Shutdown() })
	addr := ln.Addr().String()

	var failed atomic.Int64
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		client := &fasthttp.Client{
			Dial:                     func(string) (net.Conn, error) { return ln.Dial() },
			NoDefaultUserAgentHeader: true,
			MaxConnsPerHost:          512,
		}
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.Header.SetMethod(http.MethodPost)
		req.SetRequestURI("http://relay/bench/v1/chat/completions")
		req.Header.SetHost(addr)
		req.SetBodyString(payload)
		for pb.Next() {
			if err := client.DoTimeout(req, resp, 10*time.Second); err != nil || resp.StatusCode() != 200 {
				failed.Add(1)
			}
		}
	})
	if n := failed.Load(); n > 0 {
		b.Fatalf("%d parallel requests failed", n)
	}
}
