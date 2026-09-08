// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package limits is spec 012: the bounds a node holds so one client
// cannot exhaust it, and the one quota knob the consumer sets through
// the authorizer. It holds three mechanisms and the values of the
// spec's table: a token bucket per effective subject in front of every
// authenticated route, a semaphore over the git subprocesses of the
// whole node (ORIGO_MAX_GIT_PROCS), and the cached sum of the bytes
// under a repository's lfs/ prefix that the repository quota adds to
// what the log holds.
//
// Nothing here decides who may do what: the quota figure is the
// authorizer's quota_bytes (spec 007) and this package only measures
// against it.
package limits

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/wal"
)

// The values of spec 012's table.
const (
	// DefaultMaxGitProcs is ORIGO_MAX_GIT_PROCS: concurrent git
	// subprocesses per node.
	DefaultMaxGitProcs = 64
	// SlotWait is how long a request waits for a subprocess slot before
	// it is refused.
	SlotWait = 5 * time.Second
	// RequestsPerMinute is the token bucket's rate per effective
	// subject per node, and Burst its depth.
	RequestsPerMinute = 600
	Burst             = 600
	// IdleBucket is how long a bucket may go untouched before it is
	// evicted, so the table holds only active subjects.
	IdleBucket = 10 * time.Minute
	// MaxPushBytes is the largest single push: one entry, one PUT.
	MaxPushBytes = 2 << 30
	// MaxRefs is the cap on the commands of one push and on the
	// references the index holds after it.
	MaxRefs = 100000
	// MaxPushOptions is the cap on a push's options.
	MaxPushOptions = 1000
	// LFSTTL is how long the sum of the bytes under lfs/ is reused, so
	// a push does not list the prefix.
	LFSTTL = 60 * time.Second
	// DefaultQuotaBytes is the repository size limit when the
	// authorizer names none; it is auth.DefaultQuotaBytes.
	DefaultQuotaBytes = 53687091200
)

// The values of the details.limit field of spec 003's over_quota and
// rate_limited envelopes, and the label of origo_rate_limited_total.
const (
	LimitSubject      = "subject"
	LimitSubprocesses = "subprocesses"
	LimitRepository   = "repository"
	LimitPush         = "push"
	LimitRefs         = "refs"
)

// Options configures a Limits.
type Options struct {
	// MaxGitProcs is ORIGO_MAX_GIT_PROCS. DefaultMaxGitProcs when zero;
	// a negative value takes the cap off, which no deployment sets.
	MaxGitProcs int
	// SlotWait is how long a request waits for a subprocess slot.
	// SlotWait when zero.
	SlotWait time.Duration
	// PerMinute and Burst are the token bucket per effective subject.
	// RequestsPerMinute when PerMinute is zero, and PerMinute when
	// Burst is, because the spec's rate and burst are one figure. A
	// negative PerMinute turns the per-subject limit off, which is
	// ORIGO_REQUESTS_PER_MINUTE=0 on a stack driven far harder than a
	// live installation.
	PerMinute int
	Burst     int
	// Idle is how long a bucket may go untouched. IdleBucket when zero.
	Idle time.Duration
	// MaxPushBytes is the largest single push. MaxPushBytes when zero;
	// a test lowers it rather than sending two gibibytes.
	MaxPushBytes int64
	// Log resolves a repository's prefix for the lfs/ sum. Nil answers
	// zero bytes, which is every caller that has no LFS surface.
	Log *wal.Log
	// LFSTTL is how long the lfs/ sum is reused. LFSTTL when zero.
	LFSTTL time.Duration
	// Now is the clock the buckets and the lfs/ sums run on. The wall
	// clock by default.
	Now func() time.Time
	// Metrics receives origo_rate_limited_total{limit}.
	Metrics *metrics.Set
	Logger  *slog.Logger
}

// Limits is every bound of spec 012, built once by the node and shared
// by the handlers and compaction. A nil *Limits enforces nothing, which
// is what a test of a handler that asserts on another rule wants.
type Limits struct {
	slots   *Semaphore
	buckets *Buckets
	lfs     *LFSBytes
	wait    time.Duration
	maxPush int64
	refused *pkgmetrics.Counter
	logger  *slog.Logger
}

// New builds the limits.
func New(o Options) *Limits {
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	perMinute := orInt(o.PerMinute, RequestsPerMinute)
	l := &Limits{
		slots:   NewSemaphore(orInt(o.MaxGitProcs, DefaultMaxGitProcs)),
		buckets: NewBuckets(perMinute, orInt(o.Burst, perMinute), orDuration(o.Idle, IdleBucket), now),
		lfs:     NewLFSBytes(o.Log, orDuration(o.LFSTTL, LFSTTL), now),
		wait:    orDuration(o.SlotWait, SlotWait),
		maxPush: o.MaxPushBytes,
		refused: set.RateLimited,
		logger:  logger,
	}
	if l.maxPush == 0 {
		l.maxPush = MaxPushBytes
	}
	return l
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// MaxPush is the largest single push this node accepts.
func (l *Limits) MaxPush() int64 {
	if l == nil {
		return MaxPushBytes
	}
	return l.maxPush
}

// Slots is the subprocess semaphore, the value compaction takes its
// slots through (spec 006's compact.Slots). Nil for a nil *Limits, which
// is what that seam reads as "take no slot".
func (l *Limits) Slots() *Semaphore {
	if l == nil {
		return nil
	}
	return l.slots
}

// LFSBytes is the sum of the bytes under the repository's lfs/ prefix,
// reused for LFSTTL so a push does not list the prefix.
func (l *Limits) LFSBytes(ctx context.Context, repo string) (int64, error) {
	if l == nil {
		return 0, nil
	}
	return l.lfs.Bytes(ctx, repo)
}

// Refused counts one refusal by limit. Only the two limits of this
// spec's enforcement, subject and subprocesses, are rate_limited
// refusals; over_quota is a different code and is not counted here.
func (l *Limits) Refused(limit string) {
	if l == nil {
		return
	}
	l.refused.Inc(map[string]string{"limit": limit})
}

// Slot takes one subprocess slot for a request, waiting at most the
// configured wait, and writes the 429 when none came free. It reports
// whether the handler may start its subprocess; the release runs when
// the subprocess is done.
func (l *Limits) Slot(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	release, ok := l.slots.Acquire(r.Context(), l.wait)
	if ok {
		return release, true
	}
	l.Refused(LimitSubprocesses)
	l.logger.WarnContext(r.Context(), "no subprocess slot", "path", r.URL.Path, "waited", l.wait.String(), "max_git_procs", l.slots.Size())
	WriteRateLimited(w, LimitSubprocesses, l.wait)
	return nil, false
}

// RetryAfterSeconds is the whole-second Retry-After of a refusal, at
// least one second so a client never reads zero.
func RetryAfterSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// WriteRateLimited writes spec 003's 429: the rate_limited sentence,
// Retry-After in whole seconds, and the limit that refused.
func WriteRateLimited(w http.ResponseWriter, limit string, retryAfter time.Duration) {
	after := RetryAfterSeconds(retryAfter)
	w.Header().Set("Retry-After", itoa(after))
	contract.Write(w, http.StatusTooManyRequests, contract.CodeRateLimited, map[string]any{
		"limit": limit, "retry_after": after,
	})
}

// itoa keeps the header free of the fmt package on a hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
