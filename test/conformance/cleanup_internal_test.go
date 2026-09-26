// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCleanupWaitsOutADenyStillCached: a delete refused with 403 is
// asked again after the deny-cache pause until it is accepted, a 429
// waits its Retry-After, and a refusal that outlasts the budget is
// reported with the status and the time spent. This is what let the
// suite's cleanup fail in CI after 019/forbidden: the case's deny was
// still in the node's cache when the run's tail was fast.
func TestCleanupWaitsOutADenyStillCached(t *testing.T) {
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }
	answers := func(statuses ...int) func() response {
		return func() response {
			st := statuses[0]
			if len(statuses) > 1 {
				statuses = statuses[1:]
			}
			h := http.Header{}
			if st == http.StatusTooManyRequests {
				h.Set("Retry-After", "3")
			}
			return response{status: st, header: h, body: []byte("refused")}
		}
	}

	gone := answers(http.StatusNotFound)
	slept = nil
	if err := deleteUntilGone(answers(http.StatusForbidden, http.StatusForbidden, http.StatusAccepted), gone, sleep); err != nil {
		t.Fatalf("a deny that cleared: %v", err)
	}
	if !slices.Equal(slept, []time.Duration{denyCacheWait, denyCacheWait}) {
		t.Fatalf("slept %v after two 403s", slept)
	}

	slept = nil
	if err := deleteUntilGone(answers(http.StatusTooManyRequests, http.StatusNotFound), gone, sleep); err != nil {
		t.Fatalf("a rate limit that cleared: %v", err)
	}
	if !slices.Equal(slept, []time.Duration{3 * time.Second}) {
		t.Fatalf("slept %v after a 429 with Retry-After 3", slept)
	}

	slept = nil
	err := deleteUntilGone(answers(http.StatusForbidden), gone, sleep)
	if err == nil || err.Error() != "still 403 after 30s: refused" {
		t.Fatalf("a deny that never clears: %v", err)
	}
	if total := time.Duration(len(slept)) * denyCacheWait; total != cleanupBudget {
		t.Fatalf("slept %s in all, want the %s budget", total, cleanupBudget)
	}

	if err := deleteUntilGone(answers(http.StatusInternalServerError), gone, sleep); err == nil || err.Error() != "500 refused" {
		t.Fatalf("a refusal the cleanup does not wait for: %v", err)
	}
}

// TestCleanupReadsEachRepositoryBackUntilGone: an accepted delete is
// followed by reads until the repository answers 404, waiting out a 403
// the same way, because the decision cache holds a deny per action. A
// case that denied every action of its repository and read it leaves a
// repo.read deny cached, which the delete, asking repo.delete, never
// meets; the run's promise is that every id reads 404 after it, and
// without the reads a caller reading at once got 403 instead
// (TestRunCleansUp in CI, where the cases after 019/forbidden end inside
// the five seconds).
func TestCleanupReadsEachRepositoryBackUntilGone(t *testing.T) {
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }
	answers := func(statuses ...int) func() response {
		return func() response {
			st := statuses[0]
			if len(statuses) > 1 {
				statuses = statuses[1:]
			}
			return response{status: st, header: http.Header{}, body: []byte("answer")}
		}
	}
	if err := deleteUntilGone(answers(http.StatusAccepted), answers(http.StatusForbidden, http.StatusForbidden, http.StatusNotFound), sleep); err != nil {
		t.Fatalf("a read deny that cleared: %v", err)
	}
	if !slices.Equal(slept, []time.Duration{denyCacheWait, denyCacheWait}) {
		t.Fatalf("slept %v after two 403s on the read", slept)
	}
	if err := deleteUntilGone(answers(http.StatusAccepted), answers(http.StatusGone), sleep); err != nil {
		t.Fatalf("a purged repository: %v", err)
	}
	if err := deleteUntilGone(answers(http.StatusAccepted), answers(http.StatusOK), sleep); err == nil || err.Error() != "read 200 after the delete was accepted: answer" {
		t.Fatalf("a repository still served after its delete: %v", err)
	}
	slept = nil
	if err := deleteUntilGone(answers(http.StatusAccepted), answers(http.StatusForbidden), sleep); err == nil || err.Error() != "still 403 after 30s: answer" {
		t.Fatalf("a read deny that never clears: %v", err)
	}

	// The session's own cleanup against a node whose decision cache still
	// holds a read deny for the repository after the delete: once cleanup
	// returns, the next read answers 404, which is what TestRunCleansUp
	// reads.
	const id = "65f135f7-b33f-4eed-bb35-ffac2f13fbd7"
	var mu sync.Mutex
	deleted, cachedReads := false, 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/v1/repos/"+id {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.Method == http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusAccepted)
		case cachedReads > 0:
			cachedReads--
			w.WriteHeader(http.StatusForbidden)
		case deleted:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	defer func(was func(time.Duration)) { cleanupSleep = was }(cleanupSleep)
	cleanupSleep = func(time.Duration) {}
	s := &session{target: Target{URL: srv.URL, Token: "dev"}, client: srv.Client(), skipped: map[string]bool{}, created: []string{id}}
	if got := s.cleanup(t); !slices.Equal(got, []string{id}) {
		t.Fatalf("cleanup reported %v", got)
	}
	if r := s.call(t, http.MethodGet, "/v1/repos/"+id, ""); r.status != http.StatusNotFound {
		t.Fatalf("%s after the cleanup: %d %s", id, r.status, strings.TrimSpace(string(r.body)))
	}
}
