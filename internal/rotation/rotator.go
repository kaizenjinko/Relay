package rotation

import (
	"strconv"
	"time"
)

// Sender performs one upstream attempt. The body must be sent byte-for-byte as
// given; the rotator calls the same Sender once per attempt with unchanged
// input, so a sender must not consume or rewrite it.
//
// resp.Headers is only read for Retry-After; body is only read when a rule that
// requires it matched.
type Sender func(cred *Credential) (*Result, error)

// Result is one upstream response as the rotator sees it.
type Result struct {
	Status  int
	Headers limiter
	Body    []byte
}

// Attempt record of one call, filled by Rotator.Do.
type Attempt struct {
	Credential string
	Status     int
	Class      FailureClass
	Err        error
	RetryAfter time.Duration
	Duration   time.Duration
}

// Rotator is the per-route credential pool plus its retry policy.
type Rotator struct {
	cfg   Config
	rules []*failureRule
	pool  *pool
	now   func() time.Time
}

func newRotator(cfg Config, rules []*failureRule, creds []*Credential) *Rotator {
	return &Rotator{cfg: cfg, rules: rules, now: time.Now, pool: newPool(cfg, creds, nil)}
}

// Config returns the effective configuration, defaults applied.
func (r *Rotator) Config() Config { return r.cfg }

// Credentials returns the current credential set. The slice is a copy; the
// credentials themselves are shared.
func (r *Rotator) Credentials() []*Credential {
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	out := make([]*Credential, 0, len(r.pool.slots))
	for _, s := range r.pool.slots {
		out = append(out, s.cred)
	}
	return out
}

// Pick returns the next credential, skipping anything whose health says it
// must sit out. nil means every credential is cooling or dead.
func (r *Rotator) Pick() *Credential {
	return r.pool.pick(nil)
}

// setClock replaces the time source. Tests call it before any request.
func (r *Rotator) setClock(now func() time.Time) {
	r.now = now
	r.pool.now = now
}

// Do runs the retry loop. Each attempt picks a credential, calls send, and
// classifies the result: a matched retry rule (and attempts left and an
// untried credential) causes another attempt. The last response wins, so an
// exhausted route returns the upstream's own error rather than a synthesised
// one.
//
// This never inspects a stream: the caller passes a fully buffered Result, so
// by the time Do returns no bytes have reached the client and retrying is
// safe. If Pick returns nil the call returns a nil Result and a non-nil error;
// callers should treat nil as "no credential available".
func (r *Rotator) Do(send Sender) (*Result, []Attempt, error) {
	tried := make(map[string]bool, r.pool.size())
	var (
		attempts []Attempt
		last     *Result
		lastErr  error
	)
	for i := 0; i < r.cfg.MaxAttempts; i++ {
		cred := r.pool.pick(tried)
		if cred == nil {
			break
		}
		tried[cred.ID] = true

		start := r.now()
		res, err := send(cred)
		attempts = append(attempts, Attempt{
			Credential: cred.ID,
			Err:        err,
			Duration:   r.now().Sub(start),
		})
		if err != nil {
			// A transport error has no rules to match. Cool the credential so
			// the next request prefers a different one.
			attempts[len(attempts)-1].Class = FailureClass{Rule: "transport", Matched: true}
			r.pool.mu.Lock()
			if s := r.pool.byID[cred.ID]; s != nil {
				s.h.markFailed(r.now(), r.cfg, 0)
			}
			r.pool.mu.Unlock()
			lastErr = err
			continue
		}

		last, lastErr = res, nil
		class := r.classify(res)
		attempts[len(attempts)-1].Status = res.Status
		attempts[len(attempts)-1].Class = class

		if !class.Matched {
			// A response no rule matched is the one the client gets. Continue
			// looping here would send the request a second time on the next
			// credential and let that response overwrite a good result.
			r.markSuccess(cred)
			break
		}

		var retryAfter time.Duration
		if res.Headers != nil && r.ruleUsesRetryAfter(class.Rule) {
			retryAfter = parseRetryAfter(res.Headers.Peek("Retry-After"), r.now())
			attempts[len(attempts)-1].RetryAfter = retryAfter
		}
		if class.Retry || class.Abort {
			r.pool.mu.Lock()
			if s := r.pool.byID[cred.ID]; s != nil {
				s.h.markFailed(r.now(), r.cfg, retryAfter)
			}
			r.pool.mu.Unlock()
		}
		if class.Abort || !class.Retry {
			break
		}
	}
	if last == nil {
		return nil, attempts, lastErr
	}
	return last, attempts, nil
}

func (r *Rotator) classify(res *Result) FailureClass {
	return classify(r.rules, res.Status, res.Headers, res.Body)
}

// ruleUsesRetryAfter reports whether a rule asked for Retry-After to be
// honoured. Linear scan: rule lists are a handful of entries.
func (r *Rotator) ruleUsesRetryAfter(name string) bool {
	for _, rule := range r.rules {
		if rule.name == name {
			return rule.useRetryAfter
		}
	}
	return false
}

// MarkResult applies one observed response to a credential's health. It is what
// Do calls internally, exposed for callers that drive sending themselves.
func (r *Rotator) MarkResult(cred *Credential, status int, headers limiter, body []byte) FailureClass {
	class := classify(r.rules, status, headers, body)
	if !class.Matched {
		r.markSuccess(cred)
		return class
	}
	var retryAfter time.Duration
	if headers != nil && r.ruleUsesRetryAfter(class.Rule) {
		retryAfter = parseRetryAfter(headers.Peek("Retry-After"), r.now())
	}
	if class.Retry || class.Abort {
		r.pool.mu.Lock()
		if s := r.pool.byID[cred.ID]; s != nil {
			s.h.markFailed(r.now(), r.cfg, retryAfter)
		}
		r.pool.mu.Unlock()
	}
	return class
}

// MarkSuccess clears a credential's backoff.
func (r *Rotator) MarkSuccess(cred *Credential) { r.markSuccess(cred) }

// MarkFailed records a failure with an explicit retry hint (zero means
// "compute the backoff from configuration").
func (r *Rotator) MarkFailed(cred *Credential, retryAfter time.Duration) {
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	if s := r.pool.byID[cred.ID]; s != nil {
		s.h.markFailed(r.now(), r.cfg, retryAfter)
	}
}

func (r *Rotator) markSuccess(cred *Credential) {
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	if s := r.pool.byID[cred.ID]; s != nil {
		s.h.markSuccess()
	}
}

// stateOf formats a credential's health as a short token for logging.
func (r *Rotator) stateOf(cred *Credential) string {
	h, ok := r.pool.healthOf(cred.ID)
	if !ok {
		return "unknown"
	}
	if h.state == cooling {
		return "cooling(" + strconv.FormatInt(int64(h.until.Sub(r.now()).Round(time.Second)/time.Second), 10) + "s)"
	}
	return h.state.String()
}
