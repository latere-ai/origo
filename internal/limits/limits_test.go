// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package limits

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/wal"
)

const testRepo = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

// clock is a hand-wound clock, so the bucket table's minute and its
// idle window pass without the test waiting.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// env is a Limits over its own registry, so a test reads the refusal
// counter it produced.
type env struct {
	*Limits
	reg *pkgmetrics.Registry
	clk *clock
}

func newEnv(t *testing.T, o Options) *env {
	t.Helper()
	clk := newClock()
	reg := pkgmetrics.NewRegistry()
	o.Metrics = metrics.Register(reg)
	o.Logger = slog.New(slog.DiscardHandler)
	if o.Now == nil {
		o.Now = clk.Now
	}
	return &env{Limits: New(o), reg: reg, clk: clk}
}

// refused reads origo_rate_limited_total for one limit.
func (e *env) refused(limit string) float64 {
	var out strings.Builder
	e.reg.WritePrometheus(&out)
	want := `origo_rate_limited_total{limit="` + limit + `"}`
	for line := range strings.SplitSeq(out.String(), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok || name != want {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return -1
		}
		return v
	}
	return -1
}

// TestPerSubjectTokenBucket is spec 012's request rate: 600 requests a
// minute per effective subject with a burst of 600, the 601st refused
// with 429, Retry-After in whole seconds, and details.limit "subject",
// while a second subject is untouched.
func TestPerSubjectTokenBucket(t *testing.T) {
	e := newEnv(t, Options{})
	if RequestsPerMinute != 600 || Burst != 600 {
		t.Fatalf("the spec's rate is 600 a minute, burst 600; the code has %d and %d", RequestsPerMinute, Burst)
	}
	h := e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for i := range Burst {
		if code, _, _ := call(t, h, "alice"); code != http.StatusNoContent {
			t.Fatalf("request %d of the burst: %d", i+1, code)
		}
	}
	code, env, header := call(t, h, "alice")
	if code != http.StatusTooManyRequests {
		t.Fatalf("the 601st request: %d", code)
	}
	if env.Code != contract.CodeRateLimited || env.Message != contract.Sentence(contract.CodeRateLimited) {
		t.Errorf("envelope: %+v", env)
	}
	if env.Details["limit"] != LimitSubject {
		t.Errorf("details.limit: %+v", env.Details)
	}
	after := header.Get("Retry-After")
	if n, err := strconv.Atoi(after); err != nil || n < 1 {
		t.Errorf("Retry-After %q", after)
	}
	if env.Details["retry_after"] != float64(1) {
		t.Errorf("details.retry_after: %+v", env.Details)
	}
	if got := e.refused(LimitSubject); got != 1 {
		t.Errorf("origo_rate_limited_total{limit=subject} is %v, want 1", got)
	}
	// A second subject has a bucket of its own.
	if code, _, _ := call(t, h, "bob"); code != http.StatusNoContent {
		t.Errorf("a second subject: %d", code)
	}
	// A minute later the bucket is full again.
	e.clk.Advance(time.Minute)
	if code, _, _ := call(t, h, "alice"); code != http.StatusNoContent {
		t.Errorf("after a minute: %d", code)
	}
}

// TestIdleBucketsAreEvicted holds the table to the active subjects: a
// bucket untouched for ten minutes is gone.
func TestIdleBucketsAreEvicted(t *testing.T) {
	e := newEnv(t, Options{})
	if IdleBucket != 10*time.Minute {
		t.Fatalf("the spec evicts after 10 minutes; the code waits %s", IdleBucket)
	}
	h := e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	call(t, h, "alice")
	call(t, h, "bob")
	if n := e.Subjects(); n != 2 {
		t.Fatalf("%d buckets, want 2", n)
	}
	// Nine minutes on, bob is still calling and alice is not.
	e.clk.Advance(9 * time.Minute)
	call(t, h, "bob")
	if n := e.Subjects(); n != 2 {
		t.Fatalf("%d buckets after nine minutes, want 2", n)
	}
	e.clk.Advance(time.Minute + time.Second)
	if n := e.Subjects(); n != 1 {
		t.Fatalf("%d buckets after the idle window, want bob alone", n)
	}
	call(t, h, "bob")
	if n := e.Subjects(); n != 1 {
		t.Fatalf("%d buckets, want bob alone", n)
	}
}

// call sends one request through the middleware as the subject.
func call(t *testing.T, h http.Handler, subject string) (int, httpjson.Error, http.Header) {
	t.Helper()
	r := httptest.NewRequest("GET", "/r/"+testRepo+".git/info/refs", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Subject: subject}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var body struct {
		Error httpjson.Error `json:"error"`
	}
	if rec.Code >= 400 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, body.Error, rec.Header()
}

// TestSubprocessCap is spec 012's subprocess semaphore: with two slots
// a third caller waits and is then refused 429 rate_limited with
// details.limit "subprocesses", and a freed slot admits the next
// caller at once.
func TestSubprocessCap(t *testing.T) {
	if SlotWait != 5*time.Second {
		t.Fatalf("the spec waits 5 seconds for a slot; the code waits %s", SlotWait)
	}
	// The wait is the spec's 5 seconds on a node; the test lowers it so
	// the refusal is proved without the test sleeping for it.
	e := newEnv(t, Options{MaxGitProcs: 2, SlotWait: 20 * time.Millisecond, Now: time.Now})
	if n := e.Slots().Size(); n != 2 {
		t.Fatalf("%d slots, want 2", n)
	}
	first, ok := e.Slot(httptest.NewRecorder(), httptest.NewRequest("POST", "/a", nil))
	if !ok {
		t.Fatal("the first clone got no slot")
	}
	second, ok := e.Slot(httptest.NewRecorder(), httptest.NewRequest("POST", "/b", nil))
	if !ok {
		t.Fatal("the second clone got no slot")
	}
	if held := e.Slots().Held(); held != 2 {
		t.Fatalf("%d slots held, want 2", held)
	}
	rec := httptest.NewRecorder()
	started := time.Now()
	if _, ok := e.Slot(rec, httptest.NewRequest("POST", "/c", nil)); ok {
		t.Fatal("the third clone got a slot")
	}
	if waited := time.Since(started); waited < 20*time.Millisecond {
		t.Errorf("the third clone waited %s, less than the wait", waited)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the third clone: %d", rec.Code)
	}
	var body struct {
		Error httpjson.Error `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != contract.CodeRateLimited || body.Error.Details["limit"] != LimitSubprocesses {
		t.Errorf("envelope: %+v", body.Error)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if got := e.refused(LimitSubprocesses); got != 1 {
		t.Errorf("origo_rate_limited_total{limit=subprocesses} is %v, want 1", got)
	}
	// A freed slot admits the next caller; releasing twice frees one.
	first()
	first()
	third, ok := e.Slot(httptest.NewRecorder(), httptest.NewRequest("POST", "/c", nil))
	if !ok {
		t.Fatal("a freed slot admitted nobody")
	}
	third()
	second()
	if held := e.Slots().Held(); held != 0 {
		t.Fatalf("%d slots held after every release, want 0", held)
	}
}

// TestSemaphoreWaitsAndGivesUpOnTheContext covers the two other ways an
// acquire ends: a slot freed while a caller waits, and a cancelled
// request. A semaphore with no cap grants at once, which is a node that
// configures none.
func TestSemaphoreWaitsAndGivesUpOnTheContext(t *testing.T) {
	s := NewSemaphore(1)
	held, ok := s.Acquire(context.Background(), time.Second)
	if !ok {
		t.Fatal("the first acquire failed")
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		held()
	}()
	release, ok := s.Acquire(context.Background(), 5*time.Second)
	if !ok {
		t.Fatal("a slot freed while the caller waited was not taken")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := s.Acquire(ctx, time.Minute); ok {
		t.Error("a cancelled request took a slot")
	}
	release()

	uncapped := NewSemaphore(0)
	if _, ok := uncapped.Acquire(context.Background(), 0); !ok {
		t.Error("an uncapped semaphore refused")
	}
	if uncapped.Size() != 0 || uncapped.Held() != 0 {
		t.Error("an uncapped semaphore counts slots")
	}
	var nilSem *Semaphore
	if _, ok := nilSem.Acquire(context.Background(), 0); !ok || nilSem.Size() != 0 || nilSem.Held() != 0 {
		t.Error("a nil semaphore refused")
	}
}

// TestLFSBytesAreReusedForTheTTL is the 60 second cache spec 012 puts
// in front of the lfs/ listing, so a push does not list the prefix.
func TestLFSBytesAreReusedForTheTTL(t *testing.T) {
	if LFSTTL != 60*time.Second {
		t.Fatalf("the spec caches for 60 seconds; the code caches for %s", LFSTTL)
	}
	store := wal.NewMemStore()
	log := wal.New(wal.Options{Store: store, Prefix: "origo/"})
	clk := newClock()
	c := NewLFSBytes(log, LFSTTL, clk.Now)
	var lists int
	c.lists = func() { lists++ }
	put := func(key string, size int) {
		if _, err := store.Put(context.Background(), log.RepoPrefix(testRepo)+key, wal.BytesBody(make([]byte, size))); err != nil {
			t.Fatal(err)
		}
	}
	put("lfs/aa", 100)
	put("lfs/bb", 200)
	// A marker is under lfs/verified/, which the delimiter groups into a
	// prefix, so it is not summed.
	put("lfs/verified/aa", 50)

	for range 3 {
		n, err := c.Bytes(context.Background(), testRepo)
		if err != nil || n != 300 {
			t.Fatalf("sum %d, %v", n, err)
		}
	}
	if lists != 1 {
		t.Fatalf("%d listings inside the TTL, want 1", lists)
	}
	put("lfs/cc", 400)
	if n, _ := c.Bytes(context.Background(), testRepo); n != 300 {
		t.Errorf("the held sum was not reused: %d", n)
	}
	clk.Advance(LFSTTL + time.Second)
	if n, _ := c.Bytes(context.Background(), testRepo); n != 700 {
		t.Errorf("after the TTL the sum is %d, want 700", n)
	}
	if lists != 2 {
		t.Fatalf("%d listings, want 2", lists)
	}
	// Invalidate lists again at once.
	c.Invalidate(testRepo)
	if n, _ := c.Bytes(context.Background(), testRepo); n != 700 || lists != 3 {
		t.Errorf("after Invalidate: %d bytes, %d listings", n, lists)
	}
	// A cache with no log is a node with no LFS surface.
	if n, err := NewLFSBytes(nil, 0, nil).Bytes(context.Background(), testRepo); n != 0 || err != nil {
		t.Errorf("no log: %d %v", n, err)
	}
	var none *LFSBytes
	none.Invalidate(testRepo)
	if n, err := none.Bytes(context.Background(), testRepo); n != 0 || err != nil {
		t.Errorf("nil cache: %d %v", n, err)
	}
}

// TestLFSBytesWalksEveryPage sums a listing longer than one page, the
// markers grouped into a prefix, and reports a store failure.
func TestLFSBytesWalksEveryPage(t *testing.T) {
	store := wal.NewMemStore()
	log := wal.New(wal.Options{Store: store, Prefix: "origo/"})
	for i := range 1200 {
		key := log.RepoPrefix(testRepo) + "lfs/" + strconv.Itoa(100000+i)
		if _, err := store.Put(context.Background(), key, wal.BytesBody(make([]byte, 1))); err != nil {
			t.Fatal(err)
		}
	}
	c := NewLFSBytes(log, LFSTTL, nil)
	n, err := c.Bytes(context.Background(), testRepo)
	if err != nil || n != 1200 {
		t.Fatalf("sum %d, %v", n, err)
	}
	store.SetFault(func(op, _ string) error {
		if op == "List" {
			return wal.ErrNotFound
		}
		return nil
	})
	c.Invalidate(testRepo)
	if _, err := c.Bytes(context.Background(), testRepo); err == nil {
		t.Error("a failed listing was reported as a sum")
	}
}

// TestMeasureAddsTheLogTheObjectsAndTheWrite is the repository size
// rule: what the log holds plus the bytes under lfs/ plus what the
// write adds, against the authorizer's figure, with the default
// standing in when the authorizer names none.
func TestMeasureAddsTheLogTheObjectsAndTheWrite(t *testing.T) {
	store := wal.NewMemStore()
	log := wal.New(wal.Options{Store: store, Prefix: "origo/"})
	if _, err := store.Put(context.Background(), log.RepoPrefix(testRepo)+"lfs/aa", wal.BytesBody(make([]byte, 300))); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Log: log, Logger: slog.New(slog.DiscardHandler)})
	q, err := l.Measure(context.Background(), testRepo, 500, 200, 1000)
	if err != nil || q.Bytes != 1000 || q.Max != 1000 || q.Over() {
		t.Fatalf("at the limit: %+v %v", q, err)
	}
	q, err = l.Measure(context.Background(), testRepo, 500, 201, 1000)
	if err != nil || !q.Over() || q.Bytes != 1001 {
		t.Fatalf("over the limit: %+v %v", q, err)
	}
	if q, _ := l.Measure(context.Background(), testRepo, 0, 0, 0); q.Max != DefaultQuotaBytes {
		t.Errorf("no figure from the authorizer: %d, want the default", q.Max)
	}
	if DefaultQuotaBytes != auth.DefaultQuotaBytes {
		t.Errorf("the default quota is %d here and %d in auth", DefaultQuotaBytes, auth.DefaultQuotaBytes)
	}
	if _, err := l.Measure(context.Background(), "no-such-repo", 0, 0, 0); err != nil {
		t.Errorf("an empty prefix: %v", err)
	}
}

// TestNilLimitsEnforceNothing is the seam a handler's own test uses: a
// nil *Limits grants every slot, lists nothing, and refuses nobody.
func TestNilLimitsEnforceNothing(t *testing.T) {
	var l *Limits
	release, ok := l.Slot(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !ok {
		t.Fatal("a nil Limits refused a slot")
	}
	release()
	if l.Slots() != nil {
		t.Error("a nil Limits has a semaphore")
	}
	if n, err := l.LFSBytes(context.Background(), testRepo); n != 0 || err != nil {
		t.Errorf("lfs bytes: %d %v", n, err)
	}
	if l.MaxPush() != MaxPushBytes || l.Subjects() != 0 {
		t.Error("a nil Limits carries no defaults")
	}
	l.Refused(LimitSubject)
	rec := httptest.NewRecorder()
	l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).
		ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("a nil Limits refused a request: %d", rec.Code)
	}
	var b *Buckets
	if ok, _ := b.Allow("alice"); !ok || b.Len() != 0 {
		t.Error("a nil bucket table refused")
	}
}

// TestRetryAfterIsWholeSecondsAtLeastOne rounds a wait up, so a client
// never reads a Retry-After of zero.
func TestRetryAfterIsWholeSecondsAtLeastOne(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want int
	}{{0, 1}, {time.Millisecond, 1}, {time.Second, 1}, {1500 * time.Millisecond, 2}, {5 * time.Second, 5}} {
		if got := RetryAfterSeconds(c.in); got != c.want {
			t.Errorf("RetryAfterSeconds(%s) = %d, want %d", c.in, got, c.want)
		}
	}
	if itoa(0) != "0" || itoa(1205) != "1205" {
		t.Error("itoa")
	}
}

// TestOptionsTakeTheSpecsValues proves every knob falls back to the
// spec's figure and that a lowered one is honoured.
func TestOptionsTakeTheSpecsValues(t *testing.T) {
	l := New(Options{})
	if l.Slots().Size() != DefaultMaxGitProcs || l.MaxPush() != MaxPushBytes {
		t.Errorf("defaults: %d slots, %d bytes", l.Slots().Size(), l.MaxPush())
	}
	l = New(Options{MaxGitProcs: 3, MaxPushBytes: 7, PerMinute: 1, Burst: 1, Idle: time.Minute, LFSTTL: time.Second, SlotWait: time.Millisecond})
	if l.Slots().Size() != 3 || l.MaxPush() != 7 {
		t.Errorf("lowered: %d slots, %d bytes", l.Slots().Size(), l.MaxPush())
	}
	if ok, _ := l.buckets.Allow("alice"); !ok {
		t.Error("the first request was refused")
	}
	ok, retry := l.buckets.Allow("alice")
	if ok || retry <= 0 {
		t.Errorf("the second request: %v %s", ok, retry)
	}
	// A rate of zero or less lets everything through.
	off := NewBuckets(0, 0, 0, nil)
	for range 10 {
		if ok, _ := off.Allow("alice"); !ok {
			t.Fatal("a rate of zero refused")
		}
	}
	if off.Len() != 0 {
		t.Error("a rate of zero built a table")
	}
}

// TestTheRateCanBeTurnedOff is ORIGO_REQUESTS_PER_MINUTE=0, which the
// kind overlay sets: a negative figure in the options leaves every
// subject unbucketed, and an unset one takes the spec's 600 for both
// the rate and the burst.
func TestTheRateCanBeTurnedOff(t *testing.T) {
	off := New(Options{PerMinute: -1, Logger: slog.New(slog.DiscardHandler)})
	h := off.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for range Burst + 10 {
		if code, _, _ := call(t, h, "alice"); code != http.StatusNoContent {
			t.Fatalf("a request past the burst with the limit off: %d", code)
		}
	}
	on := New(Options{Logger: slog.New(slog.DiscardHandler)})
	if on.buckets.perMinute != RequestsPerMinute || on.buckets.burst != float64(RequestsPerMinute) {
		t.Errorf("the default rate is %d a minute with a burst of %v", on.buckets.perMinute, on.buckets.burst)
	}
}
