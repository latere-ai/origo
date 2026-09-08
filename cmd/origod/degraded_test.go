// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	for i := range 6 {
		if code, body := probe(t, "http://"+internal+"/readyz"); code != 503 || !strings.HasPrefix(body, "not ready: storage: ") {
			t.Fatalf("probe %d of a replica the bucket never answered: %d %q", i+1, code, body)
		}
	}
	if _, text := probe(t, "http://"+internal+"/metrics"); !strings.Contains(text, `origo_storage_breaker_state{class="read"} 1`) {
		t.Fatal("the breaker did not open")
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
	for i := range 5 {
		if code, body := probe(t, "http://"+internal+"/readyz"); code != 503 || !strings.HasPrefix(body, "not ready: storage: ") {
			t.Fatalf("probe %d with the breaker closed: %d %q", i+1, code, body)
		}
	}
	if code, body := probe(t, "http://"+internal+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("probe with the breaker open: %d %q", code, body)
	}
	_, text := probe(t, "http://"+internal+"/metrics")
	for _, want := range []string{
		`origo_storage_breaker_state{class="read"} 1`,
		`origo_storage_breaker_state{class="write"} 0`,
		`origo_storage_ops_total{op="list",result="error"} 6`,
		`origo_storage_ops_total{op="list",result="ok"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// TestStorageTimeoutBoundsTheReadinessListing: a bucket that accepts
// the connection and never answers fails the listing at
// ORIGO_STORAGE_TIMEOUT, well inside the probe's own budget.
func TestStorageTimeoutBoundsTheReadinessListing(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	t.Cleanup(func() { close(hang); srv.Close() })
	env := testEnv(t)
	env["ORIGO_S3_ENDPOINT"] = srv.URL
	env["ORIGO_S3_PATH_STYLE"] = "1"
	env["ORIGO_STORAGE_TIMEOUT"] = "100ms"
	n, stop := startNode(t, env)
	defer func() { _ = stop() }()
	_, internal, _ := n.addrs()
	started := time.Now()
	code, body := probe(t, "http://"+internal+"/readyz")
	if took := time.Since(started); code != 503 || !strings.Contains(body, "storage") || took > 1500*time.Millisecond {
		t.Fatalf("hanging bucket: %d %q after %s", code, body, took)
	}
}
