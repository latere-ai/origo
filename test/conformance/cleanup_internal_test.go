// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"slices"
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

	slept = nil
	if err := deleteUntilGone(answers(http.StatusForbidden, http.StatusForbidden, http.StatusAccepted), sleep); err != nil {
		t.Fatalf("a deny that cleared: %v", err)
	}
	if !slices.Equal(slept, []time.Duration{denyCacheWait, denyCacheWait}) {
		t.Fatalf("slept %v after two 403s", slept)
	}

	slept = nil
	if err := deleteUntilGone(answers(http.StatusTooManyRequests, http.StatusNotFound), sleep); err != nil {
		t.Fatalf("a rate limit that cleared: %v", err)
	}
	if !slices.Equal(slept, []time.Duration{3 * time.Second}) {
		t.Fatalf("slept %v after a 429 with Retry-After 3", slept)
	}

	slept = nil
	err := deleteUntilGone(answers(http.StatusForbidden), sleep)
	if err == nil || err.Error() != "still 403 after 30s: refused" {
		t.Fatalf("a deny that never clears: %v", err)
	}
	if total := time.Duration(len(slept)) * denyCacheWait; total != cleanupBudget {
		t.Fatalf("slept %s in all, want the %s budget", total, cleanupBudget)
	}

	if err := deleteUntilGone(answers(http.StatusInternalServerError), sleep); err == nil || err.Error() != "500 refused" {
		t.Fatalf("a refusal the cleanup does not wait for: %v", err)
	}
}
