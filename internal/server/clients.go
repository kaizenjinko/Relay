package server

import (
	"crypto/tls"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// clientPool hands out one fasthttp.HostClient per upstream host, built once.
//
// A HostClient must not be copied or mutated after first use: it owns the
// idle connection set. Building it per request (the original behaviour) threw
// that set away, forcing a new TCP and TLS handshake on every call. Reusing
// one client per host keeps connections alive and removes an allocation from
// the hot path.
type clientPool struct {
	mu      sync.RWMutex
	byHost  map[string]*fasthttp.HostClient
	maxConn int
}

func newClientPool(maxConns int) *clientPool {
	return &clientPool{byHost: map[string]*fasthttp.HostClient{}, maxConn: maxConns}
}

// For returns the client for hostPort, creating it on first use. isTLS decides
// whether the connection is wrapped in TLS; the key includes it so a plaintext
// and a TLS upstream on the same host:port never share a client (they cannot
// in practice, but the key stays honest).
func (p *clientPool) For(hostPort string, isTLS bool) *fasthttp.HostClient {
	key := hostPort
	if isTLS {
		key = "tls|" + hostPort
	}
	p.mu.RLock()
	c, ok := p.byHost[key]
	p.mu.RUnlock()
	if ok {
		return c
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.byHost[key]; ok {
		return c
	}
	c = p.new(hostPort, isTLS)
	p.byHost[key] = c
	return c
}

func (p *clientPool) new(hostPort string, isTLS bool) *fasthttp.HostClient {
	// Do not set MaxConnWaitTimeout: with any non-zero value fasthttp switches
	// to a different connection-acquisition path that rejects cross-protocol
	// redirects, which breaks upstreams (like opencode.ai) that answer the
	// POST with a redirect to their real host. Waiting is bounded by the
	// per-request DoTimeout instead.
	return &fasthttp.HostClient{
		Addr:                hostPort,
		IsTLS:               isTLS,
		MaxConns:            p.maxConn,
		MaxIdleConnDuration: 60 * time.Second,
		ReadBufferSize:      32 << 10,
		WriteBufferSize:     32 << 10,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

// Close releases idle connections for every host.
func (p *clientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, c := range p.byHost {
		c.CloseIdleConnections()
		delete(p.byHost, k)
	}
}

// Len reports how many distinct upstream hosts have a live client. Used by
// tests to prove connections are pooled rather than built per request.
func (p *clientPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.byHost)
}
