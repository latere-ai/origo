// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
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

// rateBudgetFactor bounds the rate_limited case: it sends at most
// rateBudgetFactor times the figure the header names, one request more.
//
// Draining a bucket of depth L that refills at L/60 tokens a second
// while the runner sends rho a second takes N requests, where the
// tokens left after N are L - N(1 - r) and r = (L/60)/rho is the share
// of each request the node gives back. A refusal needs N > L/(1 - r),
// so a bound of 2L holds for every runner that sends at least twice as
// fast as one node refills (r <= 1/2). One node is where the case must
// converge and 32 workers clear that condition by a wide margin: the
// kind stack runs at 6000 a minute, 100 tokens a second, and asks the
// runner for 200. Where the case cannot converge no bound helps: a
// balanced installation of k nodes gives one subject k buckets
// refilling at k*L/60 together, which drives r past 1 at the replica
// counts deploy/base/hpa.yaml scales to. 2L+1 is therefore the smallest
// bound carrying that condition, and it is what a live run spends
// before it reports the case unverified: 12001 requests at the service
// figure of 6000, where four times the figure would have spent 24001.
// Spec 021 records the measured margin on the stack.
const rateBudgetFactor = 2

// case012RateLimited reads RateLimit-Limit off a response under the
// token it will spend and sends past the figure the header names, under
// a subject of its own when the target can mint one, else under the
// run's token. The header is the figure in force for that subject
// (spec 012), so the case reads the second response and not the first:
// the bucket runs in front of the authorizer, so a subject the
// authorizer names a rate for reads the node's figure once before its
// own.
//
// A load balancer spreads a subject over several nodes, each with a
// bucket of its own, so the case goes on past the figure to the bound
// above. Reaching the bound with no refusal is not a failure of the
// installation, it is a shape the case cannot observe: it is recorded
// in Report.Unverified, which the stub run and the stack run require
// empty and the live run logs. A target with the limit off sends no
// header and the case is skipped.
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
	budget := rateBudgetFactor*limit + 1
	var refused atomic.Int64
	var first sync.Once
	var refusal response
	// The two responses read above cost a token each and count against
	// the bound, so what a run reports as spent is what it sent.
	sent := 2
	send := func(n int) time.Duration {
		start := time.Now()
		var wg sync.WaitGroup
		for w := range 32 {
			wg.Go(func() {
				for i := w; i < n; i += 32 {
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
		s.unverifiable(t, "a refusal past "+strconv.Itoa(limit)+" requests a minute",
			"no node refused within "+strconv.Itoa(sent)+" requests: the figure is one node's and this target spreads the subject or refills faster than the runner sends")
		return
	}
	d := expectError(t, refusal, http.StatusTooManyRequests, contract.CodeRateLimited)
	after, err := strconv.Atoi(refusal.header.Get("Retry-After"))
	failIf(t, err != nil || after < 1 || d["limit"] != "subject" || d["retry_after"] != float64(after), "Retry-After %q, details %v", refusal.header.Get("Retry-After"), d)
}
