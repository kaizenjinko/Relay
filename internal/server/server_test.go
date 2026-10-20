package server

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"

	"github.com/baongo/relay/internal/config"
	"github.com/baongo/relay/internal/script"
)

// ── A. route matching ───────────────────────────────────────────────────────

func TestRouteMatch_PrefixBoundary(t *testing.T) {
	up := newRecordingUpstream(t, "matched")
	srv, _ := newTestRelay(t, relayTOML("/api", up.URL, "s.lua"), map[string]string{"s.lua": "return nil"})

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantHit    bool
	}{
		{name: "exact prefix", path: "/api", wantStatus: 200, wantHit: true},
		{name: "child path", path: "/api/x", wantStatus: 200, wantHit: true},
		{name: "deep child", path: "/api/v1/chat", wantStatus: 200, wantHit: true},
		{name: "sibling with same stem", path: "/apix", wantStatus: 404, wantHit: false},
		{name: "sibling no slash", path: "/apix/1", wantStatus: 404, wantHit: false},
		{name: "longer prefix without slash", path: "/apiextra", wantStatus: 404, wantHit: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := up.count()
			resp := doReq(t, srv, testRequest{Path: tt.path})
			if resp.Status != tt.wantStatus {
				t.Fatalf("%s: status = %d, want %d", tt.path, resp.Status, tt.wantStatus)
			}
			if hit := up.count() > before; hit != tt.wantHit {
				t.Fatalf("%s: upstream hit = %v, want %v", tt.path, hit, tt.wantHit)
			}
			if !tt.wantHit && !strings.Contains(resp.Body, "no route") {
				t.Fatalf("%s: body = %q, want substring \"no route\"", tt.path, resp.Body)
			}
		})
	}
}

func TestRouteMatch_NoRouteBody(t *testing.T) {
	up := newRecordingUpstream(t, "x")
	srv, _ := newTestRelay(t, relayTOML("/api", up.URL, "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/nowhere/deep"})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Status)
	}
	if !strings.Contains(resp.Body, "no route") {
		t.Fatalf("body = %q, want it to mention \"no route\"", resp.Body)
	}
	if up.count() != 0 {
		t.Fatalf("upstream hit %d times for an unmatched path", up.count())
	}
}

func TestRouteMatch_LongestPrefixWins(t *testing.T) {
	short := newRecordingUpstream(t, `{"route":"short"}`)
	long := newRecordingUpstream(t, `{"route":"long"}`)

	// Declaration order is deliberately "wrong": the short prefix comes first.
	cfg := fmt.Sprintf(`
[[route]]
name = "short"
prefix = "/api"
upstream = %s
script = "s.lua"

[[route]]
name = "long"
prefix = "/api/v1"
upstream = %s
script = "s.lua"
`, tomlQuote(short.URL), tomlQuote(long.URL))
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	tests := []struct {
		path     string
		wantLong bool
	}{
		{path: "/api/v1/chat", wantLong: true},
		{path: "/api/v1", wantLong: true},
		{path: "/api/other", wantLong: false},
		{path: "/api", wantLong: false},
		{path: "/api/v10", wantLong: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp := doReq(t, srv, testRequest{Path: tt.path})
			if resp.Status != 200 {
				t.Fatalf("status = %d, want 200", resp.Status)
			}
			var want, other string
			if tt.wantLong {
				want, other = `{"route":"long"}`, `{"route":"short"}`
			} else {
				want, other = `{"route":"short"}`, `{"route":"long"}`
			}
			if resp.Body != want {
				t.Fatalf("%s: body = %q, want %q (other route %q)", tt.path, resp.Body, want, other)
			}
		})
	}

	// /api/v10 must not be swallowed by /api/v1.
	if short.count() == 0 {
		t.Fatal("/api/v10 should have gone to the /api route")
	}
	if long.count() < 2 {
		t.Fatalf("/api/v1 requests missed the long route (long hits = %d)", long.count())
	}
}

// ── B. path and query rewriting ─────────────────────────────────────────────

func TestRewrite_StripPrefixAndKeepQuery(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL+"/base", "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Method: "POST", Path: "/p/v1/chat?a=1&b=2", Body: `{}`})
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	got := up.last()
	if got.Path != "/base/v1/chat" {
		t.Fatalf("upstream path = %q, want %q", got.Path, "/base/v1/chat")
	}
	if got.Query != "a=1&b=2" {
		t.Fatalf("upstream query = %q, want %q", got.Query, "a=1&b=2")
	}
}

func TestRewrite_PrefixEqualsPath(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL+"/base", "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/p"})
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	got := up.last().Path
	if got != "/base/" && got != "/base" {
		t.Fatalf("upstream path = %q, want /base or /base/ (the prefix must not be re-appended)", got)
	}
	if strings.HasSuffix(got, "/p") {
		t.Fatalf("upstream path = %q: route prefix was not stripped", got)
	}
}

func TestRewrite_TrailingSlashUpstream(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL+"/base/", "s.lua"), map[string]string{"s.lua": "return nil"})

	if resp := doReq(t, srv, testRequest{Path: "/p/x"}); resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if got := up.last().Path; got != "/base/x" {
		t.Fatalf("upstream path = %q, want /base/x", got)
	}
}

func TestRewrite_NoBasePath(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": "return nil"})

	if resp := doReq(t, srv, testRequest{Path: "/p/x"}); resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if got := up.last().Path; got != "/x" {
		t.Fatalf("upstream path = %q, want /x", got)
	}
}

// ── C. header pipeline ──────────────────────────────────────────────────────

func TestHeaders_SetOverridesClientHeader(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + `
[route.headers.set]
X-Api-Key = "server-key"
X-Added = "yes"
`
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x", Header: map[string]string{"X-Api-Key": "client-key"}})
	h := up.last().Header
	if got := h.Get("X-Api-Key"); got != "server-key" {
		t.Fatalf("X-Api-Key = %q, want server-key", got)
	}
	if got := h.Get("X-Added"); got != "yes" {
		t.Fatalf("X-Added = %q, want yes", got)
	}
}

func TestHeaders_DropExactAndWildcard(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + `
[route.headers]
drop = ["X-Exact", "x-secret-*"]
`
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x", Header: map[string]string{
		"X-Exact":       "drop-me",
		"x-exact":       "case",
		"X-Secret-One":  "1",
		"X-Secret-Two":  "2",
		"X-Secretive":   "keep-sibling",
		"X-Unrelated":   "keep",
		"Authorization": "Bearer t",
	}})
	h := up.last().Header
	for _, name := range []string{"X-Exact", "X-Secret-One", "X-Secret-Two"} {
		if v := h.Get(name); v != "" {
			t.Fatalf("%s forwarded upstream as %q, want dropped", name, v)
		}
	}
	for _, name := range []string{"X-Secretive", "X-Unrelated", "Authorization"} {
		if !h.Has(name) {
			t.Fatalf("%s was dropped, want kept", name)
		}
	}
}

func TestHeaders_DynamicKinds(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(filePath, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_TEST_TOKEN", "env-token")

	up := newRecordingUpstream(t, "ok")
	cfg := fmt.Sprintf(`
[[route]]
prefix = "/p"
upstream = %s
script = "s.lua"

[route.headers.dynamic.X-Hex]
kind = "hex"
prefix = "h_"
bytes = 8

[route.headers.dynamic.X-HexDefault]
kind = "hex"

[route.headers.dynamic.X-UUID]
kind = "uuid"
prefix = "u_"

[route.headers.dynamic.X-Static]
kind = "static"
value = "fixed"
prefix = "s_"

[route.headers.dynamic.X-Env]
kind = "env"
name = "RELAY_TEST_TOKEN"

[route.headers.dynamic.X-File]
kind = "file"
path = %s
`, tomlQuote(up.URL), tomlQuote(filePath))
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	hexRe := regexp.MustCompile(`^h_[0-9a-f]{16}$`)
	defaultHexRe := regexp.MustCompile(`^[0-9a-f]{26}$`)
	uuidRe := regexp.MustCompile(`^u_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	if resp := doReq(t, srv, testRequest{Path: "/p/x"}); resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	h := up.last().Header

	if got := h.Get("X-Hex"); !hexRe.MatchString(got) {
		t.Fatalf("X-Hex = %q, want h_ + 16 hex chars", got)
	}
	if got := h.Get("X-HexDefault"); !defaultHexRe.MatchString(got) {
		t.Fatalf("X-HexDefault = %q, want 26 hex chars (13 bytes default)", got)
	}
	if got := h.Get("X-UUID"); !uuidRe.MatchString(got) {
		t.Fatalf("X-UUID = %q, want u_ + UUID v4", got)
	}
	if got := h.Get("X-Static"); got != "s_fixed" {
		t.Fatalf("X-Static = %q, want s_fixed", got)
	}
	if got := h.Get("X-Env"); got != "env-token" {
		t.Fatalf("X-Env = %q, want env-token", got)
	}
	if got := h.Get("X-File"); got != "file-token" {
		t.Fatalf("X-File = %q, want trimmed file-token", got)
	}

	// Dynamic values must be regenerated per request (not baked at startup).
	first := h.Get("X-UUID")
	doReq(t, srv, testRequest{Path: "/p/x"})
	if second := up.last().Header.Get("X-UUID"); second == first {
		t.Fatalf("X-UUID unchanged across requests (%q); dynamic header is static", first)
	}
}

func TestHeaders_DynamicOverridesClientValue(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + `
[route.headers.dynamic.X-Gen]
kind = "static"
value = "gen"
`
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x", Header: map[string]string{"X-Gen": "client"}})
	if got := up.last().Header.Get("X-Gen"); got != "gen" {
		t.Fatalf("X-Gen = %q, want gen", got)
	}
}

func TestHeaders_HopByHopNotForwarded(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{
		Method: "POST",
		Path:   "/p/x",
		Body:   "hello",
		Header: map[string]string{
			"Connection":          "keep-alive, x-nominated",
			"x-nominated":         "should-follow-connection",
			"Keep-Alive":          "timeout=5",
			"Transfer-Encoding":   "chunked",
			"TE":                  "trailers",
			"Upgrade":             "h2c",
			"Proxy-Authorization": "Basic zzz",
			"Content-Encoding":    "gzip",
			"X-Normal":            "forwarded",
		},
	})
	h := up.last().Header
	for _, name := range []string{"Keep-Alive", "TE", "Upgrade", "Proxy-Authorization", "Content-Encoding"} {
		if v := h.Get(name); v != "" {
			t.Fatalf("%s forwarded upstream as %q, want stripped", name, v)
		}
	}
	if v := h.Get("Connection"); v != "" && strings.Contains(v, "x-nominated") {
		t.Fatalf("Connection forwarded upstream as %q, want stripped", v)
	}
	if got := h.Get("X-Normal"); got != "forwarded" {
		t.Fatalf("X-Normal = %q, want forwarded", got)
	}
	if got := string(up.last().Body); got != "hello" {
		t.Fatalf("body = %q, want hello (hop-by-hop stripping must not corrupt the body)", got)
	}
}

func TestHeaders_ClientHostNotForwarded(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x", Host: "client.example:9999"})
	got := up.last().Host
	if strings.Contains(got, "client.example") {
		t.Fatalf("upstream Host = %q, want the upstream's own host, not the client's", got)
	}
	if got == "" {
		t.Fatal("upstream Host is empty")
	}
}

func TestHeaders_ContentLengthNotForwarded(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": `return "a much longer body than the client sent"`})

	// The client declares a content-length matching its own body; after the
	// script rewrites the body the forwarded header must be recomputed, not
	// copied, or the upstream would read a truncated body.
	doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: "short"})
	got := string(up.last().Body)
	if got != "a much longer body than the client sent" {
		t.Fatalf("upstream body = %q, want the rewritten body in full", got)
	}
	if cl := up.last().Header.Get("Content-Length"); cl != fmt.Sprint(len(got)) {
		t.Fatalf("upstream Content-Length = %q, want %d", cl, len(got))
	}
}

// ── D. request body transform ───────────────────────────────────────────────

func TestRequestScript_StringReturnRewritesBody(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + `
[route.params]
temperature = 0.2
`
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": `
if not req then return nil end
local d = json.decode(req.body)
d.model = "rewritten"
d.temperature = params.temperature
return json.encode(d)
`})

	doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{"model":"original","messages":[{"role":"user"}]}`})
	got := string(up.last().Body)
	want := `{"messages":[{"role":"user"}],"model":"rewritten","temperature":0.2}`
	if got != want {
		t.Fatalf("upstream body = %q, want %q", got, want)
	}
}

func TestRequestScript_NilReturnKeepsOriginalBytes(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": `
if req and string.find(req.body, "h\195\169llo", 1, true) == nil then
  error("request body did not reach the script intact")
end
return nil
`})

	// Deliberately awkward bytes: odd whitespace, unicode and key order that
	// any decode/encode round-trip would change. The trailing NUL is legal in
	// a Lua string and finalises the upstream Content-Length, which guards
	// against an accidental TrimSpace somewhere in the pipeline.
	original := "{\n  \"z\": 1,   \"a\": \"h\u00e9llo\",\n \"nested\": {\"y\": [1,2,3]}\n}\x00"
	doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: original})
	if got := up.last().Body; !bytes.Equal(got, []byte(original)) {
		t.Fatalf("upstream body = %q, want the original %q byte-for-byte", got, original)
	}
}

func TestRequestScript_TableReturnAppliesBodyAndHeaders(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": `
return {
  body = req.body .. "|suffixed",
  headers = { ["X-Script"] = "added", ["X-Override"] = "script-wins" },
}
`})

	doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: "base", Header: map[string]string{"X-Override": "client"}})
	last := up.last()
	if got := string(last.Body); got != "base|suffixed" {
		t.Fatalf("upstream body = %q, want %q", got, "base|suffixed")
	}
	if got := last.Header.Get("X-Script"); got != "added" {
		t.Fatalf("X-Script = %q, want added", got)
	}
	if got := last.Header.Get("X-Override"); got != "script-wins" {
		t.Fatalf("X-Override = %q, want script-wins", got)
	}
}

func TestRequestScript_SeesMethodPathHeadersAndParams(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + `
[route.params]
tenant = "acme"
`
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": `
if not req then return nil end
if req.method ~= "PUT" then error("method: " .. tostring(req.method)) end
if req.path ~= "/v2/thing" then error("path: " .. tostring(req.path)) end
if req.headers["X-Client"] ~= "abc" then error("header missing") end
if params.tenant ~= "acme" then error("params missing") end
return "ok:" .. req.method .. ":" .. req.path
`})

	resp := doReq(t, srv, testRequest{Method: "PUT", Path: "/p/v2/thing", Body: "{}", Header: map[string]string{"X-Client": "abc"}})
	if resp.Status != 200 {
		t.Fatalf("status = %d, body = %q", resp.Status, resp.Body)
	}
	if got := string(up.last().Body); got != "ok:PUT:/v2/thing" {
		t.Fatalf("upstream body = %q", got)
	}
}

func TestRequestScript_EmptyBodyPassthrough(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": "return nil"})

	if resp := doReq(t, srv, testRequest{Method: "GET", Path: "/p/x"}); resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if got := len(up.last().Body); got != 0 {
		t.Fatalf("upstream body length = %d, want 0", got)
	}
}

// ── E. response handling ────────────────────────────────────────────────────

func TestResponse_NonStreamRunsOnceOnWholeBody(t *testing.T) {
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1,"b":2}`))
	})
	// The script would fail on a single event payload (no "model" key) and only
	// succeeds on the whole body, so a per-event split would surface as an
	// untouched body.
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `
if resp then
  local d = json.decode(resp.body)
  d.a = d.a + 41
  return json.encode(d)
end
return nil
`})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if resp.Body != `{"a":42,"b":2}` {
		t.Fatalf("body = %q, want whole-body transform applied once", resp.Body)
	}
}

func TestResponse_NonStreamNilReturnKeepsOriginal(t *testing.T) {
	original := "{\n \"keep\" :  \"exact bytes\"\n}"
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(original))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `
if resp then return nil end
return nil
`})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Body != original {
		t.Fatalf("body = %q, want original %q", resp.Body, original)
	}
}

func TestResponse_PreservesUpstreamStatusAndHeaders(t *testing.T) {
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.Status)
	}
	if got := resp.Header("X-Upstream-Marker"); got != "yes" {
		t.Fatalf("X-Upstream-Marker = %q, want yes", got)
	}
	if resp.Body != `{"created":true}` {
		t.Fatalf("body = %q", resp.Body)
	}
}

const toolRenameScript = `
local ctx = resp or req
local ok, d = pcall(json.decode, ctx.body)
if not ok or type(d) ~= "table" or not d.choices then return nil end
local touched = false
for _, c in ipairs(d.choices) do
  local delta = c.delta or c.message
  if delta and delta.tool_calls then
    for _, tc in ipairs(delta.tool_calls) do
      if tc["function"] and tc["function"].name then
        tc["function"].name = "mcp_" .. tc["function"].name
        touched = true
      end
    end
  end
end
if not touched then return nil end
return json.encode(d)
`

func TestResponse_SSE_PerEventRewrite(t *testing.T) {
	events := []string{
		"event: message\n",
		"id: 1\n",
		": this is a comment\n",
		`data: {"id":"c1","choices":[{"delta":{"role":"assistant","content":"hel"}}]}` + "\n",
		"\n",
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"function":{"name":"glob","arguments":"{}"}}]}}]}` + "\n",
		"\n",
		`data: {"id":"c1","choices":[{"delta":{"content":"lo"}}],"usage":{"total_tokens":3}}` + "\n",
		"\n",
		"data: [DONE]\n",
		"\n",
	}
	stream := strings.Join(events, "")

	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(stream))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": toolRenameScript})

	resp := doReq(t, srv, testRequest{Method: "POST", Path: "/p/v1/chat", Body: `{"model":"m","stream":true}`})
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if ct := resp.Header("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	body := resp.Body
	for _, framing := range []string{
		"event: message\n",
		"id: 1\n",
		": this is a comment\n",
		"data: [DONE]\n",
	} {
		if !strings.Contains(body, framing) {
			t.Fatalf("framing line %q missing from stream:\n%s", framing, body)
		}
	}

	// Content deltas must be byte-identical: the script only touches tool names.
	for _, content := range []string{`"content":"hel"`, `"content":"lo"`, `"total_tokens":3`} {
		if !strings.Contains(body, content) {
			t.Fatalf("content field %s was modified or lost:\n%s", content, body)
		}
	}

	if !strings.Contains(body, `"name":"mcp_glob"`) {
		t.Fatalf("tool name was not rewritten:\n%s", body)
	}
	if strings.Contains(body, `"name":"glob"`) {
		t.Fatalf("original tool name still present:\n%s", body)
	}

	// [DONE] must be the last data line and nothing may follow it.
	if idx := strings.Index(body, "[DONE]"); idx < 0 || idx < strings.Index(body, "mcp_glob") {
		t.Fatalf("unexpected [DONE] position:\n%s", body)
	}
}

func TestResponse_SSE_ScriptFailurePassesEventThrough(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `
if resp then error("sse-boom") end
return nil
`})

	resp := doReq(t, srv, testRequest{Path: "/p/x", Header: map[string]string{"Accept": "text/event-stream"}})
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if resp.Body != stream {
		t.Fatalf("body = %q, want the untouched stream %q", resp.Body, stream)
	}
}

func TestResponse_SSE_NoTrailingNewlinePassthrough(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}"
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Body != stream {
		t.Fatalf("body = %q, want %q (no newline must not be invented)", resp.Body, stream)
	}
}

func TestResponse_SSE_CRLFPreserved(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"grep\"}}]}}]}\r\n\r\ndata: [DONE]\r\n\r\n"
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": toolRenameScript})

	body := doReq(t, srv, testRequest{Path: "/p/x"}).Body
	if !strings.Contains(body, `"name":"mcp_grep"`) {
		t.Fatalf("tool not rewritten:\n%q", body)
	}
	if !strings.Contains(body, "}\r\n\r\n") {
		t.Fatalf("CRLF framing lost:\n%q", body)
	}
	if strings.Contains(body, "}\n\n") && !strings.Contains(body, "}\r\n\r\n") {
		t.Fatalf("CRLF was downgraded to LF:\n%q", body)
	}
}

func TestResponse_StreamSSE_ForcedOnJSONContentType(t *testing.T) {
	// The upstream claims application/json while sending SSE framing. Each
	// data: line is valid JSON, so the only observable difference is whether
	// the script saw the whole body or one event at a time.
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\ndata: [DONE]\n\n"
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(stream))
	})

	sseScript := `
if resp then
  if resp.body == [==[data: {"choices":[{"delta":{"content":"a"}}]}
data: [DONE]

]==] then
    return "WHOLE-BODY"
  end
  local d = json.decode(resp.body)
  d.event = true
  return json.encode(d)
end
return nil
`

	t.Run("forced true", func(t *testing.T) {
		cfg := relayTOML("/p", upURL, "s.lua") + "\n[route.response]\nstream_sse = true\n"
		srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": sseScript})
		resp := doReq(t, srv, testRequest{Path: "/p/x", Method: "POST"})
		if !strings.Contains(resp.Header("Content-Type"), "text/event-stream") {
			t.Fatalf("Content-Type = %q, want the SSE content type when stream_sse = true", resp.Header("Content-Type"))
		}
		if !strings.Contains(resp.Body, `"event":true`) {
			t.Fatalf("body = %q, want per-event rewriting to have applied", resp.Body)
		}
		if resp.Body == "WHOLE-BODY" {
			t.Fatalf("body = %q, want SSE handling, not a whole-body call", resp.Body)
		}
	})

	t.Run("unset falls back to content type", func(t *testing.T) {
		srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": sseScript})
		resp := doReq(t, srv, testRequest{Path: "/p/x", Method: "POST"})
		if resp.Body != "WHOLE-BODY" {
			t.Fatalf("body = %q, want the buffered response script to see the whole body", resp.Body)
		}
	})
}

func TestResponse_StreamSSE_DisabledOnEventStreamContentType(t *testing.T) {
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\ndata: two\n"))
	})
	cfg := relayTOML("/p", upURL, "s.lua") + `
[route.response]
stream_sse = false
`
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": `
if resp then
  return "BUFFERED:" .. string.gsub(resp.body, "\n", "|")
end
return nil
`})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	// serveSSE always stamps X-Accel-Buffering; the buffered path never does.
	if v := resp.Header("X-Accel-Buffering"); v != "" {
		t.Fatalf("X-Accel-Buffering = %q: the SSE path ran despite stream_sse = false", v)
	}
	if !strings.HasPrefix(resp.Body, "BUFFERED:") {
		t.Fatalf("body = %q, want the script to run once on the whole body", resp.Body)
	}
	if resp.Body != "BUFFERED:data: one|data: two|" {
		t.Fatalf("body = %q, want whole-body rewrite", resp.Body)
	}
}

func TestResponse_SSE_ErrorStatusPassthrough(t *testing.T) {
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.Status)
	}
	if resp.Body != `{"error":{"message":"rate limited"}}` {
		t.Fatalf("body = %q, the error payload must pass through", resp.Body)
	}
}

// ── F. errors ───────────────────────────────────────────────────────────────

func TestErrors_Upstream500Passthrough(t *testing.T) {
	upBody := `{"error":"upstream exploded"}`
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(upBody))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Status)
	}
	if resp.Body != upBody {
		t.Fatalf("body = %q, want %q", resp.Body, upBody)
	}
}

func TestErrors_UpstreamConnectionRefused(t *testing.T) {
	addr := freePort(t)
	upstreamURL := "http://" + addr
	cfg := fmt.Sprintf(`
[[route]]
prefix = "/p"
upstream = %s
script = "s.lua"
timeout = "2s"
`, tomlQuote(upstreamURL))
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.Status)
	}
	if !strings.Contains(resp.Body, "upstream error") {
		t.Fatalf("body = %q, want an upstream error message", resp.Body)
	}
}

func TestErrors_UpstreamTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("late"))
	})
	cfg := fmt.Sprintf(`
[[route]]
prefix = "/p"
upstream = %s
script = "s.lua"
timeout = "150ms"
`, tomlQuote(upURL))
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	start := time.Now()
	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request took %v; the route timeout was not applied", elapsed)
	}
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on upstream timeout", resp.Status)
	}
}

func TestErrors_RequestScriptError(t *testing.T) {
	up := newRecordingUpstream(t, "should-not-be-reached")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": `
error("boom")
`})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Status)
	}
	if !strings.Contains(resp.Body, "boom") {
		t.Fatalf("body = %q, want the script error message", resp.Body)
	}
	if !strings.Contains(resp.Body, "p") {
		t.Fatalf("body = %q, want the script name (compiled as %q)", resp.Body, "p")
	}
	if up.count() != 0 {
		t.Fatalf("upstream was called %d times after a request-script failure", up.count())
	}
}

func TestErrors_RequestScriptRuntimeErrorNamesScript(t *testing.T) {
	up := newRecordingUpstream(t, "x")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "relay.toml")
	cfgTOML := "[server]\nscript_dir = " + tomlQuote(dir) + "\n" + relayTOML("/p", up.URL, "gate.lua")
	if err := os.WriteFile(cfgPath, []byte(cfgTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Compile under the filename, the way main.go does.
	sc, err := script.Compile("gate.lua", `
if req then
  local x = nil
  return x.field
end
return nil
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, map[string]*script.Script{"gate.lua": sc})
	if err != nil {
		t.Fatal(err)
	}

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Status)
	}
	if !strings.Contains(resp.Body, "gate.lua") {
		t.Fatalf("body = %q, want the compiled script name in the error", resp.Body)
	}
	if !strings.Contains(resp.Body, "x.field") && !strings.Contains(resp.Body, "index a non-table") {
		t.Fatalf("body = %q, want the runtime error detail", resp.Body)
	}
	if up.count() != 0 {
		t.Fatalf("upstream was called %d times", up.count())
	}
}

func TestErrors_RequestScriptBadReturnType(t *testing.T) {
	up := newRecordingUpstream(t, "x")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": "return 42"})

	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a non string/table return", resp.Status)
	}
	if up.count() != 0 {
		t.Fatalf("upstream hit %d times despite a failed script", up.count())
	}
}

func TestErrors_ResponseScriptErrorKeepsOriginal(t *testing.T) {
	t.Run("buffered", func(t *testing.T) {
		original := `{"usable":true}`
		upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(original))
		})
		srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `
if resp then error("response-boom") end
return nil
`})
		resp := doReq(t, srv, testRequest{Path: "/p/x"})
		if resp.Status != 200 {
			t.Fatalf("status = %d, want 200", resp.Status)
		}
		if resp.Body != original {
			t.Fatalf("body = %q, want the original %q", resp.Body, original)
		}
	})

	t.Run("sse stream", func(t *testing.T) {
		stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\ndata: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\ndata: [DONE]\n\n"
		upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		})
		srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `
if resp then error("response-boom") end
return nil
`})
		resp := doReq(t, srv, testRequest{Path: "/p/x"})
		if resp.Body != stream {
			t.Fatalf("body = %q, want the unmodified stream %q", resp.Body, stream)
		}
	})
}

func TestErrors_ResponseScriptEmptyBodyKeepsOriginal(t *testing.T) {
	original := `{"nonempty":true}`
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(original))
	})
	// Returning an empty string must not blank a payload.
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `
if resp then return "" end
return nil
`})
	resp := doReq(t, srv, testRequest{Path: "/p/x"})
	if resp.Body != original {
		t.Fatalf("body = %q, want the original %q", resp.Body, original)
	}
}

// ── G. health endpoints ─────────────────────────────────────────────────────

func TestHealthEndpoints(t *testing.T) {
	up := newRecordingUpstream(t, "x")
	cfg := fmt.Sprintf(`
[[route]]
name = "alpha"
prefix = "/alpha"
upstream = %s
script = "a.lua"

[[route]]
name = "beta"
prefix = "/beta/v1"
upstream = %s
script = "b.lua"
`, tomlQuote(up.URL), tomlQuote(up.URL))
	srv, _ := newTestRelay(t, cfg, map[string]string{"a.lua": "return nil", "b.lua": "return nil"})

	t.Run("healthz", func(t *testing.T) {
		resp := doReq(t, srv, testRequest{Path: "/healthz"})
		if resp.Status != 200 {
			t.Fatalf("status = %d, want 200", resp.Status)
		}
		if resp.Body != "ok" {
			t.Fatalf("body = %q, want ok", resp.Body)
		}
	})

	t.Run("readyz", func(t *testing.T) {
		resp := doReq(t, srv, testRequest{Path: "/readyz"})
		if resp.Status != 200 || resp.Body != "ok" {
			t.Fatalf("status = %d body = %q, want 200 ok", resp.Status, resp.Body)
		}
	})

	t.Run("healthz does not hit upstream", func(t *testing.T) {
		if up.count() != 0 {
			t.Fatalf("health endpoints reached the upstream %d times", up.count())
		}
	})

	t.Run("routes", func(t *testing.T) {
		resp := doReq(t, srv, testRequest{Path: "/routes"})
		if resp.Status != 200 {
			t.Fatalf("status = %d, want 200", resp.Status)
		}
		for _, want := range []string{"alpha", "/alpha", "beta", "/beta/v1", "a.lua", "b.lua", up.URL} {
			if !strings.Contains(resp.Body, want) {
				t.Fatalf("/routes body missing %q:\n%s", want, resp.Body)
			}
		}
		if n := strings.Count(strings.TrimSpace(resp.Body), "\n") + 1; n != 2 {
			t.Fatalf("/routes listed %d lines, want 2:\n%s", n, resp.Body)
		}
		if ct := resp.Header("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Fatalf("Content-Type = %q, want text/plain", ct)
		}
	})

	t.Run("routes is not shadowed by a catch-all route", func(t *testing.T) {
		cfg := relayTOML("/", up.URL, "s.lua")
		srvAll, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})
		resp := doReq(t, srvAll, testRequest{Path: "/routes"})
		if !strings.Contains(resp.Body, "s.lua") || strings.Contains(resp.Body, "no route") {
			t.Fatalf("body = %q, want the route listing", resp.Body)
		}
	})
}

// ── config validation at build time ─────────────────────────────────────────

func TestNew_MissingScriptForRoute(t *testing.T) {
	up := newRecordingUpstream(t, "x")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.lua"), []byte("return nil"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "relay.toml")
	cfgTOML := "[server]\nscript_dir = " + tomlQuote(dir) + "\n" + relayTOML("/p", up.URL, "s.lua")
	if err := os.WriteFile(cfgPath, []byte(cfgTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, map[string]*script.Script{}); err == nil {
		t.Fatal("New succeeded without a compiled script for the route")
	}

	sc, err := script.Compile("p", "return nil")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, map[string]*script.Script{"s.lua": sc}); err != nil {
		t.Fatalf("New with the script compiled: %v", err)
	}
}

// ── additional edge cases ───────────────────────────────────────────────────

func TestRewrite_PercentEscapesInQueryPreserved(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL+"/base", "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Path: "/p/x?v=%20a%26b&e=h%C3%A9llo&plus=a+b"})
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if got := up.last().Query; got != "v=%20a%26b&e=h%C3%A9llo&plus=a+b" {
		t.Fatalf("upstream query = %q, want it forwarded verbatim", got)
	}
	if got := up.last().Path; got != "/base/x" {
		t.Fatalf("upstream path = %q, want /base/x", got)
	}
}

func TestRewrite_DotSegmentsNotEscaped(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL+"/base", "s.lua"), map[string]string{"s.lua": "return nil"})

	if resp := doReq(t, srv, testRequest{Path: "/p/v1\u002fbank/../things"}); resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	got := up.last().Path
	if !strings.HasPrefix(got, "/base/") {
		t.Fatalf("upstream path = %q, want it under /base/", got)
	}
}

func TestHeaders_DynamicManyNamesAllApplied(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	var b strings.Builder
	b.WriteString(relayTOML("/p", up.URL, "s.lua"))
	for i := 0; i < 9; i++ {
		fmt.Fprintf(&b, "\n[route.headers.dynamic.X-Gen-%d]\nkind = \"hex\"\nbytes = 4\n", i)
	}
	srv, _ := newTestRelay(t, b.String(), map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x"})
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("X-Gen-%d", i)
		if v := up.last().Header.Get(name); v == "" {
			t.Fatalf("%s missing; dynamic headers must all be generated", name)
		}
	}
}

func TestHeaders_DynamicFileRereadPerRequest(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + "\n[route.headers.dynamic.Authorization]\nkind = \"file\"\npath = " + tomlQuote(tokenPath) + "\nprefix = \"Bearer \"\n"
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x"})
	if got := up.last().Header.Get("Authorization"); got != "Bearer first" {
		t.Fatalf("Authorization = %q, want Bearer first", got)
	}

	if err := os.WriteFile(tokenPath, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doReq(t, srv, testRequest{Path: "/p/x"})
	if got := up.last().Header.Get("Authorization"); got != "Bearer second" {
		t.Fatalf("Authorization = %q, want Bearer second after rotation", got)
	}
}

func TestHeaders_DynamicSiteOverridesDropAndSet(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + `
[route.headers]
drop = ["X-Token"]
[route.headers.set]
X-Token = "static"
[route.headers.dynamic.X-Token-Dyn]
kind = "hex"
bytes = 2
`
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x", Header: map[string]string{"X-Token": "client"}})
	h := up.last().Header
	if got := h.Get("X-Token"); got != "static" {
		t.Fatalf("X-Token = %q, want static set value to win over drop+client", got)
	}
	if got := h.Get("X-Token-Dyn"); len(got) != 4 {
		t.Fatalf("X-Token-Dyn = %q, want 4 hex chars", got)
	}
}

func TestSandbox_EscapeFunctionsRemoved(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": `
if req then
  for _, name in ipairs({"dofile", "loadfile", "load", "print", "require", "os", "io", "package"}) do
    if _G[name] ~= nil then error("sandbox leak: " .. name) end
  end
  for _, name in ipairs({"json", "sha256", "sha1", "hmac_sha256", "base64", "unix_now", "time"}) do
    if _G[name] == nil then error("missing api: " .. name) end
  end
  return "ok"
end
return nil
`})

	if resp := doReq(t, srv, testRequest{Path: "/p/x"}); resp.Status != 200 {
		t.Fatalf("status = %d, body = %q", resp.Status, resp.Body)
	}
}

func TestErrors_UpstreamConnectionRefusedWithBody(t *testing.T) {
	addr := freePort(t)
	srv, _ := newTestRelay(t, relayTOML("/p", "http://"+addr, "s.lua"), map[string]string{"s.lua": "return nil"})

	resp := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: strings.Repeat("x", 1024)})
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.Status)
	}
	if resp.Body == "" {
		t.Fatal("502 response body is empty; want a diagnostic")
	}
}

func TestServe_ConcurrentRequestsKeepIsolation(t *testing.T) {
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		n := 0
		if r.Body != nil {
			n, _ = r.Body.Read(buf)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo":%s,"n":%d}`, strings.TrimSpace(string(buf[:n])), n)
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": "return nil"})

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ln := fasthttputil.NewInmemoryListener()
			defer ln.Close()
			fs := &fasthttp.Server{Handler: srv.Handler()}
			go fs.Serve(ln)
			client := &fasthttp.Client{Dial: func(string) (net.Conn, error) { return ln.Dial() }}
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			body := fmt.Sprintf(`{"id":%d}`, i)
			req.Header.SetMethod("POST")
			req.SetRequestURI("http://relay/p/x")
			req.SetBodyString(body)
			if err := client.DoTimeout(req, resp, 10*time.Second); err != nil {
				errs <- err
				return
			}
			if want := fmt.Sprintf(`{"echo":{"id":%d},"n":%d}`, i, len(body)); string(resp.Body()) != want {
				errs <- fmt.Errorf("request %d: body %q, want %q", i, resp.Body(), want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// ── content-type sniffing ───────────────────────────────────────────────────

func TestResponse_SSESniffedFromRequestBody(t *testing.T) {
	// A vague upstream content type: the route falls back to "stream":true in
	// the request body to decide between SSE and buffered handling.
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"read\"}}]}}]}\ndata: [DONE]\n\n"
	upURL, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(stream))
	})
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": toolRenameScript})

	tests := []struct {
		name       string
		body       string
		wantSSE    bool
		wantParsed string
	}{
		{name: "stream true is sniffed as sse", body: `{"model":"m","stream":true}`, wantSSE: true, wantParsed: "mcp_read"},
		{name: "no stream flag stays buffered", body: `{"model":"m"}`, wantSSE: false, wantParsed: "data: {"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: tt.body})
			if resp.Body != stream && !tt.wantSSE {
				// Buffered path runs the script over the whole body; that body
				// is not JSON, so the script returns nil and the stream is kept.
				if resp.Body != stream {
					t.Fatalf("body = %q, want the untouched body", resp.Body)
				}
			}
			hasSSEHeaders := resp.Header("X-Accel-Buffering") != ""
			if hasSSEHeaders != tt.wantSSE {
				t.Fatalf("X-Accel-Buffering present = %v, want %v", hasSSEHeaders, tt.wantSSE)
			}
			if !strings.Contains(resp.Body, tt.wantParsed) {
				t.Fatalf("body = %q, want it to contain %q", resp.Body, tt.wantParsed)
			}
		})
	}
}

func TestNew_UpstreamParseErrors(t *testing.T) {
	up := newRecordingUpstream(t, "x")
	for _, tt := range []struct {
		name     string
		upstream string
	}{
		{name: "unsupported scheme", upstream: "ftp://example.invalid/x"},
		{name: "missing scheme", upstream: "example.invalid/x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "relay.toml")
			dir := filepath.Dir(cfgPath)
			if err := os.WriteFile(filepath.Join(dir, "s.lua"), []byte("return nil"), 0o644); err != nil {
				t.Fatal(err)
			}
			body := "[server]\nscript_dir = " + tomlQuote(dir) + "\n" + relayTOML("/p", tt.upstream, "s.lua")
			if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			sc, err := script.Compile("p", "return nil")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(cfg, map[string]*script.Script{"s.lua": sc}); err == nil {
				t.Fatalf("New accepted upstream %q", tt.upstream)
			}
		})
	}
	_ = up
}

func TestHeaders_DefaultDynamicKindIsHex(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	cfg := relayTOML("/p", up.URL, "s.lua") + "\n[route.headers.dynamic.X-Dflt]\nprefix = \"q_\"\n"
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": "return nil"})

	doReq(t, srv, testRequest{Path: "/p/x"})
	if got := up.last().Header.Get("X-Dflt"); !regexp.MustCompile(`^q_[0-9a-f]{26}$`).MatchString(got) {
		t.Fatalf("X-Dflt = %q, want q_ + 26 hex chars", got)
	}
}

// TestRequestScript_HeadersOnlyReturn guards the F-3 fix: a return table with
// only `headers` must apply the headers and leave the request body intact.
// Before the fix the body was replaced by an empty string.
func TestRequestScript_HeadersOnlyReturn(t *testing.T) {
	up := newRecordingUpstream(t, "ok")
	srv, _ := newTestRelay(t, relayTOML("/p", up.URL, "s.lua"), map[string]string{"s.lua": `
if req then
  return { headers = { ["X-Only"] = "1" } }
end
return nil
`})

	doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: "payload"})
	last := up.last()
	if got := last.Header.Get("X-Only"); got != "1" {
		t.Fatalf("X-Only = %q, want 1", got)
	}
	if got := string(last.Body); got != "payload" {
		t.Fatalf("body = %q, want the original payload (header-only results must not blank the body)", got)
	}
}
