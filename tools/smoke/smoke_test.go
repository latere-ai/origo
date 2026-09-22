// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package smoke

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

// TestReleaseSmoke is spec 017's criterion for release.sh, run through
// release_test.sh beside it: a stub whose GET /readyz answers ok, whose
// GET /version serves v1.2.3, and whose root serves the landing page of
// spec 022 naming the same version, standard input closed, a pass on
// the matching tag and a failure naming the mismatch on another. The
// script's directory is resolved from this file, never from the
// working directory, and TMPDIR is the test's own so the script's
// scratch directory is removed with the test.
//
// A second stub answers the two probes the same way and serves a root
// that is not Origo's, which is what an installation sharing its
// hostname with the browsing interface looks like. It proves LANDING=0
// skips the landing check and LANDING unset still enforces it.
//
// A third stub serves the previous version for its first two answers to
// GET /version, which is what the ingress does for a moment after the
// rollout has finished: the smoke waits for the tag rather than failing
// on the first answer, and a version that never arrives still fails.
func TestReleaseSmoke(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1.2.3","commit":"abc1234","build_time":"2026-09-09T00:00:00Z"}` + "\n"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Origo v1.2.3\n"))
	})
	stub := httptest.NewServer(mux)
	defer stub.Close()

	shared := http.NewServeMux()
	shared.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	shared.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1.2.3","commit":"abc1234","build_time":"2026-09-09T00:00:00Z"}` + "\n"))
	})
	shared.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<title>the browsing interface</title>\n"))
	})
	sharedStub := httptest.NewServer(shared)
	defer sharedStub.Close()

	// The installation just after `kubectl rollout status` returns: the
	// new pods are ready, and the ingress still answers from the old one
	// for a moment. The first two answers carry the previous version.
	var versionCalls atomic.Int32
	lagging := http.NewServeMux()
	lagging.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	lagging.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		version := "v1.2.3"
		if versionCalls.Add(1) <= 2 {
			version = "v1.2.2"
		}
		_, _ = fmt.Fprintf(w, `{"version":%q,"commit":"abc1234","build_time":"2026-09-09T00:00:00Z"}`+"\n", version)
	})
	lagging.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Origo v1.2.3\n"))
	})
	laggingStub := httptest.NewServer(lagging)
	defer laggingStub.Close()

	cmd := exec.CommandContext(context.Background(), "/bin/sh", filepath.Join(filepath.Dir(file), "release_test.sh"))
	cmd.Env = append(os.Environ(), "STUB_URL="+stub.URL, "SHARED_STUB_URL="+sharedStub.URL, "LAGGING_STUB_URL="+laggingStub.URL, "TMPDIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
