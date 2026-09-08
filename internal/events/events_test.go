// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"context"
	"crypto/sha1" //nolint:gosec // UUID v5 is SHA-1 by definition (RFC 9562)
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"

	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/sink"
)

const (
	repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

// clock is a fake clock the tests advance.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// members is a fake live set: how long ago each node was heard, on the
// fake clock, so advancing the clock does not age a live node.
type members struct {
	c    *clock
	seen map[string]time.Duration
}

func (m members) LastHeard(node string) (time.Time, bool) {
	ago, ok := m.seen[node]
	return m.c.Now().Add(-ago), ok
}

// harness is one node's dispatcher over a store, with a sink and a
// log whose clock is the dispatcher's.
type harness struct {
	t     *testing.T
	store *wal.MemStore
	log   *wal.Log
	sink  *sink.Server
	clock *clock
	reg   *pkgmetrics.Registry
	d     *Dispatcher
}

func newHarness(t *testing.T, node string, opts ...func(*Options)) *harness {
	t.Helper()
	return newHarnessOn(t, wal.NewMemStore(), sink.New(t), newClock(), node, opts...)
}

func newHarnessOn(t *testing.T, store *wal.MemStore, s *sink.Server, c *clock, node string, opts ...func(*Options)) *harness {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	l := wal.New(wal.Options{Store: store, Now: c.Now, Logger: logger})
	reg := pkgmetrics.NewRegistry()
	o := Options{Log: l, Node: node, URL: s.URL(), Secret: s.Secret(), Now: c.Now, Metrics: metrics.Register(reg), Logger: logger, RepairInterval: 10 * time.Second, RepairUnheard: 5 * time.Second}
	for _, f := range opts {
		f(&o)
	}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, store: store, log: l, sink: s, clock: c, reg: reg, d: d}
}

func (h *harness) create(id, owner, slug string) *wal.Index {
	h.t.Helper()
	ix, err := h.log.CreateRepo(context.Background(), wal.Meta{ID: id, Owner: owner, Slug: slug}, "main")
	if err != nil {
		h.t.Fatal(err)
	}
	return ix
}

// push commits a push entry and returns the committed outcome; the
// entry given to Enqueue is built from it the way the receive path does.
func (h *harness) push(id string, base *wal.Index, e wal.Entry) *wal.Committed {
	h.t.Helper()
	if e.Kind == "" {
		e.Kind = wal.KindPush
	}
	if e.Subject == "" {
		e.Subject = "alice"
	}
	c, err := h.log.Commit(context.Background(), id, base, e, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func entryOf(c *wal.Committed, refs []wal.RefUpdate, forced map[string]bool) Entry {
	return Entry{Header: c.Header, Refs: refs, Forced: forced}
}

func mainUpdate(n int) []wal.RefUpdate {
	old := wal.ZeroSHA
	if n > 1 {
		old = sha(n - 1)
	}
	return []wal.RefUpdate{{Ref: "refs/heads/main", Old: old, New: sha(n)}}
}

func (h *harness) keys(prefix string) []string {
	var out []string
	for _, k := range h.store.Keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func (h *harness) counter(name string) uint64 { return h.reg.Counter(name, "").Value(nil) }

// uuid5 is RFC 9562's name-based UUID computed here, independent of the
// package, so the test proves the id and not the library.
func uuid5(ns, name string) string {
	raw, _ := hex.DecodeString(strings.ReplaceAll(ns, "-", ""))
	sum := sha1.Sum(append(raw, []byte(name)...)) //nolint:gosec // RFC 9562 v5
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// TestEventIDIsDeterministic: the enqueue and the repair of one entry
// produce one id, two entries produce two, and the id equals the UUID
// v5 the test computes from the namespace and <repo>:<seq>.
func TestEventIDIsDeterministic(t *testing.T) {
	if got, want := PushID(repoA, 7), uuid5(Namespace, repoA+":000000000007"); got != want {
		t.Fatalf("PushID = %s, want %s", got, want)
	}
	if PushID(repoA, 7) == PushID(repoA, 8) || PushID(repoA, 7) == PushID(repoB, 7) {
		t.Fatal("two entries share an id")
	}
	at := time.Date(2026, 9, 8, 10, 0, 0, 123, time.FixedZone("x", 3600))
	if got, want := EmitID(repoA, "frozen", at), uuid5(Namespace, repoA+":frozen:2026-09-08T09:00:00Z"); got != want {
		t.Fatalf("EmitID = %s, want %s", got, want)
	}
	// The enqueue and the repair sweep write the same id for one entry.
	h := newHarness(t, "n1")
	ix := h.create(repoA, "acme", "app")
	c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(1)})
	if err := h.d.Enqueue(context.Background(), repoA, entryOf(c, mainUpdate(1), nil)); err != nil {
		t.Fatal(err)
	}
	obj, env, _, err := h.d.read(context.Background(), h.d.pushKey(repoA, 1))
	if err != nil || env.ID != PushID(repoA, 1) || obj.Attempts != 0 || !obj.NextAt.Equal(h.clock.Now()) {
		t.Fatalf("enqueued %+v %+v %v", obj, env, err)
	}
	if err := h.store.Delete(context.Background(), h.d.pushKey(repoA, 1)); err != nil {
		t.Fatal(err)
	}
	h.d.unschedule(h.d.pushKey(repoA, 1))
	h.clock.Advance(2 * time.Minute)
	if err := h.d.repairRepo(context.Background(), repoA, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	_, env2, _, err := h.d.read(context.Background(), h.d.pushKey(repoA, 1))
	if err != nil || env2.ID != env.ID {
		t.Fatalf("repaired %+v %v, enqueued %s", env2, err, env.ID)
	}
	var p Push
	obj, _, _, _ = h.d.read(context.Background(), h.d.pushKey(repoA, 1))
	_ = json.Unmarshal(obj.Event, &p)
	if p.Updates[0].Ref != "refs/heads/main" || p.Updates[0].After != sha(1) || p.Owner != "acme" || p.Slug != "app" || p.Seq != 1 {
		t.Fatalf("repaired payload %+v", p)
	}
}

// TestThousandPushesOneEventEach: every push in a 1 000 push load
// produces exactly one delivered event with a valid signature,
// Origo-Event: push, and Origo-Delivery equal to the body's id.
func TestThousandPushesOneEventEach(t *testing.T) {
	s := sink.New(t)
	h := newHarnessOn(t, wal.NewMemStore(), s, &clock{t: time.Now()}, "n1")
	h.d.now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()
	ix := h.create(repoA, "acme", "app")
	const n = 1000
	for i := 1; i <= n; i++ {
		c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(i)})
		if err := h.d.Enqueue(ctx, repoA, entryOf(c, mainUpdate(i), nil)); err != nil {
			t.Fatal(err)
		}
		ix = c.Index
	}
	got, ok := s.Wait(repoA, KindPush, n, 60*time.Second)
	if !ok {
		t.Fatalf("%d of %d delivered", len(got), n)
	}
	ids := map[string]bool{}
	for _, dl := range got {
		var p Push
		if err := json.Unmarshal(dl.Body, &p); err != nil {
			t.Fatal(err)
		}
		if !dl.Verified || dl.Kind != KindPush || dl.Headers.Get(HeaderEvent) != KindPush || dl.ID != p.ID || dl.ID != PushID(repoA, p.Seq) || p.Repo != repoA || p.Kind != KindPush {
			t.Fatalf("delivery %+v", dl)
		}
		if ids[p.ID] {
			t.Fatalf("id %s delivered twice", p.ID)
		}
		ids[p.ID] = true
	}
	if len(ids) != n {
		t.Fatalf("%d ids", len(ids))
	}
	// Nothing stays pending, the cursor is at the last push, and the
	// counter agrees with the sink.
	deadline := time.Now().Add(10 * time.Second)
	for len(h.d.Pending()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pending := h.keys(h.d.prefix + repoA + "/0"); len(pending) != 0 || len(h.d.Pending()) != 0 {
		t.Fatalf("pending: %v %v", pending, h.d.Pending())
	}
	if c, err := h.d.readCursor(ctx, repoA); err != nil || c.Seq != n {
		t.Fatalf("cursor %+v %v", c, err)
	}
	if h.counter("origo_events_delivered_total") != n || h.counter("origo_events_dead_total") != 0 {
		t.Fatalf("counters %d %d", h.counter("origo_events_delivered_total"), h.counter("origo_events_dead_total"))
	}
	// The journal names the repository once per push, flushed at stop.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	lines, err := h.d.readJournal(context.Background(), "n1", time.Now())
	if err != nil || len(lines) != n || lines[0] != repoA+" 1" || lines[n-1] != fmt.Sprintf("%s %d", repoA, n) {
		t.Fatalf("journal: %d lines, %v", len(lines), err)
	}
}

// TestRetryScheduleAndDeadLetter: a sink answering 500 sees retries at
// 1 s, 10 s, 1 min, 10 min, 1 h, and hourly with a fake clock, and a
// dead-letter object plus an increment of origo_events_dead_total 24
// hours after at.
func TestRetryScheduleAndDeadLetter(t *testing.T) {
	h := newHarness(t, "n1")
	h.sink.Fail(0, 500)
	ix := h.create(repoA, "acme", "app")
	c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(1)})
	ctx := context.Background()
	if err := h.d.Enqueue(ctx, repoA, entryOf(c, mainUpdate(1), nil)); err != nil {
		t.Fatal(err)
	}
	key := h.d.pushKey(repoA, 1)
	seen := func() int { return len(h.sink.Deliveries(repoA, KindPush)) }
	// The first attempt is due at once.
	next := h.d.deliverDue(ctx)
	if seen() != 1 || !next.Equal(h.clock.Now().Add(time.Second)) {
		t.Fatalf("first attempt: %d seen, next %v", seen(), next)
	}
	obj, _, _, _ := h.d.read(ctx, key)
	if obj.Attempts != 1 || !obj.NextAt.Equal(next) {
		t.Fatalf("object after the first failure: %+v", obj)
	}
	// Each delay of the schedule, then hourly: nothing before the due
	// time, one delivery at it.
	for i, delay := range []time.Duration{time.Second, 10 * time.Second, time.Minute, 10 * time.Minute, time.Hour, time.Hour, time.Hour} {
		before := seen()
		h.clock.Advance(delay - time.Millisecond)
		if h.d.deliverDue(ctx); seen() != before {
			t.Fatalf("attempt %d delivered %v early", i+2, delay)
		}
		h.clock.Advance(time.Millisecond)
		if next = h.d.deliverDue(ctx); seen() != before+1 {
			t.Fatalf("attempt %d not delivered at %v", i+2, delay)
		}
		want := time.Hour
		if i+2 <= len(Delays) {
			want = Delays[i+1]
		}
		if !next.Equal(h.clock.Now().Add(want)) {
			t.Fatalf("after attempt %d next in %v, want %v", i+2, next.Sub(h.clock.Now()), want)
		}
	}
	if h.counter("origo_events_dead_total") != 0 || len(h.keys(h.d.prefix+"dead/")) != 0 {
		t.Fatal("dead before the window")
	}
	// Past the window the next failure moves the object under dead/.
	h.clock.Advance(24 * time.Hour)
	before := seen()
	if next = h.d.deliverDue(ctx); seen() != before+1 || !next.IsZero() {
		t.Fatalf("attempt after the window: %d, next %v", seen()-before, next)
	}
	if _, err := h.store.Head(ctx, key); !errors.Is(err, wal.ErrNotFound) {
		t.Fatalf("pending object still there: %v", err)
	}
	dead := h.keys(h.d.prefix + "dead/")
	if len(dead) != 1 || dead[0] != h.d.deadKey(key) || h.counter("origo_events_dead_total") != 1 || len(h.d.Pending()) != 0 {
		t.Fatalf("dead: %v, counter %d, pending %v", dead, h.counter("origo_events_dead_total"), h.d.Pending())
	}
	obj, env, _, err := h.d.read(ctx, dead[0])
	if err != nil || obj.Attempts != 8 || env.ID != PushID(repoA, 1) {
		t.Fatalf("dead object %+v %+v %v", obj, env, err)
	}
	// A sink that recovers delivers a fresh event and advances the cursor.
	h.sink.Fail(0, 0)
	c2 := h.push(repoA, c.Index, wal.Entry{Refs: mainUpdate(2)})
	if err := h.d.Enqueue(ctx, repoA, entryOf(c2, mainUpdate(2), nil)); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	if cur, err := h.d.readCursor(ctx, repoA); err != nil || cur.Seq != 2 || h.counter("origo_events_delivered_total") != 1 {
		t.Fatalf("cursor %+v %v", cur, err)
	}
}

// TestEmitIsIdempotent: Emit called twice for one operation with one
// at writes one object and delivers one event; two operations of one
// kind with different at values write two.
func TestEmitIsIdempotent(t *testing.T) {
	h := newHarness(t, "n1")
	h.create(repoA, "acme", "app")
	ctx := context.Background()
	at := h.clock.Now()
	pusher := Pusher{Sub: "alice", Actor: "svc"}
	for range 2 {
		if err := h.d.Emit(ctx, repoA, "frozen", at, pusher, nil); err != nil {
			t.Fatal(err)
		}
	}
	id := EmitID(repoA, "frozen", at)
	if keys := h.keys(h.d.prefix + repoA + "/a-"); len(keys) != 1 || keys[0] != h.d.emitKey(repoA, id) {
		t.Fatalf("objects: %v", keys)
	}
	h.d.deliverDue(ctx)
	got := h.sink.Deliveries(repoA, "frozen")
	if len(got) != 1 || got[0].ID != id || !got[0].Verified || got[0].Headers.Get(HeaderEvent) != "frozen" {
		t.Fatalf("deliveries %+v", got)
	}
	var body map[string]any
	_ = json.Unmarshal(got[0].Body, &body)
	if body["id"] != id || body["kind"] != "frozen" || body["repo"] != repoA || body["owner"] != "acme" || body["slug"] != "app" || body["at"] != at.Format(time.RFC3339) || body["pusher"].(map[string]any)["actor"] != "svc" {
		t.Fatalf("payload %v", body)
	}
	// A third Emit after the delivery writes the key again, and the
	// loop deletes it by the cursor's delivered set without delivering.
	if err := h.d.Emit(ctx, repoA, "frozen", at, pusher, map[string]any{"extra": 1}); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	if len(h.sink.Deliveries(repoA, "frozen")) != 1 || len(h.keys(h.d.prefix+repoA+"/a-")) != 0 {
		t.Fatal("a repeated emit after delivery was delivered again")
	}
	if cur, _ := h.d.readCursor(ctx, repoA); len(cur.Delivered) != 1 || cur.Delivered[0].ID != id || cur.Seq != 0 {
		t.Fatalf("cursor %+v", cur)
	}
	// Two operations of one kind with different at values are two events
	// carrying their extra fields.
	for i := range 2 {
		if err := h.d.Emit(ctx, repoA, "renamed", at.Add(time.Duration(i)*time.Second), pusher, map[string]any{"to": map[string]any{"owner": "acme", "slug": fmt.Sprint("r", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	h.d.deliverDue(ctx)
	renamed := h.sink.Deliveries(repoA, "renamed")
	if len(renamed) != 2 || renamed[0].ID == renamed[1].ID || !strings.Contains(string(renamed[1].Body), `"r1"`) {
		t.Fatalf("renamed %+v", renamed)
	}
	// Delivered ids older than the window leave the set on the next rewrite.
	h.clock.Advance(Window + time.Hour)
	if err := h.d.Emit(ctx, repoA, "frozen", h.clock.Now(), pusher, nil); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	if cur, _ := h.d.readCursor(ctx, repoA); len(cur.Delivered) != 1 || cur.Delivered[0].ID == id {
		t.Fatalf("cursor after the window %+v", cur)
	}
	// A kind of push, or none, is refused; a nil dispatcher is a no-op.
	if err := h.d.Emit(ctx, repoA, KindPush, at, pusher, nil); err == nil {
		t.Fatal("push emitted")
	}
	if err := h.d.Emit(ctx, repoA, "", at, pusher, nil); err == nil {
		t.Fatal("empty kind emitted")
	}
	var none *Dispatcher
	if none.Enabled() || none.Emit(ctx, repoA, "frozen", at, pusher, nil) != nil || none.Enqueue(ctx, repoA, Entry{}) != nil {
		t.Fatal("nil dispatcher")
	}
	// An unknown repository is an error the caller logs.
	if err := h.d.Emit(ctx, repoB, "frozen", at, pusher, nil); err == nil {
		t.Fatal("unknown repository emitted")
	}
}

// TestEmittedEventsRetryAndRepairLikePushes: a sink answering 500 sees
// the same schedule and the same dead-letter move for an a-<id>.json;
// an a-<id>.json left pending by a dead node is delivered by another
// node's step 1 within one interval, unless its id is in the cursor's
// delivered set, in which case it is deleted.
func TestEmittedEventsRetryAndRepairLikePushes(t *testing.T) {
	h := newHarness(t, "n1")
	h.sink.Fail(0, 500)
	h.create(repoA, "acme", "app")
	ctx := context.Background()
	at := h.clock.Now()
	if err := h.d.Emit(ctx, repoA, "deleted", at, Pusher{Sub: "alice"}, map[string]any{"purge_after": at.Add(7 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	key := h.d.emitKey(repoA, EmitID(repoA, "deleted", at))
	elapsed := time.Duration(0)
	for i, delay := range []time.Duration{0, time.Second, 10 * time.Second, time.Minute, 10 * time.Minute, time.Hour, time.Hour} {
		h.clock.Advance(delay)
		elapsed += delay
		h.d.deliverDue(ctx)
		obj, _, _, _ := h.d.read(ctx, key)
		if len(h.sink.Deliveries(repoA, "deleted")) != i+1 || obj.Attempts != i+1 {
			t.Fatalf("attempt %d: %d deliveries, %+v", i+1, len(h.sink.Deliveries(repoA, "deleted")), obj)
		}
	}
	h.clock.Advance(Window - elapsed + time.Second)
	h.d.deliverDue(ctx)
	if dead := h.keys(h.d.prefix + "dead/"); len(dead) != 1 || dead[0] != h.d.deadKey(key) || h.counter("origo_events_dead_total") != 1 {
		t.Fatalf("dead %v", dead)
	}

	// Step 1 on another node: a pending a-<id>.json of a dead node whose
	// next_at is more than a minute old is delivered by the survivor; one
	// whose id the cursor holds is deleted instead.
	h.sink.Fail(0, 0)
	h.sink.Clear()
	survivor := newHarnessOn(t, h.store, h.sink, h.clock, "n2")
	at2 := h.clock.Now()
	if err := h.d.Emit(ctx, repoA, "frozen", at2, Pusher{Sub: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Emit(ctx, repoA, "unfrozen", at2, Pusher{Sub: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	// unfrozen was delivered before the node died; frozen was not.
	unfrozen := h.d.emitKey(repoA, EmitID(repoA, "unfrozen", at2))
	h.d.attempt(ctx, unfrozen)
	if err := h.d.Emit(ctx, repoA, "unfrozen", at2, Pusher{Sub: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	h.sink.Clear()
	// Within the lag nothing is touched; past it step 1 acts.
	survivor.d.sweep(ctx)
	if len(survivor.d.Pending()) != 0 {
		t.Fatalf("swept within the lag: %v", survivor.d.Pending())
	}
	h.clock.Advance(2 * time.Minute)
	h.store.SetClock(h.clock.Now)
	survivor.d.sweep(ctx)
	survivor.d.deliverDue(ctx)
	if got := h.sink.Deliveries(repoA, ""); len(got) != 1 || got[0].Kind != "frozen" {
		t.Fatalf("survivor delivered %+v", got)
	}
	if keys := h.keys(h.d.prefix + repoA + "/a-"); len(keys) != 0 {
		t.Fatalf("left behind: %v", keys)
	}
	if _, err := h.store.Head(ctx, unfrozen); !errors.Is(err, wal.ErrNotFound) {
		t.Fatal("the delivered id was not deleted by step 1")
	}
}

// TestRepairReadsOnlyDeadJournals: the sweep reads only the journals of
// nodes not heard for ORIGO_REPAIR_UNHEARD and the index of only the
// repositories they name, with a fake clock and a store that counts
// reads.
func TestRepairReadsOnlyDeadJournals(t *testing.T) {
	c := newClock()
	store := wal.NewMemStore()
	store.SetClock(c.Now)
	s := sink.New(t)
	now := c.Now()
	live := members{c: c, seen: map[string]time.Duration{"alive": time.Second, "stale": 10 * time.Second}}
	h := newHarnessOn(t, store, s, c, "me", func(o *Options) { o.Members = live })
	ctx := context.Background()
	// Four nodes each pushed to a repository of their own, none of the
	// pushes enqueued; the journals name them.
	repos := map[string]string{"alive": "aaaaaaaa-0000-4000-8000-000000000001", "stale": "bbbbbbbb-0000-4000-8000-000000000002", "never": "cccccccc-0000-4000-8000-000000000003", "me": "dddddddd-0000-4000-8000-000000000004"}
	for node, repo := range repos {
		ix := h.create(repo, "acme", node)
		h.push(repo, ix, wal.Entry{Refs: mainUpdate(1)})
		other := newHarnessOn(t, store, s, c, node)
		other.d.journal.add(now, repo, 1)
		if err := other.d.flushJournal(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A journal older than two days, and a day with no lines, sit beside them.
	old := h.d.journalKey("gone", now.Add(-3*24*time.Hour))
	if _, err := store.Put(ctx, old, wal.BytesBody([]byte(repos["alive"]+" 1\n"))); err != nil {
		t.Fatal(err)
	}
	c.Advance(2 * time.Minute)
	var reads []string
	store.SetFault(func(op, key string) error {
		if op == "Get" {
			reads = append(reads, key)
		}
		return nil
	})
	h.d.sweep(ctx)
	store.SetFault(nil)
	var journals, indexes []string
	for _, k := range reads {
		switch {
		case strings.Contains(k, "/nodes/"):
			journals = append(journals, k)
		case strings.Contains(k, "/index/"):
			indexes = append(indexes, k)
		}
	}
	for _, k := range journals {
		if !strings.Contains(k, "/nodes/stale/") && !strings.Contains(k, "/nodes/never/") {
			t.Errorf("journal of a live node or of this node read: %s", k)
		}
	}
	if len(journals) != 4 {
		t.Errorf("journals read: %v", journals)
	}
	for _, k := range indexes {
		if !strings.Contains(k, repos["stale"]) && !strings.Contains(k, repos["never"]) {
			t.Errorf("index of a live node's repository read: %s", k)
		}
	}
	if len(indexes) == 0 {
		t.Error("no index read")
	}
	// The two dead nodes' pushes are queued with the deterministic id,
	// the others are not, and the old journal is gone.
	if got := h.d.Pending(); len(got) != 2 || got[0] != h.d.pushKey(repos["stale"], 1) || got[1] != h.d.pushKey(repos["never"], 1) {
		t.Fatalf("pending %v", got)
	}
	if _, err := store.Head(ctx, old); !errors.Is(err, wal.ErrNotFound) {
		t.Fatal("old journal kept")
	}
	h.d.deliverDue(ctx)
	if got := s.Deliveries("", KindPush); len(got) != 2 || got[0].ID != PushID(repos["stale"], 1) {
		t.Fatalf("delivered %+v", got)
	}
	// A second sweep repairs nothing: the cursor passed the entries.
	h.d.sweep(ctx)
	if len(h.d.Pending()) != 0 {
		t.Fatalf("repaired twice: %v", h.d.Pending())
	}
	// A node's own journals are read at start-up and its lines kept.
	me := newHarnessOn(t, store, s, c, "me")
	me.d.startup(ctx)
	if lines := me.d.Lines(now); len(lines) != 1 || lines[0] != repos["me"]+" 1" {
		t.Fatalf("own journal not loaded: %v", lines)
	}
	if got := me.d.Pending(); len(got) != 1 || got[0] != h.d.pushKey(repos["me"], 1) {
		t.Fatalf("own push not repaired at start-up: %v", got)
	}
}

// TestPusherCarriesSubjectAndActor: a push with a service token
// carrying act delivers pusher.sub equal to act and pusher.actor equal
// to the token's sub, one without act delivers actor empty.
func TestPusherCarriesSubjectAndActor(t *testing.T) {
	h := newHarness(t, "n1")
	ix := h.create(repoA, "acme", "app")
	ctx := context.Background()
	// The verifier of spec 007 sets Subject to act and Actor to sub; the
	// entry header carries both and the event copies them.
	c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(1), Subject: "alice", Actor: "svc"})
	c2 := h.push(repoA, c.Index, wal.Entry{Refs: mainUpdate(2), Subject: "bob"})
	for _, x := range []struct {
		c    *wal.Committed
		refs []wal.RefUpdate
	}{{c, mainUpdate(1)}, {c2, mainUpdate(2)}} {
		if err := h.d.Enqueue(ctx, repoA, entryOf(x.c, x.refs, nil)); err != nil {
			t.Fatal(err)
		}
	}
	h.d.deliverDue(ctx)
	got := h.sink.Deliveries(repoA, KindPush)
	if len(got) != 2 {
		t.Fatalf("%d deliveries", len(got))
	}
	var p1, p2 Push
	_ = json.Unmarshal(got[0].Body, &p1)
	_ = json.Unmarshal(got[1].Body, &p2)
	if p1.Pusher != (Pusher{Sub: "alice", Actor: "svc"}) || p2.Pusher != (Pusher{Sub: "bob"}) {
		t.Fatalf("pushers %+v %+v", p1.Pusher, p2.Pusher)
	}
	if !strings.Contains(string(got[1].Body), `"actor":""`) {
		t.Fatalf("actor absent rather than empty: %s", got[1].Body)
	}
}

// TestUndeleteEmitsNoPushEvent: an undelete produces no push event from
// the enqueue and none from the repair sweep run over its entry, while
// spec 019's undeleted is delivered once.
func TestUndeleteEmitsNoPushEvent(t *testing.T) {
	h := newHarness(t, "n1")
	ix := h.create(repoA, "acme", "app")
	ctx := context.Background()
	c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(1)})
	del := h.push(repoA, c.Index, wal.Entry{Kind: wal.KindDelete, Deleted: true})
	und := h.push(repoA, del.Index, wal.Entry{})
	if err := h.d.Enqueue(ctx, repoA, entryOf(c, mainUpdate(1), nil)); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Enqueue(ctx, repoA, entryOf(und, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Emit(ctx, repoA, "undeleted", und.Header.At, Pusher{Sub: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if Detail(nil) != DetailUndelete {
		t.Fatal("Detail of an empty transaction")
	}
	h.d.deliverDue(ctx)
	if got := h.sink.Deliveries(repoA, ""); len(got) != 2 || got[0].Kind != KindPush || got[1].Kind != "undeleted" {
		t.Fatalf("deliveries %+v", got)
	}
	// The journal names both entries; the sweep over them rebuilds
	// nothing for the undelete.
	if lines := h.d.Lines(h.clock.Now()); len(lines) != 2 || lines[1] != repoA+" 3" {
		t.Fatalf("journal %v", lines)
	}
	h.clock.Advance(2 * time.Minute)
	if err := h.d.repairRepo(ctx, repoA, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	if got := h.sink.Deliveries(repoA, KindPush); len(got) != 1 || len(h.keys(h.d.prefix+repoA+"/0")) != 0 {
		t.Fatalf("the sweep wrote a push event for the undelete: %+v %v", got, h.keys(h.d.prefix+repoA+"/0"))
	}
}

// TestDefaultBranchChangeIsAHeadUpdate: a PATCH of default_branch
// produces one push event with the single HEAD update and
// kind_detail "default_branch", from the enqueue and from the sweep.
func TestDefaultBranchChangeIsAHeadUpdate(t *testing.T) {
	h := newHarness(t, "n1")
	ix := h.create(repoA, "acme", "app")
	ctx := context.Background()
	refs := []wal.RefUpdate{{Ref: "HEAD", Old: "ref: refs/heads/main", New: "ref: refs/heads/dev"}}
	c := h.push(repoA, ix, wal.Entry{Refs: refs, PushOptions: []string{"origo.operation=merge"}})
	if err := h.d.Enqueue(ctx, repoA, entryOf(c, refs, nil)); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	check := func(body []byte) {
		t.Helper()
		var p Push
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		if p.KindDetail != DetailDefaultBranch || len(p.Updates) != 1 || p.Updates[0] != (Update{Ref: "HEAD", Before: "ref: refs/heads/main", After: "ref: refs/heads/dev"}) || p.Operation != "merge" {
			t.Fatalf("payload %+v", p)
		}
	}
	got := h.sink.Deliveries(repoA, KindPush)
	if len(got) != 1 {
		t.Fatalf("%d deliveries", len(got))
	}
	check(got[0].Body)
	// The sweep rebuilds the same payload when the object is missing and
	// the cursor is behind.
	if _, err := h.store.Put(ctx, h.d.cursorKey(repoA), wal.BytesBody([]byte(`{"seq":0}`))); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Minute)
	if err := h.d.repairRepo(ctx, repoA, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	got = h.sink.Deliveries(repoA, KindPush)
	if len(got) != 2 || got[1].ID != got[0].ID {
		t.Fatalf("%d deliveries", len(got))
	}
	check(got[1].Body)
	// A push that moves a branch carries no kind_detail.
	c2 := h.push(repoA, c.Index, wal.Entry{Refs: mainUpdate(1)})
	if err := h.d.Enqueue(ctx, repoA, entryOf(c2, mainUpdate(1), map[string]bool{"refs/heads/main": true})); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	got = h.sink.Deliveries(repoA, KindPush)
	if len(got) != 3 || strings.Contains(string(got[2].Body), "kind_detail") || !strings.Contains(string(got[2].Body), `"forced":true`) {
		t.Fatalf("branch push %s", got[2].Body)
	}
}

// TestEventOffSuppressesOnlyThatPush: git push -o origo.event=off
// delivers nothing for that push and the repair sweep writes nothing
// for it, while a following push without the option delivers.
func TestEventOffSuppressesOnlyThatPush(t *testing.T) {
	h := newHarness(t, "n1")
	ix := h.create(repoA, "acme", "app")
	ctx := context.Background()
	c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(1), PushOptions: []string{"origo.event=off"}})
	c2 := h.push(repoA, c.Index, wal.Entry{Refs: mainUpdate(2)})
	if err := h.d.Enqueue(ctx, repoA, entryOf(c, mainUpdate(1), nil)); err != nil {
		t.Fatal(err)
	}
	if len(h.keys(h.d.prefix+repoA+"/")) != 0 || len(h.d.Pending()) != 0 {
		t.Fatal("an object was written for the suppressed push")
	}
	if err := h.d.Enqueue(ctx, repoA, entryOf(c2, mainUpdate(2), nil)); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	got := h.sink.Deliveries(repoA, KindPush)
	if len(got) != 1 || got[0].ID != PushID(repoA, 2) {
		t.Fatalf("deliveries %+v", got)
	}
	// The sweep over the entries, with the cursor behind both, writes
	// nothing for the suppressed one.
	if _, err := h.store.Put(ctx, h.d.cursorKey(repoA), wal.BytesBody([]byte(`{"seq":0}`))); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Minute)
	if err := h.d.repairRepo(ctx, repoA, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if got := h.d.Pending(); len(got) != 1 || got[0] != h.d.pushKey(repoA, 2) {
		t.Fatalf("repaired %v", got)
	}
}

// TestFailuresAreLoggedAndRetried covers the paths a store or a sink
// failure takes: an enqueue whose write fails is an error the push
// survives, the failpoint aborts before any write, a lost sink is a
// failed attempt, an unreadable object is retried, and a cursor or a
// dead move that cannot be written is logged.
func TestFailuresAreLoggedAndRetried(t *testing.T) {
	failAt := ""
	h := newHarness(t, "n1", func(o *Options) {
		o.Failpoint = func(name string) error {
			if name == failAt {
				return errors.New("failpoint " + name)
			}
			return nil
		}
	})
	ix := h.create(repoA, "acme", "app")
	ctx := context.Background()
	c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(1)})
	e := entryOf(c, mainUpdate(1), nil)
	failAt = FailpointBeforeEnqueue
	if err := h.d.Enqueue(ctx, repoA, e); err == nil || len(h.d.Lines(h.clock.Now())) != 0 {
		t.Fatalf("failpoint: %v %v", err, h.d.Lines(h.clock.Now()))
	}
	failAt = ""
	// A failed object write, and a failed journal flush, are errors the
	// caller logs; the line stays for the next flush.
	h.store.SetFault(func(op, key string) error {
		if op == "Put" {
			return errors.New("bucket down")
		}
		return nil
	})
	if err := h.d.Enqueue(ctx, repoA, e); err == nil {
		t.Fatal("enqueue succeeded with the bucket down")
	}
	if err := h.d.Emit(ctx, repoA, "frozen", h.clock.Now(), Pusher{}, nil); err == nil {
		t.Fatal("emit succeeded with the bucket down")
	}
	h.store.SetFault(nil)
	if err := h.d.flushJournal(ctx); err != nil {
		t.Fatal(err)
	}
	if lines, err := h.d.readJournal(ctx, "n1", h.clock.Now()); err != nil || len(lines) != 1 {
		t.Fatalf("journal after the retried flush: %v %v", lines, err)
	}
	// A meta that cannot be read.
	if err := h.d.Enqueue(ctx, repoB, e); err == nil {
		t.Fatal("enqueue of an unknown repository")
	}
	if err := h.d.Enqueue(ctx, repoA, e); err != nil {
		t.Fatal(err)
	}
	key := h.d.pushKey(repoA, 1)
	// The sink is gone: a failed attempt on the schedule.
	h.sink.Close()
	h.d.deliverDue(ctx)
	if obj, _, _, err := h.d.read(ctx, key); err != nil || obj.Attempts != 1 {
		t.Fatalf("after a lost sink: %+v %v", obj, err)
	}
	// An object that cannot be read is retried; an unparsable one too.
	h.store.SetFault(func(op, key string) error {
		if op == "Get" && strings.HasSuffix(key, ".json") {
			return errors.New("bucket down")
		}
		return nil
	})
	h.clock.Advance(time.Second)
	h.d.deliverDue(ctx)
	h.store.SetFault(nil)
	if _, err := h.store.Put(ctx, key, wal.BytesBody([]byte("nope"))); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	h.d.deliverDue(ctx)
	if _, err := h.store.Put(ctx, key, wal.BytesBody([]byte(`{"event":"x"}`))); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	h.d.deliverDue(ctx)
	if len(h.d.Pending()) != 1 {
		t.Fatalf("pending %v", h.d.Pending())
	}
	// A vanished object is dropped.
	if err := h.store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	h.d.deliverDue(ctx)
	if len(h.d.Pending()) != 0 {
		t.Fatalf("pending %v", h.d.Pending())
	}
	// A dead move whose write fails keeps the object pending; a cursor
	// that cannot be written, a delete that fails, and an emitted event
	// whose cursor cannot be read are logged.
	if err := h.d.Enqueue(ctx, repoA, e); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(Window + time.Second)
	h.store.SetFault(func(op, key string) error {
		if strings.Contains(key, "/dead/") {
			return errors.New("bucket down")
		}
		return nil
	})
	h.d.deliverDue(ctx)
	h.store.SetFault(nil)
	if _, err := h.store.Head(ctx, key); err != nil || len(h.d.Pending()) != 1 || h.counter("origo_events_dead_total") != 0 {
		t.Fatalf("dead move with the bucket down: %v %v", err, h.d.Pending())
	}
	h.store.SetFault(func(op, key string) error {
		if strings.HasSuffix(key, "/cursor") || op == "Delete" {
			return errors.New("bucket down")
		}
		return nil
	})
	s2 := sink.New(t)
	h.d.url = s2.URL()
	h.clock.Advance(time.Hour)
	h.d.deliverDue(ctx)
	if len(s2.Deliveries(repoA, KindPush)) != 1 || h.counter("origo_events_delivered_total") != 1 {
		t.Fatal("not delivered with the cursor unwritable")
	}
	if err := h.d.Emit(ctx, repoA, "frozen", h.clock.Now(), Pusher{}, nil); err != nil {
		t.Fatal(err)
	}
	h.d.deliverDue(ctx)
	if len(s2.Deliveries(repoA, "frozen")) != 0 {
		t.Fatal("delivered with the cursor unreadable")
	}
	h.store.SetFault(nil)
	// The sweep's listings and reads failing are logged and skipped.
	h.store.SetFault(func(op, key string) error {
		if op == "List" || op == "Get" {
			return errors.New("bucket down")
		}
		return nil
	})
	h.d.sweep(ctx)
	h.d.startup(ctx)
	h.store.SetFault(nil)
	// A malformed pending object, an unreadable one, a repository the
	// log no longer holds, and a journal with a bad line are skipped.
	if _, err := h.store.Put(ctx, h.d.pushKey(repoA, 9), wal.BytesBody([]byte("nope"))); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Put(ctx, h.d.journalKey("dead", h.clock.Now()), wal.BytesBody([]byte("not-a-repo 1\n"+repoB+" 1\n"+repoA+" 2\n"))); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Minute)
	h.store.SetClock(h.clock.Now)
	h.d.sweep(ctx)
	// Errors inside repairRepo: the cursor, the index, the head, the
	// entry read, and the metadata, with the delivered object gone.
	if err := h.store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	for _, fail := range []string{"/cursor", "/index/", "/wal/", "/meta"} {
		if _, err := h.store.Put(ctx, h.d.cursorKey(repoA), wal.BytesBody([]byte(`{"seq":0}`))); err != nil {
			t.Fatal(err)
		}
		h.store.SetFault(func(op, key string) error {
			if strings.Contains(key, fail) && op != "Head" {
				return errors.New("bucket down")
			}
			if fail == "/wal/" && op == "Head" && strings.Contains(key, "/events/") {
				return errors.New("bucket down")
			}
			return nil
		})
		if err := h.d.repairRepo(ctx, repoA, h.clock.Now()); err == nil {
			t.Fatalf("repair with %s failing", fail)
		}
		h.store.SetFault(nil)
	}
	if _, err := h.store.Put(ctx, h.d.cursorKey(repoA), wal.BytesBody([]byte("nope"))); err != nil {
		t.Fatal(err)
	}
	if err := h.d.repairRepo(ctx, repoA, h.clock.Now()); err == nil {
		t.Fatal("repair with a malformed cursor")
	}
	// Options are checked.
	if _, err := New(Options{}); err == nil {
		t.Fatal("no log")
	}
	if _, err := New(Options{Log: h.log, Node: "n", URL: "http://x"}); err == nil {
		t.Fatal("a URL without a secret")
	}
	if d, err := New(Options{Log: h.log, Node: "n"}); err != nil || d.Enabled() || d.repairInterval != 10*time.Minute || d.repairUnheard != 5*time.Minute || d.client.Timeout != DeliveryTimeout {
		t.Fatalf("defaults: %+v %v", d, err)
	}
	if Delay(0) != time.Second || Delay(9) != time.Hour {
		t.Fatal("Delay bounds")
	}
}

// TestRunDeliversFlushesAndSweeps runs the loops on the wall clock: an
// event enqueued while running is delivered, the journal is flushed on
// the tick, the sweep runs at its offset, and a dispatcher with events
// off returns when its context ends.
func TestRunDeliversFlushesAndSweeps(t *testing.T) {
	s := sink.New(t)
	h := newHarnessOn(t, wal.NewMemStore(), s, &clock{t: time.Now()}, "n1", func(o *Options) { o.RepairInterval = 50 * time.Millisecond })
	h.d.now = time.Now
	var lists atomic.Int32
	h.store.SetFault(func(op, _ string) error {
		if op == "List" {
			lists.Add(1)
		}
		return nil
	})
	ix := h.create(repoA, "acme", "app")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()
	c := h.push(repoA, ix, wal.Entry{Refs: mainUpdate(1)})
	if err := h.d.Enqueue(ctx, repoA, entryOf(c, mainUpdate(1), nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Wait(repoA, KindPush, 1, 10*time.Second); !ok {
		t.Fatal("not delivered by the loop")
	}
	// A sweep has run at least once within a few intervals.
	deadline := time.Now().Add(5 * time.Second)
	for lists.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lists.Load() == 0 {
		t.Fatal("the sweep did not run")
	}
	cancel()
	<-done
	off, err := New(Options{Log: h.log, Node: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := off.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run with events off: %v", err)
	}
	// The journal tick flushes a line added after the start-up flush.
	j := newJournal()
	day := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	j.add(day, repoA, 1)
	j.add(day.Add(-48*time.Hour), repoA, 1)
	j.add(day.Add(24*time.Hour), repoA, 2)
	if days := len(j.days); days != 2 {
		t.Fatalf("days in memory: %d", days)
	}
	j.load(day.Add(24*time.Hour), []string{repoB + " 5", repoA + " 2"})
	if lines := j.days[dayOf(day.Add(24*time.Hour))]; len(lines) != 2 || lines[0] != repoB+" 5" || lines[1] != repoA+" 2" || !j.repos[dayOf(day.Add(24*time.Hour))][repoB] {
		t.Fatalf("loaded lines %v", lines)
	}
	j.undo("nowhere")
	if _, ok := j.dirty["nowhere"]; ok {
		t.Fatal("undo of an unknown day")
	}
	if !isEmitKey("origo/events/x/a-1.json") || isEmitKey("origo/events/x/000000000001.json") {
		t.Fatal("isEmitKey")
	}
	if got := h.d.Days(); len(got) != 1 {
		t.Fatalf("days %v", got)
	}
	// An HTTP client of the caller's is kept, with its own timeout.
	d, err := New(Options{Log: h.log, Node: "n1", URL: s.URL(), Secret: s.Secret(), Client: &http.Client{Timeout: time.Second}})
	if err != nil || d.client.Timeout != time.Second {
		t.Fatalf("client: %v", err)
	}
}
