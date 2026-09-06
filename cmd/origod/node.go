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
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/metrics"
	versionpkg "github.com/latere-ai/origo/internal/version"
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
		drainDelay: defaultDrainDelay,
		started:    make(chan struct{}),
	}
	n.checks = append(n.checks, readyCheck{name: "disk", fn: n.diskWritable})
	n.public = http.NotFoundHandler()
	return n, nil
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

// dataPath joins a relative path onto the data directory.
func (n *node) dataPath(rel string) string { return filepath.Join(n.cfg.DataDir, rel) }
