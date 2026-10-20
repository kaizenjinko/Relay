package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the single relay.toml file. One process serves every route listed
// here; each route points at a Lua script and an upstream.
type Config struct {
	Server Server  `toml:"server"`
	Routes []Route `toml:"route"`

	// baseDir is the directory holding relay.toml. Relative paths in the file
	// are resolved against it.
	baseDir string
}

// resolvePath makes p absolute relative to the config file's directory. An
// already-absolute p is returned unchanged.
func (c *Config) resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.baseDir, p)
}

type Server struct {
	Listen      string `toml:"listen"`
	MaxConns    int    `toml:"max_conns"`
	Verbose     bool   `toml:"verbose"`
	ReadTimeout string `toml:"read_timeout"`
	ScriptDir   string `toml:"script_dir"`

	// ShutdownTimeout bounds how long an in-flight request may keep running
	// after SIGTERM. Streaming responses are allowed to finish; the relay never
	// severs a live SSE stream without giving it this long first.
	ShutdownTimeout string `toml:"shutdown_timeout"`
	// LogFormat is "text" (default) or "json". JSON is one object per line, for
	// shipping to a log collector.
	LogFormat string `toml:"log_format"`
	// MetricsPath enables a Prometheus-format endpoint at this path. Empty
	// disables it. It is served on the same listener as the routes.
	MetricsPath string `toml:"metrics_path"`
	// AdminToken, when set, is required as "Authorization: Bearer <token>" for
	// the metrics endpoint. Leave empty only when the listener is private.
	AdminToken string `toml:"admin_token"`
}

// Route maps an incoming path prefix to an upstream and a Lua script.
//
// The relay strips Prefix from the request path before forwarding, so a client
// calling /opencode/v1/chat/completions with prefix "/opencode" and upstream
// "https://opencode.ai/inference/openai" reaches
// https://opencode.ai/inference/openai/v1/chat/completions.
type Route struct {
	Name     string   `toml:"name"`
	Prefix   string   `toml:"prefix"`
	Upstream string   `toml:"upstream"`
	Script   string   `toml:"script"`
	Timeout  string   `toml:"timeout"`
	Headers  Headers  `toml:"headers"`
	Params   Params   `toml:"params"`
	Response Response `toml:"response"`
	// Rotation is optional. When nil the route sends exactly one request and
	// behaves as it did before rotation existed.
	Rotation *Rotation `toml:"rotation"`
}

// Rotation describes a per-route credential pool and its retry policy.
//
//	[[route.rotation.source]]
//	kind = "env"
//	name = "DEEPSEEK_KEYS"
//	template = { Authorization = "Bearer {key}" }
//
//	[route.rotation]
//	strategy     = "round_robin"
//	max_attempts = 3
//
//	[[route.rotation.failure.rule]]
//	name   = "auth_expired"
//	status = [401]
//	retry  = true
type Rotation struct {
	// Source is an array of tables: `[[route.rotation.source]]`. One source
	// may list several keys; several sources are concatenated.
	Source        []*RotSource `toml:"source"`
	Strategy      string       `toml:"strategy"`
	MaxAttempts   int          `toml:"max_attempts"`
	Cooldown      string       `toml:"cooldown"`
	MaxCooldown   string       `toml:"max_cooldown"`
	BackoffMult   float64      `toml:"backoff_mult"`
	FailThreshold int          `toml:"fail_threshold"`
	ResetAfter    string       `toml:"reset_after"`
	// StickyBy is accepted and validated but not implemented: it parses and
	// boots, and the rotator reports it as unavailable on /routes.
	StickyBy string  `toml:"sticky_by"`
	Failure  Failure `toml:"failure"`
}

// RotSource is one credential source.
//
//	kind = "static"     headers literal
//	kind = "list"       keys array, template applied
//	kind = "file"       one key per line, template applied
//	kind = "env"        comma-separated keys in env `name`
//	kind = "cookie_jar" JSON {"accounts":[{"id","headers","meta"}]}
type RotSource struct {
	Kind     string            `toml:"kind"`
	Name     string            `toml:"name"`
	Path     string            `toml:"path"`
	Keys     []string          `toml:"keys"`
	Template map[string]string `toml:"template"`
	Headers  map[string]string `toml:"headers"`
	Meta     map[string]string `toml:"meta"`
}

// Failure is the ordered rule list used to classify an upstream response.
// Rules are written as `[[route.rotation.failure.rule]]`: an ordered array of
// tables, so TOML order is preserved.
type Failure struct {
	Rule []*Rule `toml:"rule"`
}

// Rule matches an upstream response. Rules are evaluated in order and the
// first match wins; `abort` wins over `retry` inside a matched rule.
type Rule struct {
	Name          string            `toml:"name"`
	Status        []int             `toml:"status"`
	HeaderMatch   map[string]string `toml:"header_match"`
	BodyRegex     string            `toml:"body_regex"`
	Retry         bool              `toml:"retry"`
	Abort         bool              `toml:"abort"`
	UseRetryAfter bool              `toml:"use_retry_after"`
}

type Headers struct {
	Set     map[string]string  `toml:"set"`
	Drop    []string           `toml:"drop"`
	Dynamic map[string]*DynHdr `toml:"dynamic"`
}

// DynHdr generates a header value per request.
//
//	kind = "hex"    -> prefix + hex(rand(bytes))
//	kind = "uuid"   -> prefix + uuid v4
//	kind = "static" -> prefix + value
//	kind = "env"    -> the value of environment variable `name`, recomputed
//	                   each request so a credential can be rotated by editing
//	                   the process environment and sending SIGHUP, or simply
//	                   by restarting with a new value.
//	kind = "file"   -> the trimmed contents of `path`, re-read on change
type DynHdr struct {
	Kind   string `toml:"kind"`
	Prefix string `toml:"prefix"`
	Bytes  int    `toml:"bytes"`
	Value  string `toml:"value"`
	Name   string `toml:"name"`
	Path   string `toml:"path"`
}

// Params is passed verbatim to the Lua script as the global `params` table.
type Params map[string]any

type Response struct {
	// StreamSSE forces SSE interpretation instead of sniffing Content-Type.
	StreamSSE *bool `toml:"stream_sse"`
}

// Load reads and validates relay.toml.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Remember where the file lives so relative paths inside it (script_dir,
	// rotation sources) resolve the same way whether the relay is started from
	// the repo, from systemd, or from inside a container whose cwd is /.
	if abs, err := filepath.Abs(path); err == nil {
		c.baseDir = filepath.Dir(abs)
	}
	if u := md.Undecoded(); len(u) > 0 {
		var names []string
		for _, k := range u {
			names = append(names, k.String())
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(names, ", "))
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8787"
	}
	if c.Server.MaxConns == 0 {
		c.Server.MaxConns = 512
	}
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "30s"
	}
	if c.Server.ShutdownTimeout == "" {
		c.Server.ShutdownTimeout = "30s"
	}
	if c.Server.LogFormat == "" {
		c.Server.LogFormat = "text"
	}
	switch c.Server.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("server.log_format: want \"text\" or \"json\", got %q", c.Server.LogFormat)
	}
	if _, err := time.ParseDuration(c.Server.ShutdownTimeout); err != nil {
		return fmt.Errorf("server.shutdown_timeout: %w", err)
	}
	if c.Server.MetricsPath != "" && !strings.HasPrefix(c.Server.MetricsPath, "/") {
		c.Server.MetricsPath = "/" + c.Server.MetricsPath
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one [[route]] is required")
	}
	seen := map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Prefix == "" {
			return fmt.Errorf("route #%d: prefix is required", i)
		}
		if !strings.HasPrefix(r.Prefix, "/") {
			r.Prefix = "/" + r.Prefix
		}
		r.Prefix = strings.TrimSuffix(r.Prefix, "/")
		if seen[r.Prefix] {
			return fmt.Errorf("route #%d: duplicate prefix %q", i, r.Prefix)
		}
		seen[r.Prefix] = true
		if r.Name == "" {
			r.Name = strings.TrimPrefix(r.Prefix, "/")
		}
		if r.Upstream == "" {
			return fmt.Errorf("route %q: upstream is required", r.Name)
		}
		if r.Script == "" {
			return fmt.Errorf("route %q: script is required", r.Name)
		}
		if r.Timeout == "" {
			r.Timeout = "120s"
		}
		if _, err := time.ParseDuration(r.Timeout); err != nil {
			return fmt.Errorf("route %q: timeout: %w", r.Name, err)
		}
		for name, d := range r.Headers.Dynamic {
			if name == "" {
				return fmt.Errorf("route %q: empty dynamic header name", r.Name)
			}
			switch d.Kind {
			case "", "hex", "uuid", "static", "env", "file":
			default:
				return fmt.Errorf("route %q: dynamic header %s: unknown kind %q", r.Name, name, d.Kind)
			}
			if d.Kind == "static" && d.Value == "" {
				return fmt.Errorf("route %q: dynamic header %s: static needs value", r.Name, name)
			}
			if d.Kind == "env" && d.Name == "" {
				return fmt.Errorf("route %q: dynamic header %s: env needs name", r.Name, name)
			}
			if d.Kind == "file" && d.Path == "" {
				return fmt.Errorf("route %q: dynamic header %s: file needs path", r.Name, name)
			}
		}
		if r.Rotation != nil {
			if err := r.Rotation.validate(r.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

var rotationSources = map[string]bool{
	"static": true, "list": true, "file": true, "env": true, "cookie_jar": true,
}

var rotationStrategies = map[string]bool{
	"": true, "round_robin": true, "least_used": true, "random": true,
}

func (rot *Rotation) validate(route string) error {
	if len(rot.Source) == 0 {
		return fmt.Errorf("route %q: rotation needs at least one [[route.rotation.source]]", route)
	}
	for i, s := range rot.Source {
		if s == nil {
			return fmt.Errorf("route %q: rotation source #%d is empty", route, i)
		}
		if !rotationSources[s.Kind] {
			return fmt.Errorf("route %q: rotation source #%d: unknown kind %q", route, i, s.Kind)
		}
		switch s.Kind {
		case "static":
			if len(s.Headers) == 0 {
				return fmt.Errorf("route %q: rotation source #%d: static needs headers", route, i)
			}
		case "list":
			if len(s.Keys) == 0 {
				return fmt.Errorf("route %q: rotation source #%d: list needs keys", route, i)
			}
		case "file":
			if s.Path == "" {
				return fmt.Errorf("route %q: rotation source #%d: file needs path", route, i)
			}
		case "env":
			if s.Name == "" {
				return fmt.Errorf("route %q: rotation source #%d: env needs name", route, i)
			}
		case "cookie_jar":
			if s.Path == "" {
				return fmt.Errorf("route %q: rotation source #%d: cookie_jar needs path", route, i)
			}
		}
		if s.Kind != "static" && s.Kind != "cookie_jar" && len(s.Template) == 0 {
			s.Template = map[string]string{"Authorization": "Bearer {key}"}
		}
	}
	if !rotationStrategies[rot.Strategy] {
		return fmt.Errorf("route %q: rotation: unknown strategy %q", route, rot.Strategy)
	}
	if rot.Strategy == "" {
		rot.Strategy = "round_robin"
	}
	if rot.MaxAttempts == 0 {
		rot.MaxAttempts = 3
	}
	if rot.MaxAttempts < 1 {
		return fmt.Errorf("route %q: rotation: max_attempts must be >= 1", route)
	}
	if rot.FailThreshold == 0 {
		rot.FailThreshold = 5
	}
	if rot.FailThreshold < 1 {
		return fmt.Errorf("route %q: rotation: fail_threshold must be >= 1", route)
	}
	if rot.BackoffMult == 0 {
		rot.BackoffMult = 2.0
	}
	if rot.BackoffMult < 1 {
		return fmt.Errorf("route %q: rotation: backoff_mult must be >= 1", route)
	}
	durations := []struct {
		field string
		v     *string
		def   string
	}{
		{"cooldown", &rot.Cooldown, "60s"},
		{"max_cooldown", &rot.MaxCooldown, "30m"},
		{"reset_after", &rot.ResetAfter, "15m"},
	}
	for _, d := range durations {
		if *d.v == "" {
			*d.v = d.def
		}
		if _, err := time.ParseDuration(*d.v); err != nil {
			return fmt.Errorf("route %q: rotation: %s: %w", route, d.field, err)
		}
	}
	for i, rule := range rot.Failure.Rule {
		if rule == nil {
			return fmt.Errorf("route %q: rotation: failure rule #%d is empty", route, i)
		}
		if len(rule.Status) == 0 && len(rule.HeaderMatch) == 0 && rule.BodyRegex == "" {
			return fmt.Errorf("route %q: rotation: failure rule #%d matches nothing", route, i)
		}
	}
	return nil
}

// ScriptPaths resolves each route's script against script_dir. Returning the
// resolved path up front means a typo fails at startup, not on first request.
func (c *Config) ScriptPaths() ([]string, error) {
	out := make([]string, 0, len(c.Routes))
	for _, r := range c.Routes {
		p := r.Script
		if !strings.HasPrefix(p, "/") && c.Server.ScriptDir != "" {
			p = strings.TrimSuffix(c.Server.ScriptDir, "/") + "/" + p
		}
		p = c.resolvePath(p)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("route %q: script: %w", r.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// ResolveRotationPaths rewrites every file-bearing rotation source path to be
// relative to the config file's directory.
func (c *Config) ResolveRotationPaths(r *Rotation) {
	if r == nil {
		return
	}
	for _, src := range r.Source {
		if src == nil {
			continue
		}
		switch src.Kind {
		case "file", "cookie_jar":
			src.Path = c.resolvePath(src.Path)
		}
	}
}
