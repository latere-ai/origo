// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package limits

import (
	"net/http"
	"time"

	"latere.ai/x/pkg/ratelimit"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
)

// Buckets is the shared subject quota table; Origo owns identity and HTTP policy.
type Buckets = ratelimit.Buckets

// Allowance is the admission snapshot used to render rate-limit headers.
type Allowance = ratelimit.Allowance

// NewBuckets configures the shared limiter with Origo's minute-based limits.
func NewBuckets(perMinute, burst int, idle time.Duration, now func() time.Time) *Buckets {
	return ratelimit.New(ratelimit.Config{PerMinute: perMinute, Burst: burst, Idle: idle, Now: now})
}

// Middleware refuses a subject over its rate with 429 rate_limited,
// Retry-After in whole seconds, and details.limit "subject". It sits
// behind the verifier, so every request it sees carries a principal;
// a request without one is bucketed under the empty subject, which is
// no route of the public listener.
//
// Every response it passes carries two figures of the same IETF draft,
// both of the effective subject of that request and both read under the
// one lock Allow takes: RateLimit-Limit, the rate in force, which is
// the one the authorizer named for that subject where it named one and
// the node's ORIGO_REQUESTS_PER_MINUTE otherwise, and
// RateLimit-Remaining, the tokens left in that subject's bucket on this
// node after the request, rounded down. Neither is sent when the limit
// is off. A client reads what it has left rather than counting its own
// requests, and spec 021's rate_limited case reads the limit in force
// without exhausting it, which is the only observation that survives a
// balancer spreading one subject over a bucket a node.
//
// The bucket runs in front of the authorizer, so the first response of
// a subject the authorizer names a rate for still reports the node's
// figure, and its Remaining is the depth that figure gave it:
// SetSubjectRate is called by the handler, after this middleware has
// answered. Spec 012 records that window, which the bucket itself has
// always had.
func (l *Limits) Middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := auth.Subject(r.Context())
		if subject == auth.AnonymousSubject {
			// Every anonymous caller of this node shares one bucket
			// (spec 027), keyed by a sentinel no subject can equal. The
			// rate is set on each request rather than once at start-up,
			// so it survives the idle eviction that drops the bucket
			// after ten quiet minutes.
			//
			// The consequence is stated rather than hidden: a scraper
			// degrades other anonymous readers. It cannot degrade an
			// authenticated subject, which holds a bucket of its own
			// that this one cannot draw from.
			subject = AnonymousBucket
			l.buckets.SetRate(subject, l.anonPerMinute)
		}
		a := l.buckets.Allow(subject)
		if a.PerMinute > 0 {
			w.Header().Set(contract.HeaderRateLimit, itoa(a.PerMinute))
			w.Header().Set(contract.HeaderRateRemaining, itoa(a.Remaining))
		}
		if !a.OK {
			l.Refused(LimitSubject)
			l.logger.WarnContext(r.Context(), "subject rate limited", "path", r.URL.Path, "subject", subject, "retry_after_ms", a.Retry.Milliseconds())
			WriteRateLimited(w, LimitSubject, a.Retry)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetSubjectRate records the rate the authorizer named for a subject
// (spec 007's requests_per_minute), so the next request of that subject
// is bucketed at its own figure rather than the node's. Every handler
// that reads an allow calls it, which is where the figure arrives.
func (l *Limits) SetSubjectRate(subject string, perMinute int) {
	if l == nil {
		return
	}
	l.buckets.SetRate(subject, perMinute)
}

// SubjectRate is the rate one subject is bucketed at, for a test and
// for an operator reading why a subject is refused.
func (l *Limits) SubjectRate(subject string) int {
	if l == nil {
		return 0
	}
	return l.buckets.Rate(subject)
}

// Subjects is the number of subjects the bucket table holds, for a
// test and for an operator reading a node's memory.
func (l *Limits) Subjects() int {
	if l == nil {
		return 0
	}
	l.buckets.Sweep()
	return l.buckets.Len()
}
