package rotation

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baongo/relay/internal/config"
)

// fakeHeaders implements limiter for tests.
type fakeHeaders map[string]string

func (f fakeHeaders) Peek(key string) []byte {
	if v, ok := f[key]; ok {
		return []byte(v)
	}
	return nil
}

// fixedClock lets the tests advance time without sleeping.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fixedClock {
	return &fixedClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestRotator(t *testing.T, cfg *config.Rotation) (*Rotator, *fixedClock) {
	t.Helper()
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	r.setClock(clk.Now)
	return r, clk
}

func listSource(keys ...string) *config.RotSource {
	return &config.RotSource{
		Kind:     "list",
		Keys:     keys,
		Template: map[string]string{"Authorization": "Bearer {key}"},
	}
}

func TestStaticSource(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{{
			Kind:    "static",
			Headers: map[string]string{"Authorization": "Bearer fixed"},
		}},
	})
	creds := r.Credentials()
	if len(creds) != 1 {
		t.Fatalf("creds = %d", len(creds))
	}
	if got := creds[0].Headers["Authorization"]; got != "Bearer fixed" {
		t.Fatalf("header = %q", got)
	}
}

func TestListSourceTemplate(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{listSource("aaa", "bbb")},
	})
	creds := r.Credentials()
	if len(creds) != 2 {
		t.Fatalf("creds = %d", len(creds))
	}
	seen := map[string]bool{}
	for _, c := range creds {
		seen[c.Headers["Authorization"]] = true
	}
	if !seen["Bearer aaa"] || !seen["Bearer bbb"] {
		t.Fatalf("templates not applied: %+v", creds)
	}
}

func TestEnvSource(t *testing.T) {
	t.Setenv("RELAY_TEST_KEYS", "k1, k2 ,k3")
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{{
			Kind:     "env",
			Name:     "RELAY_TEST_KEYS",
			Template: map[string]string{"Authorization": "Bearer {key}"},
		}},
	})
	if n := len(r.Credentials()); n != 3 {
		t.Fatalf("creds = %d, want 3 (whitespace must be trimmed)", n)
	}
}

func TestFileSource(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys.txt")
	os.WriteFile(p, []byte("alpha\n\nbeta\n"), 0o600)
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{{
			Kind:     "file",
			Path:     p,
			Template: map[string]string{"Authorization": "Bearer {key}"},
		}},
	})
	if n := len(r.Credentials()); n != 2 {
		t.Fatalf("creds = %d, want 2 (blank lines skipped)", n)
	}
}

func TestCookieJarSource(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "jar.json")
	os.WriteFile(p, []byte(`{"accounts":[
	  {"id":"a1","headers":{"Cookie":"CNBSESSION=1"},"meta":{"region":"vn"}},
	  {"id":"a2","headers":{"Cookie":"CNBSESSION=2"}}
	]}`), 0o600)
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{{Kind: "cookie_jar", Path: p}},
	})
	creds := r.Credentials()
	if len(creds) != 2 {
		t.Fatalf("creds = %d", len(creds))
	}
	if creds[0].Meta["region"] != "vn" {
		t.Fatalf("meta lost: %+v", creds[0])
	}
}

func TestRoundRobinCycles(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Strategy: "round_robin",
		Source:   []*config.RotSource{listSource("a", "b", "c")},
	})
	var order []string
	for i := 0; i < 6; i++ {
		c := r.Pick()
		order = append(order, c.ID)
	}
	// Two full cycles: the set of each consecutive triple must cover all three.
	seen := map[string]int{}
	for _, id := range order {
		seen[id]++
	}
	if len(seen) != 3 {
		t.Fatalf("round robin did not cycle: %v", order)
	}
	for id, n := range seen {
		if n != 2 {
			t.Fatalf("credential %s picked %d times, want 2: %v", id, n, order)
		}
	}
}

func TestRandomReturnsMember(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Strategy: "random",
		Source:   []*config.RotSource{listSource("a", "b")},
	})
	ids := map[string]bool{}
	for i := 0; i < 50; i++ {
		c := r.Pick()
		if c == nil {
			t.Fatal("nil pick")
		}
		ids[c.ID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("random never picked both: %v", ids)
	}
}

func TestCooldownSkipsCredential(t *testing.T) {
	r, clk := newTestRotator(t, &config.Rotation{
		Strategy: "round_robin",
		Cooldown: "10s",
		Source:   []*config.RotSource{listSource("a", "b")},
	})
	a := r.Credentials()[0]
	r.MarkFailed(a, 0)

	// b must still be pickable, and a must not be handed out during cooldown.
	for i := 0; i < 5; i++ {
		c := r.Pick()
		if c != nil && c.ID == a.ID {
			t.Fatalf("cooling credential %s was picked", a.ID)
		}
	}
	clk.Advance(11 * time.Second)
	found := false
	for i := 0; i < 5; i++ {
		if c := r.Pick(); c != nil && c.ID == a.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("credential did not come back after cooldown")
	}
}

func TestBackoffDoubles(t *testing.T) {
	r, clk := newTestRotator(t, &config.Rotation{
		Strategy:    "round_robin",
		Cooldown:    "10s",
		MaxCooldown: "40s",
		Source:      []*config.RotSource{listSource("a")},
	})
	a := r.Credentials()[0]

	var observed []time.Duration
	for i := 0; i < 3; i++ {
		before := clk.Now()
		r.MarkFailed(a, 0)
		h, ok := r.pool.healthOf(a.ID)
		if !ok {
			t.Fatal("credential vanished")
		}
		observed = append(observed, h.until.Sub(before))
		clk.Advance(h.until.Sub(clk.Now()) + time.Second)
	}
	if observed[0] != 10*time.Second {
		t.Fatalf("first cooldown = %v, want 10s", observed[0])
	}
	if observed[1] != 20*time.Second {
		t.Fatalf("second cooldown = %v, want 20s", observed[1])
	}
	if observed[2] != 40*time.Second {
		t.Fatalf("third cooldown = %v, want 40s (capped)", observed[2])
	}
}

func TestFailThresholdMarksDead(t *testing.T) {
	r, clk := newTestRotator(t, &config.Rotation{
		FailThreshold: 2,
		Cooldown:      "1s",
		MaxCooldown:   "1s",
		Source:        []*config.RotSource{listSource("a")},
	})
	a := r.Credentials()[0]
	r.MarkFailed(a, 0)
	clk.Advance(2 * time.Second)
	r.MarkFailed(a, 0)
	clk.Advance(10 * time.Second)
	if c := r.Pick(); c != nil {
		t.Fatalf("dead credential returned: %s", c.ID)
	}
}

func TestMarkSuccessResets(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Cooldown: "10s",
		Source:   []*config.RotSource{listSource("a")},
	})
	a := r.Credentials()[0]
	r.MarkFailed(a, 0)
	r.MarkSuccess(a)
	if c := r.Pick(); c == nil {
		t.Fatal("credential still cooling after success")
	}
}

func statusRule(name string, statuses ...int) *config.Rule {
	return &config.Rule{Name: name, Status: statuses, Retry: true}
}

func TestFailureRuleStatus(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{listSource("a")},
		Failure: config.Failure{Rule: []*config.Rule{
			statusRule("auth", 401, 403),
			{Name: "quota", Status: []int{429}, Retry: true, UseRetryAfter: true},
		}},
	})
	cred := r.Credentials()[0]
	if class := r.MarkResult(cred, 401, fakeHeaders{}, nil); class.Rule != "auth" || !class.Retry {
		t.Fatalf("401 -> %+v", class)
	}
	if class := r.MarkResult(cred, 200, fakeHeaders{}, nil); class.Matched {
		t.Fatalf("200 should not match: %+v", class)
	}
	if class := r.MarkResult(cred, 429, fakeHeaders{}, nil); class.Rule != "quota" {
		t.Fatalf("429 -> %+v", class)
	}
}

func TestFailureRuleBodyRegex(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{listSource("a")},
		Failure: config.Failure{Rule: []*config.Rule{
			{Name: "quota", Status: []int{403}, BodyRegex: "(?i)quota", Retry: true},
		}},
	})
	cred := r.Credentials()[0]
	if class := r.MarkResult(cred, 403, fakeHeaders{}, []byte(`{"error":"QUOTA exceeded"}`)); !class.Matched {
		t.Fatal("body regex did not match")
	}
	if class := r.MarkResult(cred, 403, fakeHeaders{}, []byte(`{"error":"other"}`)); class.Matched {
		t.Fatal("body regex matched unrelated body")
	}
}

func TestFailureRuleHeaderMatch(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Source: []*config.RotSource{listSource("a")},
		Failure: config.Failure{Rule: []*config.Rule{
			{
				Name:        "exhausted",
				HeaderMatch: map[string]string{"X-RateLimit-Remaining": "^0$"},
				Retry:       true,
			},
		}},
	})
	cred := r.Credentials()[0]
	if class := r.MarkResult(cred, 200, fakeHeaders{"X-RateLimit-Remaining": "0"}, nil); !class.Matched {
		t.Fatal("header match failed")
	}
	if class := r.MarkResult(cred, 200, fakeHeaders{"X-RateLimit-Remaining": "12"}, nil); class.Matched {
		t.Fatal("header match should not fire")
	}
}

func TestAbortBeatsRetry(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		MaxAttempts: 3,
		Source:      []*config.RotSource{listSource("a", "b")},
		Failure: config.Failure{Rule: []*config.Rule{
			{Name: "fatal", Status: []int{403}, BodyRegex: "FreeTierError", Retry: true, Abort: true},
		}},
	})
	var calls int32
	_, attempts, err := r.Do(func(c *Credential) (*Result, error) {
		atomic.AddInt32(&calls, 1)
		return &Result{Status: 403, Body: []byte("FreeTierError"), Headers: fakeHeaders{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, abort must stop after one attempt", got)
	}
	if len(attempts) != 1 || attempts[0].Class.Rule != "fatal" {
		t.Fatalf("attempts = %+v", attempts)
	}
}

func TestRetryAfterParsed(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Cooldown: "10s",
		Source:   []*config.RotSource{listSource("a")},
		Failure: config.Failure{Rule: []*config.Rule{
			{Name: "quota", Status: []int{429}, Retry: true, UseRetryAfter: true},
		}},
	})
	cred := r.Credentials()[0]
	r.MarkResult(cred, 429, fakeHeaders{"Retry-After": "42"}, nil)
	h, _ := r.pool.healthOf(cred.ID)
	if got := h.until.Sub(r.now()).Round(time.Second); got != 42*time.Second {
		t.Fatalf("cooldown = %v, want 42s (Retry-After wins)", got)
	}
}

func TestDoRetriesUntilSuccess(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		MaxAttempts: 3,
		Strategy:    "round_robin",
		Source:      []*config.RotSource{listSource("a", "b")},
		Failure:     config.Failure{Rule: []*config.Rule{statusRule("auth", 401)}},
	})
	var calls int
	res, attempts, err := r.Do(func(c *Credential) (*Result, error) {
		calls++
		if calls == 1 {
			return &Result{Status: 401, Headers: fakeHeaders{}}, nil
		}
		return &Result{Status: 200, Body: []byte("ok"), Headers: fakeHeaders{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || string(res.Body) != "ok" {
		t.Fatalf("res = %+v", res)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d", len(attempts))
	}
}

func TestDoAllFailReturnsLast(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		MaxAttempts: 2,
		Source:      []*config.RotSource{listSource("a", "b")},
		Failure:     config.Failure{Rule: []*config.Rule{statusRule("auth", 401)}},
	})
	res, attempts, err := r.Do(func(c *Credential) (*Result, error) {
		return &Result{Status: 401, Body: []byte("denied"), Headers: fakeHeaders{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Status != 401 {
		t.Fatalf("res = %+v", res)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
}

func TestDoTransportErrorCoolsAndRetries(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		MaxAttempts: 2,
		Source:      []*config.RotSource{listSource("a", "b")},
	})
	var calls int
	res, _, err := r.Do(func(c *Credential) (*Result, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("connection reset")
		}
		return &Result{Status: 200, Body: []byte("ok"), Headers: fakeHeaders{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 {
		t.Fatalf("res = %+v", res)
	}
}

func TestConcurrentPickIsolated(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Strategy: "round_robin",
		Source:   []*config.RotSource{listSource("a", "b", "c")},
	})
	var wg sync.WaitGroup
	var picks int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if c := r.Pick(); c != nil {
					atomic.AddInt64(&picks, 1)
					r.MarkSuccess(c)
				}
			}
		}()
	}
	wg.Wait()
	if picks == 0 {
		t.Fatal("no picks")
	}
}

func TestExcludeIsRespected(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		Strategy: "round_robin",
		Source:   []*config.RotSource{listSource("a", "b")},
	})
	ex := map[string]bool{r.Credentials()[0].ID: true}
	for i := 0; i < 10; i++ {
		c := r.pool.pick(ex)
		if c == nil {
			t.Fatal("nil pick while a credential is available")
		}
		if ex[c.ID] {
			t.Fatalf("excluded credential %s returned", c.ID)
		}
	}
}

// CNB reports an expired session as an SSE data frame inside a 200 response,
// not as a 401. The rotator must recognise it from the body and move to the
// next account, otherwise a dead cookie is used forever.
func TestSSEBodyErrorTriggersFailover(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		MaxAttempts: 2,
		Strategy:    "round_robin",
		Source:      []*config.RotSource{listSource("acct-a", "acct-b")},
		Failure: config.Failure{Rule: []*config.Rule{{
			Name:      "auth-sse",
			Status:    []int{200},
			BodyRegex: `登录已过期|session expired`,
			Retry:     true,
		}}},
	})

	var seen []string
	res, attempts, err := r.Do(func(c *Credential) (*Result, error) {
		seen = append(seen, c.ID)
		if c.ID == "list#0" {
			return &Result{
				Status:  200,
				Headers: fakeHeaders{"Content-Type": "text/event-stream"},
				Body:    []byte("data: {\"error\":\"登录已过期\"}\n\n"),
			}, nil
		}
		return &Result{
			Status:  200,
			Headers: fakeHeaders{"Content-Type": "text/event-stream"},
			Body:    []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"),
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (%v)", len(attempts), seen)
	}
	if !bytes.Contains(res.Body, []byte(`"content":"ok"`)) {
		t.Fatalf("did not fall through to the healthy account: %q", res.Body)
	}
	if seen[0] == seen[1] {
		t.Fatalf("retried the same credential twice: %v", seen)
	}
}

// A 200 whose body matches no rule must be treated as success, or every
// streamed token would be mistaken for a failure.
func TestSSEBodySuccessNotRetried(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		MaxAttempts: 3,
		Source:      []*config.RotSource{listSource("a", "b")},
		Failure: config.Failure{Rule: []*config.Rule{{
			Name:      "auth-sse",
			Status:    []int{200},
			BodyRegex: `登录已过期`,
			Retry:     true,
		}}},
	})
	var calls int
	res, attempts, err := r.Do(func(c *Credential) (*Result, error) {
		calls++
		return &Result{Status: 200, Headers: fakeHeaders{}, Body: []byte(`data: {"ok":1}`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(attempts) != 1 {
		t.Fatalf("healthy stream retried: calls=%d attempts=%d", calls, len(attempts))
	}
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
}

// A dead account must be skipped on the next request instead of being retried
// on every call.
func TestDeadAccountSkippedOnNextRequest(t *testing.T) {
	r, _ := newTestRotator(t, &config.Rotation{
		MaxAttempts:   2,
		Strategy:      "round_robin",
		Source:        []*config.RotSource{listSource("bad", "good")},
		Cooldown:      "60s",
		FailThreshold: 1,
		Failure:       config.Failure{Rule: []*config.Rule{statusRule("auth", 401)}},
	})
	send := func(c *Credential) (*Result, error) {
		if c.ID == "list#0" {
			return &Result{Status: 401, Headers: fakeHeaders{}}, nil
		}
		return &Result{Status: 200, Headers: fakeHeaders{}, Body: []byte("ok")}, nil
	}
	if _, _, err := r.Do(send); err != nil {
		t.Fatal(err)
	}

	var tried []string
	if _, _, err := r.Do(func(c *Credential) (*Result, error) {
		tried = append(tried, c.ID)
		return send(c)
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range tried {
		if id == "list#0" {
			t.Fatalf("dead credential retried: %v", tried)
		}
	}
}
