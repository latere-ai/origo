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

// case012RateLimited reads RateLimit-Limit off a response and sends
// one request more than the figure under a subject of its own when the
// target can mint one, else under the run's token; a load balancer
// spreads a subject over several nodes, each with a bucket of its own,
// so the case goes on past the figure, to four times it, until a node
// refuses. A target with the limit off sends no header and the case is
// skipped.
func case012RateLimited(t *testing.T, s *session) {
	f := s.fixture
	r := s.call(t, "GET", "/v1/repos/"+f.id, "")
	expectStatus(t, r, http.StatusOK)
	raw := r.header.Get(contract.HeaderRateLimit)
	if raw == "" {
		s.skip(t, "012/rate_limited", "the target sends no "+contract.HeaderRateLimit+", the limit is off")
	}
	limit, err := strconv.Atoi(raw)
	failIf(t, err != nil || limit <= 0, "%s %q", contract.HeaderRateLimit, raw)
	token := s.target.Token
	if s.target.Issuer != "" {
		token = s.mint(t, "conformance-rate-"+f.id[:8], "")
	}
	var refused atomic.Int64
	var first sync.Once
	var refusal response
	send := func(n int) {
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
	}
	send(limit + 1)
	if refused.Load() == 0 {
		send(3 * limit)
	}
	failIf(t, refused.Load() == 0, "no 429 after %d requests against a limit of %d", 4*limit+1, limit)
	d := expectError(t, refusal, http.StatusTooManyRequests, contract.CodeRateLimited)
	after, err := strconv.Atoi(refusal.header.Get("Retry-After"))
	failIf(t, err != nil || after < 1 || d["limit"] != "subject" || d["retry_after"] != float64(after), "Retry-After %q, details %v", refusal.header.Get("Retry-After"), d)
}
