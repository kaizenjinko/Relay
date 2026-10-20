package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materialises a relay.toml plus its scripts in a temp dir.
func writeTree(t *testing.T, tomlBody string, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range scripts {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	cfg := filepath.Join(dir, "relay.toml")
	body := strings.ReplaceAll(tomlBody, "DIR", dir)
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfg
}

func TestBuildLoadsRoutesAndScripts(t *testing.T) {
	cfgPath := writeTree(t, `
[server]
listen = ":0"
script_dir = "DIR"

[[route]]
name     = "a"
prefix   = "/a"
upstream = "http://127.0.0.1:1"
script   = "a.lua"

[[route]]
name     = "b"
prefix   = "/b"
upstream = "http://127.0.0.1:2"
script   = "b.lua"
`, map[string]string{
		"a.lua": `return nil`,
		"b.lua": `return { headers = { ["X-B"] = "1" } }`,
	})

	cfg, srv, err := build(options{configPath: cfgPath})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if srv == nil {
		t.Fatal("build returned nil server")
	}
	if got := len(cfg.Routes); got != 2 {
		t.Fatalf("routes = %d, want 2", got)
	}
}

func TestBuildListenOverride(t *testing.T) {
	cfgPath := writeTree(t, `
[server]
listen = ":8899"
script_dir = "DIR"

[[route]]
name     = "a"
prefix   = "/a"
upstream = "http://127.0.0.1:1"
script   = "a.lua"
`, map[string]string{"a.lua": `return nil`})

	cfg, _, err := build(options{configPath: cfgPath, listen: "127.0.0.1:1234"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:1234" {
		t.Fatalf("listen = %q, want override", cfg.Server.Listen)
	}

	cfg, _, err = build(options{configPath: cfgPath, verbose: true})
	if err != nil {
		t.Fatalf("build verbose: %v", err)
	}
	if !cfg.Server.Verbose {
		t.Fatal("verbose override not applied")
	}
}

// A syntax error in any script must fail startup, not surface later as a 500.
func TestBuildRejectsScriptSyntaxError(t *testing.T) {
	cfgPath := writeTree(t, `
[server]
script_dir = "DIR"

[[route]]
name     = "a"
prefix   = "/a"
upstream = "http://127.0.0.1:1"
script   = "bad.lua"
`, map[string]string{"bad.lua": `function oops(`})

	_, _, err := build(options{configPath: cfgPath})
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "script") {
		t.Fatalf("error should name the script, got %v", err)
	}
}

func TestBuildRejectsMissingScript(t *testing.T) {
	cfgPath := writeTree(t, `
[server]
script_dir = "DIR"

[[route]]
name     = "a"
prefix   = "/a"
upstream = "http://127.0.0.1:1"
script   = "absent.lua"
`, nil)

	_, _, err := build(options{configPath: cfgPath})
	if err == nil {
		t.Fatal("expected missing script error, got nil")
	}
}

func TestBuildRejectsBadConfigPath(t *testing.T) {
	_, _, err := build(options{configPath: filepath.Join(t.TempDir(), "nope.toml")})
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestBuildRejectsUnknownConfigKey(t *testing.T) {
	cfgPath := writeTree(t, `
[server]
script_dir = "DIR"
not_a_real_key = 1
`, nil)

	_, _, err := build(options{configPath: cfgPath})
	if err == nil {
		t.Fatal("expected strict-decode error, got nil")
	}
}

func TestNewHTTPServerAppliesConfig(t *testing.T) {
	cfgPath := writeTree(t, `
[server]
listen     = ":0"
max_conns  = 77
read_timeout = "5s"
script_dir = "DIR"

[[route]]
name     = "a"
prefix   = "/a"
upstream = "http://127.0.0.1:1"
script   = "a.lua"
`, map[string]string{"a.lua": `return nil`})

	cfg, srv, err := build(options{configPath: cfgPath})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := newHTTPServer(cfg, srv.Handler())
	if s.Concurrency != 77 {
		t.Fatalf("Concurrency = %d, want 77", s.Concurrency)
	}
	if s.ReadTimeout.Seconds() != 5 {
		t.Fatalf("ReadTimeout = %v, want 5s", s.ReadTimeout)
	}
	if !s.NoDefaultServerHeader {
		t.Fatal("NoDefaultServerHeader should be set so relays are not fingerprintable")
	}
	if s.MaxRequestBodySize != 64<<20 {
		t.Fatalf("MaxRequestBodySize = %d", s.MaxRequestBodySize)
	}

	// One shared script instance must serve the route handler.
	status, body := postJSON(s.Handler, "/a/x", `{"messages":[]}`)
	if status != 502 {
		t.Fatalf("unreachable upstream should be 502, got %d (%q)", status, body)
	}
}
