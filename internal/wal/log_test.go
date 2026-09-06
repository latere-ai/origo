// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/metrics"
)

const repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

func sha(n int) string { return fmt.Sprintf("%040x", n) }

func noSleep(context.Context, time.Duration) {}

func newTestLog(t *testing.T, store Store) *Log {
	t.Helper()
	return New(Options{Store: store, Logger: slog.New(slog.DiscardHandler), Sleep: noSleep})
}

func createRepo(t *testing.T, l *Log, id string) *Index {
	t.Helper()
	ix, err := l.CreateRepo(context.Background(), Meta{ID: id, Owner: "acme", Slug: "app-" + id[:4]}, "main")
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func push(ref, old, new string) Entry {
	return Entry{Kind: KindPush, Subject: "alice", Refs: []RefUpdate{{Ref: ref, Old: old, New: new}}, Pack: BytesBody([]byte("PACK" + new))}
}

func noCatchUp(context.Context, *Index) error { return nil }

func TestCommitWritesOneEntryAndOneIndex(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	reg := metrics.New()
	l := New(Options{Store: store, Metrics: reg, Now: func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }})
	base := createRepo(t, l, repoA)
	c, err := l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Index.Seq != 1 || c.Index.Refs["refs/heads/main"] != sha(1) || c.Index.Entry != c.Key || c.Index.SizeBytes != int64(len("PACK"+sha(1))) {
		t.Fatalf("index = %+v", c.Index)
	}
	keys := store.Keys()
	var entries, indexes int
	for _, k := range keys {
		if strings.Contains(k, "/wal/") {
			entries++
		}
		if strings.Contains(k, "/index/0") {
			indexes++
		}
	}
	if entries != 1 || indexes != 2 {
		t.Fatalf("keys = %v", keys)
	}
	// The entry round-trips: header, transaction, pack.
	rc, _, err := store.Get(ctx, l.key(repoA, c.Key), "")
	if err != nil {
		t.Fatal(err)
	}
	h, refs, pack, err := ReadEntryHead(rc)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(pack)
	_ = rc.Close()
	if h.Seq != 1 || h.Kind != KindPush || h.Subject != "alice" || h.PackBytes != int64(len(data)) || h.PackSHA256 != BytesBody(data).SHA256 || refs[0].New != sha(1) || string(data) != "PACK"+sha(1) {
		t.Fatalf("entry: %+v %+v %q", h, refs, data)
	}
	if hint, err := l.Hint(ctx, repoA); err != nil || hint != 1 {
		t.Fatalf("hint = %d, %v", hint, err)
	}
	if ix, err := l.ReadIndex(ctx, repoA, 1); err != nil || ix.Seq != 1 {
		t.Fatalf("read index: %+v, %v", ix, err)
	}
	if reg.Counter("origo_wal_commits_total", "").Value() != 1 {
		t.Fatal("commit not counted")
	}
	// A second push builds on the first and lists both entries.
	c2, err := l.Commit(ctx, repoA, c.Index, push("refs/heads/dev", ZeroSHA, sha(2)), noCatchUp)
	if err != nil || c2.Index.Seq != 2 || len(c2.Index.Entries) != 2 || c2.Index.Entries[0].Key != c.Key {
		t.Fatalf("second commit: %+v, %v", c2, err)
	}
	// A delete of a reference removes it from the map; a delete entry
	// marks the repository; an entry with Deleted false clears it.
	c3, err := l.Commit(ctx, repoA, c2.Index, push("refs/heads/dev", sha(2), ZeroSHA), noCatchUp)
	if err != nil || c3.Index.Refs["refs/heads/dev"] != "" {
		t.Fatalf("ref delete: %+v, %v", c3, err)
	}
	c4, err := l.Commit(ctx, repoA, c3.Index, Entry{Kind: KindDelete, Deleted: true}, noCatchUp)
	if err != nil || c4.Index.DeletedAt == nil {
		t.Fatalf("delete: %+v, %v", c4, err)
	}
	c5, err := l.Commit(ctx, repoA, c4.Index, Entry{Kind: KindPush}, noCatchUp)
	if err != nil || c5.Index.DeletedAt != nil {
		t.Fatalf("undelete: %+v, %v", c5, err)
	}
	// Compaction replaces the pack list and folds the entries.
	c6, err := l.Commit(ctx, repoA, c5.Index, Entry{Kind: KindCompact, Packs: []string{"packs/" + sha(9) + ".pack"}, CompactedThrough: 5}, noCatchUp)
	if err != nil || c6.Index.CompactedThrough != 5 || len(c6.Index.Entries) != 1 || len(c6.Index.Packs) != 1 {
		t.Fatalf("compact: %+v, %v", c6.Index, err)
	}
}

func TestCommitRefusesAMovedReference(t *testing.T) {
	ctx := context.Background()
	l := newTestLog(t, NewMemStore())
	base := createRepo(t, l, repoA)
	c, err := l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	var conflict *ConflictError
	_, err = l.Commit(ctx, repoA, c.Index, push("refs/heads/main", ZeroSHA, sha(2)), noCatchUp)
	if !errors.As(err, &conflict) || conflict.Ref != "refs/heads/main" || conflict.Actual != sha(1) || !strings.Contains(err.Error(), "expected 000000000000") {
		t.Fatalf("err = %v", err)
	}
	// Stale base: the writer holds index 0 while the log is at 1 on the
	// same reference. The retry applies the winner and then refuses.
	caught := 0
	_, err = l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(3)), func(_ context.Context, ix *Index) error { caught = int(ix.Seq); return nil })
	if !errors.As(err, &conflict) || caught != 1 {
		t.Fatalf("stale base: %v, caught %d", err, caught)
	}
	// Stale base on a different reference lands at the next sequence.
	c2, err := l.Commit(ctx, repoA, base, push("refs/heads/dev", ZeroSHA, sha(4)), func(context.Context, *Index) error { return nil })
	if err != nil || c2.Index.Seq != 2 || c2.Index.Refs["refs/heads/main"] != sha(1) || c2.Index.Refs["refs/heads/dev"] != sha(4) {
		t.Fatalf("retry: %+v, %v", c2, err)
	}
	// A catch-up that fails stops the commit.
	boom := errors.New("apply failed")
	if _, err := l.Commit(ctx, repoA, base, push("refs/heads/x", ZeroSHA, sha(5)), func(context.Context, *Index) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("catch-up failure: %v", err)
	}
}

func TestCommitSurvivesALostResponse(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	l := newTestLog(t, store)
	base := createRepo(t, l, repoA)
	// The create of index/1 is applied but its response is lost: the
	// writer reads it back, finds its own entry, and reports success.
	store.Fault = func(op, key string) error {
		if op == "Create" && strings.HasSuffix(key, IndexKey(1)) {
			store.Fault = nil
			return ErrLostResponse
		}
		return nil
	}
	c, err := l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp)
	if err != nil || c.Index.Seq != 1 {
		t.Fatalf("lost response: %+v, %v", c, err)
	}
	// A lost response on a create that was not applied, with nothing
	// stored, is reported as the transport failure it was.
	store.Fault = func(op, key string) error {
		if op == "Create" && strings.HasSuffix(key, IndexKey(2)) {
			store.Fault = nil
			return errors.New("connection reset")
		}
		return nil
	}
	if _, err := l.Commit(ctx, repoA, c.Index, push("refs/heads/dev", ZeroSHA, sha(2)), noCatchUp); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("unapplied lost create: %v", err)
	}
	// The orphaned entry stays behind for the sweeper.
	if n := countKeys(store, "/wal/"); n != 2 {
		t.Fatalf("%d entries, want the committed one and the orphan", n)
	}
	// A 412 whose object cannot be read is an error, not a retry.
	if _, err := store.Create(ctx, l.key(repoA, IndexKey(2)), BytesBody([]byte("garbage"))); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Commit(ctx, repoA, c.Index, push("refs/heads/dev", ZeroSHA, sha(2)), noCatchUp); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("unreadable winner: %v", err)
	}
}

func countKeys(store *MemStore, sub string) int {
	n := 0
	for _, k := range store.Keys() {
		if strings.Contains(k, sub) {
			n++
		}
	}
	return n
}

func TestCommitFailpointAndWriteFailures(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	killed := errors.New("killed")
	l := New(Options{Store: store, Failpoint: func(name string) error {
		if name == FailpointBeforeIndex {
			return killed
		}
		return nil
	}})
	base := createRepo(t, l, repoA)
	if _, err := l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp); !errors.Is(err, killed) {
		t.Fatalf("failpoint: %v", err)
	}
	if countKeys(store, "/wal/") != 1 || countKeys(store, IndexKey(1)) != 0 {
		t.Fatalf("keys = %v", store.Keys())
	}
	l = newTestLog(t, store)
	store.Fault = func(op, key string) error {
		if op == "Put" && strings.Contains(key, "/wal/") {
			return errors.New("disk full")
		}
		return nil
	}
	if _, err := l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("entry write failure: %v", err)
	}
	store.Fault = nil
	// A pack that cannot be opened fails before anything is written.
	e := push("refs/heads/main", ZeroSHA, sha(1))
	e.Pack.Open = func() (io.ReadCloser, error) { return nil, errors.New("no pack") }
	if _, err := l.Commit(ctx, repoA, base, e, noCatchUp); err == nil || !strings.Contains(err.Error(), "no pack") {
		t.Fatalf("unopenable pack: %v", err)
	}
	// Sequence exhaustion is refused.
	full := &Index{V: 1, Seq: maxSeq, Refs: map[string]string{}}
	if _, err := l.Commit(ctx, repoA, full, Entry{Kind: KindPush}, noCatchUp); err == nil {
		t.Fatal("sequence overflow accepted")
	}
	// Too many lost rounds ends in ErrContended.
	l = New(Options{Store: store, MaxCommitAttempts: 2, Sleep: noSleep})
	c, _ := l.Commit(ctx, repoA, base, push("refs/heads/main", ZeroSHA, sha(1)), noCatchUp)
	next := c.Index
	for i := 2; i <= 3; i++ {
		c, err := l.Commit(ctx, repoA, next, push(fmt.Sprintf("refs/heads/b%d", i), ZeroSHA, sha(i)), noCatchUp)
		if err != nil {
			t.Fatal(err)
		}
		next = c.Index
	}
	if _, err := l.Commit(ctx, repoA, base, push("refs/heads/z", ZeroSHA, sha(9)), noCatchUp); !errors.Is(err, ErrContended) {
		t.Fatalf("contended: %v", err)
	}
	// A cancelled context ends the replay.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	l = New(Options{Store: store, Sleep: noSleep})
	if _, err := l.Commit(cctx, repoA, base, push("refs/heads/z", ZeroSHA, sha(9)), noCatchUp); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if d := commitBackoff(1); d >= time.Millisecond {
		t.Fatalf("backoff(1) = %v", d)
	}
	if d := commitBackoff(20); d >= 16*time.Millisecond {
		t.Fatalf("backoff(20) = %v", d)
	}
}

func TestNewestFindsTheNewestIndex(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	l := newTestLog(t, store)
	if _, _, err := l.Newest(ctx, repoA, 0, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no repository: %v", err)
	}
	base := createRepo(t, l, repoA)
	ix, changed, err := l.Newest(ctx, repoA, 0, false)
	if err != nil || !changed || ix.Seq != 0 {
		t.Fatalf("fresh repository: %+v %v %v", ix, changed, err)
	}
	if ix, changed, err := l.Newest(ctx, repoA, 0, true); err != nil || changed || ix != nil {
		t.Fatalf("current holder: %+v %v %v", ix, changed, err)
	}
	next := base
	for i := 1; i <= 5; i++ {
		c, err := l.Commit(ctx, repoA, next, push(fmt.Sprintf("refs/heads/b%d", i), ZeroSHA, sha(i)), noCatchUp)
		if err != nil {
			t.Fatal(err)
		}
		next = c.Index
	}
	// A holder of 2 jumps to 5 via the hint with three HEADs: 3, 5, 6.
	store.Calls["Head"] = 0
	ix, changed, err = l.Newest(ctx, repoA, 2, true)
	if err != nil || !changed || ix.Seq != 5 || len(ix.Entries) != 5 {
		t.Fatalf("holder of 2: %+v %v %v", ix, changed, err)
	}
	if store.Calls["Head"] != 3 {
		t.Fatalf("%d HEADs, want 3", store.Calls["Head"])
	}
	// A lagging hint is walked forward from; a missing hint falls back
	// to a listing; a hint above what exists falls back too.
	_, _ = store.Put(ctx, l.key(repoA, LatestKey), BytesBody([]byte(`{"seq":3}`)))
	if ix, _, err := l.Newest(ctx, repoA, 0, false); err != nil || ix.Seq != 5 {
		t.Fatalf("lagging hint: %+v, %v", ix, err)
	}
	if ix, _, err := l.Newest(ctx, repoA, 1, true); err != nil || ix.Seq != 5 {
		t.Fatalf("lagging hint from a holder: %+v, %v", ix, err)
	}
	_ = store.Delete(ctx, l.key(repoA, LatestKey))
	if ix, _, err := l.Newest(ctx, repoA, 0, false); err != nil || ix.Seq != 5 {
		t.Fatalf("no hint: %+v, %v", ix, err)
	}
	_, _ = store.Put(ctx, l.key(repoA, LatestKey), BytesBody([]byte(`{"seq":99}`)))
	if ix, _, err := l.Newest(ctx, repoA, 0, false); err != nil || ix.Seq != 5 {
		t.Fatalf("hint ahead: %+v, %v", ix, err)
	}
	_, _ = store.Put(ctx, l.key(repoA, LatestKey), BytesBody([]byte(`nope`)))
	if _, err := l.Hint(ctx, repoA); err == nil {
		t.Fatal("malformed hint accepted")
	}
	if ix, _, err := l.Newest(ctx, repoA, 0, false); err != nil || ix.Seq != 5 {
		t.Fatalf("malformed hint: %+v, %v", ix, err)
	}
	// Store failures surface.
	store.Fault = func(op, key string) error {
		if op == "Head" {
			return errors.New("head down")
		}
		return nil
	}
	if _, _, err := l.Newest(ctx, repoA, 2, true); err == nil {
		t.Fatal("head failure hidden")
	}
	if _, _, err := l.Newest(ctx, repoA, 0, false); err == nil {
		t.Fatal("head failure hidden on a cold start")
	}
	if _, err := l.HasIndex(ctx, repoA, 1); err == nil {
		t.Fatal("HasIndex hid the failure")
	}
	store.Fault = func(op, key string) error {
		if op == "List" {
			return errors.New("list down")
		}
		return nil
	}
	_ = store.Delete(ctx, l.key(repoA, LatestKey))
	if _, _, err := l.Newest(ctx, repoA, 0, false); err == nil {
		t.Fatal("list failure hidden")
	}
	if err := l.Ping(ctx); err == nil {
		t.Fatal("ping hid the failure")
	}
	store.Fault = func(op, key string) error {
		if op == "Get" && strings.Contains(key, "index/0") {
			return errors.New("get down")
		}
		return nil
	}
	if _, _, err := l.Newest(ctx, repoA, 0, false); err == nil {
		t.Fatal("get failure hidden")
	}
	store.Fault = nil
	// An index object whose seq disagrees with its key is refused.
	bad, _ := EncodeIndex(&Index{V: 1, Seq: 0, Refs: map[string]string{}})
	_, _ = store.Put(ctx, l.key(repoA, IndexKey(7)), BytesBody(bad))
	if _, err := l.ReadIndex(ctx, repoA, 7); err == nil || !strings.Contains(err.Error(), "carries seq") {
		t.Fatalf("mismatched index: %v", err)
	}
	if err := l.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	// A listing that pages through more than one page finds the highest.
	big := NewMemStore()
	l = newTestLog(t, big)
	for i := range 1500 {
		data, _ := EncodeIndex(&Index{V: 1, Seq: uint64(i), Entry: EntryKey(uint64(i), "0000000000000001"), Refs: map[string]string{}, Entries: []IndexEntry{{Seq: uint64(i), Key: EntryKey(uint64(i), "0000000000000001"), Kind: KindPush}}})
		if i == 0 {
			data, _ = EncodeIndex(&Index{V: 1, Refs: map[string]string{}})
		}
		_, _ = big.Put(ctx, l.key(repoA, IndexKey(uint64(i))), BytesBody(data))
	}
	if ix, _, err := l.Newest(ctx, repoA, 0, false); err != nil || ix.Seq != 1499 {
		t.Fatalf("paged listing: %+v, %v", ix, err)
	}
}

// Sixteen writers each commit twenty pushes to their own branch against
// one repository through one store. Every sequence has exactly one index
// object, every writer's every push lands, and the newest index carries
// all 320 references.
func TestSixteenWritersTwentyRoundsOneWinnerPerSequence(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	reg := metrics.New()
	l := New(Options{Store: store, Metrics: reg, Logger: slog.New(slog.DiscardHandler)})
	base := createRepo(t, l, repoA)
	const writers, rounds = 16, 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := map[uint64]string{}
	for w := range writers {
		wg.Go(func() {
			held := base
			catchUp := func(_ context.Context, ix *Index) error { held = ix; return nil }
			for r := range rounds {
				ref := fmt.Sprintf("refs/heads/w%d", w)
				old := ZeroSHA
				if r > 0 {
					old = sha(w*100 + r - 1)
				}
				c, err := l.Commit(ctx, repoA, held, push(ref, old, sha(w*100+r)), catchUp)
				if err != nil {
					t.Errorf("writer %d round %d: %v", w, r, err)
					return
				}
				held = c.Index
				mu.Lock()
				if prev, dup := winners[c.Index.Seq]; dup {
					t.Errorf("sequence %d won by %s and %s", c.Index.Seq, prev, c.Key)
				}
				winners[c.Index.Seq] = c.Key
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(winners) != writers*rounds {
		t.Fatalf("%d sequences committed, want %d", len(winners), writers*rounds)
	}
	newest, _, err := l.Newest(ctx, repoA, 0, false)
	if err != nil || newest.Seq != writers*rounds {
		t.Fatalf("newest = %+v, %v", newest, err)
	}
	for w := range writers {
		if got := newest.Refs[fmt.Sprintf("refs/heads/w%d", w)]; got != sha(w*100+rounds-1) {
			t.Errorf("writer %d ref = %s", w, got)
		}
	}
	for seq := uint64(1); seq <= writers*rounds; seq++ {
		if n := countKeys(store, IndexKey(seq)); n != 1 {
			t.Errorf("index %d appears %d times", seq, n)
		}
		if newest.Entries[seq-1].Key != winners[seq] {
			t.Errorf("index lists %s at %d, winner was %s", newest.Entries[seq-1].Key, seq, winners[seq])
		}
	}
	if reg.Counter("origo_wal_commit_retries_total", "").Value() == 0 {
		t.Fatal("no writer ever lost a round; the race did not race")
	}
	// Losing writers left entries behind at sequences that were taken;
	// each is an orphan the sweeper removes, never named by an index.
	orphans := countKeys(store, "/wal/") - writers*rounds
	if orphans <= 0 {
		t.Fatalf("%d orphans, expected some from lost rounds", orphans)
	}
}
