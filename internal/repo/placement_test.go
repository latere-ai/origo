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
// the previous entry, applied by 4 workers onto an empty disk. A worker
// that indexes an entry before its base landed fails once and lands on
// the retry; the references are set once at the end; the result equals
// the source and passes fsck; every entry is counted once.
func TestConcurrentWorkersApplyThinEntries(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Workers = 4 })
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
	if h.cache.thinRetries.Load() == 0 {
		t.Fatal("no thin pack was retried: the workers ran in sequence")
	}
	if got := h.cache.applied.Value(nil); got != n+1 {
		t.Fatalf("applied %d entries, want %d", got, n+1)
	}
	// Every pack is named by git's checksum, nothing is left in the
	// spool, and each entry's pack is one file.
	packs, _ := filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "pack-*.pack"))
	if len(packs) != n {
		t.Fatalf("%d packs on disk, want %d", len(packs), n)
	}
	for _, p := range packs {
		if _, err := os.Stat(strings.TrimSuffix(p, ".pack") + ".idx"); err != nil {
			t.Errorf("%s has no index", p)
		}
	}
	if left, _ := os.ReadDir(h.cache.SpoolDir()); len(left) != 0 {
		t.Fatalf("the spool holds %d files after the apply", len(left))
	}
	// A thin pack whose base never lands fails after the retry and
	// leaves nothing in the spool.
	other := gittest.NewSource(t)
	o1 := other.Commit("g.txt", "g", "g1")
	o2 := other.Commit("g.txt", "gg", "g2")
	h.push("refs/heads/other", wal.ZeroSHA, o2, other.Pack(o2, o1))
	release()
	_, _, err := h.cache.Acquire(context.Background(), repoA, false)
	if err == nil || !strings.Contains(err.Error(), missingBase) {
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
	r, release := h.acquire(false)
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
	r, release = h.acquire(true)
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
