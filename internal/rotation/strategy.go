package rotation

import (
	"math/rand/v2"
	"sync"
	"time"
)

// slot binds a credential to its health. Both are owned by the pool and must
// not be mutated elsewhere.
type slot struct {
	cred  *Credential
	h     health
	use   uint64
	order int
}

// pool owns the credentials, the strategies and the health map. One mutex
// guards all of it: Pick is a handful of comparisons and a counter bump, and a
// per-credential lock would still need a snapshot of which ones are available.
type pool struct {
	mu    sync.Mutex
	cfg   Config
	slots []*slot
	byID  map[string]*slot
	rr    uint64
	now   func() time.Time
}

func newPool(cfg Config, creds []*Credential, now func() time.Time) *pool {
	if now == nil {
		now = time.Now
	}
	p := &pool{cfg: cfg, byID: make(map[string]*slot, len(creds)), now: now}
	p.replace(creds)
	return p
}

// replace swaps the credential list. Health survives for an ID that is still
// present, so a reload does not un-cool a key the upstream just rejected.
func (p *pool) replace(creds []*Credential) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	slots := make([]*slot, 0, len(creds))
	byID := make(map[string]*slot, len(creds))
	for i, c := range creds {
		if c == nil {
			continue
		}
		s, ok := p.byID[c.ID]
		if !ok {
			s = &slot{}
		}
		s.cred = c
		s.order = i
		slots = append(slots, s)
		byID[c.ID] = s
	}
	p.slots = slots
	p.byID = byID
	return nil
}

// pick returns the next credential to try, skipping excluded IDs and any
// credential whose health says it must be skipped. It returns nil when nothing
// is available: every candidate is excluded, cooling or dead.
func (p *pool) pick(exclude map[string]bool) *Credential {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.slots) == 0 {
		return nil
	}
	now := p.now()
	switch p.cfg.Strategy {
	case "random":
		return p.pickRandom(exclude)
	case "least_used":
		return p.pickLeastUsed(exclude)
	default:
		return p.pickRoundRobin(exclude, now)
	}
}

// pickRoundRobin advances a single cursor so successive requests rotate even
// when one credential is cooling.
func (p *pool) pickRoundRobin(exclude map[string]bool, now time.Time) *Credential {
	n := len(p.slots)
	for k := 0; k < n; k++ {
		i := int(p.rr % uint64(n))
		p.rr++
		s := p.slots[i]
		if exclude[s.cred.ID] || s.h.isPaused(now) {
			continue
		}
		s.use++
		return s.cred
	}
	return nil
}

// pickLeastUsed returns the available credential with the fewest uses; ties go
// to the earlier slot. Random/cooling credentials break a hot one's monopoly
// only by virtue of being skipped.
func (p *pool) pickLeastUsed(exclude map[string]bool) *Credential {
	now := p.now()
	var best *slot
	for _, s := range p.slots {
		if exclude[s.cred.ID] || s.h.isPaused(now) {
			continue
		}
		if best == nil || s.use < best.use {
			best = s
		}
	}
	if best == nil {
		return nil
	}
	best.use++
	return best.cred
}

// pickRandom reserves random draws for slots that are actually available, so a
// dead credential cannot make attempts fail by wasting a pick.
func (p *pool) pickRandom(exclude map[string]bool) *Credential {
	now := p.now()
	cand := make([]*slot, 0, len(p.slots))
	for _, s := range p.slots {
		if exclude[s.cred.ID] || s.h.isPaused(now) {
			continue
		}
		cand = append(cand, s)
	}
	if len(cand) == 0 {
		return nil
	}
	s := cand[rand.IntN(len(cand))]
	s.use++
	return s.cred
}

// healthOf returns a copy of a credential's health, for tests and /routes.
func (p *pool) healthOf(id string) (health, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byID[id]
	if !ok {
		return health{}, false
	}
	return s.h, true
}

// size returns the number of live credentials.
func (p *pool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots)
}
