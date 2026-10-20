package server

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

// TestClientPool_ReusesConnections proves the per-request HostClient bug is
// gone: two sequential requests to the same upstream must open one connection.
func TestClientPool_ReusesConnections(t *testing.T) {
	var conns int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	srvHTTP := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srvHTTP.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	go srvHTTP.Serve(ln)
	defer srvHTTP.Close()

	upstream := "http://" + ln.Addr().String()
	srv, _ := newTestRelay(t, relayTOML("/p", upstream, "s.lua"), map[string]string{
		"s.lua": `return nil`,
	})

	for i := 0; i < 3; i++ {
		res := doReq(t, srv, testRequest{Method: "POST", Path: "/p/x", Body: `{"n":1}`})
		if res.Status != 200 {
			t.Fatalf("request %d status = %d", i, res.Status)
		}
	}
	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Fatalf("connections opened = %d, want 1 (keep-alive must be reused)", got)
	}
}

// TestClientPool_SharedAcrossRoutes proves two routes pointing at the same
// host share one client, so a second route does not double the connection
// count for the same upstream.
func TestClientPool_SharedAcrossRoutes(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	defer closeUp()

	cfg := fmt.Sprintf(`
[[route]]
name = "a"
prefix = "/a"
upstream = %q
script = "s.lua"

[[route]]
name = "b"
prefix = "/b"
upstream = %q
script = "s.lua"
`, upURL, upURL)
	srv, _ := newTestRelay(t, cfg, map[string]string{"s.lua": `return nil`})

	doReq(t, srv, testRequest{Method: "POST", Path: "/a/x", Body: "{}"})
	doReq(t, srv, testRequest{Method: "POST", Path: "/b/x", Body: "{}"})

	if got := srv.clients.Len(); got != 1 {
		t.Fatalf("pool size = %d, want 1 for a single upstream host", got)
	}
}

// TestServe_ConcurrentSameUpstream fires 200 concurrent requests and asserts
// every answer is correct. Combined with -race this is the isolation guard for
// the shared client.
func TestServe_ConcurrentSameUpstream(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo":%q}`, r.Header.Get("X-Req"))
	})
	defer closeUp()

	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{
		"s.lua": `return nil`,
	})

	const n = 200
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			res := doReq(t, srv, testRequest{
				Method: "POST",
				Path:   "/p/x",
				Body:   `{"n":1}`,
				Header: map[string]string{"X-Req": id},
			})
			if res.Status != 200 {
				errs <- fmt.Sprintf("%s: status %d", id, res.Status)
				return
			}
			want := fmt.Sprintf(`{"echo":%q}`, id)
			if res.Body != want {
				errs <- fmt.Sprintf("%s: body %q want %q", id, res.Body, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
