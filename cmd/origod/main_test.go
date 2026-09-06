// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/config"
)

func testEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"ORIGO_S3_ENDPOINT":   "http://127.0.0.1:1",
		"ORIGO_S3_REGION":     "us-east-1",
		"ORIGO_S3_BUCKET":     "origo",
		"ORIGO_S3_KEY":        "k",
		"ORIGO_S3_SECRET":     "s",
		"ORIGO_PUBLIC_URL":    "http://127.0.0.1",
		"ORIGO_DEV_TOKEN":     "dev",
		"ORIGO_DATA_DIR":      t.TempDir(),
		"ORIGO_PUBLIC_ADDR":   "127.0.0.1:0",
		"ORIGO_INTERNAL_ADDR": "127.0.0.1:0",
		"ORIGO_GOSSIP_ADDR":   "127.0.0.1:0",
	}
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
	if got := errOut.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "missing ORIGO_S3_BUCKET") || !strings.Contains(got, "missing ORIGO_DEV_TOKEN") {
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
	env := testEnv(t)
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
	req.Header.Set("Authorization", "Bearer dev")
	resp, err = app.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		// The fake bucket answers every request with an empty listing,
		// so the create's read-back fails: the route is reached.
		t.Fatalf("with token: %d", resp.StatusCode)
	}
	req, _ = http.NewRequestWithContext(context.Background(), "GET", "http://"+public+"/nope", nil)
	req.SetBasicAuth("x", "dev")
	resp, err = app.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown route: %d", resp.StatusCode)
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
