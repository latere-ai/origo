// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package repo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

const repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

type harness struct {
	t     *testing.T
	store *wal.MemStore
	log   *wal.Log
	cache *Cache
	src   *gittest.Source
	held  *wal.Index
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	store := wal.NewMemStore()
	l := wal.New(wal.Options{Store: store, Logger: slog.New(slog.DiscardHandler)})
	o := Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: slog.New(slog.DiscardHandler)}
	for _, f := range opts {
		f(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := l.CreateRepo(context.Background(), wal.Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, store: store, log: l, cache: c, src: gittest.NewSource(t), held: ix}
}

// push commits a push entry moving ref from old to new with pack.
func (h *harness) push(ref, old, new string, pack []byte) *wal.Index {
	h.t.Helper()
	e := wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{Ref: ref, Old: old, New: new}}}
	if pack != nil {
		e.Pack = wal.BytesBody(pack)
	}
	c, err := h.log.Commit(context.Background(), repoA, h.held, e, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		h.t.Fatal(err)
	}
	h.held = c.Index
	return c.Index
}

func (h *harness) acquire(write bool) (*Repo, func()) {
	h.t.Helper()
	r, release, err := h.cache.Acquire(context.Background(), repoA, write)
	if err != nil {
		h.t.Fatal(err)
	}
	return r, release
}

func (h *harness) localRef(r *Repo, ref string) string {
	h.t.Helper()
	out, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "rev-parse", "--verify", "-q", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestMaterializeFromAnEmptyDiskThenCatchUp(t *testing.T) {
	h := newHarness(t)
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	c2 := h.src.Commit("b.txt", "two", "second")
	h.push("refs/heads/main", c1, c2, h.src.Pack(c2, c1))

	r, release := h.acquire(false)
	if r.Seq != 2 || !r.Local || r.Index.Seq != 2 || h.localRef(r, "refs/heads/main") != c2 {
		t.Fatalf("after materialize: seq %d local %v ref %s", r.Seq, r.Local, h.localRef(r, "refs/heads/main"))
	}
	if got := gittest.RevList(t, r.Dir); got != gittest.RevList(t, h.src.Dir) {
		t.Fatalf("history differs:\n%s\n%s", got, gittest.RevList(t, h.src.Dir))
	}
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "fsck", "--connectivity-only"); err != nil {
		t.Fatal(err)
	}
	head, _ := h.cache.Git().Run(context.Background(), r.Dir, nil, "symbolic-ref", "HEAD")
	if strings.TrimSpace(string(head)) != "refs/heads/main" {
		t.Fatalf("HEAD = %q", head)
	}
	release()

	// Current: one HEAD, nothing changes.
	h.store.Calls["Head"] = 0
	r, release = h.acquire(false)
	release()
	if h.store.Calls["Head"] != 1 || r.Seq != 2 {
		t.Fatalf("current copy: %d HEADs, seq %d", h.store.Calls["Head"], r.Seq)
	}

	// Another node commits: a branch, a tag, HEAD moves, a branch goes.
	c3 := h.src.Commit("c.txt", "three", "third")
	h.push("refs/heads/dev", wal.ZeroSHA, c3, h.src.Pack(c3, c2))
	h.push("refs/tags/v1", wal.ZeroSHA, c1, nil)
	h.push("HEAD", "ref: refs/heads/main", "ref: refs/heads/dev", nil)
	h.push("refs/tags/v1", c1, wal.ZeroSHA, nil)
	r, release = h.acquire(false)
	defer release()
	if r.Seq != 6 || h.localRef(r, "refs/heads/dev") != c3 || h.localRef(r, "refs/tags/v1") != "" {
		t.Fatalf("after catch-up: seq %d dev %s tag %s", r.Seq, h.localRef(r, "refs/heads/dev"), h.localRef(r, "refs/tags/v1"))
	}
	head, _ = h.cache.Git().Run(context.Background(), r.Dir, nil, "symbolic-ref", "HEAD")
	if strings.TrimSpace(string(head)) != "refs/heads/dev" {
		t.Fatalf("HEAD = %q", head)
	}
	if r.Index.DefaultBranch() != "dev" {
		t.Fatalf("default branch = %q", r.Index.DefaultBranch())
	}
}

func TestStateSurvivesAProcessRestart(t *testing.T) {
	h := newHarness(t)
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	r, release := h.acquire(true)
	release()
	dir := r.Dir

	// A new cache over the same disk reads origo.json and the index once.
	c, err := New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h.store.Calls["Get"] = 0
	r2, release, err := c.Acquire(context.Background(), repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if r2.Dir != dir || r2.Seq != 1 || r2.Index == nil || r2.Index.Seq != 1 {
		t.Fatalf("restarted: %+v", r2)
	}
	if h.store.Calls["Get"] != 1 {
		t.Fatalf("%d GETs to reload, want 1 (the index object)", h.store.Calls["Get"])
	}
	// A state file without a repository behind it is ignored.
	_ = os.RemoveAll(dir)
	c, _ = New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	r3, release, err := c.Acquire(context.Background(), repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if h.localRef(r3, "refs/heads/main") != c1 {
		t.Fatal("not rebuilt after the directory vanished")
	}
	// Garbage in the state file is ignored too.
	_ = os.WriteFile(c.stateFile(repoA), []byte("{"), 0o644)
	c, _ = New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	if _, release, err := c.Acquire(context.Background(), repoA, false); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

func TestCorruptCopyIsRebuiltFromTheLog(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.FsckEvery = 2 })
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	r, release := h.acquire(true)
	release()
	// Damage every pack, then verify: the copy is evicted.
	packs, _ := filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "*.pack"))
	if len(packs) == 0 {
		t.Fatal("no pack on disk")
	}
	damage(t, packs)
	if err := h.cache.Verify(context.Background(), r); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("verify: %v", err)
	}
	if r.Local {
		t.Fatal("corrupt copy not evicted")
	}
	r, release = h.acquire(false)
	if h.localRef(r, "refs/heads/main") != c1 || gittest.RevList(t, r.Dir) != gittest.RevList(t, h.src.Dir) {
		t.Fatal("not rebuilt from the log")
	}
	release()
	// The sampled check runs on every second write open and finds the
	// damage on its own.
	packs, _ = filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "*.pack"))
	damage(t, packs)
	_, release = h.acquire(true)
	release()
	if _, _, err := h.cache.Acquire(context.Background(), repoA, true); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("sampled fsck: %v", err)
	}
	r, release = h.acquire(true)
	release()
	if h.localRef(r, "refs/heads/main") != c1 {
		t.Fatal("not rebuilt after the sampled check")
	}
	// A corrupt pack that index-pack refuses during apply evicts too.
	c2 := h.src.Commit("b.txt", "two", "second")
	h.push("refs/heads/main", c1, c2, h.src.Pack(c2, c1))
	packs, _ = filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "*.pack"))
	damage(t, packs)
	if _, _, err := h.cache.Acquire(context.Background(), repoA, true); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("apply onto a corrupt copy: %v", err)
	}
	r, release = h.acquire(false)
	defer release()
	if h.localRef(r, "refs/heads/main") != c2 {
		t.Fatal("not rebuilt after a failed apply")
	}
}

// damage overwrites packs git wrote read-only.
func damage(t *testing.T, packs []string) {
	t.Helper()
	for _, p := range packs {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeletedAndMissingRepositories(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.cache.Acquire(ctx, "not-an-id", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid id: %v", err)
	}
	if _, _, err := h.cache.Acquire(ctx, "00000000-0000-4000-8000-000000000000", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	r, release := h.acquire(false)
	release()
	c, err := h.log.Commit(ctx, repoA, h.held, wal.Entry{Kind: wal.KindDelete, Deleted: true}, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	h.held = c.Index
	if _, _, err := h.cache.Acquire(ctx, repoA, false); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
		t.Fatal("deleted repository still on disk")
	}
	if _, _, err := h.cache.Acquire(ctx, repoA, true); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted, current holder: %v", err)
	}
	// An undelete brings it back from the log.
	c, err = h.log.Commit(ctx, repoA, h.held, wal.Entry{Kind: wal.KindPush}, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	h.held = c.Index
	r, release = h.acquire(false)
	defer release()
	if h.localRef(r, "refs/heads/main") != c1 {
		t.Fatal("not rebuilt after the undelete")
	}
}

func TestApplyRefusesABadEntry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c1 := h.src.Commit("a.txt", "one", "first")
	ix := h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	// Swap the stored entry's pack for other bytes: the digest fails.
	key := h.log.RepoPrefix(repoA) + ix.Entry
	rc, _, _ := h.store.Get(ctx, key, "")
	hdr, refs, _, _ := wal.ReadEntryHead(rc)
	_ = rc.Close()
	head, _ := wal.EncodeEntryHead(hdr, refs)
	_, _ = h.store.Put(ctx, key, wal.BytesBody(append(head, bytes.Repeat([]byte("x"), int(hdr.PackBytes))...)))
	if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil || !strings.Contains(err.Error(), "does not match the entry") {
		t.Fatalf("digest mismatch: %v", err)
	}
	// A header whose sequence disagrees with the index is refused.
	hdr.Seq = 9
	head, _ = wal.EncodeEntryHead(hdr, refs)
	_, _ = h.store.Put(ctx, key, wal.BytesBody(append(head, h.src.Pack(c1)...)))
	if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil || !strings.Contains(err.Error(), "carries seq") {
		t.Fatalf("seq mismatch: %v", err)
	}
	// An entry that vanished is reported.
	_ = h.store.Delete(ctx, key)
	if _, _, err := h.cache.Acquire(ctx, repoA, false); !errors.Is(err, wal.ErrNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	// Garbage where the entry was is refused by the parser.
	_, _ = h.store.Put(ctx, key, wal.BytesBody([]byte("garbage")))
	if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil {
		t.Fatal("garbage entry accepted")
	}
	// A truncated pack is refused before git sees it.
	hdr.Seq = 1
	head, _ = wal.EncodeEntryHead(hdr, refs)
	_, _ = h.store.Put(ctx, key, wal.BytesBody(append(head, h.src.Pack(c1)[:10]...)))
	if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil || !strings.Contains(err.Error(), "does not match the entry") {
		t.Fatalf("short pack: %v", err)
	}
}

// compact builds a pack holding want with its .idx the way compaction
// would, stores both under the log's pack keys, and commits a compact
// entry that lists the pack and folds every entry so far. It returns the
// hash git named the pack by.
func (h *harness) compact(want string) string {
	h.t.Helper()
	ctx := context.Background()
	pack := h.src.Pack(want)
	tmp := h.t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "p.pack"), pack, 0o644); err != nil {
		h.t.Fatal(err)
	}
	name := strings.TrimSpace(gittest.Run(h.t, tmp, nil, "index-pack", filepath.Join(tmp, "p.pack")))
	name = strings.TrimPrefix(name, "pack\t")
	idx, _ := os.ReadFile(filepath.Join(tmp, "p.idx"))
	key := "packs/" + name + ".pack"
	_, _ = h.store.Put(ctx, h.log.RepoPrefix(repoA)+key, wal.BytesBody(pack))
	_, _ = h.store.Put(ctx, h.log.RepoPrefix(repoA)+strings.TrimSuffix(key, ".pack")+".idx", wal.BytesBody(idx))
	e := wal.Entry{Kind: wal.KindCompact, Packs: []string{key}, PacksBytes: int64(len(pack)), CompactedThrough: h.held.Seq}
	c, err := h.log.Commit(ctx, repoA, h.held, e, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		h.t.Fatal(err)
	}
	h.held = c.Index
	return name
}

// packFiles returns the paths of the fetched pack and its .idx, named
// the way git reads them: pack-<hash>.pack and pack-<hash>.idx.
func packFiles(r *Repo, name string) (pack, idx string) {
	dir := filepath.Join(r.Dir, "objects", "pack")
	return filepath.Join(dir, "pack-"+name+".pack"), filepath.Join(dir, "pack-"+name+".idx")
}

// verifyPack fails the test unless git reads the pack under its name.
func (h *harness) verifyPack(r *Repo, name string) {
	h.t.Helper()
	pack, idx := packFiles(r, name)
	for _, f := range []string{pack, idx} {
		if _, err := os.Stat(f); err != nil {
			h.t.Fatalf("fetched pack: %v", err)
		}
	}
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "verify-pack", idx); err != nil {
		h.t.Fatalf("git does not read the fetched pack: %v", err)
	}
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "fsck", "--connectivity-only", "--no-progress"); err != nil {
		h.t.Fatalf("fsck after the fetch: %v", err)
	}
}

func TestCompactionPacksAreFetched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	name := h.compact(c1)
	key := "packs/" + name + ".pack"
	r, release := h.acquire(false)
	// The file is named the way git reads it, and only that way: the
	// log key's base name is not a file git opens.
	h.verifyPack(r, name)
	if _, err := os.Stat(filepath.Join(r.Dir, "objects", "pack", name+".pack")); err == nil {
		t.Fatalf("pack written under the log key's base name %s.pack", name)
	}
	if h.localRef(r, "refs/heads/main") != c1 {
		t.Fatal("refs not reconciled from the index")
	}
	release()
	// A fresh node builds from the packs alone: the compaction pack is
	// its only source of objects, so fsck passes only when git reads it.
	h.cache.Evict(repoA)
	r, release = h.acquire(false)
	if r.Seq != 2 || h.localRef(r, "refs/heads/main") != c1 {
		t.Fatalf("from packs: seq %d ref %s", r.Seq, h.localRef(r, "refs/heads/main"))
	}
	h.verifyPack(r, name)
	if packs, _ := filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "*.pack")); len(packs) != 1 {
		t.Fatalf("packs on a copy built from the compaction pack alone: %v", packs)
	}
	release()
	// A missing pack fails the materialization.
	h.cache.Evict(repoA)
	_ = h.store.Delete(ctx, h.log.RepoPrefix(repoA)+strings.TrimSuffix(key, ".pack")+".idx")
	if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil || !strings.Contains(err.Error(), "pack") {
		t.Fatalf("missing pack: %v", err)
	}
}

// A copy that is current and above compacted_through loses a pack file;
// the next apply restores it, because every listed pack missing on disk
// is fetched whatever the copy holds.
func TestPacksAreFetchedForACurrentCopy(t *testing.T) {
	h := newHarness(t)
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	name := h.compact(c1)
	// Built from the compaction pack alone, so that pack is the only
	// source of objects the copy has.
	r, release := h.acquire(false)
	h.verifyPack(r, name)
	release()
	packFile, _ := packFiles(r, name)
	if err := os.Chmod(packFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(packFile); err != nil {
		t.Fatal(err)
	}
	// Another writer commits a push with no pack. The copy is at 2 with
	// compacted_through 1, so the pack is fetched only because it is
	// missing.
	h.push("refs/tags/v1", wal.ZeroSHA, c1, nil)
	var packGets int
	h.store.SetFault(func(op, key string) error {
		if op == "Get" && strings.Contains(key, "/packs/") {
			packGets++
		}
		return nil
	})
	r, release = h.acquire(false)
	release()
	if r.Seq != 3 || h.localRef(r, "refs/tags/v1") != c1 {
		t.Fatalf("after the push: seq %d tag %s", r.Seq, h.localRef(r, "refs/tags/v1"))
	}
	h.verifyPack(r, name)
	if packGets != 2 {
		t.Fatalf("%d pack GETs to restore the pack, want the .idx and the .pack", packGets)
	}
	// With the pack back on disk, a further apply pays no GET for it:
	// a listed pack the copy holds costs one stat.
	h.push("refs/tags/v2", wal.ZeroSHA, c1, nil)
	packGets = 0
	r, release = h.acquire(false)
	defer release()
	if r.Seq != 4 || packGets != 0 {
		t.Fatalf("after a further push: seq %d, %d pack GETs for a pack on disk", r.Seq, packGets)
	}
}

func TestReadersUpgradeAndWritersAdvance(t *testing.T) {
	h := newHarness(t)
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	// Many readers open a cold repository at once; one materializes.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, release, err := h.cache.Acquire(context.Background(), repoA, false)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if r.Seq != 1 {
				t.Errorf("seq %d", r.Seq)
			}
		})
	}
	wg.Wait()
	if h.cache.materialized.Value(nil) != 1 {
		t.Fatalf("materialized %d times", h.cache.materialized.Value(nil))
	}
	// A writer that applied through git's own path, as a served push
	// does, advances the record without a second apply.
	r, release := h.acquire(true)
	c2 := h.src.Commit("b.txt", "two", "second")
	pack := h.src.Pack(c2, c1)
	next := h.push("refs/heads/main", c1, c2, pack)
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, bytes.NewReader(pack), "index-pack", "--stdin", "--fix-thin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "update-ref", "refs/heads/main", c2); err != nil {
		t.Fatal(err)
	}
	if err := h.cache.Advance(r, next); err != nil {
		t.Fatal(err)
	}
	release()
	h.store.Calls["Head"] = 0
	r, release = h.acquire(false)
	release()
	if r.Seq != 2 || h.store.Calls["Head"] != 1 {
		t.Fatalf("after advance: seq %d, %d HEADs", r.Seq, h.store.Calls["Head"])
	}
	// A reference the disk holds that the index does not is removed by
	// the next apply, and one that differs is set: the index is the
	// authority.
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "update-ref", "refs/heads/stray", c1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cache.Git().Run(context.Background(), r.Dir, nil, "update-ref", "refs/heads/main", c1); err != nil {
		t.Fatal(err)
	}
	c3 := h.src.Commit("c.txt", "three", "third")
	h.push("refs/heads/dev", wal.ZeroSHA, c3, h.src.Pack(c3, c2))
	r, release = h.acquire(false)
	defer release()
	if h.localRef(r, "refs/heads/main") != c2 || h.localRef(r, "refs/heads/dev") != c3 || h.localRef(r, "refs/heads/stray") != "" {
		t.Fatalf("reconcile: main %s dev %s stray %s", h.localRef(r, "refs/heads/main"), h.localRef(r, "refs/heads/dev"), h.localRef(r, "refs/heads/stray"))
	}
}

func TestStoreAndGitFailuresSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c1 := h.src.Commit("a.txt", "one", "first")
	h.push("refs/heads/main", wal.ZeroSHA, c1, h.src.Pack(c1))
	h.store.Fault = func(op, key string) error {
		if op == "Head" {
			return errors.New("storage down")
		}
		return nil
	}
	if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil || !strings.Contains(err.Error(), "storage down") {
		t.Fatalf("head failure: %v", err)
	}
	h.store.Fault = nil
	r, release := h.acquire(true)
	release()
	// The index reload after a restart can fail.
	c, _ := New(Options{Dir: h.cache.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	h.store.Fault = func(op, key string) error {
		if op == "Get" {
			return errors.New("get down")
		}
		return nil
	}
	if _, _, err := c.Acquire(ctx, repoA, false); err == nil || !strings.Contains(err.Error(), "get down") {
		t.Fatalf("index reload: %v", err)
	}
	h.store.Fault = nil
	// A data directory that cannot be written fails the apply.
	if os.Getuid() != 0 {
		c2 := h.src.Commit("b.txt", "two", "second")
		h.push("refs/heads/main", c1, c2, h.src.Pack(c2, c1))
		_ = os.Chmod(filepath.Join(h.cache.dir, "repos"), 0o500)
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.cache.dir, "repos"), 0o755) })
		if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil {
			t.Fatal("unwritable state accepted")
		}
		_ = os.Chmod(filepath.Join(h.cache.dir, "repos"), 0o755)
		h.cache.Evict(repoA)
		_ = os.Chmod(filepath.Join(h.cache.dir, "repos"), 0o500)
		if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil {
			t.Fatal("unwritable init accepted")
		}
		_ = os.Chmod(filepath.Join(h.cache.dir, "repos"), 0o755)
		_ = os.Chmod(h.cache.SpoolDir(), 0o500)
		t.Cleanup(func() { _ = os.Chmod(h.cache.SpoolDir(), 0o755) })
		if _, _, err := h.cache.Acquire(ctx, repoA, false); err == nil {
			t.Fatal("unwritable spool accepted")
		}
		_ = os.Chmod(h.cache.SpoolDir(), 0o755)
	}
	_ = r
	// A git failure that is not corruption is reported as it is.
	r, release = h.acquire(true)
	release()
	_ = os.WriteFile(filepath.Join(r.Dir, "config"), []byte("[core\n"), 0o644)
	h.push("refs/heads/x", wal.ZeroSHA, c1, nil)
	if _, _, err := h.cache.Acquire(ctx, repoA, true); err == nil || errors.Is(err, ErrCorrupt) {
		t.Fatalf("broken config: %v", err)
	}
	if _, err := h.cache.Git().Run(ctx, r.Dir, nil, "rev-parse", "HEAD"); err == nil {
		t.Fatal("git ran on a broken config")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	l := wal.New(wal.Options{Store: wal.NewMemStore()})
	if _, err := New(Options{}); err == nil {
		t.Fatal("empty options accepted")
	}
	if _, err := New(Options{Dir: t.TempDir(), Log: l, GitBin: "no-such-git-binary"}); err == nil {
		t.Fatal("missing git accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	if _, err := New(Options{Dir: filepath.Join(file, "x"), Log: l}); err == nil {
		t.Fatal("unusable dir accepted")
	}
	c, err := New(Options{Dir: t.TempDir(), Log: l, GitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c.Git().Timeout != time.Second || c.fsckEvery != 256 {
		t.Fatalf("options not applied: %+v", c)
	}
}

func TestGitErrorsAndCorruptionMarkers(t *testing.T) {
	g := &Git{Bin: "git", Home: t.TempDir(), Timeout: time.Second}
	dir := t.TempDir()
	if _, err := g.Run(context.Background(), dir, nil, "init", "-q", "--bare", "."); err != nil {
		t.Fatal(err)
	}
	_, err := g.Run(context.Background(), dir, nil, "rev-parse", "--verify", "nope")
	var ge *Error
	if !errors.As(err, &ge) || ge.Stderr == "" || !strings.Contains(err.Error(), "rev-parse") {
		t.Fatalf("err = %v", err)
	}
	if IsCorruption(err) || IsCorruption(errors.New("plain")) {
		t.Fatal("a missing revision is not corruption")
	}
	if !IsCorruption(&Error{Stderr: "fatal: bad object HEAD"}) || !IsCorruption(&Error{Stderr: "error: packfile x is corrupt"}) {
		t.Fatal("corruption markers not recognised")
	}
	if !errors.Is(ge, ge.Err) {
		t.Fatal("Unwrap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Run(ctx, dir, nil, "rev-parse", "HEAD"); err == nil {
		t.Fatal("cancelled context ran")
	}
	if err := writeAtomic(filepath.Join(dir, "objects", "x", "y"), strings.NewReader("z")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, "HEAD", "y"), strings.NewReader("z")); err == nil {
		t.Fatal("file as directory accepted")
	}
	failing := &failingReader{}
	if err := writeAtomic(filepath.Join(dir, "objects", "f"), failing); err == nil {
		t.Fatal("failed copy accepted")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
