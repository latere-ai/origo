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

// bucket is one subject's tokens and when it was last touched.
type bucket struct {
	tokens  float64
	updated time.Time
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
// goes on and, when it does not, how long until the next token.
func (b *Buckets) Allow(subject string) (bool, time.Duration) {
	if b == nil || b.perMinute <= 0 {
		return true, 0
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep(now)
	e, ok := b.buckets[subject]
	if !ok {
		e = &bucket{tokens: b.burst, updated: now}
		b.buckets[subject] = e
	} else {
		e.tokens = min(b.burst, e.tokens+now.Sub(e.updated).Minutes()*float64(b.perMinute))
		e.updated = now
	}
	if e.tokens < 1 {
		// The wait until one whole token has accrued.
		return false, time.Duration((1 - e.tokens) / float64(b.perMinute) * float64(time.Minute))
	}
	e.tokens--
	return true, 0
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
// force, so a client and the conformance suite of spec 021 read the
// limit before they meet it rather than guessing at the default.
func (l *Limits) Middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.buckets.perMinute > 0 {
			w.Header().Set(contract.HeaderRateLimit, itoa(l.buckets.perMinute))
		}
		subject := auth.Subject(r.Context())
		ok, retry := l.buckets.Allow(subject)
		if !ok {
			l.Refused(LimitSubject)
			l.logger.WarnContext(r.Context(), "subject rate limited", "path", r.URL.Path, "subject", subject, "retry_after_ms", retry.Milliseconds())
			WriteRateLimited(w, LimitSubject, retry)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Subjects is the number of subjects the bucket table holds, for a
// test and for an operator reading a node's memory.
func (l *Limits) Subjects() int {
	if l == nil {
		return 0
	}
	return l.buckets.Len()
}
