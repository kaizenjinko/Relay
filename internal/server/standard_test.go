package server

import (
	"net/http"
	"testing"
)

// A 307/308 from the upstream must reach the gateway in front, not be
// swallowed. The relay is an origin server for Bifrost: it answers with what
// the upstream said, and the gateway decides what to do with the redirect.
// Following it server-side would hide a routing decision the gateway owns.
func TestUpstreamRedirectIsForwardedNotFollowed(t *testing.T) {
	redir, closeRedir := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/real", http.StatusTemporaryRedirect)
	})
	defer closeRedir()

	srv, _ := newTestRelay(t, relayTOML("/p", redir, "s.lua"), map[string]string{"s.lua": `return nil`})
	res := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{}`})

	if res.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 passed through", res.Status)
	}
	if loc := res.Headers["Location"]; loc != "https://elsewhere.example/real" {
		t.Fatalf("Location = %q, want it preserved for the gateway", loc)
	}
}

// The relay must not invent a Server header: it sits behind a gateway that
// advertises itself, and leaking the proxy's identity is noise at best.
func TestNoServerHeaderAdded(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	defer closeUp()

	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `return nil`})
	res := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{}`})
	if v, ok := res.Headers["Server"]; ok {
		t.Fatalf("relay added Server header %q", v)
	}
}

// X-Forwarded-* from the gateway must reach the upstream unchanged: the
// upstream uses them for IP-based limits, and overwriting them would make
// every request look like it came from the relay.
func TestForwardedHeadersNotRewritten(t *testing.T) {
	var gotFor, gotProto string
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotFor = r.Header.Get("X-Forwarded-For")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	defer closeUp()

	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `return nil`})
	res := doReq(t, srv, testRequest{
		Method: "POST", Path: "/p/x", Body: `{}`,
		Header: map[string]string{
			"X-Forwarded-For":   "203.0.113.7",
			"X-Forwarded-Proto": "https",
		},
	})
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
	if gotFor != "203.0.113.7" {
		t.Fatalf("X-Forwarded-For = %q, want the gateway's value", gotFor)
	}
	if gotProto != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want the gateway's value", gotProto)
	}
}

// JSON responses must come back byte-identical when no script touches them.
// A gateway comparing hashes or an upstream signature depends on that.
func TestJSONBodyUnchangedWithoutScript(t *testing.T) {
	const payload = `{"choices":[{"delta":{"content":"a\"b\\c\n"}}],"n":1.50}`
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(payload))
	})
	defer closeUp()

	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `return nil`})
	res := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{}`})
	if res.Body != payload {
		t.Fatalf("body mutated:\n got %q\nwant %q", res.Body, payload)
	}
}

// SSE event boundaries must be preserved exactly: an empty data line, comments
// and multi-line data are all meaningful to the client's parser.
func TestSSEFramingPreserved(t *testing.T) {
	const stream = "data: {\"n\":1}\n\n" +
		": keepalive\n\n" +
		"data: {\"n\":2}\n" +
		"id: 7\n\n" +
		"data: [DONE]\n\n"
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(stream))
	})
	defer closeUp()

	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{"s.lua": `return nil`})
	res := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{}`})
	if res.Body != stream {
		t.Fatalf("SSE framing changed:\n got %q\nwant %q", res.Body, stream)
	}
}
