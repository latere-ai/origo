// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/httpgit"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/repo"
	versionpkg "github.com/latere-ai/origo/internal/version"
	"github.com/latere-ai/origo/internal/wal"
)

// Shutdown budgets. The drain delay lets a load balancer see the replica
// unready before the listener closes; the grace period is what in-flight
// requests get after that. A git push can be long, so the grace period is
// generous and the Deployment's termination grace period exceeds the sum.
const (
	defaultDrainDelay = 3 * time.Second
	gracePeriod       = 60 * time.Second
	readyCheckTimeout = 2 * time.Second

	// readHeaderTimeout bounds the wait for request headers. There is no
	// read or write timeout on the public listener: a pack transfer is as
	// long as the client's link makes it, and the per-request git timeout
	// in httpgit bounds the work.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

// readyCheck is one named readiness dependency.
type readyCheck struct {
	name string
	fn   func(context.Context) error
}

// node is the process: three listeners, the readiness checks, and the
// background loops, started together and stopped in order.
type node struct {
	cfg    *config.Config
	logger *slog.Logger
	reg    *metrics.Registry
	log    *wal.Log
	cache  *repo.Cache

	// exit ends the process at an injected failpoint. os.Exit outside
	// tests: the end-to-end suite kills a node between the entry write
	// and the index commit this way.
	exit func(int)

	public     http.Handler
	checks     []readyCheck
	background []func(context.Context) error

	drainDelay time.Duration
	draining   atomic.Bool

	mu           sync.Mutex
	publicAddr   string
	internalAddr string
	gossipAddr   string
	started      chan struct{}
}

// newNode assembles the node from the configuration. Every dependency the
// listeners serve is built here, so the start-up order reads in one place.
func newNode(cfg *config.Config, logger *slog.Logger) (*node, error) {
	n := &node{
		cfg:        cfg,
		logger:     logger,
		reg:        metrics.New(),
		exit:       os.Exit,
		drainDelay: defaultDrainDelay,
		started:    make(chan struct{}),
	}
	store, err := wal.NewS3(wal.S3Options{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		Key: cfg.S3Key, Secret: cfg.S3Secret, PathStyle: cfg.S3PathStyle,
		Client: &http.Client{Transport: storageTransport()},
	})
	if err != nil {
		return nil, err
	}
	n.log = wal.New(wal.Options{
		Store: store, Prefix: config.Prefix, Metrics: n.reg, Logger: logger,
		Failpoint: n.failpoint,
	})
	n.cache, err = repo.New(repo.Options{Dir: cfg.DataDir, Log: n.log, Logger: logger, Metrics: n.reg})
	if err != nil {
		return nil, err
	}
	n.checks = append(n.checks,
		readyCheck{name: "storage", fn: n.log.Ping},
		readyCheck{name: "disk", fn: n.diskWritable},
	)
	n.background = append(n.background, n.sweep)

	// The application surface: smart HTTP and the repository API behind
	// the phase 1 bearer, every response stamped with the contract
	// version.
	app := http.NewServeMux()
	httpgit.New(httpgit.Options{Cache: n.cache, Logger: logger, Metrics: n.reg}).Register(app)
	api.New(n.cache, logger).Register(app)
	app.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		contract.WriteError(w, http.StatusNotFound, contract.CodeRepoNotFound, "no such route")
	})
	guard := &auth.StaticBearer{Token: cfg.DevToken, Deny: func(w http.ResponseWriter, _ *http.Request, code, message string) {
		contract.WriteError(w, http.StatusUnauthorized, code, message)
	}}
	n.public = contract.Middleware(guard.Middleware(app))
	return n, nil
}

// storageTransport is the transport every request to object storage
// goes through: pooled connections with a header deadline, and no
// proxy from the environment on a path that carries credentials.
func storageTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
	}
}

// failpoint ends the process when the configured failpoint is reached.
// ORIGO_FAILPOINT is empty in every deployment.
func (n *node) failpoint(name string) error {
	if n.cfg.Failpoint != "" && name == n.cfg.Failpoint {
		n.logger.Error("failpoint reached, exiting", "failpoint", name)
		n.exit(3)
		return fmt.Errorf("failpoint %s", name)
	}
	return nil
}

// sweep runs the sweeper on every repository at the configured interval
// until ctx ends.
func (n *node) sweep(ctx context.Context) error {
	if n.cfg.SweepInterval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	t := time.NewTicker(n.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := n.log.SweepAll(ctx, n.cfg.SweepMinAge); err != nil && ctx.Err() == nil {
				n.logger.WarnContext(ctx, "sweep", "error", err)
			}
		}
	}
}

// diskWritable proves the data directory accepts a write. A read-only
// mount or a full disk takes the replica out of the endpoint list rather
// than failing every push.
func (n *node) diskWritable(context.Context) error {
	f, err := os.CreateTemp(n.cfg.DataDir, ".readyz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// internalHandler serves the probe and telemetry paths. They are on their
// own listener so nothing in front of the public surface can shadow them.
func (n *node) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", n.handleReady)
	mux.HandleFunc("GET /version", handleVersion)
	mux.Handle("GET /metrics", n.reg.Handler())
	return mux
}

// publicHandler serves the application surface with /readyz and /version
// in front of it. The two probes are public as well as internal so the
// release smoke reaches them through the ingress; /livez and /metrics stay
// internal.
func (n *node) publicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", n.handleReady)
	mux.HandleFunc("GET /version", handleVersion)
	mux.Handle("/", n.public)
	return mux
}

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": versionpkg.Version, "commit": versionpkg.Commit, "date": versionpkg.Date,
	})
}

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// handleReady runs every check with a bounded context. During the drain
// window the answer is 503 without running them: the replica is leaving.
func (n *node) handleReady(w http.ResponseWriter, r *http.Request) {
	if n.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "draining"})
		return
	}
	status := http.StatusOK
	results := make([]checkResult, 0, len(n.checks))
	for _, c := range n.checks {
		ctx, cancel := context.WithTimeout(r.Context(), readyCheckTimeout)
		err := c.fn(ctx)
		cancel()
		res := checkResult{Name: c.name, Status: "ok"}
		if err != nil {
			res.Status, res.Error = "fail", err.Error()
			status = http.StatusServiceUnavailable
		}
		results = append(results, res)
	}
	body := map[string]any{"status": "ok", "checks": results}
	if status != http.StatusOK {
		body["status"] = "fail"
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// run binds the listeners, serves until ctx ends or a listener fails, then
// drains: unready first, the drain delay, the HTTP servers with the grace
// period, the gossip socket, and the background loops last.
func (n *node) run(ctx context.Context) error {
	var lc net.ListenConfig
	publicLn, err := lc.Listen(ctx, "tcp", n.cfg.PublicAddr)
	if err != nil {
		return fmt.Errorf("public listener: %w", err)
	}
	internalLn, err := lc.Listen(ctx, "tcp", n.cfg.InternalAddr)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("internal listener: %w", err)
	}
	gossip, err := lc.ListenPacket(ctx, "udp", n.cfg.GossipAddr)
	if err != nil {
		_ = publicLn.Close()
		_ = internalLn.Close()
		return fmt.Errorf("gossip listener: %w", err)
	}
	n.mu.Lock()
	n.publicAddr, n.internalAddr, n.gossipAddr = publicLn.Addr().String(), internalLn.Addr().String(), gossip.LocalAddr().String()
	n.mu.Unlock()

	publicSrv := &http.Server{Handler: n.publicHandler(), ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout, ErrorLog: slog.NewLogLogger(n.logger.Handler(), slog.LevelWarn)}
	internalSrv := &http.Server{Handler: n.internalHandler(), ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: idleTimeout, ErrorLog: slog.NewLogLogger(n.logger.Handler(), slog.LevelWarn)}

	failed := make(chan error, 2)
	go func() { failed <- serveHTTP(publicSrv, publicLn, "public") }()
	go func() { failed <- serveHTTP(internalSrv, internalLn, "internal") }()

	bgCtx, cancelBackground := context.WithCancel(context.WithoutCancel(ctx))
	var bg sync.WaitGroup
	bg.Go(func() { readGossip(gossip) })
	for _, loop := range n.background {
		bg.Go(func() {
			if err := loop(bgCtx); err != nil && !errors.Is(err, context.Canceled) {
				n.logger.ErrorContext(bgCtx, "background loop stopped", "error", err)
			}
		})
	}

	n.logger.InfoContext(ctx, "serving", "public", n.publicAddr, "internal", n.internalAddr, "gossip", n.gossipAddr, "node", n.cfg.NodeName, "data_dir", n.cfg.DataDir, "version", versionpkg.String())
	close(n.started)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-failed:
	}

	// Shutdown outlives the cancelled run context on purpose.
	shutdownCtx := context.WithoutCancel(ctx)
	n.draining.Store(true)
	n.logger.InfoContext(shutdownCtx, "draining", "delay", n.drainDelay)
	if runErr == nil {
		sleepContext(shutdownCtx, n.drainDelay)
	}
	graceCtx, cancelGrace := context.WithTimeout(shutdownCtx, gracePeriod)
	defer cancelGrace()
	errs := []error{runErr}
	errs = append(errs, publicSrv.Shutdown(graceCtx), internalSrv.Shutdown(graceCtx))
	errs = append(errs, gossip.Close())
	cancelBackground()
	bg.Wait()
	n.logger.InfoContext(shutdownCtx, "stopped")
	return errors.Join(errs...)
}

func serveHTTP(srv *http.Server, ln net.Listener, name string) error {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("%s listener: %w", name, err)
}

// readGossip drains the gossip socket until it closes. Spec 005 gives the
// datagrams a meaning; until then a peer that announces a sequence is
// simply not listened to, and the port answers so a manifest can open it.
func readGossip(conn net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := conn.ReadFrom(buf); err != nil {
			return
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// addrs reports the bound listener addresses once run has opened them.
func (n *node) addrs() (public, internal, gossip string) {
	<-n.started
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.publicAddr, n.internalAddr, n.gossipAddr
}
