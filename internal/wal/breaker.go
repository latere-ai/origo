// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"latere.ai/x/pkg/circuitbreaker"
	pkgmetrics "latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/wait"

	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/tracing"
)

// ErrStorageOpen is the refusal of a Store call while its breaker is
// open (spec 015): the call was not sent. Handlers map it to 503
// storage_unavailable with details.error "breaker open".
var ErrStorageOpen = errors.New("breaker open")

// Class is one of the two breaker classes of spec 015: reads (Get,
// Head, List) and writes (Put, Create, Delete), so a write-side outage
// does not stop the reads the cache can answer.
type Class string

// The two classes, which are also the class label of
// origo_storage_breaker_state.
const (
	ClassRead  Class = "read"
	ClassWrite Class = "write"
)

// Clock is what every duration of spec 015 runs on, so the suite drives
// the open window and the stale bound with a fake one.
type Clock interface {
	Now() time.Time
}

// wallClock is the default Clock.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// OpError is a Store call that failed under the BreakerStore: the
// operation, the key, and the cause, which handlers render as the
// details of storage_unavailable (spec 003: op, key, error).
type OpError struct {
	Op  string
	Key string
	Err error
}

func (e *OpError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("wal: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("wal: %s %s: %v", e.Op, e.Key, e.Err)
}

func (e *OpError) Unwrap() error { return e.Err }

// opClass is the breaker class of a store operation: the three that
// change the bucket are writes, the three that read it are reads. It is
// the one place the mapping lives, so a call and a Retry-After for the
// same operation cannot name different breakers.
func opClass(op string) Class {
	switch op {
	case "create", "put", "delete":
		return ClassWrite
	}
	return ClassRead
}

// ClassOf is the breaker class behind a failed call, read off the
// operation its OpError names. A handler answering Retry-After takes
// the class from here rather than assuming one, because the class that
// refused is the class whose open window remains: a write refused while
// the read breaker is closed would otherwise be told to retry after
// zero seconds. An error that carries no OpError reads as the read
// class, which is every failure that never reached the store.
func ClassOf(err error) Class {
	if oe, ok := errors.AsType[*OpError](err); ok {
		return opClass(oe.Op)
	}
	return ClassRead
}

// ErrorDetails is the developer fields of a storage_unavailable envelope
// for err: op, key, and error when the call ran under the BreakerStore,
// error alone otherwise.
func ErrorDetails(err error) map[string]any {
	if oe, ok := errors.AsType[*OpError](err); ok {
		d := map[string]any{"op": oe.Op, "error": oe.Err.Error()}
		if oe.Key != "" {
			d["key"] = oe.Key
		}
		return d
	}
	return map[string]any{"error": err.Error()}
}

// Breaker values of spec 015.
const (
	// BreakerThreshold is the consecutive failures that open a breaker.
	BreakerThreshold = 5
	// BreakerOpenFor is how long a breaker stays open before one probe.
	BreakerOpenFor = 30 * time.Second
	// DefaultStorageTimeout is ORIGO_STORAGE_TIMEOUT's default.
	DefaultStorageTimeout = 10 * time.Second
)

// breaker is the three-state circuit breaker of
// latere.ai/x/pkg/circuitbreaker.Breaker with one difference: it reads
// its clock from a function, so a test advances the open window
// without waiting for it. It has the package's semantics exactly: it
// opens on the threshold-th consecutive failure, stays open for the
// window, then Allow admits one probe and refuses everyone else until
// the probe reports; a success closes it, a failure reopens it for
// another window. Its home is pkg/circuitbreaker, as
// New(threshold, openDuration, WithClock(now)); it lives here until
// that option exists (spec 015's item for pkg).
type breaker struct {
	threshold int
	openFor   time.Duration
	now       func() time.Time

	mu       sync.Mutex
	state    circuitbreaker.State
	failures int
	openedAt time.Time
}

func newBreaker(threshold int, openFor time.Duration, now func() time.Time) *breaker {
	return &breaker{threshold: threshold, openFor: openFor, now: now}
}

// Allow reports whether a call may run now. In the open state the
// first call after the window becomes the probe and moves the breaker
// to half-open; every other call is refused until the probe reports.
func (b *breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case circuitbreaker.Closed:
		return true
	case circuitbreaker.Open:
		if b.now().Sub(b.openedAt) < b.openFor {
			return false
		}
		b.state = circuitbreaker.HalfOpen
		return true
	default:
		return false
	}
}

// Admits reports whether a call would be allowed now without taking
// the probe slot: closed, or open past its window. A poller asks this
// and lets its next call be the probe.
func (b *breaker) Admits() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case circuitbreaker.Closed:
		return true
	case circuitbreaker.Open:
		return b.now().Sub(b.openedAt) >= b.openFor
	default:
		return false
	}
}

// RecordSuccess closes the breaker.
func (b *breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures = circuitbreaker.Closed, 0
}

// RecordFailure counts one failure: the threshold-th consecutive one
// opens a closed breaker, and a failed probe reopens a half-open one
// for one more window, never a longer one.
func (b *breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	switch b.state {
	case circuitbreaker.Closed:
		if b.failures >= b.threshold {
			b.state, b.openedAt = circuitbreaker.Open, b.now()
		}
	case circuitbreaker.HalfOpen:
		b.state, b.openedAt = circuitbreaker.Open, b.now()
	}
}

// State reports the state, the value of origo_storage_breaker_state.
func (b *breaker) State() circuitbreaker.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Remaining is the whole seconds of the window that remain, at least
// one second, while the breaker is open or half-open; zero when closed.
// It is the Retry-After a refusal carries.
func (b *breaker) Remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == circuitbreaker.Closed {
		return 0
	}
	left := b.openFor - b.now().Sub(b.openedAt)
	left = left.Truncate(time.Second)
	return max(left, time.Second)
}

// BreakerOptions configures a BreakerStore.
type BreakerOptions struct {
	// Store is the store the calls go to; required.
	Store Store
	// Timeout is ORIGO_STORAGE_TIMEOUT, the deadline of one Store call.
	// DefaultStorageTimeout when zero.
	Timeout time.Duration
	// Clock is the clock the breakers and Retry-After run on. The wall
	// clock by default.
	Clock Clock
	// Metrics holds the handles the wrapper records through: the ops
	// counter, the latency histogram, and the breaker gauge (spec 011).
	// A set of its own by default.
	Metrics *metrics.Set
	// Threshold and OpenFor are the breaker values, BreakerThreshold
	// and BreakerOpenFor by default; a test may lower them.
	Threshold int
	OpenFor   time.Duration
}

// BreakerStore is the Store of spec 015: every call runs under the
// storage deadline, is counted on origo_storage_ops_total and timed on
// origo_storage_seconds under a span named by its op, and is gated by
// the breaker of its class. One Store call is one count whatever the
// client did inside it: a 404, a 412, and a 304 are successes, and
// everything else, a timeout included, is a failure.
type BreakerStore struct {
	store   Store
	timeout time.Duration
	clock   Clock
	read    *breaker
	write   *breaker

	ops     *pkgmetrics.Counter
	seconds *pkgmetrics.Histogram
}

// NewBreakerStore wraps the store.
func NewBreakerStore(o BreakerOptions) *BreakerStore {
	if o.Store == nil {
		panic("wal: BreakerStore needs a Store")
	}
	b := &BreakerStore{store: o.Store, timeout: o.Timeout, clock: o.Clock}
	if b.timeout <= 0 {
		b.timeout = DefaultStorageTimeout
	}
	if b.clock == nil {
		b.clock = wallClock{}
	}
	threshold, openFor := o.Threshold, o.OpenFor
	if threshold <= 0 {
		threshold = BreakerThreshold
	}
	if openFor <= 0 {
		openFor = BreakerOpenFor
	}
	b.read = newBreaker(threshold, openFor, b.clock.Now)
	b.write = newBreaker(threshold, openFor, b.clock.Now)
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	b.ops, b.seconds = set.StorageOps, set.StorageSeconds
	set.StorageBreaker.BindFor(string(ClassRead), func() float64 { return float64(b.read.State()) })
	set.StorageBreaker.BindFor(string(ClassWrite), func() float64 { return float64(b.write.State()) })
	return b
}

// Timeout is the deadline of one call.
func (b *BreakerStore) Timeout() time.Duration { return b.timeout }

func (b *BreakerStore) breaker(c Class) *breaker {
	if c == ClassWrite {
		return b.write
	}
	return b.read
}

// State reports a class's breaker state.
func (b *BreakerStore) State(c Class) circuitbreaker.State { return b.breaker(c).State() }

// Admits reports whether a call of the class would run now: the
// breaker is closed, or open past its window so that the next call is
// the probe. It takes no probe slot itself.
func (b *BreakerStore) Admits(c Class) bool { return b.breaker(c).Admits() }

// RetryAfter is the whole seconds of the open window that remain for
// the class, at least one second, and zero when the breaker is closed.
func (b *BreakerStore) RetryAfter(c Class) time.Duration { return b.breaker(c).Remaining() }

// Wait polls Admits for the class every poll of real time, for at most
// limit of the clock's time, and returns nil once a call would be
// admitted, so the caller's next call is the probe. It returns
// ErrStorageOpen when the limit passes with no admission and the
// context's error when the context ends first. The spooled push of
// spec 015 waits here for the write breaker.
func (b *BreakerStore) Wait(ctx context.Context, c Class, poll, limit time.Duration) error {
	br := b.breaker(c)
	started := b.clock.Now()
	for {
		if br.Admits() {
			return nil
		}
		if b.clock.Now().Sub(started) >= limit {
			return &OpError{Op: "wait", Err: ErrStorageOpen}
		}
		if err := wait.Sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// call runs one Store call of op on key under the class's breaker: the
// gate, the deadline, the span, the count, and the timing. fn receives
// the context to send under and reports the call's error, which
// classify turns into the result label and the breaker's verdict. A
// call the caller's own context ended is neither a success nor a
// failure toward the breaker: the bucket did not fail it, and a client
// that went away says nothing about the bucket.
func (b *BreakerStore) call(ctx context.Context, op, key string, fn func(context.Context) error) error {
	br := b.breaker(opClass(op))
	if !br.Allow() {
		b.ops.Inc(map[string]string{"op": op, "result": "error"})
		return &OpError{Op: op, Key: key, Err: ErrStorageOpen}
	}
	parent := ctx
	ctx, end := tracing.Start(ctx, op)
	defer end()
	started := time.Now()
	err := fn(ctx)
	b.seconds.Observe(map[string]string{"op": op}, time.Since(started).Seconds())
	result, ok := classify(err)
	b.ops.Inc(map[string]string{"op": op, "result": result})
	if ok {
		br.RecordSuccess()
		return err
	}
	if parent.Err() == nil {
		br.RecordFailure()
	}
	return &OpError{Op: op, Key: key, Err: err}
}

// classify maps a call's error to its result label and to the
// breaker's verdict: nil, a 404, a 412, and a 304 are successes.
func classify(err error) (result string, ok bool) {
	switch {
	case err == nil:
		return "ok", true
	case errors.Is(err, ErrNotFound):
		return "not_found", true
	case errors.Is(err, ErrExists):
		return "exists", true
	case errors.Is(err, ErrNotModified):
		return "ok", true
	}
	return "error", false
}

// deadline bounds ctx by the storage timeout. The error a call reports
// after the deadline is context.DeadlineExceeded, whichever context
// ended it.
func (b *BreakerStore) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.timeout)
}

// Create implements Store under the write breaker.
func (b *BreakerStore) Create(ctx context.Context, key string, body Body) (etag string, err error) {
	err = b.call(ctx, "create", key, func(ctx context.Context) error {
		ctx, cancel := b.deadline(ctx)
		defer cancel()
		var cerr error
		etag, cerr = b.store.Create(ctx, key, body)
		return cerr
	})
	return etag, err
}

// Put implements Store under the write breaker.
func (b *BreakerStore) Put(ctx context.Context, key string, body Body) (etag string, err error) {
	err = b.call(ctx, "put", key, func(ctx context.Context) error {
		ctx, cancel := b.deadline(ctx)
		defer cancel()
		var perr error
		etag, perr = b.store.Put(ctx, key, body)
		return perr
	})
	return etag, err
}

// Delete implements Store under the write breaker.
func (b *BreakerStore) Delete(ctx context.Context, key string) error {
	return b.call(ctx, "delete", key, func(ctx context.Context) error {
		ctx, cancel := b.deadline(ctx)
		defer cancel()
		return b.store.Delete(ctx, key)
	})
}

// Head implements Store under the read breaker.
func (b *BreakerStore) Head(ctx context.Context, key string) (o Object, err error) {
	err = b.call(ctx, "head", key, func(ctx context.Context) error {
		ctx, cancel := b.deadline(ctx)
		defer cancel()
		var herr error
		o, herr = b.store.Head(ctx, key)
		return herr
	})
	return o, err
}

// List implements Store under the read breaker.
func (b *BreakerStore) List(ctx context.Context, opts ListOptions) (res ListResult, err error) {
	err = b.call(ctx, "list", opts.Prefix, func(ctx context.Context) error {
		ctx, cancel := b.deadline(ctx)
		defer cancel()
		var lerr error
		res, lerr = b.store.List(ctx, opts)
		return lerr
	})
	return res, err
}

// Get implements Store under the read breaker. The deadline bounds the
// call, which returns when the object's headers arrive; the body is
// read after it under the caller's own context, because a pack of two
// gibibytes takes longer than any per-call deadline, so the timer is
// stopped once the call returns and the request's context is released
// when the body is closed.
func (b *BreakerStore) Get(ctx context.Context, key, ifNoneMatch string) (rc io.ReadCloser, o Object, err error) {
	err = b.call(ctx, "get", key, func(ctx context.Context) error {
		ctx, cancel := context.WithCancelCause(ctx)
		timer := time.AfterFunc(b.timeout, func() { cancel(context.DeadlineExceeded) })
		body, obj, gerr := b.store.Get(ctx, key, ifNoneMatch)
		if !timer.Stop() && gerr != nil {
			gerr = fmt.Errorf("%w: %w", context.DeadlineExceeded, gerr)
		}
		if gerr != nil {
			cancel(gerr)
			return gerr
		}
		rc, o = &releasingBody{ReadCloser: body, release: func() { cancel(nil) }}, obj
		return nil
	})
	return rc, o, err
}

// releasingBody releases the call's context when the body is closed.
type releasingBody struct {
	io.ReadCloser
	release func()
}

func (r *releasingBody) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}
