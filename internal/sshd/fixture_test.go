// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"golang.org/x/crypto/ssh"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/httpgit"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sshkeys"
)

const (
	repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

// fixture is one node with its SSH listener: a memory store, the log,
// the cache, the git handler, a stub authorizer, and a stub key
// resolver, all of them reachable so a test drives one rule at a time.
type fixture struct {
	t         *testing.T
	store     wal.Store
	mem       *wal.MemStore
	breakers  *wal.BreakerStore
	clock     *fakeClock
	log       *wal.Log
	cache     *repo.Cache
	git       *httpgit.Handler
	server    *Server
	keys      *sshkeys.Server
	authz     *authorizer.Server
	set       *metrics.Set
	addr      string
	client    ssh.Signer
	clientKey crypto.PrivateKey
	hosts     *HostKeys
}

type fixtureConfig struct {
	hostKeys  []crypto.PrivateKey
	limits    *limits.Options
	handshake time.Duration
	now       func() time.Time
	store     wal.Store
	breakers  bool
	staleMax  time.Duration
}

type fixtureOption func(*fixtureConfig)

func withHostKeys(keys ...crypto.PrivateKey) fixtureOption {
	return func(c *fixtureConfig) { c.hostKeys = keys }
}

func withLimits(o limits.Options) fixtureOption {
	return func(c *fixtureConfig) { c.limits = &o }
}

func withHandshake(d time.Duration) fixtureOption {
	return func(c *fixtureConfig) { c.handshake = d }
}

func withNow(now func() time.Time) fixtureOption {
	return func(c *fixtureConfig) { c.now = now }
}

// withBreakers puts the breaker store of spec 015 in front of the memory
// store at threshold one, on the fixture's fake clock, so one failed
// call opens a class.
func withBreakers(staleMax time.Duration) fixtureOption {
	return func(c *fixtureConfig) { c.breakers, c.staleMax = true, staleMax }
}

// fakeClock moves only when advanced.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// generateKey is a P-256 key, the algorithm both a host key and a client
// key are allowed to be.
func generateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func generateEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// writeKey writes a private key in OpenSSH's own format, which is what
// both ssh-keygen writes and the OpenSSH client reads.
func writeKey(t *testing.T, dir, name string, key crypto.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newFixture(t *testing.T, options ...fixtureOption) *fixture {
	t.Helper()
	cfg := fixtureConfig{store: wal.NewMemStore()}
	for _, o := range options {
		o(&cfg)
	}
	if len(cfg.hostKeys) == 0 {
		cfg.hostKeys = []crypto.PrivateKey{generateEd25519(t)}
	}
	logger := slog.New(slog.DiscardHandler)
	set := metrics.Register(pkgmetrics.NewRegistry())
	clock := &fakeClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	store := cfg.store
	var mem *wal.MemStore
	var breakers *wal.BreakerStore
	if cfg.breakers {
		mem = wal.NewMemStore()
		breakers = wal.NewBreakerStore(wal.BreakerOptions{Store: mem, Clock: clock, Metrics: set, Threshold: 1})
		store = breakers
	}
	log := wal.New(wal.Options{Store: store, Logger: logger, Metrics: set})
	repoOpts := repo.Options{Dir: filepath.Join(t.TempDir(), "data"), Log: log, Logger: logger, Metrics: set}
	if cfg.breakers {
		repoOpts.Now, repoOpts.StaleMax = clock.Now, cfg.staleMax
	}
	cache, err := repo.New(repoOpts)
	if err != nil {
		t.Fatal(err)
	}
	authz := authorizer.New(t)
	client := &http.Client{Transport: &http.Transport{}}
	authorizerClient, err := auth.NewClient(auth.ClientOptions{URL: authz.URL(), Token: authz.Token(), HTTP: client, Metrics: set})
	if err != nil {
		t.Fatal(err)
	}
	guard := auth.NewGuard(authorizerClient, logger)

	var l *limits.Limits
	if cfg.limits != nil {
		cfg.limits.Metrics, cfg.limits.Logger, cfg.limits.Log = set, logger, log
		l = limits.New(*cfg.limits)
	} else {
		l = limits.New(limits.Options{Metrics: set, Logger: logger, Log: log})
	}
	git := httpgit.New(httpgit.Options{Cache: cache, Logger: logger, Metrics: set, Guard: guard, Limits: l, Timeout: time.Minute})

	keyDir := t.TempDir()
	var paths []string
	for i, k := range cfg.hostKeys {
		paths = append(paths, writeKey(t, keyDir, "host"+string(rune('a'+i)), k))
	}
	hosts, err := ParseHostKeys(paths)
	if err != nil {
		t.Fatal(err)
	}
	stub := sshkeys.New(t)
	resolver, err := NewResolver(ResolverOptions{URL: stub.URL(), Token: stub.Token(), HTTP: client, Metrics: set, Now: cfg.now})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{
		HostKeys: hosts, Keys: resolver, Guard: guard, Cache: cache, Git: git,
		Limits: l, Metrics: set, Logger: logger, Handshake: cfg.handshake,
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })

	clientKey := generateKey(t)
	signer, err := ssh.NewSignerFromKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, store: store, mem: mem, breakers: breakers, clock: clock, log: log, cache: cache, git: git, server: server,
		keys: stub, authz: authz, set: set, addr: ln.Addr().String(), client: signer, clientKey: clientKey, hosts: hosts,
	}
	f.register("u_7f3c")
	return f
}

// register puts the fixture's client key in the stub store under the
// subject.
func (f *fixture) register(subject string) {
	f.t.Helper()
	f.keys.Register(sshkeys.Key{Fingerprint: ssh.FingerprintSHA256(f.client.PublicKey()), Subject: subject, KeyID: "k_1"})
}

func (f *fixture) create(id, owner, slug string) {
	f.t.Helper()
	if _, err := f.log.CreateRepo(context.Background(), wal.Meta{ID: id, Owner: owner, Slug: slug}, "main"); err != nil {
		f.t.Fatal(err)
	}
}

// dial opens an authenticated client connection to the fixture's
// listener with the signer given, or the fixture's own key.
func (f *fixture) dial(signers ...ssh.Signer) (*ssh.Client, error) {
	f.t.Helper()
	if len(signers) == 0 {
		signers = []ssh.Signer{f.client}
	}
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // the test asserts on the host key itself
		Timeout:         20 * time.Second,
	})
}

func (f *fixture) mustDial(signers ...ssh.Signer) *ssh.Client {
	f.t.Helper()
	c, err := f.dial(signers...)
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}

// sessions and auths read one series of the three spec 024 owns.
func (f *fixture) sessions(service, result string) uint64 {
	return f.set.SSHSessions.Value(map[string]string{"service": service, "result": result})
}

func (f *fixture) auths(result string) uint64 {
	return f.set.SSHAuth.Value(map[string]string{"result": result})
}

func (f *fixture) keyCalls(result string) uint64 {
	return f.set.SSHKeys.Count(map[string]string{"result": result})
}

// cutReads makes every read of the store fail: the bucket unreachable
// for reads, writes untouched (spec 015).
func (f *fixture) cutReads() {
	f.mem.SetFault(func(op, _ string) error {
		if op == "Head" || op == "Get" || op == "List" {
			return errors.New("unreachable")
		}
		return nil
	})
}

// openWrites fails one write through the breaker store, which opens the
// write breaker at threshold one, and keeps every later write failing.
func (f *fixture) openWrites() {
	f.t.Helper()
	f.mem.SetFault(func(op, _ string) error {
		if op == "Put" || op == "Create" || op == "Delete" {
			return errors.New("write refused")
		}
		return nil
	})
	if _, err := f.breakers.Put(context.Background(), "origo/probe", wal.BytesBody(nil)); err == nil {
		f.t.Fatal("the opening write answered")
	}
	if f.breakers.Admits(wal.ClassWrite) {
		f.t.Fatal("the write breaker is not open")
	}
}

// workingCopy clones the fixture's repository with the real git client
// and returns the directory.
func (f *fixture) workingCopy(t *testing.T, sshCommand, host, port string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	f.mustGit(t, t.TempDir(), sshCommand, "clone", "-q", "ssh://git@"+host+":"+port+"/acme/app.git", dir)
	return dir
}

// writeBlob writes an incompressible file of n bytes, so a push crosses
// a byte bound rather than being packed away.
func writeBlob(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), gittest.Bytes(n, 7), 0o644); err != nil {
		t.Fatal(err)
	}
}
