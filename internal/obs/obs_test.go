package obs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLoggerJSONIsParseable(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, true)
	l.Info("hello", "route", "p", "status", 200)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["level"] != "info" {
		t.Fatalf("bad record: %v", rec)
	}
	if rec["route"] != "p" || rec["status"] != float64(200) {
		t.Fatalf("kv missing: %v", rec)
	}
	if _, ok := rec["ts"]; !ok {
		t.Fatal("missing timestamp")
	}
}

func TestLoggerTextFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, false)
	l.Warn("careful", "route", "p")
	got := buf.String()
	if !strings.Contains(got, "warn careful") || !strings.Contains(got, "route=p") {
		t.Fatalf("unexpected text log: %q", got)
	}
	if strings.HasSuffix(got, "\n\n") {
		t.Fatal("double newline")
	}
}

func TestLoggerWithCarriesFields(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, true).With("component", "relay")
	l.Error("boom", "err", "x")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["component"] != "relay" || rec["err"] != "x" {
		t.Fatalf("bound field lost: %v", rec)
	}
}

// With must not mutate the parent logger: two derived loggers from one parent
// cannot see each other's fields.
func TestLoggerWithIsIsolated(t *testing.T) {
	var buf bytes.Buffer
	base := New(&buf, true)
	a := base.With("k", "a")
	b := base.With("k", "b")
	b.Info("from-b")

	if strings.Contains(buf.String(), `"from-b"`) && !strings.Contains(buf.String(), `"k":"b"`) {
		t.Fatalf("derived logger lost its field: %s", buf.String())
	}
	a.Info("from-a")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 records, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"k":"b"`) || !strings.Contains(lines[1], `"k":"a"`) {
		t.Fatalf("fields crossed between loggers: %v", lines)
	}
}

func TestMetricsExposition(t *testing.T) {
	m := NewMetrics()
	m.Observe("p", 200, false, 10*time.Millisecond)
	m.Observe("p", 200, false, 20*time.Millisecond)
	m.Observe("p", 500, true, 30*time.Millisecond)
	m.InFlight.Store(2)
	m.UpstreamOK.Store(7)
	m.UpstreamErr.Store(1)

	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		`relay_requests_total{route="p",status="200",stream="false"} 2`,
		`relay_requests_total{route="p",status="500",stream="true"} 1`,
		`relay_request_duration_seconds_count{route="p",status="200",stream="false"} 2`,
		`relay_request_duration_seconds_sum{route="p",status="200",stream="false"} 0.03`,
		`relay_inflight_requests 2`,
		`relay_upstream_requests_total{result="ok"} 7`,
		`relay_upstream_requests_total{result="error"} 1`,
		`relay_uptime_seconds`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// Label order must be stable so a scraper diffing consecutive samples only
// sees numbers change. Uptime is a gauge and legitimately differs between
// calls, so compare only the series lines excluding it.
func TestMetricsOutputIsSorted(t *testing.T) {
	m := NewMetrics()
	m.Observe("z", 200, false, time.Millisecond)
	m.Observe("a", 200, false, time.Millisecond)

	series := func() string {
		var buf bytes.Buffer
		m.WriteTo(&buf)
		var out []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.HasPrefix(l, "relay_") && !strings.HasPrefix(l, "relay_uptime") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	first, second := series(), series()
	if first != second {
		t.Fatalf("series lines are not stable:\n%s\n---\n%s", first, second)
	}
	if strings.Index(first, `route="a"`) > strings.Index(first, `route="z"`) {
		t.Fatal("routes are not sorted")
	}
}
