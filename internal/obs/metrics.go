package obs

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is a small Prometheus-format registry. It implements exactly the
// subset needed here: counters and gauges with labels. No histogram buckets
// yet — the latency summary reports sum and count, which is enough for rate
// and average, the two numbers actually used when paging.
type Metrics struct {
	requests map[reqKey]*reqStat
	mu       sync.Mutex

	// Gauges are updated by the server as connections and pools change.
	InFlight    atomic.Int64
	OpenConns   atomic.Int64
	UpstreamOK  atomic.Int64
	UpstreamErr atomic.Int64

	start time.Time
}

type reqKey struct {
	Route  string
	Status int
	Stream bool
}

type reqStat struct {
	Count    atomic.Int64
	Duration atomic.Int64 // nanoseconds, summed
}

// NewMetrics returns an empty registry.
func NewMetrics() *Metrics {
	return &Metrics{requests: make(map[reqKey]*reqStat), start: time.Now()}
}

// Observe records one finished request.
func (m *Metrics) Observe(route string, status int, stream bool, d time.Duration) {
	k := reqKey{Route: route, Status: status, Stream: stream}
	m.mu.Lock()
	st := m.requests[k]
	if st == nil {
		st = &reqStat{}
		m.requests[k] = st
	}
	m.mu.Unlock()
	st.Count.Add(1)
	st.Duration.Add(int64(d))
}

// WriteTo renders the registry in Prometheus text exposition format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	m.mu.Lock()
	keys := make([]reqKey, 0, len(m.requests))
	stats := make(map[reqKey]*reqStat, len(m.requests))
	for k, v := range m.requests {
		keys = append(keys, k)
		stats[k] = v
	}
	m.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Route != keys[j].Route {
			return keys[i].Route < keys[j].Route
		}
		if keys[i].Status != keys[j].Status {
			return keys[i].Status < keys[j].Status
		}
		return !keys[i].Stream && keys[j].Stream
	})

	var n int64
	write := func(s string) {
		c, _ := io.WriteString(w, s)
		n += int64(c)
	}

	write("# HELP relay_requests_total Requests served, by route and status.\n")
	write("# TYPE relay_requests_total counter\n")
	for _, k := range keys {
		write(fmt.Sprintf("relay_requests_total{route=%q,status=\"%d\",stream=\"%t\"} %d\n",
			k.Route, k.Status, k.Stream, stats[k].Count.Load()))
	}

	write("# HELP relay_request_duration_seconds_sum Total time spent serving requests.\n")
	write("# TYPE relay_request_duration_seconds_sum counter\n")
	for _, k := range keys {
		write(fmt.Sprintf("relay_request_duration_seconds_sum{route=%q,status=\"%d\",stream=\"%t\"} %s\n",
			k.Route, k.Status, k.Stream, formatFloat(float64(stats[k].Duration.Load())/1e9)))
	}

	write("# HELP relay_request_duration_seconds_count Number of timed requests.\n")
	write("# TYPE relay_request_duration_seconds_count counter\n")
	for _, k := range keys {
		write(fmt.Sprintf("relay_request_duration_seconds_count{route=%q,status=\"%d\",stream=\"%t\"} %d\n",
			k.Route, k.Status, k.Stream, stats[k].Count.Load()))
	}

	write("# HELP relay_inflight_requests Requests currently being served.\n")
	write("# TYPE relay_inflight_requests gauge\n")
	write(fmt.Sprintf("relay_inflight_requests %d\n", m.InFlight.Load()))

	write("# HELP relay_upstream_requests_total Upstream attempts, by result.\n")
	write("# TYPE relay_upstream_requests_total counter\n")
	write(fmt.Sprintf("relay_upstream_requests_total{result=\"ok\"} %d\n", m.UpstreamOK.Load()))
	write(fmt.Sprintf("relay_upstream_requests_total{result=\"error\"} %d\n", m.UpstreamErr.Load()))

	write("# HELP relay_uptime_seconds Seconds since the process started serving.\n")
	write("# TYPE relay_uptime_seconds gauge\n")
	write(fmt.Sprintf("relay_uptime_seconds %s\n", formatFloat(time.Since(m.start).Seconds())))

	return n, nil
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
