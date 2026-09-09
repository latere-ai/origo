// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSweepRemovesOrphansAndKeepsWhatAnIndexNames(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store.SetClock(clock)
	l := New(Options{Store: store, Now: clock, Logger: slog.New(slog.DiscardHandler)})
	base := createRepo(t, l, repoA)
	c, err := l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	// An orphan at a taken sequence (a lost round) and one above the
	// newest (a writer that died before its index).
	lost := l.key(repoA, EntryKey(1, "deadbeefdeadbeef"))
	inflight := l.key(repoA, EntryKey(2, "deadbeefdeadbeef"))
	for _, k := range []string{lost, inflight} {
		if _, err := store.Put(ctx, k, BytesBody([]byte("orphan"))); err != nil {
			t.Fatal(err)
		}
	}
	// An LFS object and its marker (spec 010): the sweeper never touches
	// lfs/, whatever its age, and the purge takes it with the prefix.
	lfsObject := l.key(repoA, "lfs/"+strings.Repeat("ab", 32))
	lfsMarker := l.key(repoA, "lfs/verified/"+strings.Repeat("ab", 32))
	for _, k := range []string{lfsObject, lfsMarker} {
		if _, err := store.Put(ctx, k, BytesBody([]byte("lfs"))); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := l.Sweep(ctx, repoA, time.Hour)
	if err != nil || len(rep.Deleted) != 0 {
		t.Fatalf("young orphans swept: %+v, %v", rep, err)
	}
	now = now.Add(2 * time.Hour)
	rep, err = l.Sweep(ctx, repoA, time.Hour)
	if err != nil || rep.Orphans != 2 || len(rep.Deleted) != 2 {
		t.Fatalf("sweep: %+v, %v", rep, err)
	}
	if _, err := store.Head(ctx, l.key(repoA, c.Key)); err != nil {
		t.Fatal("the committed entry was swept")
	}
	for _, k := range []string{lfsObject, lfsMarker} {
		if _, err := store.Head(ctx, k); err != nil {
			t.Fatalf("the sweeper removed %s", k)
		}
	}
	for _, k := range []string{lost, inflight} {
		if _, err := store.Head(ctx, k); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s survived", k)
		}
	}
	// Folded entries go once compaction moved past them; every index
	// object stays, whatever compacted_through says, so a warm node
	// holding index n still learns of index n+1 from a HEAD (spec 006).
	next := c.Index
	for i := 2; i <= 70; i++ {
		c, err := l.Commit(ctx, repoA, next, push(fmt.Sprintf("refs/heads/b%d", i), ZeroSHA, sha(i)), noCatchUp)
		if err != nil {
			t.Fatal(err)
		}
		next = c.Index
	}
	c2, err := l.Commit(ctx, repoA, next, Entry{Kind: KindCompact, Packs: []string{"packs/" + sha(1) + ".pack"}, CompactedThrough: 70}, noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	rep, err = l.Sweep(ctx, repoA, time.Hour)
	if err != nil || rep.Folded != 70 {
		t.Fatalf("after compaction: %+v, %v", rep, err)
	}
	// index/0 to index/71 exist and all 72 stay.
	if n := countKeys(store, "/index/0"); n != 72 {
		t.Fatalf("%d index objects kept, want 72", n)
	}
	if _, err := store.Head(ctx, l.key(repoA, c2.Key)); err != nil {
		t.Fatal("the compaction entry was swept")
	}
	// A deleted repository is purged after the hold, name included.
	c3, err := l.Commit(ctx, repoA, c2.Index, Entry{Kind: KindDelete, Deleted: true}, noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(DeleteHold - time.Minute)
	if rep, err := l.Sweep(ctx, repoA, time.Hour); err != nil || rep.Purged {
		t.Fatalf("purged inside the hold: %+v, %v", rep, err)
	}
	// An undelete inside the hold cancels the purge.
	c4, err := l.Commit(ctx, repoA, c3.Index, Entry{Kind: KindPush}, noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(DeleteHold)
	if rep, err := l.Sweep(ctx, repoA, time.Hour); err != nil || rep.Purged {
		t.Fatalf("purged after an undelete: %+v, %v", rep, err)
	}
	if _, err := l.Commit(ctx, repoA, c4.Index, Entry{Kind: KindDelete, Deleted: true}, noCatchUp); err != nil {
		t.Fatal(err)
	}
	now = now.Add(DeleteHold)
	rep, err = l.Sweep(ctx, repoA, time.Hour)
	if err != nil || !rep.Purged {
		t.Fatalf("purge: %+v, %v", rep, err)
	}
	// The purge removes lfs/ with the rest of the prefix (spec 010).
	for _, k := range []string{lfsObject, lfsMarker} {
		if _, err := store.Head(ctx, k); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s survived the purge", k)
		}
	}
	// meta is the tombstone of spec 019 and is the one key left.
	if keys := store.Keys(); len(keys) != 1 || keys[0] != l.metaKey(repoA) {
		t.Fatalf("%d keys survived the purge: %v", len(keys), keys)
	}
	if _, err := l.Resolve(ctx, "acme", "app-"+repoA[:4]); !errors.Is(err, ErrNotFound) {
		t.Fatal("name survived the purge")
	}
	if rep, err := l.Sweep(ctx, repoA, time.Hour); err != nil || len(rep.Deleted) != 0 {
		t.Fatalf("sweep of a gone repository: %+v, %v", rep, err)
	}
}

// TestSweepRemovesUnlistedPacks is spec 006's sweeper rule: a pack the
// newest index does not list goes once it is older than
// ORIGO_SWEEP_MIN_AGE, with its .idx, and a listed one is kept whatever
// its age. The age is what lets a node that started materializing on the
// previous index finish.
func TestSweepRemovesUnlistedPacks(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store.SetClock(clock)
	l := New(Options{Store: store, Now: clock, Logger: slog.New(slog.DiscardHandler)})
	base := createRepo(t, l, repoA)

	kept, gone := "packs/"+sha(1)+".pack", "packs/"+sha(2)+".pack"
	for _, k := range []string{kept, gone, strings.TrimSuffix(kept, ".pack") + ".idx", strings.TrimSuffix(gone, ".pack") + ".idx"} {
		if _, err := store.Put(ctx, l.key(repoA, k), BytesBody([]byte("pack"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Commit(ctx, repoA, base, Entry{Kind: KindCompact, Packs: []string{kept}, CompactedThrough: 0}, noCatchUp); err != nil {
		t.Fatal(err)
	}
	// Young: nothing goes, so a reader that holds the previous index can
	// still finish from the pack it names.
	if rep, err := l.Sweep(ctx, repoA, time.Hour); err != nil || rep.Packs != 0 {
		t.Fatalf("young packs swept: %+v, %v", rep, err)
	}
	now = now.Add(2 * time.Hour)
	rep, err := l.Sweep(ctx, repoA, time.Hour)
	if err != nil || rep.Packs != 2 {
		t.Fatalf("sweep: %+v, %v", rep, err)
	}
	for _, k := range []string{kept, strings.TrimSuffix(kept, ".pack") + ".idx"} {
		if _, err := store.Head(ctx, l.key(repoA, k)); err != nil {
			t.Fatalf("the newest index lists %s and it was swept", k)
		}
	}
	for _, k := range []string{gone, strings.TrimSuffix(gone, ".pack") + ".idx"} {
		if _, err := store.Head(ctx, l.key(repoA, k)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s survived", k)
		}
	}
	store.SetFault(func(op, key string) error {
		if op == "List" && strings.Contains(key, "/packs/") {
			return errors.New("pack list refused")
		}
		return nil
	})
	if _, err := l.Sweep(ctx, repoA, time.Hour); err == nil {
		t.Fatal("pack listing failure hidden")
	}
}

func TestSweepAllVisitsEveryRepositoryAndReportsFailures(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	l := New(Options{Store: store, Now: func() time.Time { return now.Add(3 * time.Hour) }, Logger: slog.New(slog.DiscardHandler)})
	for _, id := range []string{repoA, repoB} {
		createRepo(t, l, id)
		if _, err := store.Put(ctx, l.key(id, EntryKey(5, "deadbeefdeadbeef")), BytesBody([]byte("orphan"))); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := l.Repos(ctx)
	if err != nil || strings.Join(ids, ",") != repoA+","+repoB {
		t.Fatalf("repos = %v, %v", ids, err)
	}
	if err := l.SweepAll(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := countKeys(store, "/wal/"); n != 0 {
		t.Fatalf("%d orphans survived", n)
	}
	store.Fault = func(op, key string) error {
		if op == "Delete" {
			return errors.New("delete refused")
		}
		return nil
	}
	_, _ = store.Put(ctx, l.key(repoA, EntryKey(5, "deadbeefdeadbeef")), BytesBody([]byte("orphan")))
	rep, err := l.Sweep(ctx, repoA, time.Hour)
	if err != nil || len(rep.Warnings) != 1 {
		t.Fatalf("delete refusal: %+v, %v", rep, err)
	}
	store.Fault = func(op, key string) error {
		if op == "List" && strings.Contains(key, "/wal/") {
			return errors.New("list refused")
		}
		return nil
	}
	if err := l.SweepAll(ctx, time.Hour); err == nil || !strings.Contains(err.Error(), "list refused") {
		t.Fatalf("sweep all: %v", err)
	}
	store.Fault = func(op, key string) error {
		if op == "List" {
			return errors.New("list refused")
		}
		return nil
	}
	if err := l.SweepAll(ctx, time.Hour); err == nil {
		t.Fatal("repository listing failure hidden")
	}
	store.Fault = func(op, key string) error {
		if op == "Head" {
			return errors.New("head refused")
		}
		return nil
	}
	if _, err := l.Sweep(ctx, repoA, time.Hour); err == nil {
		t.Fatal("newest failure hidden")
	}
	store.Fault = nil
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := l.SweepAll(cctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sweep: %v", err)
	}
	// A purge whose deletes fail reports the failure.
	c, err := l.Commit(ctx, repoA, mustNewest(t, l, repoA), Entry{Kind: KindDelete, Deleted: true}, noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	afterHold := c.Index.DeletedAt.Add(DeleteHold)
	l = New(Options{Store: store, Now: func() time.Time { return afterHold }, Logger: slog.New(slog.DiscardHandler)})
	store.Fault = func(op, key string) error {
		if op == "Delete" {
			return errors.New("delete refused")
		}
		return nil
	}
	if _, err := l.Sweep(ctx, repoA, time.Hour); err == nil {
		t.Fatal("purge failure hidden")
	}
	store.Fault = func(op, key string) error {
		if op == "Delete" && !strings.Contains(key, "names/") {
			return errors.New("delete refused")
		}
		return nil
	}
	if _, err := l.Sweep(ctx, repoA, time.Hour); err == nil {
		t.Fatal("purge object failure hidden")
	}
	store.Fault = func(op, key string) error {
		if op == "List" && strings.HasSuffix(key, repoA+"/") {
			return errors.New("list refused")
		}
		return nil
	}
	if _, err := l.Sweep(ctx, repoA, time.Hour); err == nil {
		t.Fatal("purge listing failure hidden")
	}
	// Repos pages through many prefixes.
	big := NewMemStore()
	l = newTestLog(t, big)
	for i := range 1200 {
		_, _ = big.Put(ctx, l.key(fmt.Sprintf("%08x-0000-4000-8000-000000000000", i), "meta"), BytesBody(nil))
	}
	if ids, err := l.Repos(ctx); err != nil || len(ids) != 1200 {
		t.Fatalf("paged repos: %d, %v", len(ids), err)
	}
}

func mustNewest(t *testing.T, l *Log, repo string) *Index {
	t.Helper()
	ix, _, err := l.Newest(context.Background(), repo, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// TestPurgeLeavesATombstone is spec 019's rule for the purge: every
// object of the repository goes except meta, which is rewritten with
// purged_at and keeps the id taken forever, while the name is deleted
// so a consumer reuses it.
func TestPurgeLeavesATombstone(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store.SetClock(clock)
	l := New(Options{Store: store, Now: clock, Logger: slog.New(slog.DiscardHandler)})
	base := createRepo(t, l, repoA)
	m, err := l.ReadMeta(ctx, repoA)
	if err != nil {
		t.Fatal(err)
	}
	owner, slug := m.Owner, m.Slug
	if _, err := l.Commit(ctx, repoA, base, Entry{Kind: KindDelete, Deleted: true}, noCatchUp); err != nil {
		t.Fatal(err)
	}
	now = now.Add(DeleteHold)
	purgedAt := now
	rep, err := l.Sweep(ctx, repoA, time.Hour)
	if err != nil || !rep.Purged {
		t.Fatalf("purge: %+v, %v", rep, err)
	}
	// Exactly meta is left, and it carries purged_at.
	keys := store.Keys()
	if len(keys) != 1 || keys[0] != l.metaKey(repoA) {
		t.Fatalf("keys after the purge: %v", keys)
	}
	tomb, err := l.ReadMeta(ctx, repoA)
	if err != nil {
		t.Fatalf("the tombstone is unreadable: %v", err)
	}
	if tomb.PurgedAt == nil || !tomb.PurgedAt.Equal(purgedAt.UTC()) {
		t.Fatalf("purged_at %v, want %v", tomb.PurgedAt, purgedAt.UTC())
	}
	if tomb.Owner != owner || tomb.Slug != slug {
		t.Fatalf("the tombstone lost its labels: %+v", tomb)
	}
	// The index is gone, so the repository has no state to serve.
	if _, _, err := l.Newest(ctx, repoA, 0, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("newest after the purge: %v", err)
	}
	// The id stays taken and the name is free.
	if _, err := l.CreateRepo(ctx, Meta{ID: repoA, Owner: owner, Slug: slug}, "main"); !errors.Is(err, ErrExists) {
		t.Fatalf("create of a purged id: %v", err)
	}
	if _, err := l.Resolve(ctx, owner, slug); !errors.Is(err, ErrNotFound) {
		t.Fatal("the name survived the purge")
	}
	if _, err := l.CreateRepo(ctx, Meta{ID: repoB, Owner: owner, Slug: slug}, "main"); err != nil {
		t.Fatalf("the name is not reusable: %v", err)
	}
	// A second sweep of a purged repository does nothing.
	if rep, err := l.Sweep(ctx, repoA, time.Hour); err != nil || rep.Purged || len(rep.Deleted) != 0 {
		t.Fatalf("second sweep: %+v, %v", rep, err)
	}
}
