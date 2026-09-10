// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// The rows of spec 012: a push over the authorizer's quota_bytes
// refused in the sideband, and the rate limit met by reading
// RateLimit-Limit off a response and sending past it.

func cases012() []testCase {
	return []testCase{
		{name: "over_quota", group: GroupQuota, run: case012OverQuota},
		{name: "rate_limited", run: case012RateLimited},
	}
}

func case012OverQuota(t *testing.T, s *session) {
	id := s.create(t, "quota")
	s.setRules(t, authorizer.Rule{Repo: id, Allow: true, QuotaBytes: 1024})
	work := clone(t, s.repoURL(id))
	commitFile(t, work, "a.bin", gittest.Bytes(16<<10, 5), "first")
	out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
	failIf(t, err == nil, "the push over the quota landed:\n%s", redact(out))
	if want := "remote: " + contract.Line(contract.CodeOverQuota); !strings.Contains(out, want) {
		t.Fatalf("git's output lacks %q:\n%s", want, redact(out))
	}
	r := s.call(t, "GET", "/v1/repos/"+id, "")
	expectStatus(t, r, http.StatusOK)
	failIf(t, r.json["head"] != "" || r.json["size_bytes"] != float64(0), "the refused push left something: %s", r.body)
	// The LFS batch meets the same figure in the LFS shape.
	lfsError(t, s.lfsCall(t, id, "objects/batch", `{"operation":"upload","objects":[{"oid":"`+oidOf([]byte("x"))+`","size":4096}]}`), http.StatusRequestEntityTooLarge, contract.CodeOverQuota)
}

// The rate_limited case's two figures.
const (
	// rateBurst is the requests the case spends to watch
	// RateLimit-Remaining fall. It is twice the largest replica count
	// deploy/base/hpa.yaml scales to, so every bucket behind a balancer
	// is hit at least twice however wide the installation has scaled.
	// The burst is sent by rateWorkers at once, because a bucket refills
	// while the burst runs and only requests that arrive faster than
	// L/60 a second move the counter down.
	rateBurst   = 64
	rateWorkers = 32
	// rateBudgetFactor bounds the exhaustion half, which runs only where
	// the burst showed one bucket. Draining a bucket of depth L that
	// refills at L/60 tokens a second while the runner sends rho a
	// second leaves L - N(1 - r) tokens after N requests, r = (L/60)/rho,
	// so a refusal needs N > L/(1 - r) and 2L holds for every runner
	// that sends at least twice as fast as one node refills. The kind
	// stack runs at 6000 a minute, 100 tokens a second, and so asks the
	// runner for 200 a second, which rateWorkers against a NodePort
	// clear by a wide margin.
	rateBudgetFactor = 2
)

// case012RateLimited proves the per-subject limit of spec 012 against
// any target, balanced or not.
//
// It reads RateLimit-Limit and RateLimit-Remaining off responses under
// the token it will spend, and off the second response and not the
// first: the two figures are the effective subject's own, and the
// bucket runs in front of the authorizer, so a subject the authorizer
// names a rate for reads the node's figure once before its own.
//
// The limit is then proved by the counter rather than by exhaustion.
// Every response of a burst must carry both figures, the limit must not
// move under one subject, and what is left must be inside it: that is a
// real assertion on any installation and it costs a few dozen requests.
// The counter must also fall, which holds on one node and behind a
// balancer alike, where the subject has a bucket a node and the
// responses carry several interleaved descending sequences rather than
// one. How far it fell is how the case reads the shape of the target
// without a header naming the node: one bucket falls by the burst less
// what refilled, and k buckets cut the fall to about the burst over k.
//
// Exhaustion, the 429, its Retry-After and its details.limit are
// asserted where they converge, which is a target answering from one
// bucket. Where the burst showed several, no bound converges, because
// the balancer's total refill outruns the runner, and spending L
// requests to learn it buys nothing: the refusal is recorded in
// Report.Unverified, which the stub run and the stack run require empty
// and the live run logs. A target with the limit off sends no header
// and the case is skipped.
func case012RateLimited(t *testing.T, s *session) {
	f := s.fixture
	token := s.target.Token
	if s.target.Issuer != "" {
		token = s.mint(t, "conformance-rate-"+f.id[:8], "")
	}
	r := s.as(t, token, "GET", "/v1/repos/"+f.id, "")
	expectStatus(t, r, http.StatusOK)
	r = s.as(t, token, "GET", "/v1/repos/"+f.id, "")
	expectStatus(t, r, http.StatusOK)
	raw := r.header.Get(contract.HeaderRateLimit)
	if raw == "" {
		s.skip(t, "012/rate_limited", "the target sends no "+contract.HeaderRateLimit+", the limit is off")
	}
	limit, err := strconv.Atoi(raw)
	failIf(t, err != nil || limit <= 0, "%s %q", contract.HeaderRateLimit, raw)

	// The burst never spends more than half the figure, so a target with
	// a small limit meets the counter rather than the refusal here. The
	// workers only collect; every assertion is made on the test's own
	// goroutine, where a Fatalf ends the case.
	burst := min(rateBurst, max(4, limit/2))
	got := make([]response, burst)
	var wg sync.WaitGroup
	start := time.Now()
	for w := range rateWorkers {
		wg.Go(func() {
			for i := w; i < burst; i += rateWorkers {
				got[i] = s.as(t, token, "GET", "/v1/repos/"+f.id, "")
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	left := make([]int, burst)
	for i, resp := range got {
		expectStatus(t, resp, http.StatusOK)
		failIf(t, resp.header.Get(contract.HeaderRateLimit) != raw,
			"request %d: %s moved from %q to %q under one subject", i+1, contract.HeaderRateLimit, raw, resp.header.Get(contract.HeaderRateLimit))
		n, err := strconv.Atoi(resp.header.Get(contract.HeaderRateRemaining))
		failIf(t, err != nil || n < 0 || n >= limit,
			"request %d: %s %q against a limit of %d", i+1, contract.HeaderRateRemaining, resp.header.Get(contract.HeaderRateRemaining), limit)
		left[i] = n
	}

	// The shape of the target, read off the values alone because no
	// header names the node. One bucket answers a burst with one run of
	// consecutive figures, nearly one per request: it falls a token a
	// request and refill only repeats a figure, never skips one. Several
	// buckets break that in one of two ways, and the test takes both: at
	// different depths they leave a gap between their runs, and at the
	// same depth they leave one run but each figure is answered once a
	// bucket, so the run is a fraction of the burst.
	values := slices.Compact(slices.Sorted(slices.Values(left)))
	runs := 1
	for i := 1; i < len(values); i++ {
		if values[i] != values[i-1]+1 {
			runs++
		}
	}
	// A bucket refills while the burst runs, and what it gives back is
	// known: limit/60 a second over the burst's own wall clock. One
	// bucket therefore answers with about burst - refill figures, and
	// the test is against that rather than against the burst, so a slow
	// target is not mistaken for a wide one. The fifth is slack for the
	// clock the runner measures against the clock the node refills on.
	refill := float64(limit) * elapsed.Seconds() / 60
	expect := float64(burst) - refill
	one := runs == 1 && expect >= 2 && float64(len(values)) >= expect*0.8
	t.Logf("%s over %d requests in %s: %d down to %d, %d figures in %d runs, about %.0f refilled",
		contract.HeaderRateRemaining, burst, elapsed.Round(time.Millisecond), values[len(values)-1], values[0], len(values), runs, refill)
	if len(values) < 2 {
		// Every bucket gave back at least what the burst took from it.
		// The counter is in force and inside the figure, which the loop
		// above asserted on every response, but nothing here can watch
		// it fall.
		s.unverifiable(t, "a falling "+contract.HeaderRateRemaining+" and a 429 rate_limited refusal past "+raw+" requests a minute",
			"the target answered the burst from buckets that refill at least as fast as the runner spends")
		return
	}
	if !one {
		s.unverifiable(t, "a 429 rate_limited refusal past "+raw+" requests a minute",
			"the target answered "+strconv.Itoa(burst)+" requests with "+strconv.Itoa(len(values))+" figures in "+strconv.Itoa(runs)+" runs, so it holds more than one bucket for this subject and no bounded run exhausts one")
		return
	}

	budget := rateBudgetFactor * limit
	var refused atomic.Int64
	var first sync.Once
	var refusal response
	sent := 0
	send := func(n int) time.Duration {
		start := time.Now()
		var wg sync.WaitGroup
		for w := range rateWorkers {
			wg.Go(func() {
				for i := w; i < n; i += rateWorkers {
					if refused.Load() > 0 {
						return
					}
					resp := s.as(t, token, "GET", "/v1/repos/"+f.id, "")
					if resp.status == http.StatusTooManyRequests {
						first.Do(func() { refusal = resp })
						refused.Add(1)
					}
				}
			})
		}
		wg.Wait()
		sent += n
		return time.Since(start)
	}
	took := send(limit + 1)
	if refused.Load() == 0 {
		// The runner's own rate against one node's refill. A runner
		// slower than L/60 a second cannot drain one bucket whatever it
		// spends, so it stops here rather than spending the rest of the
		// bound to learn it.
		if rho := float64(limit+1) / took.Seconds(); rho > float64(limit)/60 {
			send(budget - sent)
		}
	}
	if refused.Load() == 0 {
		s.unverifiable(t, "a 429 rate_limited refusal past "+raw+" requests a minute",
			"one bucket answered the burst but nothing refused within "+strconv.Itoa(sent)+" requests, so this target refills faster than the runner sends")
		return
	}
	d := expectError(t, refusal, http.StatusTooManyRequests, contract.CodeRateLimited)
	after, err := strconv.Atoi(refusal.header.Get("Retry-After"))
	failIf(t, err != nil || after < 1 || d["limit"] != "subject" || d["retry_after"] != float64(after), "Retry-After %q, details %v", refusal.header.Get("Retry-After"), d)
	failIf(t, refusal.header.Get(contract.HeaderRateRemaining) != "0", "%s on the refusal is %q", contract.HeaderRateRemaining, refusal.header.Get(contract.HeaderRateRemaining))
}
