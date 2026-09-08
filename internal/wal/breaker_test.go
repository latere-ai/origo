// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/circuitbreaker"
	pkgmetrics "latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/s3/s3test"

	"github.com/latere-ai/origo/internal/metrics"
)

// fakeClock is the Clock of the suite: it moves only when advanced, or
// by step on every reading when a step is set, which drives a poll loop
// to its limit without a second goroutine.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *fakeClock) Step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = d
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestBreakerOpensOnTimeoutsAndRecovers is spec 015's breaker criterion:
// a slow store fails a call at the deadline, the fifth failure opens the
// breaker, an open breaker refuses at once, the window admits one probe
// and refuses a concurrent call, a failed probe reopens for one window
// and not two, and a successful probe closes it.
func TestBreakerOpensOnTimeoutsAndRecovers(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	clock := newFakeClock()
	reg := pkgmetrics.NewRegistry()
	set := metrics.Register(reg)
	store := NewBreakerStore(BreakerOptions{Store: mem, Timeout: 30 * time.Millisecond, Clock: clock, Metrics: set})
	if store.Timeout() != 30*time.Millisecond {
		t.Fatalf("timeout %s", store.Timeout())
	}
	if _, err := store.Put(ctx, "k", BytesBody([]byte("v"))); err != nil {
		t.Fatal(err)
	}

	// Every call answers after a second: reads and writes fail at the
	// deadline, well before the store would have answered.
	mem.SetLatency(time.Second)
	started := time.Now()
	_, err := store.Head(ctx, "k")
	if took := time.Since(started); !errors.Is(err, context.DeadlineExceeded) || took > 500*time.Millisecond {
		t.Fatalf("slow HEAD: %v after %s", err, took)
	}
	var oe *OpError
	if !errors.As(err, &oe) || oe.Op != "head" || oe.Key != "k" {
		t.Fatalf("slow HEAD error %#v", err)
	}
	if _, err := store.Put(ctx, "k", BytesBody([]byte("v"))); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow PUT: %v", err)
	}
	if store.State(ClassRead) != circuitbreaker.Closed || store.State(ClassWrite) != circuitbreaker.Closed {
		t.Fatalf("one failure each: read %s write %s", store.State(ClassRead), store.State(ClassWrite))
	}
	// Four more read failures: the fifth opens the read breaker alone.
	for i := range 4 {
		if store.State(ClassRead) != circuitbreaker.Closed {
			t.Fatalf("read breaker open after %d failures", i+1)
		}
		if _, _, err := store.Get(ctx, "k", ""); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("slow GET %d: %v", i, err)
		}
	}
	if store.State(ClassRead) != circuitbreaker.Open || store.State(ClassWrite) != circuitbreaker.Closed {
		t.Fatalf("after the fifth read failure: read %s write %s", store.State(ClassRead), store.State(ClassWrite))
	}
	// Refused at once, without a call to the store.
	heads := mem.Calls["Head"]
	started = time.Now()
	_, err = store.Head(ctx, "k")
	if !errors.Is(err, ErrStorageOpen) || time.Since(started) > 20*time.Millisecond || mem.Calls["Head"] != heads {
		t.Fatalf("open breaker: %v after %s, %d calls", err, time.Since(started), mem.Calls["Head"]-heads)
	}
	if d := ErrorDetails(err); d["op"] != "head" || d["key"] != "k" || d["error"] != "breaker open" {
		t.Fatalf("details %v", d)
	}
	if store.Admits(ClassRead) || store.RetryAfter(ClassRead) != 30*time.Second {
		t.Fatalf("admits %v, retry after %s", store.Admits(ClassRead), store.RetryAfter(ClassRead))
	}
	clock.Advance(29 * time.Second)
	if store.Admits(ClassRead) || store.RetryAfter(ClassRead) != time.Second {
		t.Fatalf("at 29s: admits %v, retry after %s", store.Admits(ClassRead), store.RetryAfter(ClassRead))
	}
	clock.Advance(500 * time.Millisecond)
	if store.RetryAfter(ClassRead) != time.Second {
		t.Fatalf("at 29.5s: retry after %s, want the floor of one second", store.RetryAfter(ClassRead))
	}
	clock.Advance(500 * time.Millisecond)
	if !store.Admits(ClassRead) {
		t.Fatal("at 30s the breaker admits nothing")
	}

	// The window passed: one probe runs while a concurrent call is
	// refused; the probe fails and the breaker reopens for 30 s, not 60.
	mem.SetLatency(0)
	release := make(chan struct{})
	mem.SetFault(func(op, _ string) error {
		if op == "Head" {
			<-release
			return errors.New("still down")
		}
		return nil
	})
	probe := make(chan error, 1)
	go func() {
		_, err := store.Head(ctx, "k")
		probe <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for store.State(ClassRead) != circuitbreaker.HalfOpen && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := store.Head(ctx, "k"); !errors.Is(err, ErrStorageOpen) {
		t.Fatalf("a call beside the probe: %v", err)
	}
	close(release)
	if err := <-probe; err == nil || errors.Is(err, ErrStorageOpen) {
		t.Fatalf("the probe: %v", err)
	}
	if store.State(ClassRead) != circuitbreaker.Open || store.RetryAfter(ClassRead) != 30*time.Second {
		t.Fatalf("after the failed probe: %s, retry after %s", store.State(ClassRead), store.RetryAfter(ClassRead))
	}
	clock.Advance(30 * time.Second)
	// The store recovered: the probe succeeds and closes the breaker.
	mem.SetFault(nil)
	if _, err := store.Head(ctx, "k"); err != nil {
		t.Fatalf("the second probe: %v", err)
	}
	if store.State(ClassRead) != circuitbreaker.Closed || store.RetryAfter(ClassRead) != 0 {
		t.Fatalf("after the successful probe: %s, retry after %s", store.State(ClassRead), store.RetryAfter(ClassRead))
	}
	var text bytes.Buffer
	reg.WritePrometheus(&text)
	for _, want := range []string{
		`origo_storage_breaker_state{class="read"} 0`,
		`origo_storage_breaker_state{class="write"} 0`,
		`origo_storage_ops_total{op="head",result="error"} 4`,
		`origo_storage_ops_total{op="head",result="ok"} 1`,
		`origo_storage_ops_total{op="get",result="error"} 4`,
		`origo_storage_ops_total{op="put",result="error"} 1`,
		`origo_storage_ops_total{op="put",result="ok"} 1`,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, text.String())
		}
	}
	// The refused calls counted on the ops counter but were not timed:
	// no operation ran.
	if got := set.StorageSeconds.Count(map[string]string{"op": "head"}); got != 3 {
		t.Fatalf("head observed %d times, want the 3 that ran", got)
	}
}

// TestOneCallWithThreeAttemptsIsOneFailure runs the S3 adapter under
// the wrapper against the fake endpoint: pkg/s3 retries a 503 three
// times inside one Store call, and the breaker counts one failure.
func TestOneCallWithThreeAttemptsIsOneFailure(t *testing.T) {
	ctx := context.Background()
	f := s3test.New(t, "origo")
	s3, err := NewS3(S3Options{Endpoint: f.URL(), Region: s3test.Region, Bucket: "origo", Key: s3test.Key, Secret: s3test.Secret, PathStyle: true, Client: f.HTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	store := NewBreakerStore(BreakerOptions{Store: s3, Threshold: 2})
	f.Fail(3, http.StatusServiceUnavailable)
	if _, err := store.Head(ctx, "k"); err == nil || errors.Is(err, ErrStorageOpen) {
		t.Fatalf("HEAD under three 503s: %v", err)
	}
	if f.Count("HEAD") != 3 {
		t.Fatalf("the client sent %d attempts", f.Count("HEAD"))
	}
	if store.State(ClassRead) != circuitbreaker.Closed || store.read.failures != 1 {
		t.Fatalf("after one call of three attempts: %s, %d failures", store.State(ClassRead), store.read.failures)
	}
	f.Fail(3, http.StatusServiceUnavailable)
	if _, err := store.Head(ctx, "k"); err == nil {
		t.Fatal("second call answered")
	}
	if store.State(ClassRead) != circuitbreaker.Open {
		t.Fatalf("after two calls: %s", store.State(ClassRead))
	}
}

// TestBreakerStoreResultsAndBodies covers the result labels of every
// op, the successes that are not nil errors, the body a Get answers
// after its call, and the error details of a plain failure.
func TestBreakerStoreResultsAndBodies(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	reg := pkgmetrics.NewRegistry()
	store := NewBreakerStore(BreakerOptions{Store: mem, Metrics: metrics.Register(reg)})
	if store.Timeout() != DefaultStorageTimeout {
		t.Fatalf("default timeout %s", store.Timeout())
	}
	if _, err := store.Create(ctx, "k", BytesBody([]byte("v"))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "k", BytesBody([]byte("w"))); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: %v", err)
	}
	if _, err := store.Head(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head absent: %v", err)
	}
	rc, o, err := store.Get(ctx, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(data) != "v" || o.Key != "k" {
		t.Fatalf("get: %q %+v %v", data, o, err)
	}
	if _, _, err := store.Get(ctx, "k", o.ETag); !errors.Is(err, ErrNotModified) {
		t.Fatalf("conditional get: %v", err)
	}
	if res, err := store.List(ctx, ListOptions{Prefix: "k"}); err != nil || len(res.Objects) != 1 {
		t.Fatalf("list: %+v %v", res, err)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	// A store failure that is none of the sentinels is an error with
	// the op and the key in its details.
	mem.SetFault(func(string, string) error { return errors.New("bucket full") })
	err = store.Delete(ctx, "k")
	if d := ErrorDetails(err); d["op"] != "delete" || d["key"] != "k" || d["error"] != "bucket full" {
		t.Fatalf("details %v of %v", d, err)
	}
	if d := ErrorDetails(errors.New("plain")); d["error"] != "plain" || d["op"] != nil {
		t.Fatalf("plain details %v", d)
	}
	if got := (&OpError{Op: "list", Err: errors.New("x")}).Error(); got != "wal: list: x" {
		t.Fatalf("OpError without a key: %q", got)
	}
	var text bytes.Buffer
	reg.WritePrometheus(&text)
	for _, want := range []string{
		`origo_storage_ops_total{op="create",result="ok"} 1`,
		`origo_storage_ops_total{op="create",result="exists"} 1`,
		`origo_storage_ops_total{op="head",result="not_found"} 1`,
		`origo_storage_ops_total{op="get",result="ok"} 2`,
		`origo_storage_ops_total{op="list",result="ok"} 1`,
		`origo_storage_ops_total{op="delete",result="ok"} 1`,
		`origo_storage_ops_total{op="delete",result="error"} 1`,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, text.String())
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a wrapper without a store did not panic")
		}
	}()
	NewBreakerStore(BreakerOptions{})
}

// TestWaitPollsTheWriteBreaker covers Wait: admitted at once when the
// breaker is closed, admitted once the window passes under the fake
// clock, refused with ErrStorageOpen when the limit passes first, and
// ended by the context.
func TestWaitPollsTheWriteBreaker(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	clock := newFakeClock()
	store := NewBreakerStore(BreakerOptions{Store: mem, Clock: clock, Threshold: 1})
	if err := store.Wait(ctx, ClassWrite, time.Millisecond, time.Minute); err != nil {
		t.Fatalf("closed: %v", err)
	}
	mem.SetFault(func(string, string) error { return errors.New("down") })
	if _, err := store.Put(ctx, "k", BytesBody(nil)); err == nil {
		t.Fatal("put under a fault answered")
	}
	if store.State(ClassWrite) != circuitbreaker.Open {
		t.Fatalf("threshold 1: %s", store.State(ClassWrite))
	}
	// The window passes while Wait polls: the clock moves a second per
	// reading, so the poll meets the window before its minute.
	clock.Step(time.Second)
	if err := store.Wait(ctx, ClassWrite, time.Millisecond, time.Minute); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if !store.Admits(ClassWrite) {
		t.Fatal("admitted by Wait and not by Admits")
	}
	// A failed probe reopens it; a limit shorter than the window is
	// refused once the clock passes it.
	if _, err := store.Put(ctx, "k", BytesBody(nil)); err == nil {
		t.Fatal("probe under a fault answered")
	}
	if err := store.Wait(ctx, ClassWrite, time.Millisecond, 10*time.Second); !errors.Is(err, ErrStorageOpen) {
		t.Fatalf("past the limit: %v", err)
	}
	clock.Step(0)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Wait(cctx, ClassWrite, time.Millisecond, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// TestGetDeadlineBoundsTheCallNotTheBody: a Get whose headers arrive in
// time serves a body that takes longer than the deadline to read.
func TestGetDeadlineBoundsTheCallNotTheBody(t *testing.T) {
	ctx := context.Background()
	slow := &slowBodyStore{Store: NewMemStore(), hold: 60 * time.Millisecond}
	store := NewBreakerStore(BreakerOptions{Store: slow, Timeout: 20 * time.Millisecond})
	if _, err := store.Put(ctx, "k", BytesBody([]byte("body"))); err != nil {
		t.Fatal(err)
	}
	rc, _, err := store.Get(ctx, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	if err != nil || string(data) != "body" {
		t.Fatalf("body after the deadline: %q %v", data, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	// A Get whose context is already spent reports the deadline.
	slow.hold = 0
	spent, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, _, err := store.Get(spent, "k", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("spent context: %v", err)
	}
}

// slowBodyStore answers a Get at once with a body that holds each read
// for hold, the shape of a large object on a slow link.
type slowBodyStore struct {
	Store
	hold time.Duration
}

func (s *slowBodyStore) Get(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, Object{}, err
	}
	rc, o, err := s.Store.Get(ctx, key, ifNoneMatch)
	if err != nil {
		return nil, o, err
	}
	return &heldReader{ReadCloser: rc, hold: s.hold}, o, nil
}

type heldReader struct {
	io.ReadCloser
	hold time.Duration
}

func (h *heldReader) Read(p []byte) (int, error) {
	time.Sleep(h.hold)
	return h.ReadCloser.Read(p)
}

// TestReadIndexReportsACorruptObjectAsIntegrity: an index object that
// does not parse, or carries another sequence, is an IntegrityError
// naming the key, counted once each.
func TestReadIndexReportsACorruptObjectAsIntegrity(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	reg := pkgmetrics.NewRegistry()
	l := New(Options{Store: store, Metrics: metrics.Register(reg)})
	createRepo(t, l, repoA)
	key := l.key(repoA, IndexKey(0))
	if _, err := store.Put(ctx, key, BytesBody([]byte("{not json"))); err != nil {
		t.Fatal(err)
	}
	_, err := l.ReadIndex(ctx, repoA, 0)
	var ie *IntegrityError
	if !errors.As(err, &ie) || ie.Key != key || !strings.Contains(err.Error(), key) {
		t.Fatalf("unparsable index: %v", err)
	}
	base := createRepo(t, l, repoB)
	c, err := l.Commit(ctx, repoB, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := EncodeIndex(c.Index)
	if _, err := store.Put(ctx, key, BytesBody(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadIndex(ctx, repoA, 0); !errors.As(err, &ie) || errors.Unwrap(err) == nil {
		t.Fatalf("index of another sequence: %v", err)
	}
	var text bytes.Buffer
	reg.WritePrometheus(&text)
	if !strings.Contains(text.String(), "origo_log_integrity_errors_total 2") {
		t.Fatalf("metrics:\n%s", text.String())
	}
	if l.Breakers() != nil {
		t.Fatal("a log over a plain store reports breakers")
	}
	if wrapped := New(Options{Store: NewBreakerStore(BreakerOptions{Store: store})}); wrapped.Breakers() == nil {
		t.Fatal("a log over the wrapper reports none")
	}
}
