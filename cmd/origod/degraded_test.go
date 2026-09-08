// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
// three listings and a count of probes is not a count of failures.
func awaitOpenBreaker(t *testing.T, internal string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 1; ; i++ {
		if _, text := probe(t, "http://"+internal+"/metrics"); strings.Contains(text, `origo_storage_breaker_state{class="read"} 1`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the breaker did not open after %d probes", i-1)
		}
		if code, body := probe(t, "http://"+internal+"/readyz"); code != 503 || !strings.HasPrefix(body, "not ready: storage: ") {
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
