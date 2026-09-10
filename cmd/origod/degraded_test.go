// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/wal"
)

// TestReadyzStaysReadyWhileTheBreakerIsOpen: every readiness listing
// against an unreachable bucket counts toward the read breaker; a
// replica the bucket never answered stays unready with the breaker
// open, and one it did answer reports ready once the breaker is open,
// because it serves warm repositories stale and refuses writes with a
// retry message (spec 015); the breaker gauge on /metrics reads open.
func TestReadyzStaysReadyWhileTheBreakerIsOpen(t *testing.T) {
	// Never answered: unready before and after the breaker opens.
	env := testEnv(t)
	n, stop := startNode(t, env)
	_, internal, _ := n.addrs()
	awaitOpenBreaker(t, internal)
	if code, body := probe(t, "http://"+internal+"/readyz"); code != 503 || !strings.HasPrefix(body, "not ready: storage: ") {
		t.Fatalf("probe of a replica the bucket never answered, breaker open: %d %q", code, body)
	}
	_ = stop()

	// Answered once, then gone: unready for the five failing listings,
	// ready once the breaker is open.
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<ListBucketResult></ListBucketResult>`))
	}))
	env["ORIGO_S3_ENDPOINT"] = bucket.URL
	env["ORIGO_S3_PATH_STYLE"] = "1"
	n, stop = startNode(t, env)
	defer func() { _ = stop() }()
	_, internal, _ = n.addrs()
	if code, body := probe(t, "http://"+internal+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("with the bucket answering: %d %q", code, body)
	}
	bucket.Close()
	awaitOpenBreaker(t, internal)
	if code, body := probe(t, "http://"+internal+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("probe with the breaker open: %d %q", code, body)
	}
	_, text := probe(t, "http://"+internal+"/metrics")
	for _, want := range []string{
		`origo_storage_breaker_state{class="read"} 1`,
		`origo_storage_breaker_state{class="write"} 0`,
		`origo_storage_ops_total{op="list",result="ok"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// The threshold-th failure opened it, so at least that many listings
	// failed; how many probes it took to get there is the machine's.
	m := regexp.MustCompile(`origo_storage_ops_total\{op="list",result="error"\} (\d+)`).FindStringSubmatch(text)
	if failed, _ := strconv.Atoi(strings.Join(m[1:], "")); failed < wal.BreakerThreshold {
		t.Errorf("metrics count %d failed listings, the threshold is %d", failed, wal.BreakerThreshold)
	}
}

// awaitOpenBreaker probes /readyz until the read breaker gauge reads
// open, and fails a probe that is not a 503 with the storage prefix on
// the way there. Probes and listings are not one to one: a listing runs
// detached under the storage deadline and every probe that arrives
// while it runs shares it, so on a loaded machine six probes can be
// three listings and a count of probes is not a count of failures. The
// probe whose listing is the one that opens the breaker answers ready
// on a replica the bucket has answered, because readiness reads the
// breaker's state after the listing reports: the gauge read again is
// what tells that probe apart from one with the breaker still closed.
func awaitOpenBreaker(t *testing.T, internal string) {
	t.Helper()
	open := func() bool {
		_, text := probe(t, "http://"+internal+"/metrics")
		return strings.Contains(text, `origo_storage_breaker_state{class="read"} 1`)
	}
	deadline := time.Now().Add(30 * time.Second)
	for i := 1; ; i++ {
		if open() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the breaker did not open after %d probes", i-1)
		}
		code, body := probe(t, "http://"+internal+"/readyz")
		if code == 200 && open() {
			return
		}
		if code != 503 || !strings.HasPrefix(body, "not ready: storage: ") {
			t.Fatalf("probe %d with the breaker closed: %d %q", i, code, body)
		}
	}
}

// TestStorageTimeoutBoundsTheReadinessListing: a bucket that accepts
// the connection and never answers fails the listing at
// ORIGO_STORAGE_TIMEOUT, whatever the probe's own budget does: a
// deadline inside the budget answers the probe at the deadline, and
// one past it answers the probe from its budget while the listing runs
// on, shared by the probes that arrive meanwhile, and counts toward
// the breaker when it ends.
func TestStorageTimeoutBoundsTheReadinessListing(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	t.Cleanup(func() { close(hang); srv.Close() })
	env := testEnv(t)
	env["ORIGO_S3_ENDPOINT"] = srv.URL
	env["ORIGO_S3_PATH_STYLE"] = "1"
	env["ORIGO_STORAGE_TIMEOUT"] = "100ms"
	n, stop := startNode(t, env)
	_, internal, _ := n.addrs()
	started := time.Now()
	code, body := probe(t, "http://"+internal+"/readyz")
	if took := time.Since(started); code != 503 || !strings.Contains(body, "storage") || took > 1500*time.Millisecond {
		t.Fatalf("hanging bucket: %d %q after %s", code, body, took)
	}
	_ = stop()

	env["ORIGO_STORAGE_TIMEOUT"] = "3s"
	n, stop = startNode(t, env)
	defer func() { _ = stop() }()
	_, internal, _ = n.addrs()
	var wg sync.WaitGroup
	codes := make([]int, 3)
	started = time.Now()
	for i := range codes {
		wg.Go(func() { codes[i], _ = probe(t, "http://"+internal+"/readyz") })
	}
	wg.Wait()
	if took := time.Since(started); took > 2900*time.Millisecond || codes[0] != 503 || codes[1] != 503 || codes[2] != 503 {
		t.Fatalf("probes past their budget: %v after %s", codes, took)
	}
	// The one listing the three probes shared ends at its deadline and
	// counts once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, text := probe(t, "http://"+internal+"/metrics"); strings.Contains(text, `origo_storage_ops_total{op="list",result="error"} 1`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, text := probe(t, "http://"+internal+"/metrics")
	t.Fatalf("the shared listing did not count once:\n%s", text)
}

// storageClock moves only when advanced, so the breaker's open window
// passes in a test without waiting for it.
type storageClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *storageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *storageClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// readyProbe runs one readiness listing under a probe's budget.
func readyProbe(t *testing.T, n *node, budget time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return n.storageReady(ctx)
}

// TestReadinessFollowsTheReadBreakerAndNotTheListingsError: once the
// bucket has answered a replica, a tripped read breaker keeps it ready
// however the listing failed (spec 015). The three ways a listing fails
// under a tripped breaker are the refusal by the open breaker, the
// half-open probe's own failure, and the probe's budget ending while
// the shared listing still runs; keying readiness on wal.ErrStorageOpen
// answered only the first, so a replica serving stale left the endpoint
// list once every open window for as long as the outage lasted. A
// listing that fails while the breaker is closed still takes the
// replica out of rotation.
func TestReadinessFollowsTheReadBreakerAndNotTheListingsError(t *testing.T) {
	mem := wal.NewMemStore()
	clock := &storageClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	set := metrics.Register(pkgmetrics.NewRegistry())
	bs := wal.NewBreakerStore(wal.BreakerOptions{Store: mem, Clock: clock, Metrics: set, Threshold: 2, Timeout: time.Minute})
	n := &node{log: wal.New(wal.Options{Store: bs, Logger: slog.New(slog.DiscardHandler), Metrics: set})}

	// The bucket answers: the replica has something warm to serve.
	if err := readyProbe(t, n, time.Second); err != nil {
		t.Fatalf("with the bucket answering: %v", err)
	}
	// It goes away. The first failing listing runs with the breaker
	// closed, which takes the replica out of rotation, and opens it.
	mem.SetFault(func(op, _ string) error {
		if op == "List" {
			return errors.New("bucket unreachable")
		}
		return nil
	})
	if err := readyProbe(t, n, time.Second); err == nil {
		t.Fatal("a listing that failed with the breaker closed left the replica ready")
	}
	if bs.Tripped(wal.ClassRead) {
		t.Fatal("one failed listing of two opened the read breaker")
	}
	// The second failure opens it, and the replica is ready from that
	// listing on: it serves its warm repositories stale.
	if err := readyProbe(t, n, time.Second); err != nil {
		t.Fatalf("the listing that opened the breaker: %v", err)
	}
	if !bs.Tripped(wal.ClassRead) {
		t.Fatal("two failed listings did not open the read breaker")
	}
	// Refused by the open breaker: ready, which is what the rule always
	// answered.
	if err := readyProbe(t, n, time.Second); err != nil {
		t.Fatalf("refused by the open breaker: %v", err)
	}
	// The window passes and the listing runs as the half-open probe. It
	// fails, reopening the window; the replica stays ready.
	clock.Advance(wal.BreakerOpenFor)
	if err := readyProbe(t, n, time.Second); err != nil {
		t.Fatalf("the half-open probe failed: %v", err)
	}
	// The window passes again and the half-open probe is slow, so the
	// probe's own budget ends while the shared listing still runs.
	clock.Advance(wal.BreakerOpenFor)
	mem.SetLatency(2 * time.Second)
	defer mem.SetLatency(0)
	if err := readyProbe(t, n, 100*time.Millisecond); err != nil {
		t.Fatalf("the probe's budget ended while the half-open listing ran: %v", err)
	}
}
