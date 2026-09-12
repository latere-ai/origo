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
	if got := header.Get(contract.HeaderRateLimit); got != strconv.Itoa(RequestsPerMinute) {
		t.Errorf("RateLimit-Limit on the refusal is %q, want %d", got, RequestsPerMinute)
	}
	if got := e.refused(LimitSubject); got != 1 {
		t.Errorf("origo_rate_limited_total{limit=subject} is %v, want 1", got)
	}
	// A second subject has a bucket of its own, and reads the figure in
	// force off its own response (spec 021 sends one request more than
	// the header names).
	code, _, header = call(t, h, "bob")
	if code != http.StatusNoContent {
		t.Errorf("a second subject: %d", code)
	}
	if got := header.Get(contract.HeaderRateLimit); got != strconv.Itoa(RequestsPerMinute) {
		t.Errorf("RateLimit-Limit on an admitted response is %q, want %d", got, RequestsPerMinute)
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
	if a := b.Allow("alice"); !a.OK || b.Len() != 0 {
		t.Error("a nil bucket table refused")
	}
	release, wait, ok := l.Acquire(context.Background())
	if !ok || wait != 0 {
		t.Errorf("a nil Limits refused an SSH slot: %v %v", wait, ok)
	}
	release()
	if ok, retry := l.Take(context.Background(), "alice"); !ok || retry != 0 {
		t.Errorf("a nil Limits refused an SSH session: %v %v", ok, retry)
	}
}

// TestSSHTakesTheSameSlotAndTheSameToken is spec 024's rule that the SSH
// listener meets spec 012's bounds through the same table the HTTP
// surface does: one session costs one subprocess slot and one token of
// the subject's bucket, and a refusal names the wait rather than
// rendering an envelope.
func TestSSHTakesTheSameSlotAndTheSameToken(t *testing.T) {
	l := New(Options{MaxGitProcs: 1, SlotWait: 10 * time.Millisecond, PerMinute: 1, Burst: 1})
	release, _, ok := l.Acquire(context.Background())
	if !ok {
		t.Fatal("the first session got no slot")
	}
	if _, wait, ok := l.Acquire(context.Background()); ok || wait != 10*time.Millisecond {
		t.Errorf("the second session took the only slot: %v %v", wait, ok)
	}
	release()
	if _, _, ok := l.Acquire(context.Background()); !ok {
		t.Error("the slot was not released")
	}
	if ok, _ := l.Take(context.Background(), "alice"); !ok {
		t.Error("the first session spent no token")
	}
	ok, retry := l.Take(context.Background(), "alice")
	if ok || retry <= 0 {
		t.Errorf("a subject over its rate was admitted: %v %v", ok, retry)
	}
	if ok, _ := l.Take(context.Background(), "bob"); !ok {
		t.Error("another subject's bucket was spent")
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
	if a := l.buckets.Allow("alice"); !a.OK {
		t.Error("the first request was refused")
	}
	a := l.buckets.Allow("alice")
	if a.OK || a.Retry <= 0 {
		t.Errorf("the second request: %v %s", a.OK, a.Retry)
	}
	// A rate of zero or less lets everything through.
	off := NewBuckets(0, 0, 0, nil)
	for range 10 {
		if a := off.Allow("alice"); !a.OK {
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
		code, _, header := call(t, h, "alice")
		if code != http.StatusNoContent {
			t.Fatalf("a request past the burst with the limit off: %d", code)
		}
		if got := header.Get(contract.HeaderRateLimit); got != "" {
			t.Fatalf("RateLimit-Limit %q with the limit off", got)
		}
	}
	on := New(Options{Logger: slog.New(slog.DiscardHandler)})
	if a := on.buckets.Allow("defaults"); a.PerMinute != RequestsPerMinute || a.Remaining != RequestsPerMinute-1 {
		t.Errorf("default quota = %+v", a)
	}
}

// TestASubjectTheAuthorizerNamesARateForIsBucketedAtIt is spec 012's
// item, built under spec 020: the authorizer's optional
// requests_per_minute is the rate and the burst of that subject's
// bucket, and every other subject stays on the node's own figure.
func TestASubjectTheAuthorizerNamesARateForIsBucketedAtIt(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	l := New(Options{PerMinute: 2, Burst: 2, Now: clock, Logger: slog.New(slog.DiscardHandler)})

	// Without a figure the subject is bucketed at the node's rate.
	if l.SubjectRate("bot") != 2 {
		t.Fatalf("the default rate is %d", l.SubjectRate("bot"))
	}
	// The figure raises the rate and fills the bucket to the new depth,
	// so a tool that was granted a higher rate is not held to the one
	// its bucket was created with.
	if a := l.buckets.Allow("bot"); !a.OK {
		t.Fatal("the first request was refused")
	}
	l.SetSubjectRate("bot", 10)
	if l.SubjectRate("bot") != 10 {
		t.Fatalf("the named rate is %d", l.SubjectRate("bot"))
	}
	for i := range 10 {
		if a := l.buckets.Allow("bot"); !a.OK {
			t.Fatalf("request %d of a subject bucketed at 10 was refused", i)
		}
	}
	if a := l.buckets.Allow("bot"); a.OK || a.Retry <= 0 {
		t.Errorf("the eleventh request: %v %s", a.OK, a.Retry)
	}
	// The refill is the subject's own rate: six seconds buys one token
	// at 10 a minute.
	now = now.Add(6 * time.Second)
	if a := l.buckets.Allow("bot"); !a.OK {
		t.Error("the bucket did not refill at the named rate")
	}
	// Another subject keeps the node's figure.
	if l.SubjectRate("alice") != 2 {
		t.Fatalf("another subject is at %d", l.SubjectRate("alice"))
	}
	for range 2 {
		if a := l.buckets.Allow("alice"); !a.OK {
			t.Fatal("a request inside the node's rate was refused")
		}
	}
	if a := l.buckets.Allow("alice"); a.OK {
		t.Error("a request past the node's rate was admitted")
	}
	// A lowered figure holds the subject to it at once, and a figure of
	// zero or less leaves the subject where it is.
	l.SetSubjectRate("bot", 1)
	if l.SubjectRate("bot") != 1 {
		t.Fatalf("the lowered rate is %d", l.SubjectRate("bot"))
	}
	l.SetSubjectRate("bot", 0)
	if l.SubjectRate("bot") != 1 {
		t.Fatalf("a figure of zero changed the rate to %d", l.SubjectRate("bot"))
	}
	// A subject the authorizer names a figure for before its first
	// request is bucketed at it from that request on.
	l.SetSubjectRate("fresh", 5)
	if l.SubjectRate("fresh") != 5 {
		t.Fatalf("a fresh subject is at %d", l.SubjectRate("fresh"))
	}
	var nilLimits *Limits
	nilLimits.SetSubjectRate("bot", 5)
	if nilLimits.SubjectRate("bot") != 0 {
		t.Error("a nil Limits carries a rate")
	}
	var nilBuckets *Buckets
	nilBuckets.SetRate("bot", 5)
	if nilBuckets.Rate("bot") != 0 {
		t.Error("a nil table carries a rate")
	}
}

// TestRateLimitHeadersAreTheSubjectsFigures is spec 012's header rows:
// the figure RateLimit-Limit names is the one in force for the effective
// subject of the response, not the node's variable. A subject the
// authorizer named a rate for reads that rate, a subject it named none
// for reads the node's, and the header is absent when the limit is off
// even for a subject that carries a recorded rate. Reporting the node's
// figure to a subject on an override is what made spec 021's
// rate_limited case send 2401 requests against a budget of 6000 in the
// v0.1.2 release run.
func TestRateLimitHeadersAreTheSubjectsFigures(t *testing.T) {
	e := newEnv(t, Options{PerMinute: 60, Burst: 60})
	h := e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	// A subject with no override reads the node's figure, and
	// RateLimit-Remaining falls by one a request from the depth that
	// figure gave the bucket.
	for i := range 5 {
		code, _, header := call(t, h, "alice")
		if code != http.StatusNoContent || header.Get(contract.HeaderRateLimit) != "60" {
			t.Fatalf("without an override: %d, %s %q", code, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
		}
		if want := strconv.Itoa(59 - i); header.Get(contract.HeaderRateRemaining) != want {
			t.Fatalf("request %d: %s %q, want %s", i+1, contract.HeaderRateRemaining, header.Get(contract.HeaderRateRemaining), want)
		}
	}
	var header http.Header
	var code int

	// Once the authorizer has named a rate the header reports it. The
	// handler calls SetSubjectRate after this middleware has answered,
	// so the override is read from the next response on, which is the
	// window spec 012 records.
	e.SetSubjectRate("bot", 6000)
	code, _, header = call(t, h, "bot")
	if code != http.StatusNoContent || header.Get(contract.HeaderRateLimit) != "6000" {
		t.Fatalf("with an override: %d, %s %q", code, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
	}
	// Raising the rate raised the depth, so what is left is measured
	// against the figure beside it.
	if header.Get(contract.HeaderRateRemaining) != "5999" {
		t.Errorf("%s under the override is %q", contract.HeaderRateRemaining, header.Get(contract.HeaderRateRemaining))
	}
	// The other subject is untouched by it.
	if _, _, header = call(t, h, "alice"); header.Get(contract.HeaderRateLimit) != "60" {
		t.Errorf("a second subject reads %q", header.Get(contract.HeaderRateLimit))
	}
	// A lowered override is reported the same way.
	e.SetSubjectRate("bot", 10)
	if _, _, header = call(t, h, "bot"); header.Get(contract.HeaderRateLimit) != "10" {
		t.Errorf("after a lowered override: %q", header.Get(contract.HeaderRateLimit))
	}

	// A refusal carries the subject's figure too: a client that meets
	// the limit reads the same figure it was measured against.
	e = newEnv(t, Options{PerMinute: 2, Burst: 2})
	h = e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	e.SetSubjectRate("bot", 3)
	for i := range 3 {
		if code, _, _ := call(t, h, "bot"); code != http.StatusNoContent {
			t.Fatalf("request %d of the override's burst: %d", i+1, code)
		}
	}
	code, _, header = call(t, h, "bot")
	if code != http.StatusTooManyRequests || header.Get(contract.HeaderRateLimit) != "3" {
		t.Fatalf("the refusal: %d, %s %q", code, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
	}
	// A refused request has nothing left, which is what it reports.
	if header.Get(contract.HeaderRateRemaining) != "0" {
		t.Errorf("%s on the refusal is %q", contract.HeaderRateRemaining, header.Get(contract.HeaderRateRemaining))
	}

	// With the limit off there is no header, and an override recorded
	// on a table that admits everything does not bring one back.
	off := newEnv(t, Options{PerMinute: -1})
	h = off.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	off.SetSubjectRate("bot", 6000)
	for range 3 {
		code, _, header = call(t, h, "bot")
		if code != http.StatusNoContent || header.Get(contract.HeaderRateLimit) != "" {
			t.Fatalf("with the limit off: %d, %s %q", code, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
		}
		if header.Get(contract.HeaderRateRemaining) != "" {
			t.Fatalf("%s %q with the limit off", contract.HeaderRateRemaining, header.Get(contract.HeaderRateRemaining))
		}
	}
}

// TestAnonymousShareOneBucket is the denial-of-service answer of spec 027.
// An anonymous caller has no subject, so it has no bucket under the
// per-subject rule; every one of them shares one bucket per node at
// AnonymousRequestsPerMinute, and that bucket cannot draw from an
// authenticated subject's.
func TestAnonymousShareOneBucket(t *testing.T) {
	const anonRate = 4
	e := newEnv(t, Options{AnonymousPerMinute: anonRate})
	h := e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	// The anonymous subject is the empty string, and two callers with no
	// credential are one bucket: the second one pays for what the first
	// one spent.
	for i := range anonRate {
		if code, _, header := call(t, h, auth.AnonymousSubject); code != http.StatusNoContent {
			t.Fatalf("anonymous request %d: %d %v", i+1, code, header)
		}
	}
	code, env, header := call(t, h, auth.AnonymousSubject)
	if code != http.StatusTooManyRequests {
		t.Fatalf("the anonymous request past the rate: %d", code)
	}
	if env.Details["limit"] != LimitSubject {
		t.Errorf("details.limit: %+v", env.Details)
	}
	if got := header.Get(contract.HeaderRateLimit); got != "4" {
		t.Errorf("RateLimit-Limit = %q, want the anonymous rate", got)
	}

	// An authenticated subject is untouched by the exhausted anonymous
	// bucket, which is the property the shared bucket is chosen for.
	if code, _, _ := call(t, h, "alice"); code != http.StatusNoContent {
		t.Fatalf("a named subject after the anonymous bucket emptied: %d", code)
	}

	// And the anonymous bucket is not the named subject's, so alice
	// spending hers leaves the anonymous one where it was: still empty.
	if code, _, _ := call(t, h, auth.AnonymousSubject); code != http.StatusTooManyRequests {
		t.Errorf("the anonymous bucket refilled from a named subject's: %d", code)
	}
}

// TestAnonymousRateDefaults records the figure, so a change to it is a
// deliberate edit rather than a drift.
func TestAnonymousRateDefaults(t *testing.T) {
	if AnonymousRequestsPerMinute != 60 {
		t.Errorf("the anonymous rate is %d; spec 027 says 60", AnonymousRequestsPerMinute)
	}
	if AnonymousRequestsPerMinute >= RequestsPerMinute {
		t.Errorf("the anonymous rate %d is not below the per-subject rate %d",
			AnonymousRequestsPerMinute, RequestsPerMinute)
	}
	e := newEnv(t, Options{})
	if e.anonPerMinute != AnonymousRequestsPerMinute {
		t.Errorf("an unset AnonymousPerMinute gave %d, want the default", e.anonPerMinute)
	}
}

func TestSharedSemaphoreRejectsCanceledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release, ok := NewSemaphore(1).Acquire(ctx, time.Second)
	if ok || release != nil {
		t.Fatal("canceled caller acquired a subprocess slot")
	}
}

func TestAnonymousQuotaSurvivesIdleSweep(t *testing.T) {
	now := time.Unix(100, 0)
	b := NewBuckets(60, 60, time.Minute, func() time.Time { return now })
	b.SetRate("anonymous", 1)
	b.Allow("anonymous")
	now = now.Add(2 * time.Minute)
	b.SetRate("anonymous", 1)
	a := b.Allow("anonymous")
	if !a.OK || a.PerMinute != 1 || a.Remaining != 0 {
		t.Fatalf("anonymous quota reset: %+v", a)
	}
}
