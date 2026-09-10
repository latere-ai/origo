// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package limits

import (
	"net/http"
	"sync"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
)

// Buckets is one token bucket per effective subject: the bucket fills
// at PerMinute tokens a minute up to Burst and a request takes one. A
// bucket untouched for Idle is evicted on the next sweep, so the table
// holds only the subjects that are calling.
type Buckets struct {
	perMinute int
	burst     float64
	idle      time.Duration
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

// bucket is one subject's tokens and when it was last touched. rate is
// the figure the authorizer named for this subject alone (spec 007's
// requests_per_minute, read under spec 020); zero is the table's own
// rate, the value of ORIGO_REQUESTS_PER_MINUTE.
type bucket struct {
	tokens  float64
	updated time.Time
	rate    int
}

// rateOf is the figure a bucket fills at, and the depth it fills to:
// the subject's own when the authorizer named one, the table's
// otherwise. The rate is the burst, the way the table's two figures are
// one figure.
func (b *Buckets) rateOf(e *bucket) (perMinute int, burst float64) {
	if e.rate > 0 {
		return e.rate, float64(e.rate)
	}
	return b.perMinute, b.burst
}

// NewBuckets builds the table. A rate of zero or less lets everything
// through.
func NewBuckets(perMinute, burst int, idle time.Duration, now func() time.Time) *Buckets {
	if now == nil {
		now = time.Now
	}
	return &Buckets{
		perMinute: perMinute, burst: float64(burst), idle: idle, now: now,
		buckets: map[string]*bucket{}, swept: now(),
	}
}

// Allowance is what Allow answers about one request: whether it goes
// on, how long a refused one waits, the figure in force for the
// subject, and the tokens left in its bucket after this request. The
// two figures travel back from here because Allow read the bucket under
// the lock it already takes, so the two RateLimit headers on every
// response cost no second acquisition. PerMinute is zero when the limit
// is off, which is how the middleware knows to send neither header.
type Allowance struct {
	OK        bool
	Retry     time.Duration
	PerMinute int
	Remaining int
}

// Allow takes one token for the subject and reports what the request
// may do and what is left. The rate in force is the one the authorizer
// named for this subject where it named one, the table's own otherwise.
func (b *Buckets) Allow(subject string) Allowance {
	if b == nil || b.perMinute <= 0 {
		return Allowance{OK: true}
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep(now)
	e, ok := b.buckets[subject]
	if !ok {
		e = &bucket{tokens: b.burst, updated: now}
		b.buckets[subject] = e
	}
	perMinute, burst := b.rateOf(e)
	if ok {
		e.tokens = min(burst, e.tokens+now.Sub(e.updated).Minutes()*float64(perMinute))
		e.updated = now
	}
	if e.tokens < 1 {
		// The wait until one whole token has accrued. Nothing is left,
		// which is what the refusal reports.
		return Allowance{Retry: time.Duration((1 - e.tokens) / float64(perMinute) * float64(time.Minute)), PerMinute: perMinute}
	}
	e.tokens--
	// Rounded down, so a client that trusts the figure never sends a
	// request the bucket cannot pay for.
	return Allowance{OK: true, PerMinute: perMinute, Remaining: int(e.tokens)}
}

// SetRate records the rate the authorizer named for one subject (spec
// 007's optional requests_per_minute), which is the rate and the burst
// of that subject's bucket from here on; a figure of zero or less
// leaves the subject on the table's own rate. Raising the figure fills
// the bucket to the new depth at once, so a tool the authorizer just
// granted a higher rate is not held to the depth it was created with.
func (b *Buckets) SetRate(subject string, perMinute int) {
	if b == nil || perMinute <= 0 {
		return
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.buckets[subject]
	if !ok {
		e = &bucket{tokens: float64(perMinute), updated: now}
		b.buckets[subject] = e
	}
	if e.rate == perMinute {
		return
	}
	if perMinute > e.rate {
		e.tokens = max(e.tokens, float64(perMinute))
	} else {
		e.tokens = min(e.tokens, float64(perMinute))
	}
	e.rate = perMinute
}

// Rate is the figure a subject's bucket fills at: the one the
// authorizer named for it, or the table's own. A subject with no bucket
// reads the table's.
func (b *Buckets) Rate(subject string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.buckets[subject]; ok && e.rate > 0 {
		return e.rate
	}
	return b.perMinute
}

// sweep drops the buckets untouched for Idle. It runs at most once per
// idle window, under the caller's lock, so a busy node walks the table
// rarely and an idle one still empties it.
func (b *Buckets) sweep(now time.Time) {
	if b.idle <= 0 || now.Sub(b.swept) < b.idle {
		return
	}
	b.swept = now
	for subject, e := range b.buckets {
		if now.Sub(e.updated) >= b.idle {
			delete(b.buckets, subject)
		}
	}
}

// Len is the number of subjects the table holds, for a test.
func (b *Buckets) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep(b.now())
	return len(b.buckets)
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
	return l.buckets.Len()
}
