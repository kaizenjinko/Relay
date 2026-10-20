package server

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// TestHeaderPoolIsolation proves a pooled header map never leaks values from
// one request into the next, even under concurrency.
func TestHeaderPoolIsolation(t *testing.T) {
	upURL, closeUp := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"x":%q}`, r.Header.Get("X-Unique"))
	})
	defer closeUp()
	srv, _ := newTestRelay(t, relayTOML("/p", upURL, "s.lua"), map[string]string{
		"s.lua": `
			-- read headers so the map is materialised per request
			local n = 0
			for k, v in pairs(req.headers) do n = n + 1 end
			return nil
		`,
	})

	const n = 300
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("u%d", i)
			res := doReq(t, srv, testRequest{
				Method: "POST", Path: "/p/x", Body: `{"a":1}`,
				Header: map[string]string{"X-Unique": id},
			})
			want := fmt.Sprintf(`{"x":%q}`, id)
			if res.Body != want {
				errs <- fmt.Sprintf("%s: got %q want %q", id, res.Body, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
