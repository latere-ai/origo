// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package repo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/wal"
)

const repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

// fakeClock moves only when advanced.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// degraded is a harness over the breaker store of spec 015 with a fake
// clock and a metrics registry.
type degraded struct {
	*harness
	mem   *wal.MemStore
	clock *fakeClock
	reg   *pkgmetrics.Registry
}

func newDegraded(t *testing.T, staleMax time.Duration) *degraded {
	t.Helper()
	mem := wal.NewMemStore()
	clock := &fakeClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	reg := pkgmetrics.NewRegistry()
	set := metrics.Register(reg)
	store := wal.NewBreakerStore(wal.BreakerOptions{Store: mem, Clock: clock, Metrics: set, Threshold: 1})
	l := wal.New(wal.Options{Store: store, Logger: slog.New(slog.DiscardHandler), Metrics: set})
	c, err := New(Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: slog.New(slog.DiscardHandler), Metrics: set, Now: clock.Now, StaleMax: staleMax})
	if err != nil {
		t.Fatal(err)
	}
	ix, err := l.CreateRepo(context.Background(), wal.Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	return &degraded{harness: &harness{t: t, store: mem, log: l, cache: c, src: gittest.NewSource(t), held: ix}, mem: mem, clock: clock, reg: reg}
}

func (d *degraded) metric(name string) string {
	var text bytes.Buffer
	d.reg.WritePrometheus(&text)
	for line := range strings.SplitSeq(text.String(), "\n") {
		if after, ok := strings.CutPrefix(line, name+" "); ok {
			return after
		}
	}
	return ""
}

// cut makes every read of the store fail, the bucket unreachable.
func (d *degraded) cut() {
	d.mem.SetFault(func(op, _ string) error {
		if op == "Head" || op == "Get" || op == "List" {
			return errors.New("unreachable")
		}
		return nil
	})
}

// TestStaleLeaseIsBoundedByStaleMax is the cache's half of spec 015's
// first criterion: under an open read breaker a warm copy is leased
// stale for StaleMax from its last check that answered and refused
// after, a cold copy and a writer are refused at once, and the first
// lease after the breaker closes is consistent.
func TestStaleLeaseIsBoundedByStaleMax(t *testing.T) {
	d := newDegraded(t, 5*time.Minute)
	ctx := context.Background()
	c1 := d.src.Commit("a.txt", "one", "first")
	d.push("refs/heads/main", wal.ZeroSHA, c1, d.src.Pack(c1))
	if _, err := d.log.CreateRepo(ctx, wal.Meta{ID: repoB, Owner: "acme", Slug: "cold"}, "main"); err != nil {
		t.Fatal(err)
	}
	// Warm: a consistent lease, whose check is the last that answered.
	l, err := d.cache.Lease(ctx, repoA, false)
	if err != nil || l.Stale || l.Seq != 1 {
		t.Fatalf("warm lease: %+v %v", l, err)
	}
	l.Release()
	// The bucket goes away. The first check fails on its own error and
	// opens the breaker (threshold 1 here); nothing is served stale for
	// a check that ran.
	d.cut()
	d.clock.Advance(time.Minute)
	if _, err := d.cache.Lease(ctx, repoA, false); err == nil || errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("the failing check: %v", err)
	}
	// Open: the warm copy is leased stale, aged from the last check.
	l, err = d.cache.Lease(ctx, repoA, false)
	if err != nil || !l.Stale || l.StaleFor != time.Minute || l.Seq != 1 {
		t.Fatalf("stale lease: %+v %v", l, err)
	}
	l.Release()
	if got := d.metric("origo_stale_responses_total"); got != "1" {
		t.Fatalf("stale responses %q", got)
	}
	// A writer is refused at once with the breaker's error, whatever
	// the copy holds; so is a cold repository.
	if _, err := d.cache.Lease(ctx, repoA, true); !errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("writer under the open breaker: %v", err)
	}
	if _, _, err := d.cache.Acquire(ctx, repoB, false); !errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("cold repository: %v", err)
	}
	// Four minutes on, the window has passed: the next check is the
	// probe, which fails on the store's error and reopens the breaker.
	// At the bound the copy is still served; past it, refused.
	d.clock.Advance(4 * time.Minute)
	if _, err := d.cache.Lease(ctx, repoA, false); err == nil || errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("the probe: %v", err)
	}
	l, err = d.cache.Lease(ctx, repoA, false)
	if err != nil || !l.Stale || l.StaleFor != 5*time.Minute {
		t.Fatalf("at the bound: %+v %v", l, err)
	}
	l.Release()
	d.clock.Advance(time.Second)
	if _, err := d.cache.Lease(ctx, repoA, false); !errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("past the bound: %v", err)
	}
	// Acquire hides the verdict but serves the same copy.
	d.clock.Advance(-2 * time.Second)
	if r, release, err := d.cache.Acquire(ctx, repoA, false); err != nil || r.Seq != 1 {
		t.Fatalf("acquire stale: %v", err)
	} else {
		release()
	}
	// The bucket returns and the window has passed: the next check is
	// the probe, it answers, and the lease is consistent again.
	d.mem.SetFault(nil)
	d.clock.Advance(time.Hour)
	l, err = d.cache.Lease(ctx, repoA, false)
	if err != nil || l.Stale {
		t.Fatalf("after recovery: %+v %v", l, err)
	}
	l.Release()
	if got := d.metric("origo_stale_responses_total"); got != "3" {
		t.Fatalf("stale responses %q", got)
	}
	// An evicted copy has no last check: cold under the next outage.
	d.cache.Evict(repoA)
	d.cut()
	if _, err := d.cache.Lease(ctx, repoA, false); err == nil {
		t.Fatal("the failing check after an eviction answered")
	}
	if _, err := d.cache.Lease(ctx, repoA, false); !errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("evicted copy under the open breaker: %v", err)
	}
	if _, err := d.cache.Lease(ctx, "not-an-id", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid id: %v", err)
	}
}

// TestMissingPackIsAnIntegrityError is spec 015's partial-failure
// criterion: a pack the index names that is gone makes the repository
// fail with a wal.IntegrityError naming the key, which the handlers
// answer as 503 repository_unavailable, counts one integrity error, and
// leaves another repository served. A gone entry and an entry whose
// digest differs from its header are the same error.
func TestMissingPackIsAnIntegrityError(t *testing.T) {
	d := newDegraded(t, time.Minute)
	ctx := context.Background()
	c1 := d.src.Commit("a.txt", "one", "first")
	d.push("refs/heads/main", wal.ZeroSHA, c1, d.src.Pack(c1))
	name := d.compact(c1)
	packKey := d.log.RepoPrefix(repoA) + "packs/" + name + ".pack"
	if err := d.mem.Delete(ctx, packKey); err != nil {
		t.Fatal(err)
	}
	other, err := d.log.CreateRepo(ctx, wal.Meta{ID: repoB, Owner: "acme", Slug: "other"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.cache.Lease(ctx, repoA, false)
	var ie *wal.IntegrityError
	if !errors.As(err, &ie) || ie.Key != packKey {
		t.Fatalf("missing pack: %v", err)
	}
	if errors.Is(err, ErrCorrupt) || errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("a missing pack read as corruption or an outage: %v", err)
	}
	if got := d.metric("origo_log_integrity_errors_total"); got != "1" {
		t.Fatalf("integrity errors %q", got)
	}
	// The other repository is served.
	if l, err := d.cache.Lease(ctx, repoB, false); err != nil || l.Seq != other.Seq {
		t.Fatalf("other repository: %v", err)
	} else {
		l.Release()
	}
	// The next request materializes again: the pack restored, the
	// repository serves.
	if _, err := d.mem.Put(ctx, packKey, wal.BytesBody(d.src.Pack(c1))); err != nil {
		t.Fatal(err)
	}
	l, err := d.cache.Lease(ctx, repoA, false)
	if err != nil || d.localRef(l.Repo, "refs/heads/main") != c1 {
		t.Fatalf("after the restore: %v", err)
	}
	l.Release()

	// An entry the index names that is gone, on a fresh copy.
	c2 := d.src.Commit("b.txt", "two", "second")
	ix := d.push("refs/heads/main", c1, c2, d.src.Pack(c2, c1))
	entryKey := d.log.RepoPrefix(repoA) + ix.Entry
	entry, _, _ := d.mem.Get(ctx, entryKey, "")
	data := new(bytes.Buffer)
	_, _ = data.ReadFrom(entry)
	_ = entry.Close()
	d.cache.Evict(repoA)
	if err := d.mem.Delete(ctx, entryKey); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cache.Lease(ctx, repoA, false); !errors.As(err, &ie) || ie.Key != entryKey {
		t.Fatalf("missing entry: %v", err)
	}
	// The entry restored with its last byte changed: the digest differs
	// from the header.
	bad := append([]byte(nil), data.Bytes()...)
	bad[len(bad)-1] ^= 0xff
	if _, err := d.mem.Put(ctx, entryKey, wal.BytesBody(bad)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cache.Lease(ctx, repoA, false); !errors.As(err, &ie) || ie.Key != entryKey || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("corrupt entry: %v", err)
	}
	// An entry that is not an entry at all.
	if _, err := d.mem.Put(ctx, entryKey, wal.BytesBody([]byte("nonsense"))); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cache.Lease(ctx, repoA, false); !errors.As(err, &ie) || ie.Key != entryKey {
		t.Fatalf("unparsable entry: %v", err)
	}
	if got := d.metric("origo_log_integrity_errors_total"); got != "4" {
		t.Fatalf("integrity errors %q", got)
	}
	// Restored whole, the repository serves again.
	if _, err := d.mem.Put(ctx, entryKey, wal.BytesBody(data.Bytes())); err != nil {
		t.Fatal(err)
	}
	if l, err := d.cache.Lease(ctx, repoA, false); err != nil || d.localRef(l.Repo, "refs/heads/main") != c2 {
		t.Fatalf("after the second restore: %v", err)
	} else {
		l.Release()
	}
	// A warm copy that lost nothing but a pack object it already holds
	// is served: the pack on disk costs one stat and no fetch.
	if err := d.mem.Delete(ctx, packKey); err != nil {
		t.Fatal(err)
	}
	if l, err := d.cache.Lease(ctx, repoA, false); err != nil {
		t.Fatalf("warm copy with the pack object gone: %v", err)
	} else {
		l.Release()
	}
}

// TestThinPackWithoutBaseIsStorageUnavailable holds the rule of spec
// 015 for a thin pack whose base is in no entry and no pack: git
// refuses the batch, the error is neither an IntegrityError nor
// corruption of the copy, so the handlers answer 503
// storage_unavailable and nothing is evicted, and it is counted once
// on origo_log_integrity_errors_total as the integrity error of the
// log it is.
func TestThinPackWithoutBaseIsStorageUnavailable(t *testing.T) {
	d := newDegraded(t, time.Minute)
	ctx := context.Background()
	c1 := d.src.Commit("a.txt", "one", "first")
	c2 := d.src.Commit("a.txt", "one and two", "second")
	// The pack of c2 is thin against c1, which no entry carries.
	d.push("refs/heads/main", wal.ZeroSHA, c2, d.src.Pack(c2, c1))
	_, err := d.cache.Lease(ctx, repoA, false)
	if err == nil {
		t.Fatal("a thin pack with no base materialized")
	}
	var ie *wal.IntegrityError
	if errors.As(err, &ie) || errors.Is(err, ErrCorrupt) || errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("classified as integrity, corruption, or the breaker: %v", err)
	}
	if !IsMissingBase(err) {
		t.Fatalf("not read as a missing base: %v", err)
	}
	if got := d.metric("origo_log_integrity_errors_total"); got != "1" {
		t.Fatalf("integrity errors %q", got)
	}
	if got := d.metric("origo_repo_rebuilt_total"); got != "0" {
		t.Fatalf("rebuilt %q", got)
	}
	// The base pushed later completes the history: the copy serves.
	d.push("refs/heads/base", wal.ZeroSHA, c1, d.src.Pack(c1))
	l, err := d.cache.Lease(ctx, repoA, false)
	if err != nil || d.localRef(l.Repo, "refs/heads/main") != c2 {
		t.Fatalf("after the base landed: %v", err)
	}
	l.Release()
}
