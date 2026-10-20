// Package rotation holds a pool of upstream credentials and decides which one
// a request goes out with.
//
// A Rotator is built once per route at startup; List on every configured
// Source is called at build time. Pick and MarkResult are safe for concurrent
// use and never contend on a source.
package rotation

import (
	"fmt"
	"strings"
	"time"

	"github.com/baongo/relay/internal/config"
)

// Credential is one identity that can be attached to a request.
//
// Headers are applied over the route headers and win on conflict. Meta is
// carried for logging and future sticky routing; nothing in this package
// interprets it.
type Credential struct {
	ID      string
	Headers map[string]string
	Meta    map[string]string
}

// Source produces credentials.
type Source interface {
	// List returns the current credential set. Called at build time and on
	// reload; implementations may cache.
	List() ([]*Credential, error)
}

const bodyCap = 1 << 20

// FailureClass is the outcome of matching an upstream response against the
// configured rules.
type FailureClass struct {
	Rule    string // matched rule name, "" when no rule matched
	Retry   bool
	Abort   bool
	Matched bool
}

func (f FailureClass) String() string {
	if !f.Matched {
		return "none"
	}
	switch {
	case f.Abort:
		return f.Rule + ":abort"
	case f.Retry:
		return f.Rule + ":retry"
	}
	return f.Rule
}

// Config is the rotator's knobs. Durations are already parsed so the request
// path never touches a string.
type Config struct {
	Strategy      string
	MaxAttempts   int
	Cooldown      time.Duration
	MaxCooldown   time.Duration
	BackoffMult   float64
	FailThreshold int
	ResetAfter    time.Duration
	// StickyBy is parsed and carried but not implemented: picking is decided
	// per attempt by Strategy alone. See README-level note in the phase report.
	StickyBy string
	Rules    []*config.Rule
}

// DefaultConfig mirrors the defaults applied by config.validate, for callers
// that build a rotator directly.
func DefaultConfig() Config {
	return Config{
		Strategy:      "round_robin",
		MaxAttempts:   3,
		Cooldown:      60 * time.Second,
		MaxCooldown:   30 * time.Minute,
		BackoffMult:   2.0,
		FailThreshold: 5,
		ResetAfter:    15 * time.Minute,
	}
}

// New builds a rotator from a config.Route's rotation block. It fails when
// there is no rotation block, when no source yields a credential, or when a
// rule regex does not compile: a route that cannot authenticate should not
// start.
func New(cfg *config.Rotation) (*Rotator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("rotation: no configuration")
	}
	return NewWithConfig(cfg, DefaultConfig())
}

// NewWithConfig is New with explicit health/retry defaults. strconv-level
// fields (strategy, max_attempts, ...) come from cfg; the durations and
// thresholds in defaults are overridden by any duration present in cfg.
func NewWithConfig(cfg *config.Rotation, defaults Config) (*Rotator, error) {
	rc := defaults
	if cfg.Strategy != "" {
		rc.Strategy = cfg.Strategy
	}
	if cfg.MaxAttempts > 0 {
		rc.MaxAttempts = cfg.MaxAttempts
	}
	if cfg.FailThreshold > 0 {
		rc.FailThreshold = cfg.FailThreshold
	}
	if cfg.BackoffMult > 0 {
		rc.BackoffMult = cfg.BackoffMult
	}
	var err error
	if rc.Cooldown, err = pickDuration(cfg.Cooldown, rc.Cooldown); err != nil {
		return nil, fmt.Errorf("rotation: cooldown: %w", err)
	}
	if rc.MaxCooldown, err = pickDuration(cfg.MaxCooldown, rc.MaxCooldown); err != nil {
		return nil, fmt.Errorf("rotation: max_cooldown: %w", err)
	}
	if rc.ResetAfter, err = pickDuration(cfg.ResetAfter, rc.ResetAfter); err != nil {
		return nil, fmt.Errorf("rotation: reset_after: %w", err)
	}
	if rc.MaxCooldown < rc.Cooldown {
		rc.MaxCooldown = rc.Cooldown
	}
	if rc.FailThreshold < 1 {
		rc.FailThreshold = 1
	}
	if rc.BackoffMult < 1 {
		rc.BackoffMult = 1
	}
	if rc.MaxAttempts < 1 {
		rc.MaxAttempts = 1
	}
	if rc.Strategy == "" {
		rc.Strategy = "round_robin"
	}
	switch rc.Strategy {
	case "round_robin", "least_used", "random":
	default:
		return nil, fmt.Errorf("rotation: unknown strategy %q", rc.Strategy)
	}
	rules, err := compileRules(cfg.Failure.Rule)
	if err != nil {
		return nil, err
	}
	rc.Rules = nil
	if cfg.StickyBy != "" {
		rc.StickyBy = cfg.StickyBy
	}

	creds, err := loadSources(cfg.Source)
	if err != nil {
		return nil, err
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("rotation: source produced no credentials")
	}
	return newRotator(rc, rules, creds), nil
}

func pickDuration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

// loadSources merges every configured source into one credential list. Keys
// are concatenated in TOML order and duplicates removed by ID, so a key listed
// twice is tried once.
func loadSources(sources []*config.RotSource) ([]*Credential, error) {
	var all []*Credential
	for _, sc := range sources {
		var (
			s   Source
			err error
		)
		switch sc.Kind {
		case "static":
			s = newStaticSource(sc.Headers, sc.Meta)
		case "list":
			s = newListSource(sc.Keys, sc.Template, sc.Meta)
		case "file":
			s = newFileSource(sc.Path, sc.Template, sc.Meta)
		case "env":
			s = newEnvSource(sc.Name, sc.Template, sc.Meta)
		case "cookie_jar":
			s, err = newCookieJarSource(sc.Path)
		default:
			return nil, fmt.Errorf("rotation: unknown source kind %q", sc.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("rotation: source %s: %w", sc.Kind, err)
		}
		creds, err := s.List()
		if err != nil {
			return nil, fmt.Errorf("rotation: source %s: %w", sc.Kind, err)
		}
		all = append(all, creds...)
	}
	return dedupe(all), nil
}

func dedupe(in []*Credential) []*Credential {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, c := range in {
		if c == nil || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	return out
}

// splitKeys trims whitespace, drops empty entries and duplicates. A key list
// with a stray blank line (file) or trailing comma (env) must not produce a
// credential that can never succeed.
func splitKeys(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, k := range raw {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// renderTemplate substitutes {key}.
//
// A template value without a {key} placeholder is used verbatim: the key
// appears nowhere in that header, which is what "Authorization = \"Bearer\""
// plus a key means to nobody. Callers who want the key always write {key}.
// All occurrences are replaced; other braces are left alone.
func renderTemplate(tpl map[string]string, key string) map[string]string {
	if len(tpl) == 0 {
		return nil
	}
	out := make(map[string]string, len(tpl))
	for name, v := range tpl {
		out[name] = strings.ReplaceAll(v, "{key}", key)
	}
	return out
}

// reload re-lists every source and swaps the pool contents. Exposed for a
// future SIGHUP path; nothing in the server calls it yet.
func (r *Rotator) reload(sources []*config.RotSource) error {
	creds, err := loadSources(sources)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		return fmt.Errorf("rotation: reload produced no credentials")
	}
	return r.pool.replace(creds)
}
