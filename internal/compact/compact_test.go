// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package compact

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

const (
	repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	nodeA = "origod-0"
	nodeB = "origod-1"
)

// placer answers a fixed preference list, whose first name is the
// compaction primary.
type placer []string

func (p placer) Prefer(_ string, replicas int) []string {
	return p[:min(max(replicas, 1), len(p))]
}

// slots is a stand-in for the subprocess semaphore of spec 012: free
// grants at once, and a zero value grants nothing.
type slots struct {
	free  bool
	taken int
}

func (s *slots) Acquire(context.Context, time.Duration) (func(), bool) {
	if !s.free {
		return nil, false
	}
	s.taken++
	return func() {}, true
}

type harness struct {
	t     *testing.T
	dir   string
	store *wal.MemStore
	reg   *pkgmetrics.Registry
	log   *wal.Log
	cache *repo.Cache
	m     *Manager
	src   *gittest.Source
	held  *wal.Index
	head  string
	now   time.Time
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, store: wal.NewMemStore(), reg: pkgmetrics.NewRegistry(), now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	h.store.SetClock(func() time.Time { return h.now })
	h.log = wal.New(wal.Options{Store: h.store, Now: func() time.Time { return h.now }, Logger: slog.New(slog.DiscardHandler)})
	h.dir = filepath.Join(t.TempDir(), "data")
	cache, err := repo.New(repo.Options{Dir: h.dir, Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h.cache = cache
	o := Options{
		Cache: cache, Placement: placer{nodeA}, Node: nodeA,
		Now: func() time.Time { return h.now }, Logger: slog.New(slog.DiscardHandler),
		Metrics: metrics.Register(h.reg), GCWait: 2 * time.Second,
	}
	for _, f := range opts {
		f(&o)
	}
	h.m, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := h.log.CreateRepo(context.Background(), wal.Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	h.held = ix
	h.src = gittest.NewSource(t)
	return h
}

// push commits one push entry with a real pack over the previous commit.
func (h *harness) push(n int) *wal.Index {
	h.t.Helper()
	old := h.head
	if old == "" {
		old = wal.ZeroSHA
	}
	head := h.src.Commit("f.txt", strings.Repeat("x", n), "commit")
	h.head = head
	pack := h.src.Pack(head)
	if old != wal.ZeroSHA {
		pack = h.src.Pack(head, old)
	}
	e := wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{Ref: "refs/heads/main", Old: old, New: head}}, Pack: wal.BytesBody(pack)}
	c, err := h.log.Commit(context.Background(), repoA, h.held, e, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		h.t.Fatal(err)
	}
	h.held = c.Index
	return c.Index
}

// pushes commits n pushes and answers the newest index.
func (h *harness) pushes(n int) *wal.Index {
	h.t.Helper()
	for i := range n {
		h.push(i + 1)
	}
	return h.held
}

// newest reads the newest index object out of the log.
func (h *harness) newest() *wal.Index {
	h.t.Helper()
	ix, _, err := h.log.Newest(context.Background(), repoA, 0, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return ix
}

// warm materializes the local copy.
func (h *harness) warm() *repo.Repo {
	h.t.Helper()
	r, release, err := h.cache.Acquire(context.Background(), repoA, false)
	if err != nil {
		h.t.Fatal(err)
	}
	release()
	return r
}

// packFiles lists the pack files of the local copy.
func (h *harness) packFiles(r *repo.Repo) []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.Dir, "objects", "pack"))
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") {
			out = append(out, e.Name())
		}
	}
	return out
}

// bitmaps counts the multi-pack-index bitmaps under objects/pack: a
// rewrite that left the previous one behind would leave two.
func bitmaps(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "objects", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "multi-pack-index-") && strings.HasSuffix(e.Name(), ".bitmap") {
			n++
		}
	}
	return n
}

// elsewhere materializes the repository on a second node's empty disk
// and answers the copy, which is what a fetch through another node
// serves.
func (h *harness) elsewhere() *repo.Repo {
	h.t.Helper()
	cache, err := repo.New(repo.Options{Dir: filepath.Join(h.t.TempDir(), "other"), Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		h.t.Fatal(err)
	}
	r, release, err := cache.Acquire(context.Background(), repoA, false)
	if err != nil {
		h.t.Fatal(err)
	}
	defer release()
	if _, err := cache.Git().Run(context.Background(), r.Dir, nil, "fsck", "--connectivity-only", "--no-progress"); err != nil {
		h.t.Fatalf("fsck on the copy a second node materialized: %v", err)
	}
	return r
}

// wantMetrics fails unless every line is in the registry.
func (h *harness) wantMetrics(lines ...string) {
	h.t.Helper()
	var text bytes.Buffer
	h.reg.WritePrometheus(&text)
	for _, want := range lines {
		if !strings.Contains(text.String(), want) {
			h.t.Errorf("metrics lack %q:\n%s", want, text.String())
		}
	}
}

// TestThresholdFoldsEntries is the unit half of the 500 push criterion:
// 65 entries written through Log.Commit are folded into packs, the
// newest index lists at most 64 entries and at most 6 packs, and a node
// materializing on an empty disk passes git fsck.
func TestThresholdFoldsEntries(t *testing.T) {
	h := newHarness(t)
	ix := h.pushes(65)
	if !Crossed(ix) {
		t.Fatalf("65 entries did not cross the threshold: %d entries", len(ix.Entries))
	}
	h.warm()
	if out, _, _, err := h.m.compact(context.Background(), repoA); err != nil || out != OutcomeOK {
		t.Fatalf("compact: %s, %v", out, err)
	}
	newest := h.newest()
	if len(newest.Entries) > MaxEntries || len(newest.Entries) != 1 {
		t.Fatalf("the newest index lists %d entries", len(newest.Entries))
	}
	if len(newest.Packs) > 6 || len(newest.Packs) == 0 {
		t.Fatalf("the newest index lists %d packs", len(newest.Packs))
	}
	if newest.CompactedThrough != 65 || newest.Seq != 66 {
		t.Fatalf("compacted_through %d at seq %d", newest.CompactedThrough, newest.Seq)
	}
	if newest.SizeBytes <= 0 {
		t.Fatalf("size_bytes = %d", newest.SizeBytes)
	}
	// The packs on disk are exactly the ones the index lists, and the
	// multi-pack index was rewritten over them.
	local := h.warm()
	files := h.packFiles(local)
	if len(files) != len(newest.Packs) {
		t.Fatalf("%d pack files on disk, %d listed", len(files), len(newest.Packs))
	}
	if _, err := os.Stat(filepath.Join(local.Dir, "objects", "pack", "multi-pack-index")); err != nil {
		t.Fatal(err)
	}
	if n := bitmaps(t, local.Dir); n != 1 {
		t.Fatalf("%d multi-pack-index bitmaps after the run, want 1", n)
	}
	// A second node builds the same history from the packs alone.
	other := h.elsewhere()
	if got, want := gittest.RevList(t, other.Dir), gittest.RevList(t, h.src.Dir); got != want {
		t.Fatalf("history differs:\n%s\n%s", got, want)
	}
}

// TestPushDuringCompactionIsPreserved: a push that lands between step 1
// and step 5 makes the run abort with result stale, the push is in the
// newest index, and the next run folds it.
func TestPushDuringCompactionIsPreserved(t *testing.T) {
	h := newHarness(t)
	h.pushes(65)
	h.warm()
	landed := ""
	h.m.afterRepack = func(string) {
		h.m.afterRepack = nil
		landed = h.push(1).Entry
	}
	out, _, _, err := h.m.compact(context.Background(), repoA)
	if err != nil || out != OutcomeStale {
		t.Fatalf("compact: %s, %v", out, err)
	}
	newest := h.newest()
	if newest.Seq != 66 || newest.Entry != landed {
		t.Fatalf("the newest index is %d, %q, want the push %q", newest.Seq, newest.Entry, landed)
	}
	if newest.CompactedThrough != 0 {
		t.Fatalf("a stale run committed: compacted_through %d", newest.CompactedThrough)
	}
	// The entry the lost round wrote is an orphan the sweeper removes.
	h.now = h.now.Add(2 * time.Hour)
	rep, err := h.log.Sweep(context.Background(), repoA, time.Hour)
	if err != nil || rep.Orphans != 1 {
		t.Fatalf("sweep: %+v, %v", rep, err)
	}
	// The next run folds the push the first one lost to.
	h.warm()
	if out, _, _, err := h.m.compact(context.Background(), repoA); err != nil || out != OutcomeOK {
		t.Fatalf("second run: %s, %v", out, err)
	}
	folded := h.newest()
	if folded.CompactedThrough != 66 || len(folded.Entries) != 1 {
		t.Fatalf("after the second run: compacted_through %d, %d entries", folded.CompactedThrough, len(folded.Entries))
	}
	h.elsewhere()
}

// TestTriggerIsBackgroundAndSingle: the after-push trigger returns
// before the run starts, a second trigger while one runs starts none, a
// gc while one runs answers with running and the run's start time inside
// the wait, and a clone served during the repack succeeds.
func TestTriggerIsBackgroundAndSingle(t *testing.T) {
	h := newHarness(t)
	ix := h.pushes(65)
	h.warm()
	inRepack, hold := make(chan struct{}), make(chan struct{})
	var runs int
	h.m.beforeRepack = func(string) {
		runs++
		close(inRepack)
		<-hold
	}
	started := time.Now()
	h.m.After(repoA, ix)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the trigger blocked for %s", elapsed)
	}
	<-inRepack
	// A second trigger and a gc find the run in flight and start none.
	h.m.After(repoA, ix)
	res, err := h.m.GC(context.Background(), repoA)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Running || res.Ran || !res.StartedAt.Equal(h.now) {
		t.Fatalf("gc while a run is in flight: %+v", res)
	}
	// A clone during the repack succeeds: the run holds the read lock,
	// which fetches share.
	r, release, err := h.cache.Acquire(context.Background(), repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "clone.git")
	if _, err := gittest.Try(t.TempDir(), nil, "clone", "--bare", "--quiet", r.Dir, dest); err != nil {
		t.Fatalf("clone during the repack: %v", err)
	}
	release()
	close(hold)
	h.m.Wait()
	if runs != 1 {
		t.Fatalf("%d runs started, want 1", runs)
	}
	if got := h.newest(); got.CompactedThrough != 65 {
		t.Fatalf("the scheduled run did not compact: compacted_through %d", got.CompactedThrough)
	}
	// A trigger below every threshold starts nothing.
	h.m.After(repoA, h.newest())
	h.m.After(repoA, nil)
	h.m.Wait()
	if _, ok := h.m.Running(repoA); ok {
		t.Fatal("a run is still recorded")
	}
}

// TestGcRequestIsPickedUpByThePrimary: a gc on a node that is not the
// primary writes origo/gc/<id>, answers with the primary's name and
// compacts nothing, and the primary's sweep compacts and removes the
// request.
func TestGcRequestIsPickedUpByThePrimary(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Placement = placer{nodeB, nodeA} })
	h.pushes(65)
	ctx := context.Background()
	res, err := h.m.GC(ctx, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != nodeB || res.Ran || res.Running {
		t.Fatalf("gc on a node that is not the primary: %+v", res)
	}
	req, err := h.m.ReadRequest(ctx, repoA)
	if err != nil || req.Reason != ReasonGC || req.Node != nodeA {
		t.Fatalf("request object: %+v, %v", req, err)
	}
	if got := h.newest(); got.CompactedThrough != 0 {
		t.Fatal("a node that is not the primary compacted")
	}
	// A second request of the other reason leaves the first alone.
	if err := h.m.request(ctx, repoA, ReasonThreshold); err != nil {
		t.Fatal(err)
	}
	if req, _ := h.m.ReadRequest(ctx, repoA); req.Reason != ReasonGC {
		t.Fatalf("the request was rewritten: %+v", req)
	}
	// Its sweep leaves a request whose primary it is not.
	if rep, err := h.m.Sweep(ctx); err != nil || rep.Requested != 0 {
		t.Fatalf("sweep on a node that is not the primary: %+v, %v", rep, err)
	}
	if _, err := h.m.ReadRequest(ctx, repoA); err != nil {
		t.Fatal("the request was removed by a node that is not the primary")
	}

	// The primary's sweep materializes the repository, compacts it, and
	// removes the request.
	primary := newHarnessOn(t, h, placer{nodeA}, nodeA)
	rep, err := primary.Sweep(ctx)
	if err != nil || rep.Requested != 1 {
		t.Fatalf("the primary's sweep: %+v, %v", rep, err)
	}
	if got := h.newest(); got.CompactedThrough != 65 {
		t.Fatalf("compacted_through %d", got.CompactedThrough)
	}
	if _, err := h.m.ReadRequest(ctx, repoA); !errors.Is(err, wal.ErrNotFound) {
		t.Fatalf("the request object survived: %v", err)
	}
	// A request nobody acted on for a day goes on any node's sweep.
	if err := h.m.request(ctx, repoA, ReasonThreshold); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(RequestMaxAge)
	if rep, err := h.m.Sweep(ctx); err != nil || rep.Expired != 1 {
		t.Fatalf("expiry sweep: %+v, %v", rep, err)
	}
	if _, err := h.m.ReadRequest(ctx, repoA); !errors.Is(err, wal.ErrNotFound) {
		t.Fatal("the expired request survived")
	}
}

// newHarnessOn builds a second node's manager over the same log, with
// its own empty data directory, so a sweep on it materializes.
func newHarnessOn(t *testing.T, h *harness, p placer, node string) *Manager {
	t.Helper()
	cache, err := repo.New(repo.Options{Dir: filepath.Join(t.TempDir(), node), Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		Cache: cache, Placement: p, Node: node, Now: func() time.Time { return h.now },
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestPushOnANonPrimaryRequestsCompaction: 65 pushes through a node that
// is not the primary leave the request object with reason threshold, and
// the primary, holding no copy, materializes and folds them.
func TestPushOnANonPrimaryRequestsCompaction(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Placement = placer{nodeB, nodeA} })
	ctx := context.Background()
	for i := range 65 {
		ix := h.push(i + 1)
		h.m.After(repoA, ix)
		h.m.Wait()
	}
	req, err := h.m.ReadRequest(ctx, repoA)
	if err != nil || req.Reason != ReasonThreshold || req.Node != nodeA {
		t.Fatalf("after the 65th push: %+v, %v", req, err)
	}
	if got := h.newest(); got.CompactedThrough != 0 {
		t.Fatal("a node that is not the primary compacted")
	}
	primary := newHarnessOn(t, h, placer{nodeB}, nodeB)
	if rep, err := primary.Sweep(ctx); err != nil || rep.Requested != 1 {
		t.Fatalf("the primary's sweep: %+v, %v", rep, err)
	}
	folded := h.newest()
	if folded.CompactedThrough != 65 || len(folded.Entries) > MaxEntries {
		t.Fatalf("compacted_through %d, %d entries", folded.CompactedThrough, len(folded.Entries))
	}
	if _, err := h.m.ReadRequest(ctx, repoA); !errors.Is(err, wal.ErrNotFound) {
		t.Fatal("the request object survived")
	}
	h.elsewhere()
	// The primary now holds the copy, so its next sweep finds the
	// threshold locally rather than through a request.
	for i := range 65 {
		h.push(i + 1)
	}
	rep, err := primary.Sweep(ctx)
	if err != nil || rep.Threshold != 1 {
		t.Fatalf("threshold sweep: %+v, %v", rep, err)
	}
	second := h.newest()
	if second.CompactedThrough != 131 || len(second.Packs) > 6 {
		t.Fatalf("compacted_through %d, %d packs", second.CompactedThrough, len(second.Packs))
	}
	h.elsewhere()
}

// TestDelayedReaderSurvivesCompaction: a node that read the previous
// index before a compaction and fetches its entries after it completes
// materializes, as long as the entries are younger than
// ORIGO_SWEEP_MIN_AGE.
func TestDelayedReaderSurvivesCompaction(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Two nodes with an empty disk open the repository before the pushes,
	// so each holds sequence 0 and applies the index it is given rather
	// than the newest one; that is the delayed reader of the criterion.
	reader, first := h.reader("delayed")
	defer first()
	late, second := h.reader("late")
	defer second()

	before := h.pushes(65)
	h.warm()
	if out, _, _, err := h.m.compact(ctx, repoA); err != nil || out != OutcomeOK {
		t.Fatalf("compact: %s, %v", out, err)
	}
	// The reader read index 65 before the run and fetches its entries
	// after it: they are younger than ORIGO_SWEEP_MIN_AGE and there.
	if err := reader.cache.Apply(ctx, reader.repo, before); err != nil {
		t.Fatalf("a reader holding the index the compaction folded: %v", err)
	}
	if got, want := gittest.RevList(t, reader.repo.Dir), gittest.RevList(t, h.src.Dir); got != want {
		t.Fatalf("history differs:\n%s\n%s", got, want)
	}
	// Past the age the folded entries are gone, and a reader that waited
	// longer than that rebuilds from the packs the newest index lists,
	// which is what elsewhere does.
	h.now = h.now.Add(2 * time.Hour)
	if rep, err := h.log.Sweep(ctx, repoA, time.Hour); err != nil || rep.Folded != 65 {
		t.Fatalf("sweep: %+v, %v", rep, err)
	}
	if err := late.cache.Apply(ctx, late.repo, before); err == nil {
		t.Fatal("a swept entry was read")
	}
	h.elsewhere()
}

// held is a node holding the repository at the sequence it opened it on.
type held struct {
	cache *repo.Cache
	repo  *repo.Repo
}

// reader opens the repository on a second node's empty disk under the
// write lock and keeps it there, so the copy stays at the sequence of
// the moment it opened and a later Apply is the delayed fetch.
func (h *harness) reader(name string) (held, func()) {
	h.t.Helper()
	cache, err := repo.New(repo.Options{Dir: filepath.Join(h.t.TempDir(), name), Log: h.log, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		h.t.Fatal(err)
	}
	r, release, err := cache.Acquire(context.Background(), repoA, true)
	if err != nil {
		h.t.Fatal(err)
	}
	return held{cache: cache, repo: r}, release
}

// TestCompactionSkipsWhenNoSlot is spec 012's criterion in this package:
// a run that waits more than five seconds for a subprocess slot skips
// and the next sweep retries it.
func TestCompactionSkipsWhenNoSlot(t *testing.T) {
	free := &slots{}
	h := newHarness(t, func(o *Options) { o.Slots = free })
	h.pushes(65)
	h.warm()
	out, _, _, err := h.m.compact(context.Background(), repoA)
	if err != nil || out != OutcomeSkipped {
		t.Fatalf("without a slot: %s, %v", out, err)
	}
	if got := h.newest(); got.CompactedThrough != 0 {
		t.Fatal("a skipped run committed")
	}
	free.free = true
	out, _, _, err = h.m.compact(context.Background(), repoA)
	if err != nil || out != OutcomeOK {
		t.Fatalf("with a slot: %s, %v", out, err)
	}
	if free.taken < 3 {
		t.Fatalf("%d slots taken; every git subprocess of the run takes one", free.taken)
	}
	if got := h.newest(); got.CompactedThrough != 65 {
		t.Fatalf("compacted_through %d", got.CompactedThrough)
	}
}
