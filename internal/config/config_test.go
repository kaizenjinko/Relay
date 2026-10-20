package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `
[server]
listen = ":9999"
script_dir = "scripts"

[[route]]
name = "opencode"
prefix = "/opencode"
upstream = "https://opencode.ai/inference/openai"
script = "opencode.lua"

  [route.headers.set]
  User-Agent = "opencode/1.19.3"

  [route.headers.dynamic.x-opencode-session]
  kind = "hex"
  prefix = "ses_"
  bytes = 13

[[route]]
name = "cnb"
prefix = "/cnb"
upstream = "https://cnb.cool"
script = "cnb.lua"
params = { prefix = "cnb_" }
`

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "relay.toml")
	if err := os.WriteFile(p, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Routes) != 2 {
		t.Fatalf("routes = %d", len(c.Routes))
	}
	r := c.Routes[0]
	if r.Prefix != "/opencode" || r.Name != "opencode" {
		t.Errorf("route0 = %+v", r)
	}
	if r.Timeout != "120s" {
		t.Errorf("default timeout = %q", r.Timeout)
	}
	if r.Headers.Dynamic["x-opencode-session"].Prefix != "ses_" {
		t.Errorf("dynamic header lost: %+v", r.Headers.Dynamic)
	}
	if c.Routes[1].Params["prefix"] != "cnb_" {
		t.Errorf("params lost: %+v", c.Routes[1].Params)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "relay.toml")
	os.WriteFile(p, []byte(`
[[route]]
prefix = "/x"
upstream = "https://x"
script = "x.lua"
bogus_key = 1
`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestLoadRejectsDuplicatePrefix(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "relay.toml")
	os.WriteFile(p, []byte(`
[[route]]
prefix = "/x"
upstream = "https://x"
script = "x.lua"

[[route]]
prefix = "/x"
upstream = "https://y"
script = "y.lua"
`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected duplicate prefix error")
	}
}

func TestLoadNormalizesPrefix(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "relay.toml")
	os.WriteFile(p, []byte(`
[[route]]
prefix = "api/"
upstream = "https://x"
script = "x.lua"
`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Routes[0].Prefix != "/api" {
		t.Fatalf("prefix = %q", c.Routes[0].Prefix)
	}
}

func TestScriptPathsFailsOnMissingScript(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "relay.toml")
	os.WriteFile(p, []byte(`
[server]
script_dir = "`+dir+`"

[[route]]
prefix = "/x"
upstream = "https://x"
script = "missing.lua"
`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ScriptPaths(); err == nil {
		t.Fatal("expected missing script error")
	}
}
