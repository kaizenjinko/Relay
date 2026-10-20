package rotation

import (
	"time"
)

// state is a credential's health.
type state int

const (
	healthy state = iota
	cooling
	dead
)

func (s state) String() string {
	switch s {
	case cooling:
		return "cooling"
	case dead:
		return "dead"
	}
	return "healthy"
}

// health is the per-credential health record. All fields are guarded by
// pool.mu; nothing here is atomic so the pool can update several fields at
// once.
type health struct {
	state state
	// until is when a cooling credential may be picked again.
	until time.Time
	// failures counts consecutive failures since the last success or expiry of
	// the reset window; it drives the backoff exponent.
	failures int
	// lastFailure is when failures was last bumped, for reset_after.
	lastFailure time.Time
	// cooldown is the current backoff window, kept so a repeated MarkFailed
	// while still cooling grows the window rather than restarting it.
	cooldown time.Duration
}

// isPaused reports whether the credential must be skipped at now.
func (h *health) isPaused(now time.Time) bool {
	switch h.state {
	case dead:
		return true
	case cooling:
		return now.Before(h.until)
	}
	return false
}

// markFailed applies one failure at now and returns the new state.
func (h *health) markFailed(now time.Time, cfg Config, retryAfter time.Duration) state {
	// Failures spaced further apart than reset_after are a new incident, not a
	// continuation: start the backoff over.
	if cfg.ResetAfter > 0 && !h.lastFailure.IsZero() && now.Sub(h.lastFailure) > cfg.ResetAfter {
		h.failures = 0
		h.cooldown = 0
		h.state = healthy
	}
	h.failures++
	h.lastFailure = now

	// The threshold counts consecutive failures regardless of the current
	// state: after the first failure the credential is cooling, not healthy,
	// and requiring healthy here would mean a credential that fails forever
	// never reaches dead.
	if cfg.FailThreshold > 0 && h.failures >= cfg.FailThreshold {
		h.state = dead
		h.until = time.Time{}
		return h.state
	}

	// The window doubles per consecutive failure, capped at max_cooldown, and a
	// Retry-After longer than the computed window wins. The multiplier is
	// applied to the previous window, so the sequence is base, base*mult,
	// base*mult^2, ...; the first failure starts at base.
	base := cfg.Cooldown
	if base <= 0 {
		base = 60 * time.Second
	}
	mult := cfg.BackoffMult
	if mult < 1 {
		mult = 1
	}
	d := base
	if h.failures > 1 {
		d = base
		for i := 1; i < h.failures; i++ {
			d = time.Duration(float64(d) * mult)
			if max := cfg.MaxCooldown; max > 0 && d >= max {
				d = max
				break
			}
		}
	}
	if max := cfg.MaxCooldown; max > 0 && d > max {
		d = max
	}
	if retryAfter > d {
		d = retryAfter
	}
	h.cooldown = d
	h.state = cooling
	h.until = now.Add(d)
	return h.state
}

// markSuccess clears the backoff. A dead credential stays dead: only a
// successful mark on a live request proves it, and a dead one is never picked.
func (h *health) markSuccess() {
	if h.state == dead {
		return
	}
	h.state = healthy
	h.until = time.Time{}
	h.failures = 0
	h.cooldown = 0
	h.lastFailure = time.Time{}
}
