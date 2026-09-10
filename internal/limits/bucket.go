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

// Allow takes one token for the subject. It reports whether the request
// goes on, when it does not how long until the next token, and the
// figure in force for this subject: the rate the authorizer named for
// it when it has one, the table's own otherwise, and zero when the
// limit is off. The figure travels back from here because Allow has
// already read it under the lock, so RateLimit-Limit on every response
// costs no second acquisition.
func (b *Buckets) Allow(subject string) (bool, time.Duration, int) {
	if b == nil || b.perMinute <= 0 {
		return true, 0, 0
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
		// The wait until one whole token has accrued.
		return false, time.Duration((1 - e.tokens) / float64(perMinute) * float64(time.Minute)), perMinute
	}
	e.tokens--
	return true, 0, perMinute
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
// Every response it passes carries RateLimit-Limit, the figure in
// force for the effective subject of that request: the rate the
// authorizer named for it where it named one, the node's
// ORIGO_REQUESTS_PER_MINUTE otherwise, and no header at all when the
// limit is off. Allow reads that figure under the lock it already
// takes, so the header costs nothing extra. A client and the
// conformance suite of spec 021 read the limit before they meet it
// rather than guessing at the default.
//
// The bucket runs in front of the authorizer, so the first response of
// a subject the authorizer names a rate for still reports the node's
// figure: SetSubjectRate is called by the handler, after this
// middleware has answered. Spec 012 records that window, which the
// bucket itself has always had.
func (l *Limits) Middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := auth.Subject(r.Context())
		ok, retry, perMinute := l.buckets.Allow(subject)
		if perMinute > 0 {
			w.Header().Set(contract.HeaderRateLimit, itoa(perMinute))
		}
		if !ok {
			l.Refused(LimitSubject)
			l.logger.WarnContext(r.Context(), "subject rate limited", "path", r.URL.Path, "subject", subject, "retry_after_ms", retry.Milliseconds())
			WriteRateLimited(w, LimitSubject, retry)
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
