package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/baongo/relay/internal/config"
	"github.com/baongo/relay/internal/obs"
	"github.com/baongo/relay/internal/script"
	"github.com/baongo/relay/internal/server"
)

// options holds the resolved command-line flags so the build step can be
// exercised by tests without parsing os.Args.
type options struct {
	configPath  string
	listen      string
	verbose     bool
	printRoutes bool
}

// build loads the config, compiles every script, and returns a ready server.
//
// Scripts are compiled up front on purpose: a syntax error must stop the
// process, not surface as a 500 on the first request to that route.
func build(o options) (*config.Config, *server.Server, error) {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	if o.listen != "" {
		cfg.Server.Listen = o.listen
	}
	if o.verbose {
		cfg.Server.Verbose = true
	}

	paths, err := cfg.ScriptPaths()
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}

	scripts := make(map[string]*script.Script, len(paths))
	for i, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("script %s: %w", p, err)
		}
		sc, err := script.Compile(cfg.Routes[i].Name, string(src))
		if err != nil {
			return nil, nil, fmt.Errorf("script %s: %w", p, err)
		}
		scripts[cfg.Routes[i].Script] = sc
	}

	srv, err := server.New(cfg, scripts)
	if err != nil {
		return nil, nil, fmt.Errorf("server: %w", err)
	}
	return cfg, srv, nil
}

// newHTTPServer applies the relay's transport settings to a handler.
func newHTTPServer(cfg *config.Config, handler fasthttp.RequestHandler) *fasthttp.Server {
	readTimeout, _ := time.ParseDuration(cfg.Server.ReadTimeout)
	return &fasthttp.Server{
		Handler:               handler,
		ReadBufferSize:        64 << 10,
		WriteBufferSize:       64 << 10,
		Concurrency:           cfg.Server.MaxConns,
		MaxRequestBodySize:    64 << 20,
		NoDefaultServerHeader: true,
		ReadTimeout:           readTimeout,
	}
}

// serve runs the listener until a termination signal, then drains.
//
// Shutdown order matters for streaming responses. The relay first stops
// accepting new work (so a load balancer can withdraw it), then asks fasthttp
// to close idle connections and wait for active handlers. A long SSE stream is
// allowed to finish; if it outlives shutdown_timeout the listener is closed
// hard, which is the only case where a client sees a truncated stream.
func serve(cfg *config.Config, srv *server.Server, logger *obs.Logger) error {
	drainTimeout, err := time.ParseDuration(cfg.Server.ShutdownTimeout)
	if err != nil {
		return fmt.Errorf("shutdown_timeout: %w", err)
	}
	s := newHTTPServer(cfg, srv.Handler())

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Serve(ln)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	logger.Info("relay listening", "listen", cfg.Server.Listen, "routes", len(cfg.Routes))

	select {
	case err := <-done:
		// Serve returned on its own: a listener error, not a shutdown.
		srv.Close()
		return err
	case got := <-sig:
		logger.Info("signal received, draining", "signal", got.String(), "timeout", drainTimeout.String())
	}

	// Refuse new requests immediately, keep the admin endpoints answering so a
	// health check observes the transition rather than a connection error.
	srv.PrepareShutdown()

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	shutdownErr := s.ShutdownWithContext(drainCtx)
	srv.Close()
	if shutdownErr != nil {
		logger.Warn("drain timed out, closing remaining connections", "err", shutdownErr.Error())
	}
	logger.Info("relay stopped")
	return nil
}

// healthcheck probes /healthz on the given address. It exists so a container
// image without a shell or curl can still define a HEALTHCHECK.
func healthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	if host == "" {
		host = "127.0.0.1"
	}
	c := fasthttp.HostClient{
		Addr: net.JoinHostPort(host, port),
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("http://" + c.Addr + "/healthz")
	if err := c.DoTimeout(req, resp, 2*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode())
		return 1
	}
	return 0
}

func main() {
	var (
		cfgPath   = flag.String("config", "relay.toml", "path to relay.toml")
		listen    = flag.String("listen", "", "override server.listen")
		verbose   = flag.Bool("verbose", false, "override server.verbose")
		printPath = flag.Bool("print-routes", false, "print resolved routes and exit")
		health    = flag.Bool("healthcheck", false, "probe /healthz on -listen and exit 0/1")
	)
	flag.Parse()

	if *health {
		os.Exit(healthcheck(*listen))
	}

	cfg, srv, err := build(options{
		configPath:  *cfgPath,
		listen:      *listen,
		verbose:     *verbose,
		printRoutes: *printPath,
	})
	if err != nil {
		log.Fatalf("%v", err)
	}

	if *printPath {
		fmt.Printf("listen %s\n", cfg.Server.Listen)
		for _, r := range cfg.Routes {
			fmt.Printf("  %-20s %-18s -> %-45s %s\n", r.Name, r.Prefix, r.Upstream, r.Script)
		}
		return
	}

	logger := obs.NewStdout(cfg.Server.LogFormat == "json")
	if err := serve(cfg, srv, logger); err != nil {
		logger.Error("relay failed", "err", err.Error())
		os.Exit(1)
	}
}
