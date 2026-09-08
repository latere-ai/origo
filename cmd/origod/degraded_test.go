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
// against an unreachable bucket counts toward the read breaker, and
// once the breaker is open the replica reports ready, because it
// serves warm repositories stale and refuses writes with a retry
// message (spec 015); the breaker gauge on /metrics reads open.
func TestReadyzStaysReadyWhileTheBreakerIsOpen(t *testing.T) {
	env := testEnv(t)
	n, stop := startNode(t, env)
	defer func() { _ = stop() }()
	_, internal, _ := n.addrs()
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
