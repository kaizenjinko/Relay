package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

// rotationTOML builds a route with a rotation block. The template is the same
// shape a user would write for two DeepSeek keys.
func rotationTOML(prefix, upstream, script string, extra string) string {
	return fmt.Sprintf(`
[[route]]
name = "rot"
prefix = %q
upstream = %q
script = %q
timeout = "5s"

  [route.headers]
  drop = ["accept-encoding"]

  [route.rotation]
  strategy = "round_robin"
  max_attempts = 3
  cooldown = "60s"

  [[route.rotation.source]]
  kind = "list"
  keys = ["bad-key", "good-key"]
  template = { Authorization = "Bearer {key}" }

  [[route.rotation.failure.rule]]
  name = "auth"
  status = [401]
  retry = true
%s
`, prefix, upstream, script, extra)
}

// TestRotation_RetriesToSecondCredential proves the retry loop reaches the
// second key when the first is rejected.
func TestRotation_RetriesToSecondCredential(t *testing.T) {
	var bad, good int32
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer bad-key":
			atomic.AddInt32(&bad, 1)
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"invalid key"}`)
		case "Bearer good-key":
			atomic.AddInt32(&good, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		default:
			w.WriteHeader(400)
			fmt.Fprintf(w, `unexpected auth %q`, auth)
		}
	})
	defer closeUp()

	srv, _ := newTestRelay(t, rotationTOML("/rot", upURL, "s.lua", ""), map[string]string{
		"s.lua": `return nil`,
	})
	res := doReq(t, srv, testRequest{Method: "POST", Path: "/rot/chat", Body: `{"x":1}`})

	if res.Status != 200 {
		t.Fatalf("status = %d body = %s", res.Status, res.Body)
	}
	if atomic.LoadInt32(&bad) != 1 {
		t.Fatalf("bad key used %d times, want 1", bad)
	}
	if atomic.LoadInt32(&good) != 1 {
		t.Fatalf("good key used %d times, want 1", good)
	}
}

// TestRotation_AllCredentialsFail proves the client sees the last upstream
// response, not a synthesised 502.
func TestRotation_AllCredentialsFail(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"denied"}`)
	})
	defer closeUp()
	srv, _ := newTestRelay(t, rotationTOML("/rot", upURL, "s.lua", ""), map[string]string{
		"s.lua": `return nil`,
	})
	res := doReq(t, srv, testRequest{Method: "POST", Path: "/rot/chat", Body: `{}`})
	if res.Status != 401 {
		t.Fatalf("status = %d, want 401", res.Status)
	}
	if got := string(res.Body); got != `{"error":"denied"}` {
		t.Fatalf("body = %q", got)
	}
}

// TestRotation_RouteWithoutBlockSendsOnce guards that the feature is optional:
// a route with no [route.rotation] must make exactly one upstream call.
func TestRotation_RouteWithoutBlockSendsOnce(t *testing.T) {
	var calls int32
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
		fmt.Fprint(w, "denied")
	})
	defer closeUp()
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{
		"s.lua": `return nil`,
	})
	res := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{}`})
	if res.Status != 401 {
		t.Fatalf("status = %d", res.Status)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no rotation configured)", n)
	}
}

// TestRotation_BodyIsIdenticalAcrossAttempts proves the retried request
// carries the same body: a credential swap must not drop the payload.
func TestRotation_BodyIsIdenticalAcrossAttempts(t *testing.T) {
	var bodies []string
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		bodies = append(bodies, string(b))
		if r.Header.Get("Authorization") == "Bearer bad-key" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	})
	defer closeUp()
	srv, _ := newTestRelay(t, rotationTOML("/rot", upURL, "s.lua", ""), map[string]string{
		"s.lua": `return req.body`,
	})
	const payload = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	doReq(t, srv, testRequest{Method: "POST", Path: "/rot/chat", Body: payload})

	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("body changed between attempts:\n  1: %s\n  2: %s", bodies[0], bodies[1])
	}
}
