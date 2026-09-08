// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/health"
	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/wait"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/httpgit"
	"github.com/latere-ai/origo/internal/lfs"
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

// dialTimeout bounds one connection attempt of the two outbound
// transports. Without it a bucket or an issuer that drops packets holds
// a request for the operating system's connect timeout, minutes, times
// the client's retries; spec 015's ORIGO_STORAGE_TIMEOUT bounds the
// whole operation once it lands. A variable so a test shortens it.
var dialTimeout = 10 * time.Second

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

	verifier *auth.Verifier
	signer   *auth.Signer
	events   *events.Dispatcher

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
		reg:        metrics.NewRegistry(),
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

	// Identity (spec 007): the verifier over the configured issuers and
	// the node's own key, the authorizer client, and the signer of
	// repository-bound tokens. The verifier's loop fetches the issuers'
	// keys at start and keeps them fresh.
	authClient := &http.Client{Transport: outboundTransport()}
	n.verifier, err = auth.NewVerifier(auth.VerifierOptions{
		Issuers: cfg.OIDCIssuers, LocalIssuer: cfg.PublicURL.String(), LocalKey: &cfg.TokenKey.PublicKey,
		Client: authClient, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	authorizer, err := auth.NewClient(auth.ClientOptions{URL: cfg.AuthorizerURL, Token: cfg.AuthorizerToken, HTTP: authClient, Metrics: n.reg})
	if err != nil {
		return nil, err
	}
	guard := auth.NewGuard(authorizer, logger)
	n.signer = auth.NewSigner(cfg.TokenKey, cfg.PublicURL.String(), nil)
	n.background = append(n.background, n.verifier.Run)
	if err := n.newEvents(); err != nil {
		return nil, err
	}

	// The application surface: smart HTTP and the repository API behind
	// the verifier, every request authorized before its repository is
	// looked up.
	app := http.NewServeMux()
	httpgit.New(httpgit.Options{Cache: n.cache, Logger: logger, Metrics: n.reg, Guard: guard, Events: n.events}).Register(app)
	api.New(api.Options{Cache: n.cache, Logger: logger, Guard: guard, Signer: n.signer, Events: n.events}).Register(app)
	// LFS (spec 010): the batch answers presigned URLs signed against
	// the endpoint LFS clients reach, so object bytes never pass through
	// the node.
	presigner, err := lfs.NewPresigner(lfs.PresignerOptions{
		Endpoint: cfg.S3PublicEndpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		Key: cfg.S3Key, Secret: cfg.S3Secret, PathStyle: cfg.S3PathStyle,
	})
	if err != nil {
		return nil, err
	}
	lfs.New(lfs.Options{Log: n.log, Guard: guard, Presigner: presigner, Logger: logger}).Register(app)
	app.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, map[string]any{"reason": "no such route"})
	})
	n.public = n.verifier.Middleware(app)
	return n, nil
}

// outboundTransport is the transport the calls to the issuers and the
// authorizer go through: pooled connections with a header deadline, and
// no proxy from the environment on a path that carries a bearer.
func outboundTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
	}
}

// storageTransport is the transport every request to object storage
// goes through: pooled connections with a header deadline, and no
// proxy from the environment on a path that carries credentials.
func storageTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
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
	wait.Every(ctx, n.cfg.SweepInterval, func(ctx context.Context) {
		if err := n.log.SweepAll(ctx, n.cfg.SweepMinAge); err != nil && ctx.Err() == nil {
			n.logger.WarnContext(ctx, "sweep", "error", err)
		}
	})
	return ctx.Err()
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

// internalHandler serves the probe and telemetry paths, the four every
// Latere service carries (pkg/health). They are on their own listener so
// nothing in front of the public surface can shadow them.
func (n *node) internalHandler() http.Handler {
	return health.Handler(health.Options{
		Ready: n.ready, Timeout: readyCheckTimeout, Metrics: n.metricsHandler(),
		Version: versionpkg.Version, Commit: versionpkg.Commit, BuildTime: versionpkg.Date,
	})
}

// metricsHandler serves the registry in the Prometheus text format.
func (n *node) metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		n.reg.WritePrometheus(w)
	})
}

// publicHandler serves the application surface with the three
// unauthenticated paths in front of it: /readyz and /version, public as
// well as internal so the release smoke reaches them through the
// ingress, and the key set of spec 007. /livez and /metrics stay
// internal. Every response of the listener carries the contract
// version (spec 003).
func (n *node) publicHandler() http.Handler {
	probes := n.internalHandler()
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", probes)
	mux.Handle("GET /version", probes)
	mux.Handle("GET /.well-known/jwks.json", n.signer.JWKS())
	mux.Handle("/", n.public)
	return contract.Middleware(mux)
}

// ready runs every check. During the drain window the answer is not
// ready without running them: the replica is leaving.
func (n *node) ready(ctx context.Context) error {
	if n.draining.Load() {
		return errDraining
	}
	checks := make([]health.Check, 0, len(n.checks))
	for _, c := range n.checks {
		checks = append(checks, health.Check{Name: c.name, Run: c.fn})
	}
	return health.Checks(checks...)(ctx)
}

var errDraining = errors.New("draining")

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
		_ = wait.Sleep(shutdownCtx, n.drainDelay)
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

// addrs reports the bound listener addresses once run has opened them.
func (n *node) addrs() (public, internal, gossip string) {
	<-n.started
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.publicAddr, n.internalAddr, n.gossipAddr
}
