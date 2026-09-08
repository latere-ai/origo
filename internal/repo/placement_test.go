// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

// TestConcurrentWorkersApplyThinEntries is spec 005's materialization
// budget in the small: 24 entries, each a thin pack whose base is in
// the previous entry, applied by 4 workers onto an empty disk. The
// workers fetch and spool concurrently; the indexer joins consecutive
// packs, here at most 5 per batch, and indexes each batch in sequence
// order, so a thin pack's base is in its batch or already in the
// store; the references are set once at the end; the result equals the
// source and passes fsck; every entry is counted once.
func TestConcurrentWorkersApplyThinEntries(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Workers = 4 })
	h.cache.maxBatchEntries = 5
	const n = 24
	prev := ""
	for i := range n {
		c := h.src.Commit("f.txt", fmt.Sprintf("content %d", i), fmt.Sprintf("c%d", i))
		if prev == "" {
			h.push("refs/heads/main", wal.ZeroSHA, c, h.src.Pack(c))
		} else {
			h.push("refs/heads/main", prev, c, h.src.Pack(c, prev))
		}
		prev = c
	}
	// An entry with no pack lands beside the others.
	h.push("refs/tags/v1", wal.ZeroSHA, prev, nil)
	r, release := h.acquire(false)
	if r.Seq != n+1 || h.localRef(r, "refs/heads/main") != prev || h.localRef(r, "refs/tags/v1") != prev {
		t.Fatalf("after apply: seq %d main %s", r.Seq, h.localRef(r, "refs/heads/main"))
	}
	if got := gittest.RevList(t, r.Dir); got != gittest.RevList(t, h.src.Dir) {
		t.Fatalf("history differs:\n%s\n%s", got, gittest.RevList(t, h.src.Dir))
	}
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "fsck", "--strict", "--no-progress"); err != nil {
		t.Fatal(err)
	}
	if got := h.cache.applied.Value(nil); got != n+1 {
		t.Fatalf("applied %d entries, want %d", got, n+1)
	}
	// 24 packs in batches of 5 are 5 packs on disk, each named by git's
	// checksum with its index, and nothing is left in the spool.
	packs, _ := filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "pack-*.pack"))
	if len(packs) != 5 {
		t.Fatalf("%d packs on disk, want 5", len(packs))
	}
	for _, p := range packs {
		if _, err := os.Stat(strings.TrimSuffix(p, ".pack") + ".idx"); err != nil {
			t.Errorf("%s has no index", p)
		}
	}
	if left, _ := os.ReadDir(h.cache.SpoolDir()); len(left) != 0 {
		t.Fatalf("the spool holds %d files after the apply", len(left))
	}
	release()
	// The byte bound splits a batch too: two more entries over a bound
	// smaller than their sum are two packs.
	h.cache.maxBatchBytes = 1
	for i := range 2 {
		c := h.src.Commit("g.txt", fmt.Sprintf("more %d", i), fmt.Sprintf("m%d", i))
		h.push("refs/heads/main", prev, c, h.src.Pack(c, prev))
		prev = c
	}
	r, release = h.acquire(false)
	packs, _ = filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "pack-*.pack"))
	if len(packs) != 7 || h.localRef(r, "refs/heads/main") != prev {
		t.Fatalf("%d packs on disk after the byte-bound apply, want 7", len(packs))
	}
	release()
	// A pack that is not a packfile is refused before git sees it.
	h.push("refs/heads/junk", wal.ZeroSHA, prev, []byte("not a pack at all, though long enough to hold a header and a trailer"))
	if _, _, err := h.cache.Acquire(context.Background(), repoA, false); err == nil || !strings.Contains(err.Error(), "not a version 2 packfile") {
		t.Fatalf("junk pack: %v", err)
	}
	_ = h.store.Delete(context.Background(), h.log.RepoPrefix(repoA)+h.held.Entry)
	_ = h.store.Delete(context.Background(), h.log.RepoPrefix(repoA)+wal.IndexKey(h.held.Seq))
	h.held, _, _ = h.log.Newest(context.Background(), repoA, 0, false)
	// A thin pack whose base is in no entry fails, and leaves nothing
	// in the spool.
	other := gittest.NewSource(t)
	o1 := other.Commit("g.txt", "g", "g1")
	o2 := other.Commit("g.txt", "gg", "g2")
	h.push("refs/heads/other", wal.ZeroSHA, o2, other.Pack(o2, o1))
	_, _, err := h.cache.Acquire(context.Background(), repoA, false)
	if err == nil || !strings.Contains(err.Error(), "did not receive expected object") {
		t.Fatalf("thin pack with no base: %v", err)
	}
	if left, _ := os.ReadDir(h.cache.SpoolDir()); len(left) != 0 {
		t.Fatalf("the spool holds %d files after the failure", len(left))
	}
}

// TestLostSequenceRebuildsFromTheLog is the open item of spec 013's
// Outcome: a warm cache whose bucket was reset answered
// storage_unavailable, because HEAD index/<n+1> is 404 when the log
// holds nothing at all, and the read of index/<n> then failed. The
// copy is removed and the repository materialized from what the log
// holds now: nothing (404), or a new history under the same id.
func TestLostSequenceRebuildsFromTheLog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	c2 := h.src.Commit("a.txt", "two", "second")
	h.push("refs/heads/main", c1, c2, h.src.Pack(c2, c1))
	_, release := h.acquire(false)
	release()

	// The bucket is reset under a restarted process.
	for _, k := range h.store.Keys() {
		_ = h.store.Delete(ctx, k)
	}
	c, err := New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Acquire(ctx, repoA, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset bucket: %v", err)
	}
	if _, err := os.Stat(c.repoDir(repoA)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stale copy was kept")
	}
	// The repository is created again with another history: the next
	// open materializes that one.
	ix, err := h.log.CreateRepo(ctx, wal.Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	h.held = ix
	fresh := gittest.NewSource(t)
	f1 := fresh.Commit("b.txt", "b", "other")
	h.push("refs/heads/main", wal.ZeroSHA, f1, fresh.Pack(f1))
	r, release, err := c.Acquire(ctx, repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != 1 || h.localRef(r, "refs/heads/main") != f1 {
		t.Fatalf("after the reset: seq %d main %s", r.Seq, h.localRef(r, "refs/heads/main"))
	}

	// A held sequence swept while a newer index exists: the copy is
	// rebuilt to the newest, not served stale.
	f2 := fresh.Commit("b.txt", "bb", "again")
	h.push("refs/heads/main", f1, f2, fresh.Pack(f2, f1))
	f3 := fresh.Commit("b.txt", "bbb", "again")
	h.push("refs/heads/main", f2, f3, fresh.Pack(f3, f2))
	release()
	_ = h.store.Delete(ctx, h.log.RepoPrefix(repoA)+wal.IndexKey(1))
	_ = h.store.Delete(ctx, h.log.RepoPrefix(repoA)+wal.IndexKey(2))
	c, _ = New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	r, release, err = c.Acquire(ctx, repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != 3 || h.localRef(r, "refs/heads/main") != f3 {
		t.Fatalf("after the sweep: seq %d main %s", r.Seq, h.localRef(r, "refs/heads/main"))
	}
	release()
	// The read of index/<n> failing for another reason surfaces.
	c, _ = New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	h.store.SetFault(func(op, key string) error {
		if op == "Get" && strings.HasSuffix(key, wal.IndexKey(3)) {
			return errors.New("bucket down")
		}
		return nil
	})
	if _, _, err := c.Acquire(ctx, repoA, false); err == nil || !strings.Contains(err.Error(), "bucket down") {
		t.Fatalf("read failure: %v", err)
	}
	h.store.SetFault(nil)
}

// TestCopiesAndHeldFollowTheLifecycle covers what the evictor and the
// gossip of spec 005 read: Held without a lock, Copies with the size
// and the last acquire, Stats, TryEvict skipping a copy in use, and a
// restarted cache registering the copies on disk as acquired at start.
func TestCopiesAndHeldFollowTheLifecycle(t *testing.T) {
	clock := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	h := newHarness(t, func(o *Options) { o.Now = now })
	ctx := context.Background()
	if _, ok := h.cache.Held(repoA); ok {
		t.Fatal("held before any copy")
	}
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	_, release := h.acquire(false)
	if seq, ok := h.cache.Held(repoA); !ok || seq != 1 {
		t.Fatalf("Held = %d %v", seq, ok)
	}
	copies := h.cache.Copies()
	if len(copies) != 1 || copies[0].ID != repoA || copies[0].Bytes <= 0 || !copies[0].Acquired.Equal(clock) {
		t.Fatalf("Copies = %+v", copies)
	}
	if bytes, repos := h.cache.Stats(); bytes != copies[0].Bytes || repos != 1 {
		t.Fatalf("Stats = %d %d", bytes, repos)
	}
	// In use: not evicted.
	if h.cache.TryEvict(repoA) {
		t.Fatal("evicted a copy under a read lock")
	}
	release()
	clock = clock.Add(time.Hour)
	// A push through Advance measures the copy and moves the sequence.
	r, release := h.acquire(true)
	before := r.bytes.Load()
	c2 := h.src.Commit("a.txt", "two", "second")
	ix := h.push("refs/heads/main", c1, c2, h.src.Pack(c2, c1))
	if _, err := h.cache.Git().Run(ctx, r.Dir, nil, "index-pack", "--stdin", "--fix-thin", "--strict"); err == nil {
		t.Fatal("index-pack with no input succeeded")
	}
	if err := h.cache.Advance(r, ix); err != nil {
		t.Fatal(err)
	}
	release()
	if seq, _ := h.cache.Held(repoA); seq != 2 || r.bytes.Load() < before || !h.cache.Copies()[0].Acquired.Equal(clock) {
		t.Fatalf("after Advance: seq %d bytes %d acquired %v", seq, r.bytes.Load(), h.cache.Copies()[0].Acquired)
	}
	// A restarted cache registers the copy at its start time with its
	// size; an unrelated file in repos/ is ignored.
	_ = os.WriteFile(filepath.Join(h.cache.dir, "repos", "junk.origo.json"), []byte("{}"), 0o644)
	clock = clock.Add(time.Hour)
	c, err := New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	copies = c.Copies()
	if len(copies) != 1 || copies[0].Bytes != r.bytes.Load() || !copies[0].Acquired.Equal(clock) {
		t.Fatalf("restarted Copies = %+v", copies)
	}
	if seq, ok := c.Held(repoA); !ok || seq != 2 {
		t.Fatalf("restarted Held = %d %v", seq, ok)
	}
	// Free: evicted; the next open rebuilds it; an unknown or absent
	// copy is not evicted.
	if !c.TryEvict(repoA) || c.TryEvict(repoA) || c.TryEvict("00000000-0000-0000-0000-000000000009") {
		t.Fatal("TryEvict")
	}
	if _, ok := c.Held(repoA); ok || len(c.Copies()) != 0 {
		t.Fatal("held after eviction")
	}
	r2, release, err := c.Acquire(ctx, repoA, false)
	if err != nil || h.localRef(r2, "refs/heads/main") != c2 {
		t.Fatalf("after eviction: %v", err)
	}
	release()
	// A cache whose repos directory cannot be read fails to start.
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.MkdirAll(filepath.Join(bad, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(bad, "repos"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(bad, "repos"), 0o755) })
	if os.Getuid() != 0 {
		if _, err := New(Options{Dir: bad, Log: h.log, Logger: slog.New(slog.DiscardHandler)}); err == nil {
			t.Fatal("an unreadable repos directory started")
		}
	}
}

// TestReaderUpgradeAppliesTheIndexItRead: a reader whose currency check
// finds a newer index applies that index once it holds the write lock,
// paying one check and one read of the index, not two; a reader whose
// copy moved while it waited runs the check again.
func TestReaderUpgradeAppliesTheIndexItRead(t *testing.T) {
	h := newHarness(t)
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	_, release := h.acquire(false)
	release()
	c2 := h.src.Commit("a.txt", "two", "second")
	h.push("refs/heads/main", c1, c2, h.src.Pack(c2, c1))
	// HEAD index/2 (200), the hint, HEAD index/3 (404): two HEADs and
	// the index and entry reads, once.
	h.store.Calls["Head"], h.store.Calls["Get"] = 0, 0
	r, release := h.acquire(false)
	release()
	if r.Seq != 2 || h.store.Calls["Head"] != 2 || h.store.Calls["Get"] != 3 {
		t.Fatalf("catch-up: seq %d, %d HEADs, %d GETs", r.Seq, h.store.Calls["Head"], h.store.Calls["Get"])
	}
	// A deletion found by a reader is applied under the lock the same way.
	c3 := h.src.Commit("a.txt", "three", "third")
	h.push("refs/heads/main", c2, c3, h.src.Pack(c3, c2))
	e := wal.Entry{Kind: wal.KindDelete, Deleted: true}
	del, err := h.log.Commit(context.Background(), repoA, h.held, e, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	h.held = del.Index
	if _, _, err := h.cache.Acquire(context.Background(), repoA, false); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted: %v", err)
	}
	if _, ok := h.cache.Held(repoA); ok {
		t.Fatal("a deleted copy is held")
	}
}

// TestBatchWithADuplicateObjectFallsBackToOnePackPerEntry: two pushes
// from one base each add a file with the same content, so both packs
// carry the same blob; the joined pack is refused by index-pack, the
// batch is indexed one pack at a time, and the copy holds both
// branches and passes fsck.
func TestBatchWithADuplicateObjectFallsBackToOnePackPerEntry(t *testing.T) {
	h := newHarness(t)
	base := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, base, h.src.Pack(base))
	h.src.Branch("left")
	left := h.src.Commit("x.txt", "the same content", "left")
	h.src.Checkout("main")
	h.src.Branch("right")
	right := h.src.Commit("y.txt", "the same content", "right")
	// Each pack is thin against the base, as a push's pack is, and
	// each carries the shared blob because neither client knew of the
	// other's push.
	h.push("refs/heads/left", wal.ZeroSHA, left, h.src.Pack(left, base))
	h.push("refs/heads/right", wal.ZeroSHA, right, h.src.Pack(right, base))
	r, release := h.acquire(false)
	defer release()
	if h.localRef(r, "refs/heads/left") != left || h.localRef(r, "refs/heads/right") != right {
		t.Fatalf("branches: left %s right %s", h.localRef(r, "refs/heads/left"), h.localRef(r, "refs/heads/right"))
	}
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "fsck", "--strict", "--no-progress"); err != nil {
		t.Fatal(err)
	}
	if h.cache.batchSplits.Load() != 1 {
		t.Fatalf("batch splits %d, want 1: the joined pack with a duplicate object was not refused", h.cache.batchSplits.Load())
	}
	if got := h.cache.applied.Value(nil); got != 3 {
		t.Fatalf("applied %d entries, want 3", got)
	}
	if left, _ := os.ReadDir(h.cache.SpoolDir()); len(left) != 0 {
		t.Fatalf("the spool holds %d files after the apply", len(left))
	}
}
