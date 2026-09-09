// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/wal"
)

// TestOrphanSweepRunsOnOneNode is spec 019's sweep criterion: with
// three nodes the weekly sweep runs on the node whose name sorts first
// and on the next name once that node is out of the live set, the other
// nodes report the gauges from origo/sweep/latest, and an object under
// a prefix the sweep does not understand is reported by key and
// counted.
func TestOrphanSweepRunsOnOneNode(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC) // a Sunday
	clock := func() time.Time { return now }
	store := wal.NewMemStore()
	store.SetClock(clock)
	live := &liveNodes{names: []string{"origod-0", "origod-1", "origod-2"}}

	first := newHarness(t, withStore(store), withNow(clock), withNode("origod-0", live))
	second := newHarness(t, withStore(store), withNow(clock), withNode("origod-1", live))
	third := newHarness(t, withStore(store), withNow(clock), withNode("origod-2", live))
	first.create(repoA, "acme", "app")

	// The objects a sweep must understand and the ones it must not.
	prefix := first.log.RepoPrefix(repoA)
	put := func(key string, body string) {
		t.Helper()
		if _, err := store.Put(ctx, key, wal.BytesBody([]byte(body))); err != nil {
			t.Fatal(err)
		}
	}
	oid := strings.Repeat("ab", 32)
	put(prefix+"lfs/"+oid, "verified object")
	put(prefix+"lfs/verified/"+oid, `{"size":15}`)
	unverified := prefix + "lfs/" + strings.Repeat("cd", 32)
	put(unverified, "no marker")
	entry := prefix + wal.EntryKey(9, "deadbeefdeadbeef")
	put(entry, "an entry no index names")
	pack := prefix + "packs/" + strings.Repeat("11", 20) + ".pack"
	put(pack, "a pack no index lists")
	unknown := first.log.Prefix() + "scratch/left-behind"
	put(unknown, "written by something else")

	// Nothing is an orphan until it is a day old.
	rep, err := first.sweeper().Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 0 || rep.Deleted != 0 || rep.StorageBytes == 0 {
		t.Fatalf("sweep of young objects: %+v", rep)
	}

	// A day on, every object no index, marker, or metadata names is an
	// orphan, the one outside every prefix the deck defines is reported
	// by key and counted as unknown, and none is deleted yet.
	now = now.Add(OrphanAge)
	rep, err = first.sweeper().Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 4 || rep.Unknown != 1 || rep.Deleted != 0 || rep.Node != "origod-0" {
		t.Fatalf("sweep after a day: %+v", rep)
	}
	for _, key := range []string{unverified, entry, pack, unknown} {
		if !slices.Contains(rep.Keys, key) {
			t.Fatalf("%s is not in the report: %v", key, rep.Keys)
		}
	}
	// The verified object, its marker, meta, and the index objects are
	// named and are never orphans.
	for _, key := range []string{prefix + "lfs/" + oid, prefix + "lfs/verified/" + oid, prefix + "meta", prefix + wal.IndexKey(0)} {
		if slices.Contains(rep.Keys, key) {
			t.Fatalf("%s was called an orphan", key)
		}
	}

	// Past the hold they go.
	now = now.Add(OrphanHold)
	rep, err = first.sweeper().Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 4 || rep.Deleted != 4 {
		t.Fatalf("sweep past the hold: %+v", rep)
	}
	for _, key := range []string{unverified, entry, pack, unknown} {
		if _, err := store.Head(ctx, key); !errors.Is(err, wal.ErrNotFound) {
			t.Fatalf("%s survived: %v", key, err)
		}
	}

	// The first name sweeps; the others report the run they read.
	if first.sweeper().sweepNode() != "origod-0" || second.sweeper().sweepNode() != "origod-0" {
		t.Fatalf("sweep node %q %q", first.sweeper().sweepNode(), second.sweeper().sweepNode())
	}
	second.sweeper().readReport(ctx)
	third.sweeper().readReport(ctx)
	for _, h := range []*harness{second, third} {
		if got := h.sweeper().Report(); got.Node != "origod-0" || got.Orphans != rep.Orphans || got.StorageBytes != rep.StorageBytes {
			t.Fatalf("%s reports %+v", h.handler.node, got)
		}
	}
	// The gauges of spec 011 come off that report on every node.
	reg := pkgmetrics.NewRegistry()
	third.sweeper().Bind(metrics.Register(reg))
	var text bytes.Buffer
	reg.WritePrometheus(&text)
	for _, want := range []string{
		fmt.Sprintf("origo_orphan_objects %d", rep.Orphans),
		fmt.Sprintf("origo_storage_bytes %d", rep.StorageBytes),
	} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, text.String())
		}
	}

	// Once the first name leaves the live set the next name sweeps.
	live.remove("origod-0")
	if second.sweeper().sweepNode() != "origod-1" || third.sweeper().sweepNode() != "origod-1" {
		t.Fatalf("after origod-0 left: %q %q", second.sweeper().sweepNode(), third.sweeper().sweepNode())
	}
	rep, err = second.sweeper().Sweep(ctx)
	if err != nil || rep.Node != "origod-1" {
		t.Fatalf("sweep on the next name: %+v, %v", rep, err)
	}

	// The hour is Sunday at 03:00 UTC and one run an hour.
	sunday := time.Date(2026, 9, 6, SweepHour, 0, 0, 0, time.UTC)
	if !sweepDue(sunday, time.Time{}) {
		t.Fatal("the sweep is not due on Sunday at 03:00 UTC")
	}
	if sweepDue(sunday, sunday) {
		t.Fatal("the sweep ran twice in one hour")
	}
	if sweepDue(sunday.Add(time.Hour), time.Time{}) || sweepDue(sunday.AddDate(0, 0, 1), time.Time{}) {
		t.Fatal("the sweep is due outside its hour")
	}

	// A node with no live set at all sweeps for itself.
	alone := newHarness(t, withStore(store), withNow(clock), withNode("origod-9"))
	if alone.sweeper().sweepNode() != "origod-9" {
		t.Fatalf("a single node's sweep node is %q", alone.sweeper().sweepNode())
	}
}

// TestSweepLeavesARepositoryItCannotRead is the sweep's caution: an
// index it cannot read leaves every object of that repository alone,
// because an outage is not a reason to call them orphans.
func TestSweepLeavesARepositoryItCannotRead(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := wal.NewMemStore()
	store.SetClock(clock)
	h := newHarness(t, withStore(store), withNow(clock), withNode("origod-0"))
	h.create(repoA, "acme", "app")
	prefix := h.log.RepoPrefix(repoA)
	if _, err := store.Put(ctx, prefix+wal.EntryKey(9, "deadbeefdeadbeef"), wal.BytesBody([]byte("orphan"))); err != nil {
		t.Fatal(err)
	}
	now = now.Add(OrphanAge)
	store.SetFault(func(op, key string) error {
		if op == "Get" && strings.Contains(key, "/index/") {
			return errors.New("index unreadable")
		}
		return nil
	})
	rep, err := h.sweeper().Sweep(ctx)
	store.SetFault(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 0 {
		t.Fatalf("a repository whose index failed produced %d orphans: %v", rep.Orphans, rep.Keys)
	}
}

// sweeper is the handler's weekly orphan sweep.
func (h *harness) sweeper() *Sweeper { return h.handler.Sweeper() }

// liveNodes is a live set a test changes.
type liveNodes struct{ names []string }

func (l *liveNodes) Live() []string { return slices.Clone(l.names) }

func (l *liveNodes) remove(name string) {
	l.names = slices.DeleteFunc(l.names, func(n string) bool { return n == name })
}

// TestSweepLoopRunsInItsHour drives the background loop: the node whose
// name sorts first runs when the hour comes and writes the report, and
// a node that is not it reads that report instead.
func TestSweepLoopRunsInItsHour(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC) // a Sunday, before the hour
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	store := wal.NewMemStore()
	store.SetClock(clock)
	live := &liveNodes{names: []string{"origod-0", "origod-1"}}
	first := newHarness(t, withStore(store), withNow(clock), withNode("origod-0", live), withSweepTick(5*time.Millisecond))
	second := newHarness(t, withStore(store), withNow(clock), withNode("origod-1", live), withSweepTick(5*time.Millisecond))
	first.create(repoA, "acme", "app")

	done := make(chan struct{}, 2)
	go func() { _ = first.sweeper().Run(ctx); done <- struct{}{} }()
	go func() { _ = second.sweeper().Run(ctx); done <- struct{}{} }()

	// Outside the hour neither runs and neither has a report.
	time.Sleep(50 * time.Millisecond)
	if got := first.sweeper().Report(); got.Node != "" {
		t.Fatalf("a sweep ran outside its hour: %+v", got)
	}
	advance(2 * time.Hour)
	waitFor(t, "the sweep to run and both nodes to report it", func() bool {
		return first.sweeper().Report().Node == "origod-0" && second.sweeper().Report().Node == "origod-0"
	})
	cancel()
	<-done
	<-done
}

// TestSweepReportFailuresAreLogged covers the report the sweeping node
// writes and the other nodes read: an unreadable object leaves the
// gauges where they were.
func TestSweepReportFailuresAreLogged(t *testing.T) {
	ctx := context.Background()
	store := wal.NewMemStore()
	h := newHarness(t, withStore(store), withNode("origod-0"))
	// Nothing written yet: the read is a no-op.
	h.sweeper().readReport(ctx)
	if got := h.sweeper().Report(); got.Node != "" {
		t.Fatalf("a report out of nowhere: %+v", got)
	}
	if _, err := store.Put(ctx, h.log.Prefix()+SweepKey, wal.BytesBody([]byte("{"))); err != nil {
		t.Fatal(err)
	}
	h.sweeper().readReport(ctx)
	if got := h.sweeper().Report(); got.Node != "" {
		t.Fatalf("a malformed report was read: %+v", got)
	}
	store.SetFault(func(op, key string) error {
		if strings.HasSuffix(key, SweepKey) {
			return errors.New("refused")
		}
		return nil
	})
	h.sweeper().readReport(ctx)
	// A run that cannot write its report still answers what it found.
	rep, err := h.sweeper().Sweep(ctx)
	store.SetFault(nil)
	if err == nil {
		t.Fatal("the report write failure was hidden")
	}
	if rep.Node != "origod-0" {
		t.Fatalf("report %+v", rep)
	}
	// A listing that fails ends the run.
	store.SetFault(func(op, _ string) error {
		if op == "List" {
			return errors.New("refused")
		}
		return nil
	})
	_, err = h.sweeper().Sweep(ctx)
	store.SetFault(nil)
	if err == nil {
		t.Fatal("the listing failure was hidden")
	}
}

// waitFor polls until the condition holds or a second passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
