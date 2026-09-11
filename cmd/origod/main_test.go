// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/issuer"
)

// identity is the stub issuer and authorizer a node under test is
// configured with, and the key it signs with.
type identity struct {
	issuer *issuer.Server
	authz  *authorizer.Server
	key    *ecdsa.PrivateKey
}

// token mints a token the node accepts.
func (id *identity) token() string { return id.issuer.Mint(issuer.Claims{Sub: "dev"}) }

// newEnv is a complete configuration against the stubs, with an
// unreachable bucket unless a test replaces it.
func newEnv(t *testing.T) (map[string]string, *identity) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	id := &identity{issuer: issuer.New(t), authz: authorizer.New(t), key: key}
	return map[string]string{
		"ORIGO_S3_ENDPOINT":      "http://127.0.0.1:1",
		"ORIGO_S3_REGION":        "us-east-1",
		"ORIGO_S3_BUCKET":        "origo",
		"ORIGO_S3_KEY":           "k",
		"ORIGO_S3_SECRET":        "s",
		"ORIGO_PUBLIC_URL":       "http://127.0.0.1",
		"ORIGO_OIDC_ISSUERS":     id.issuer.URL(),
		"ORIGO_AUTHORIZER_URL":   id.authz.URL(),
		"ORIGO_AUTHORIZER_TOKEN": id.authz.Token(),
		"ORIGO_TOKEN_KEY":        string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})),
		"ORIGO_DATA_DIR":         t.TempDir(),
		"ORIGO_PUBLIC_ADDR":      "127.0.0.1:0",
		"ORIGO_INTERNAL_ADDR":    "127.0.0.1:0",
		"ORIGO_GOSSIP_ADDR":      "127.0.0.1:0",
	}, id
}

func testEnv(t *testing.T) map[string]string {
	t.Helper()
	env, _ := newEnv(t)
	return env
}

func getenv(m map[string]string) config.Getenv {
	return func(k string) string { return m[k] }
}

func TestVersionFlagPrintsIdentity(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"-version"}, getenv(nil), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "origod ") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"-nope"}, getenv(nil), &out, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestMissingConfigurationIsOneMessage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, getenv(nil), &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "missing ORIGO_S3_BUCKET") || !strings.Contains(got, "missing ORIGO_TOKEN_KEY") || !strings.Contains(got, "missing ORIGO_OIDC_ISSUERS") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestUnusableDataDirFailsStartup(t *testing.T) {
	env := testEnv(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env["ORIGO_DATA_DIR"] = filepath.Join(file, "x")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, getenv(env), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "ORIGO_DATA_DIR") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestListenerCollisionFailsStartup(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, key := range []string{"ORIGO_PUBLIC_ADDR", "ORIGO_INTERNAL_ADDR"} {
		env := testEnv(t)
		env[key] = ln.Addr().String()
		var out, errOut bytes.Buffer
		if code := run(context.Background(), nil, getenv(env), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "listener") {
			t.Fatalf("%s: exit %d, stderr %q", key, code, errOut.String())
		}
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	env := testEnv(t)
	env["ORIGO_GOSSIP_ADDR"] = udp.LocalAddr().String()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, getenv(env), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "gossip listener") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

// startNode runs a node in the background and returns it with a stop
// function that waits for run to return.
func startNode(t *testing.T, env map[string]string) (*node, func() error) {
	t.Helper()
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	n, err := newNode(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	n.drainDelay = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.run(ctx) }()
	n.addrs()
	stop := func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("run did not return")
			return nil
		}
	}
	return n, stop
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body
}

// probe reads a text probe: the status and the body as sent.
func probe(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestInternalListenerServesProbes(t *testing.T) {
	env, id := newEnv(t)
	env["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
	env["ORIGO_S3_PATH_STYLE"] = "1"
	n, stop := startNode(t, env)
	public, internal, gossip := n.addrs()
	base := "http://" + internal
	if code, body := probe(t, base+"/livez"); code != 200 || body != "ok\n" {
		t.Fatalf("livez: %d %q", code, body)
	}
	// The public listener serves the two probes the release smoke reads,
	// and nothing else yet.
	if code, body := probe(t, "http://"+public+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("public readyz: %d %q", code, body)
	}
	if code, body := get(t, "http://"+public+"/version"); code != 200 || body["version"] != "dev" {
		t.Fatalf("public version: %d %v", code, body)
	}
	if code, body := get(t, "http://"+public+"/.well-known/jwks.json"); code != 200 || body["keys"] == nil {
		t.Fatalf("public jwks: %d %v", code, body)
	}
	if code, _ := get(t, "http://"+public+"/livez"); code != 401 {
		t.Fatalf("public livez without a token: %d", code)
	}
	// The application surface refuses without the bearer and answers
	// with it, contract version stamped.
	app := &http.Client{Transport: &http.Transport{}}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://"+public+"/v1/repos", strings.NewReader(`{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"acme","slug":"app"}`))
	resp, err := app.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" || resp.Header.Get("Origo-Contract") != "1" {
		t.Fatalf("without token: %d %v", resp.StatusCode, resp.Header)
	}
	req, _ = http.NewRequestWithContext(context.Background(), "POST", "http://"+public+"/v1/repos", strings.NewReader(`{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"acme","slug":"app"}`))
	req.Header.Set("Authorization", "Bearer "+id.token())
	resp, err = app.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		// The fake bucket answers every request with an empty listing,
		// so the create's read-back fails: the route is reached, after
		// the authorizer allowed it.
		t.Fatalf("with token: %d", resp.StatusCode)
	}
	if reqs := id.authz.Requests(); len(reqs) != 1 || reqs[0].Subject != "dev" || reqs[0].Action != "admin" || reqs[0].Repo.Slug != "app" {
		t.Fatalf("authorizer: %+v", reqs)
	}
	req, _ = http.NewRequestWithContext(context.Background(), "GET", "http://"+public+"/nope", nil)
	req.SetBasicAuth("x", id.token())
	resp, err = app.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var envelope httpjson.ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	resp.Body.Close()
	if resp.StatusCode != 400 || envelope.Error.Code != contract.CodeInvalid || envelope.Error.Details["reason"] != "no such route" {
		t.Fatalf("unknown route: %d %+v", resp.StatusCode, envelope.Error)
	}
	if code, body := probe(t, base+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("readyz: %d %q", code, body)
	}
	if code, body := get(t, base+"/version"); code != 200 || body["version"] != "dev" {
		t.Fatalf("version: %d %v", code, body)
	}
	client := &http.Client{Transport: &http.Transport{}}
	resp, err = client.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The gossip port accepts a datagram; spec 005 gives it a meaning.
	conn, err := net.Dial("udp", gossip)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("seq 1"))
	conn.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestReadyzFailsWhenTheDiskIsNotWritable(t *testing.T) {
	env := testEnv(t)
	env["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
	env["ORIGO_S3_PATH_STYLE"] = "1"
	n, stop := startNode(t, env)
	defer func() { _ = stop() }()
	_, internal, _ := n.addrs()
	if err := os.Chmod(env["ORIGO_DATA_DIR"], 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(env["ORIGO_DATA_DIR"], 0o755) })
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	code, body := probe(t, "http://"+internal+"/readyz")
	if code != 503 || !strings.HasPrefix(body, "not ready: disk: ") {
		t.Fatalf("readyz: %d %q", code, body)
	}
}

func TestReadyzReportsDrainingDuringShutdown(t *testing.T) {
	n, stop := startNode(t, testEnv(t))
	n.draining.Store(true)
	_, internal, _ := n.addrs()
	if code, body := probe(t, "http://"+internal+"/readyz"); code != 503 || body != "not ready: draining\n" {
		t.Fatalf("readyz: %d %q", code, body)
	}
	_ = stop()
}

func TestBackgroundLoopsStopWithTheNode(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	n, err := newNode(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	n.drainDelay = 0
	stopped := make(chan struct{})
	n.background = append(n.background, func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}, func(context.Context) error {
		return os.ErrClosed
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.run(ctx) }()
	n.addrs()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("background loop still running")
	}
}

// fakeBucket answers the one request the readiness check and the
// sweeper make: a listing, as empty XML. It counts them.
func fakeBucket(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var lists atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			lists.Add(1)
		}
		_, _ = w.Write([]byte(`<ListBucketResult></ListBucketResult>`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &lists
}

func TestReadyzReportsStorage(t *testing.T) {
	env := testEnv(t)
	n, stop := startNode(t, env)
	_, internal, _ := n.addrs()
	code, body := probe(t, "http://"+internal+"/readyz")
	if code != 503 || !strings.HasPrefix(body, "not ready: storage: ") {
		t.Fatalf("unreachable storage: %d %q", code, body)
	}
	_ = stop()

	url, lists := fakeBucket(t)
	env["ORIGO_S3_ENDPOINT"] = url
	env["ORIGO_S3_PATH_STYLE"] = "1"
	env["ORIGO_SWEEP_INTERVAL"] = "10ms"
	n, stop = startNode(t, env)
	_, internal, _ = n.addrs()
	if code, body := probe(t, "http://"+internal+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("reachable storage: %d %q", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for lists.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if lists.Load() < 3 {
		t.Fatalf("the sweeper listed %d times", lists.Load())
	}
}

func TestFailpointExitsTheProcess(t *testing.T) {
	env := testEnv(t)
	env["ORIGO_FAILPOINT"] = "commit.before-index"
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	n, err := newNode(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	exited := 0
	n.exit = func(code int) { exited = code }
	if err := n.failpoint("other"); err != nil || exited != 0 {
		t.Fatalf("unrelated failpoint: %v, exit %d", err, exited)
	}
	if err := n.failpoint("commit.before-index"); err == nil || exited != 3 {
		t.Fatalf("configured failpoint: %v, exit %d", err, exited)
	}
	// Without an interval the sweeper only waits.
	cfg.SweepInterval = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.sweep(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep: %v", err)
	}
}

func TestBadStorageOptionsFailStartup(t *testing.T) {
	env := testEnv(t)
	env["ORIGO_S3_ENDPOINT"] = "minio:9000"
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, getenv(env), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "endpoint") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	env = testEnv(t)
	t.Setenv("PATH", t.TempDir())
	if code := run(context.Background(), nil, getenv(env), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "git") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

// syncBuffer is a bytes.Buffer safe to read while the node logs to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestEveryRouteRequiresAToken is the route sweep of spec 007: every
// route of the public listener runs the verifier, refusing a missing
// token, a token for another audience, and an expired one with the
// row's reason, while the three unauthenticated paths answer without
// one; and every response of the listener carries Origo-Contract.
func TestEveryRouteRequiresAToken(t *testing.T) {
	// The sweep runs in both states of ORIGO_ANONYMOUS_READ (spec 027),
	// rather than keeping a second list of routes beside this one.
	//
	// With the switch on, the routes of the anonymous set do reach the
	// authorizer, which denies here because no rule allows them; and the
	// assertions below are unchanged, because an anonymous refusal is
	// rendered as the same 401 with reason "missing" that a request with
	// no credential gets with the switch off. That identity is the
	// existence-hiding rule, and this is where it is asserted over every
	// route at once.
	for _, anonymous := range []bool{false, true} {
		name := "anonymous read off"
		if anonymous {
			name = "anonymous read on"
		}
		t.Run(name, func(t *testing.T) { everyRouteRequiresAToken(t, anonymous) })
	}
}

func everyRouteRequiresAToken(t *testing.T, anonymous bool) {
	env, id := newEnv(t)
	env["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
	env["ORIGO_S3_PATH_STYLE"] = "1"
	if anonymous {
		env["ORIGO_ANONYMOUS_READ"] = "1"
		// The authorizer denies every request, which is what auth
		// answers an empty subject for a repository nobody marked
		// public. The sweep then asserts that the deny reaches the
		// client as the same 401 a request with no credential gets
		// with the switch off.
		id.authz.Deny(authorizer.Rule{}, "anonymous_subject")
	}
	n, stop := startNode(t, env)
	defer func() { _ = stop() }()
	public, _, _ := n.addrs()
	const repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	// nameForm holds the two owner/slug routes of the anonymous set. The
	// name form resolves the name against the bucket before it asks the
	// authorizer, because an authorizer keys on the id (spec 007), and
	// this sweep runs on a fake bucket that answers a storage error to
	// every resolve. So with anonymous read on they never reach the
	// decision, and the name form is asserted against a working stack by
	// internal/auth's route-set tests and internal/httpgit instead.
	nameForm := map[string]bool{
		"/acme/app.git/info/refs?service=git-upload-pack": true,
		"/acme/app.git/git-upload-pack":                   true,
	}
	routes := []struct{ method, path string }{
		{"GET", "/r/" + repoA + ".git/info/refs?service=git-upload-pack"},
		{"GET", "/r/" + repoA + ".git/info/refs?service=git-receive-pack"},
		{"POST", "/r/" + repoA + ".git/git-upload-pack"},
		{"POST", "/r/" + repoA + ".git/git-receive-pack"},
		{"GET", "/acme/app.git/info/refs?service=git-upload-pack"},
		{"POST", "/acme/app.git/git-upload-pack"},
		{"POST", "/acme/app.git/git-receive-pack"},
		{"POST", "/v1/repos"},
		// The collection route of spec 026, in both modes.
		{"GET", "/v1/repos"},
		{"GET", "/v1/repos?owner=acme&slug=app"},
		{"GET", "/v1/repos/" + repoA},
		{"PATCH", "/v1/repos/" + repoA},
		{"DELETE", "/v1/repos/" + repoA},
		{"POST", "/v1/repos/" + repoA + "/undelete"},
		{"POST", "/v1/repos/" + repoA + "/tokens"},
		// Repository administration (spec 019).
		{"POST", "/v1/repos/" + repoA + "/transfer"},
		{"POST", "/v1/repos/" + repoA + "/freeze"},
		{"POST", "/v1/repos/" + repoA + "/unfreeze"},
		{"POST", "/v1/repos/" + repoA + "/gc"},
		{"GET", "/v1/repos/" + repoA + "/stats"},
		{"GET", "/v1/repos/" + repoA + "/export.bundle"},
		{"POST", "/v1/repos/" + repoA + "/import"},
		{"GET", "/v1/repos/" + repoA + "/import"},
		// Migration (spec 014).
		{"POST", "/v1/repos/" + repoA + "/verify"},
		{"GET", "/v1/repos/" + repoA + "/refs"},
		{"GET", "/v1/repos/" + repoA + "/commits"},
		{"GET", "/v1/repos/" + repoA + "/commits/main"},
		{"GET", "/v1/repos/" + repoA + "/compare/main...main"},
		{"GET", "/v1/repos/" + repoA + "/tree/main"},
		{"GET", "/v1/repos/" + repoA + "/blob/main"},
		{"GET", "/v1/repos/" + repoA + "/archive/main.tar.gz"},
		// Server-side git operations (spec 020).
		{"POST", "/v1/repos/" + repoA + "/commits"},
		{"POST", "/v1/repos/" + repoA + "/merge"},
		{"POST", "/v1/repos/" + repoA + "/cherry-pick"},
		{"POST", "/v1/repos/" + repoA + "/revert"},
		// LFS (spec 010). The verifier runs in front of the handler, so
		// a request without a token answers spec 003's envelope here
		// rather than the LFS body; the spec's Outcome records it.
		{"POST", "/r/" + repoA + ".git/info/lfs/objects/batch"},
		{"POST", "/r/" + repoA + ".git/info/lfs/verify"},
		{"POST", "/r/" + repoA + ".git/info/lfs/locks"},
		{"POST", "/r/" + repoA + ".git/info/lfs/locks/verify"},
		{"POST", "/acme/app.git/info/lfs/objects/batch"},
		{"POST", "/acme/app.git/info/lfs/verify"},
		{"POST", "/acme/app.git/info/lfs/locks/verify"},
		{"GET", "/nope"},
		{"GET", "/livez"},
		{"GET", "/metrics"},
		// The landing page and its favicon (spec 022) are registered on
		// GET alone, so every other method of those two paths falls to
		// the application surface and demands a token like the rest.
		{"POST", "/"},
		{"POST", "/favicon.ico"},
	}
	now := time.Now()
	tokens := []struct{ reason, token string }{
		{auth.ReasonMissing, ""},
		{auth.ReasonAudience, id.issuer.Mint(issuer.Claims{Aud: issuer.StringList{"other"}})},
		{auth.ReasonExpired, id.issuer.Mint(issuer.Claims{Exp: now.Add(-2 * time.Minute).Unix()})},
	}
	client := &http.Client{Transport: &http.Transport{}}
	for _, r := range routes {
		if anonymous && nameForm[r.path] {
			continue
		}
		for _, tok := range tokens {
			req, _ := http.NewRequestWithContext(context.Background(), r.method, "http://"+public+r.path, strings.NewReader("{}"))
			if tok.token != "" {
				req.Header.Set("Authorization", "Bearer "+tok.token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var env httpjson.ErrorEnvelope
			_ = json.NewDecoder(resp.Body).Decode(&env)
			resp.Body.Close()
			if resp.StatusCode != 401 || env.Error.Code != contract.CodeUnauthenticated || env.Error.Details["reason"] != tok.reason || resp.Header.Get("WWW-Authenticate") != `Basic realm="origo"` || resp.Header.Get(contract.Header) != contract.Version {
				t.Errorf("%s %s with %s: %d %+v %v", r.method, r.path, tok.reason, resp.StatusCode, env.Error, resp.Header)
			}
		}
	}
	// With the switch off nothing reaches the authorizer, which is the
	// property that says the refusal is made before any repository is
	// looked at. With it on the anonymous set does reach it and is
	// denied there, and the sweep above has already asserted that the
	// denial is the identical 401.
	switch reached := len(id.authz.Requests()); {
	case !anonymous && reached != 0:
		t.Fatalf("a refused request reached the authorizer %d times", reached)
	case anonymous && reached == 0:
		t.Fatal("with anonymous read on, no request reached the authorizer")
	}
	// The unauthenticated paths, each stamped as well. The list is the
	// deliberate one: the two probes and the key set of the contract,
	// and the landing page with its favicon (spec 022), which are
	// served without a token because a person who has not authenticated
	// is exactly who they are for. A route added anywhere else belongs
	// in the sweep above, not here.
	for _, p := range []struct {
		path   string
		status int
	}{
		{"/readyz", 200},
		{"/version", 200},
		{"/.well-known/jwks.json", 200},
		{"/", 200},
		{"/favicon.ico", 204},
	} {
		resp, err := client.Get("http://" + public + p.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != p.status || resp.Header.Get(contract.Header) != contract.Version || resp.Header.Get("WWW-Authenticate") != "" {
			t.Errorf("%s: %d %v", p.path, resp.StatusCode, resp.Header)
		}
	}
	// A token the node minted itself is accepted on the route its scope
	// allows and refused on the others by scope, not by the verifier.
	signer := auth.NewSigner(id.key, env["ORIGO_PUBLIC_URL"], nil)
	bound, _, err := signer.Mint(auth.Principal{Subject: "ci"}, repoA, auth.ScopeRead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://"+public+"/v1/repos/"+repoA+"/tokens", strings.NewReader(`{"scope":"read","ttl":60}`))
	req.Header.Set("Authorization", "Bearer "+bound)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var env2 httpjson.ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env2)
	resp.Body.Close()
	if resp.StatusCode != 403 || env2.Error.Details["reason"] != auth.ReasonScope {
		t.Fatalf("bound token on admin: %d %+v", resp.StatusCode, env2.Error)
	}
}

// TestTransportsBoundTheDial: a bucket or an issuer that drops packets
// (192.0.2.1 is TEST-NET-1, routed nowhere) fails within the dial
// timeout rather than the operating system's connect timeout.
func TestTransportsBoundTheDial(t *testing.T) {
	old := dialTimeout
	dialTimeout = 100 * time.Millisecond
	t.Cleanup(func() { dialTimeout = old })
	for name, transport := range map[string]*http.Transport{"storage": storageTransport(), "outbound": outboundTransport()} {
		client := &http.Client{Transport: transport}
		start := time.Now()
		req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://192.0.2.1:9/", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: a black-holed address answered", name)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("%s: the dial took %s", name, took)
		}
	}
	if dialTimeout = old; dialTimeout != 10*time.Second {
		t.Fatalf("default dial timeout %s", dialTimeout)
	}
}

func TestRunStopsOnSignalContext(t *testing.T) {
	env := testEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, nil, getenv(env), &out, &errOut) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "serving") {
		if time.Now().After(deadline) {
			t.Fatalf("node did not start: %s %s", out.String(), errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return")
	}
}

// TestEventsLoopRunsWithTheSink: with ORIGO_EVENTS_URL and the secret
// set the node runs the dispatcher, whose start-up reads its own
// journals from the bucket and whose sweep lists the events prefix on
// the repair interval; without the URL the dispatcher is off and lists
// nothing.
func TestEventsLoopRunsWithTheSink(t *testing.T) {
	env := testEnv(t)
	url, lists := fakeBucket(t)
	env["ORIGO_S3_ENDPOINT"] = url
	env["ORIGO_S3_PATH_STYLE"] = "1"
	env["ORIGO_SWEEP_INTERVAL"] = "0"
	env["ORIGO_EVENTS_URL"] = "http://127.0.0.1:1"
	env["ORIGO_EVENTS_SECRET"] = "k"
	env["ORIGO_REPAIR_INTERVAL"] = "20ms"
	env["ORIGO_NODE_NAME"] = "node-1"
	n, stop := startNode(t, env)
	if !n.events.Enabled() {
		t.Fatal("events off with the sink configured")
	}
	deadline := time.Now().Add(5 * time.Second)
	for lists.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if lists.Load() < 2 {
		t.Fatalf("the repair sweep listed %d times", lists.Load())
	}
	delete(env, "ORIGO_EVENTS_URL")
	delete(env, "ORIGO_EVENTS_SECRET")
	before := lists.Load()
	n, stop = startNode(t, env)
	if n.events.Enabled() {
		t.Fatal("events on without a sink")
	}
	time.Sleep(60 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if lists.Load() != before {
		t.Fatalf("the dispatcher swept with events off: %d listings", lists.Load()-before)
	}
}

// TestGossipWiresTwoNodes is spec 005's wiring in the node: two nodes
// with each other as peers under one secret hear each other's
// heartbeats, the live set of each names both, Origo-Prefer on the
// public surface names the preferred nodes, and a node with peers and
// no secret refuses to start in the one message.
func TestGossipWiresTwoNodes(t *testing.T) {
	envA, id := newEnv(t)
	envA["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
	envA["ORIGO_S3_PATH_STYLE"] = "1"
	envA["ORIGO_NODE_NAME"] = "origod-0"
	envA["ORIGO_GOSSIP_SECRET"] = strings.Repeat("s", 32)
	// B's port is reserved first so A's peer list can name it, and the
	// reservation is held until A has bound its own socket: released
	// earlier, the kernel could hand the same port to A, whose peer
	// list would then name itself and whose own heartbeat would count
	// as a packet received from B.
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrB := udp.LocalAddr().String()
	envA["ORIGO_GOSSIP_PEERS"] = addrB
	a, stopA := startNode(t, envA)
	defer func() { _ = stopA() }()
	publicA, internalA, gossipA := a.addrs()
	_ = udp.Close()

	envB, _ := newEnv(t)
	envB["ORIGO_S3_ENDPOINT"] = envA["ORIGO_S3_ENDPOINT"]
	envB["ORIGO_S3_PATH_STYLE"] = "1"
	envB["ORIGO_NODE_NAME"] = "origod-1"
	envB["ORIGO_GOSSIP_SECRET"] = envA["ORIGO_GOSSIP_SECRET"]
	envB["ORIGO_GOSSIP_ADDR"] = addrB
	envB["ORIGO_GOSSIP_PEERS"] = gossipA
	envB["ORIGO_OIDC_ISSUERS"] = envA["ORIGO_OIDC_ISSUERS"]
	envB["ORIGO_AUTHORIZER_URL"] = envA["ORIGO_AUTHORIZER_URL"]
	envB["ORIGO_AUTHORIZER_TOKEN"] = envA["ORIGO_AUTHORIZER_TOKEN"]
	b, stopB := startNode(t, envB)
	defer func() { _ = stopB() }()
	_, internalB, _ := b.addrs()

	// B's heartbeat at start reaches A; A heard B and B sent. Each
	// counter is waited for, because the send is counted after the
	// datagram left and a slow runner reads B between the two.
	for _, node := range []struct{ name, internal, direction string }{{"A", internalA, "received"}, {"B", internalB, "sent"}} {
		counted := regexp.MustCompile(`origo_gossip_packets_total\{direction="` + node.direction + `"\} [1-9]`)
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, metrics := probe(t, "http://"+node.internal+"/metrics")
			if counted.MatchString(metrics) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s nothing:\n%s", node.name, node.direction, metrics)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if live := a.set.Live(); strings.Join(live, ",") != "origod-0,origod-1" {
		t.Fatalf("A's live set %v", live)
	}
	// The header names the preferred node of a repository, on the 404
	// the fake bucket's empty listing produces as on any status.
	client := &http.Client{Transport: &http.Transport{}}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://"+publicA+"/r/0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+id.token())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 || resp.Header.Get("Origo-Prefer") != a.set.Prefer("0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f", 1)[0] {
		t.Fatalf("Origo-Prefer: %d %q", resp.StatusCode, resp.Header.Get("Origo-Prefer"))
	}
	// The eviction gauges are served at 0 with nothing cached.
	if _, metricsA := probe(t, "http://"+internalA+"/metrics"); !strings.Contains(metricsA, "origo_cache_repos 0\n") || !strings.Contains(metricsA, `origo_evictions_total{reason="pressure"} 0`) {
		t.Fatalf("eviction metrics:\n%s", metricsA)
	}
	// Peers without the secret is a configuration problem.
	envC := testEnv(t)
	envC["ORIGO_GOSSIP_PEERS"] = "origod-gossip"
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, getenv(envC), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "missing ORIGO_GOSSIP_SECRET") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}
