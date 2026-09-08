// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package compact

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

func TestNewValidatesOptionsAndDefaults(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a manager without a cache")
	}
	h := newHarness(t)
	if _, err := New(Options{Cache: h.cache}); err == nil {
		t.Fatal("a manager without a placer")
	}
	m, err := New(Options{Cache: h.cache, Placement: placer{nodeA}, Node: nodeA, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if m.interval != SweepInterval || m.gcWait != GCWait || m.deadline != RunDeadline || m.now == nil {
		t.Fatalf("defaults: %s %s %s", m.interval, m.gcWait, m.deadline)
	}
	if m.git.Timeout != RunDeadline {
		t.Fatalf("the repack runs under %s, want the run's deadline", m.git.Timeout)
	}
	if got := m.Primary(repoA); got != nodeA {
		t.Fatalf("primary = %q", got)
	}
	empty, err := New(Options{Cache: h.cache, Placement: placer{}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.Primary(repoA); got != "" {
		t.Fatalf("primary of an empty live set = %q", got)
	}
}

func TestCrossedThresholds(t *testing.T) {
	base := &wal.Index{V: wal.Version, Refs: map[string]string{}}
	if Crossed(base) {
		t.Fatal("an empty index crossed a threshold")
	}
	entries := base.Clone()
	for i := range MaxEntries + 1 {
		entries.Entries = append(entries.Entries, wal.IndexEntry{Seq: uint64(i + 1), Kind: wal.KindPush}) //nolint:gosec // a test index
	}
	if !Crossed(entries) {
		t.Fatal("65 entries did not cross")
	}
	bytes := base.Clone()
	bytes.Entries = []wal.IndexEntry{{Seq: 1, Kind: wal.KindPush, PackBytes: MaxEntryBytes + 1}}
	if !Crossed(bytes) {
		t.Fatal("the byte threshold did not cross")
	}
	big := base.Clone()
	big.Refs = map[string]string{}
	for i := range 6000 {
		big.Refs["refs/heads/branch-"+strings.Repeat("x", 40)+wal.SeqString(uint64(i))] = strings.Repeat("a", 40) //nolint:gosec // a test index
	}
	if !Crossed(big) {
		t.Fatal("an index object above 512 KiB did not cross")
	}
	// The check never mutates the index a request holds.
	if base.Entries != nil || base.Packs != nil {
		t.Fatal("Crossed filled the index it read")
	}
}

// TestGcOnThePrimaryRunsAndReportsFigures covers the two answers of spec
// 019's endpoint on the primary: a run that finished inside the wait
// with its before and after, and one still going.
func TestGcOnThePrimaryRunsAndReportsFigures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pushes(3)
	h.warm()
	res, err := h.m.GC(ctx, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || res.Running {
		t.Fatalf("gc: %+v", res)
	}
	if res.Before.Entries != 3 || res.Before.Packs != 0 || res.After.Entries != 1 || res.After.Packs == 0 {
		t.Fatalf("figures: %+v", res)
	}
	if res.After.SizeBytes <= 0 || res.After.SizeBytes >= res.Before.SizeBytes+1<<20 {
		t.Fatalf("size_bytes %d after, %d before", res.After.SizeBytes, res.Before.SizeBytes)
	}
	h.wantMetrics(`origo_compactions_total{result="ok"} 1`, `origo_compactions_total{result="stale"} 0`,
		`origo_compactions_total{result="error"} 0`, `origo_compactions_total{result="skipped"} 0`,
		"origo_compaction_seconds_count 1")

	// A run that outlives the wait is answered with its start time.
	slow := make(chan struct{})
	h.m.beforeRepack = func(string) { <-slow }
	h.m.gcWait = 10 * time.Millisecond
	h.pushes(1)
	res, err = h.m.GC(ctx, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ran || !res.Running || !res.StartedAt.Equal(h.now) {
		t.Fatalf("gc over a slow run: %+v", res)
	}
	close(slow)
	h.m.Wait()

	// A caller that goes away ends the wait.
	h.m.beforeRepack = func(string) { time.Sleep(50 * time.Millisecond) }
	h.m.gcWait = time.Minute
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.m.GC(cancelled, repoA); !errors.Is(err, context.Canceled) {
		t.Fatalf("gc under a cancelled request: %v", err)
	}
	h.m.Wait()
}

// TestRunLoopSweeps proves the background loop sweeps and stops.
func TestRunLoopSweeps(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Interval = time.Millisecond })
	h.pushes(65)
	h.warm()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.m.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for h.newest().CompactedThrough == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the sweep did not compact")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
}

// TestRunFailuresAreCountedAndReported covers the failure paths of one
// run: a repository the log does not hold, a store that refuses the
// upload, and a copy that fails the connectivity check.
func TestRunFailuresAreCountedAndReported(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if out := h.m.runNow(ctx, "not-a-uuid"); out != OutcomeError {
		t.Fatalf("a repository that does not exist: %s", out)
	}
	h.pushes(3)
	h.warm()
	h.store.SetFault(func(op, key string) error {
		if op == "Put" && strings.Contains(key, "/packs/") {
			return errors.New("upload refused")
		}
		return nil
	})
	if out := h.m.runNow(ctx, repoA); out != OutcomeError {
		t.Fatalf("a refused upload: %s", out)
	}
	h.store.SetFault(nil)
	// A copy that fails the connectivity check is evicted for rebuild
	// before anything is uploaded: a reference here points at an object
	// the copy does not hold.
	r := h.warm()
	if err := os.WriteFile(filepath.Join(r.Dir, "refs", "heads", "dangling"), []byte(strings.Repeat("a", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := h.m.runNow(ctx, repoA); out != OutcomeError {
		t.Fatalf("a copy that fails the connectivity check: %s", out)
	}
	if _, err := os.Stat(r.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the damaged copy was not evicted")
	}
	// The rebuilt copy compacts.
	h.warm()
	if out := h.m.runNow(ctx, repoA); out != OutcomeOK {
		t.Fatalf("after the rebuild: %s", out)
	}
	h.wantMetrics(`origo_compactions_total{result="error"} 3`, `origo_compactions_total{result="ok"} 1`)
}

// TestSweepReportsStoreFailures covers the sweep's error paths and the
// request object a bad body leaves.
func TestSweepReportsStoreFailures(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Placement = placer{nodeB, nodeA} })
	ctx := context.Background()
	if err := h.m.request(ctx, "not-a-uuid", ReasonGC); err == nil {
		t.Fatal("a request for an invalid id")
	}
	if err := h.m.request(ctx, repoA, ReasonGC); err != nil {
		t.Fatal(err)
	}
	h.store.SetFault(func(op, key string) error {
		if op == "List" && strings.Contains(key, "gc/") {
			return errors.New("list refused")
		}
		return nil
	})
	if _, err := h.m.Sweep(ctx); err == nil {
		t.Fatal("a refused listing was hidden")
	}
	h.store.SetFault(func(op, _ string) error {
		if op == "Get" {
			return errors.New("get refused")
		}
		return nil
	})
	if _, err := h.m.Sweep(ctx); err == nil {
		t.Fatal("a refused read was hidden")
	}
	h.store.SetFault(nil)
	// A body that does not parse is reported.
	if _, err := h.store.Put(ctx, h.m.requestKey(repoA), wal.BytesBody([]byte("{"))); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.ReadRequest(ctx, repoA); err == nil {
		t.Fatal("an unparsable request was accepted")
	}
	if _, err := h.m.Sweep(ctx); err == nil {
		t.Fatal("an unparsable request was swept over")
	}
	// A delete the store refuses is reported.
	if err := h.store.Delete(ctx, h.m.requestKey(repoA)); err != nil {
		t.Fatal(err)
	}
	if err := h.m.request(ctx, repoA, ReasonGC); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(RequestMaxAge)
	h.store.SetFault(func(op, _ string) error {
		if op == "Delete" {
			return errors.New("delete refused")
		}
		return nil
	})
	if _, err := h.m.Sweep(ctx); err == nil {
		t.Fatal("a refused delete was hidden")
	}
	h.store.SetFault(nil)
	// A cancelled sweep stops at the first repository.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.m.Sweep(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sweep: %v", err)
	}
	if out := h.m.runNow(cancelled, repoA); out != OutcomeError {
		t.Fatalf("a cancelled run: %s", out)
	}
	h.m.Wait()
}

// TestSweepSkipsARepositoryItCannotOpen covers the local threshold pass
// over a copy the log no longer holds.
func TestSweepSkipsARepositoryItCannotOpen(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pushes(2)
	h.warm()
	rep, err := h.m.Sweep(ctx)
	if err != nil || rep.Threshold != 0 {
		t.Fatalf("a copy below every threshold: %+v, %v", rep, err)
	}
	h.store.SetFault(func(op, _ string) error {
		if op == "Head" {
			return errors.New("head refused")
		}
		return nil
	})
	if rep, err := h.m.Sweep(ctx); err != nil || rep.Threshold != 0 {
		t.Fatalf("a copy that cannot be opened: %+v, %v", rep, err)
	}
	h.store.SetFault(nil)
}

func TestPackSetReadsTheMultiPackIndex(t *testing.T) {
	dir := t.TempDir()
	// No objects/pack at all: no pack, no set.
	if packs, err := packSet(dir); err != nil || packs != nil {
		t.Fatalf("an empty repository: %v, %v", packs, err)
	}
	packs := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packs, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := packSet(dir); err != nil || got != nil {
		t.Fatalf("no packs: %v, %v", got, err)
	}
	// A pack with no multi-pack index means the repack wrote none.
	if err := os.WriteFile(filepath.Join(packs, "pack-"+strings.Repeat("a", 40)+".pack"), []byte("PACK"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := packSet(dir); err == nil {
		t.Fatal("a missing multi-pack index was accepted")
	}
	// A directory in place of the file surfaces the read error.
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "objects", "pack", "multi-pack-index"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := packSet(other); err == nil {
		t.Fatal("an unreadable multi-pack index was accepted")
	}
}

func TestMidxPacksRefusesBadFraming(t *testing.T) {
	good := midx(t, "pack-"+strings.Repeat("a", 40)+".idx", "pack-"+strings.Repeat("b", 40)+".idx")
	names, err := midxPacks(good)
	if err != nil || len(names) != 2 || names[0] != "pack-"+strings.Repeat("a", 40)+".pack" {
		t.Fatalf("names = %v, %v", names, err)
	}
	bad := map[string][]byte{
		"short":        good[:4],
		"no signature": append([]byte("XIDX"), good[4:]...),
	}
	version := append([]byte(nil), good...)
	version[4] = 2
	bad["version"] = version
	chunks := append([]byte(nil), good...)
	chunks[6] = 0
	bad["no chunks"] = chunks
	truncated := append([]byte(nil), good[:midxHeaderSize+midxChunkSize]...)
	truncated[6] = 4
	bad["truncated table"] = truncated
	other := midx(t, "pack-"+strings.Repeat("a", 40)+".idx")
	copy(other[midxHeaderSize:midxHeaderSize+4], "OIDF")
	bad["no PNAM"] = other
	far := append([]byte(nil), good...)
	binary.BigEndian.PutUint64(far[midxHeaderSize+4:midxHeaderSize+12], uint64(len(far)+1))
	bad["offset past the end"] = far
	for name, data := range bad {
		if _, err := midxPacks(data); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// midx builds a minimal multi-pack index holding a PNAM chunk.
func midx(t *testing.T, names ...string) []byte {
	t.Helper()
	var pnam []byte
	for _, n := range names {
		pnam = append(pnam, []byte(n)...)
		pnam = append(pnam, 0)
	}
	head := make([]byte, midxHeaderSize)
	copy(head, "MIDX")
	head[4], head[5], head[6], head[7] = midxVersion, 1, 1, 0
	binary.BigEndian.PutUint32(head[8:12], uint32(len(names))) //nolint:gosec // a test fixture
	table := make([]byte, 2*midxChunkSize)
	copy(table[:4], "PNAM")
	start := uint64(midxHeaderSize + len(table))
	binary.BigEndian.PutUint64(table[4:12], start)
	binary.BigEndian.PutUint64(table[midxChunkSize+4:midxChunkSize+12], start+uint64(len(pnam)))
	return append(append(head, table...), pnam...)
}

func TestSwapPacksReportsAMissingDirectory(t *testing.T) {
	h := newHarness(t)
	r := &repo.Repo{ID: repoA, Dir: filepath.Join(t.TempDir(), "gone")}
	if err := h.m.swapPacks(context.Background(), r, &wal.Index{}); err == nil {
		t.Fatal("a missing objects/pack was accepted")
	}
	if base, ok := packBase("multi-pack-index"); ok {
		t.Fatalf("the multi-pack index read as pack %q", base)
	}
	if base, ok := packBase("pack-abc.tmp"); ok {
		t.Fatalf("a temporary file read as pack %q", base)
	}
}

func TestUploadReportsAMissingPack(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.m.upload(context.Background(), repoA, t.TempDir(), &wal.Index{}, []string{"pack-x.pack"}); err == nil {
		t.Fatal("a pack that is not on disk was accepted")
	}
}
