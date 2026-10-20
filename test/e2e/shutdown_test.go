package e2e

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildBinary compiles the relay once for the process-level tests. These tests
// exercise the real signal path, which cannot be covered in-process, so they
// live outside the package and drive the compiled binary.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "relay")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/baongo/relay")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// freePort returns an address that was free a moment ago.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func waitForListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("relay never listened on %s", addr)
}

// TestGracefulShutdownFinishesInFlightStream is the reason shutdown handling
// exists: an SSE stream mid-flight must reach its client on SIGTERM instead of
// being severed.
func TestGracefulShutdownFinishesInFlightStream(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}

	// Upstream sends one event, waits, then a second. The client should see
	// both even though SIGTERM arrives between them.
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		time.Sleep(700 * time.Millisecond)
		fmt.Fprint(w, "data: {\"n\":2}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	dir := t.TempDir()
	script := filepath.Join(dir, "s.lua")
	if err := os.WriteFile(script, []byte("return nil"), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := freePort(t)
	cfg := fmt.Sprintf(`
[server]
listen = %q
script_dir = %q
shutdown_timeout = "5s"

[[route]]
prefix = "/s"
upstream = %q
script = %q
timeout = "10s"

  [route.response]
  stream_sse = true
`, addr, dir, upstream.URL, "s.lua")
	cfgPath := filepath.Join(dir, "relay.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(buildBinary(t), "-config", cfgPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	waitForListen(t, addr)

	resp, err := http.Post("http://"+addr+"/s/x", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	<-started
	// The stream is now open. Ask the relay to stop.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	var got []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			got = append(got, line)
		}
	}
	if len(got) != 2 {
		t.Fatalf("stream truncated on shutdown: got %d events %v", len(got), got)
	}
	if !strings.Contains(got[1], `"n":2`) {
		t.Fatalf("lost the tail of the stream: %v", got)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay exited non-zero after graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not exit after SIGTERM")
	}
}

// TestShutdownRefusesNewRequests: after SIGTERM the listener either refuses the
// connection or answers 503 quickly. It must not hang.
func TestShutdownRefusesNewRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.lua"), []byte("return nil"), 0o644)
	addr := freePort(t)
	cfg := fmt.Sprintf(`
[server]
listen = %q
script_dir = %q
shutdown_timeout = "5s"

[[route]]
prefix = "/s"
upstream = %q
script = "s.lua"
timeout = "5s"
`, addr, dir, upstream.URL)
	cfgPath := filepath.Join(dir, "relay.toml")
	os.WriteFile(cfgPath, []byte(cfg), 0o644)

	cmd := exec.Command(buildBinary(t), "-config", cfgPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	waitForListen(t, addr)

	// One request to confirm the process is serving, then shut down.
	if _, err := http.Get("http://" + addr + "/s/x"); err != nil {
		t.Fatalf("pre-shutdown request: %v", err)
	}
	cmd.Process.Signal(syscall.SIGTERM)

	// Wait for the process to exit; the port must become unreachable.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("listener still accepting connections after SIGTERM")
}

// TestJSONLogOnStartup: log_format = "json" must emit parseable records.
func TestJSONLogOnStartup(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.lua"), []byte("return nil"), 0o644)
	addr := freePort(t)
	cfg := fmt.Sprintf(`
[server]
listen = %q
script_dir = %q
log_format = "json"

[[route]]
prefix = "/s"
upstream = %q
script = "s.lua"
`, addr, dir, upstream.URL)
	cfgPath := filepath.Join(dir, "relay.toml")
	os.WriteFile(cfgPath, []byte(cfg), 0o644)

	cmd := exec.Command(buildBinary(t), "-config", cfgPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		t.Fatal("no startup log line")
	}
	line := sc.Text()
	for _, want := range []string{`"level":"info"`, `"msg":"relay listening"`, `"routes":1`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %q: %s", want, line)
		}
	}
}
