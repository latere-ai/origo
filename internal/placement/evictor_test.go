// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package placement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// cacheHarness is a cache over an in-process log with n repositories,
// each holding the same one-commit history under its own id.
type cacheHarness struct {
	t     *testing.T
	clk   *clock
	log   *wal.Log
	cache *repo.Cache
	src   *gittest.Source
	ids   []string
	reg   *pkgmetrics.Registry
}

func newCacheHarness(t *testing.T, n int) *cacheHarness {
	t.Helper()
	clk := newClock()
	store := wal.NewMemStore()
	l := wal.New(wal.Options{Store: store, Logger: slog.New(slog.DiscardHandler)})
	reg := pkgmetrics.NewRegistry()
	c, err := repo.New(repo.Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: slog.New(slog.DiscardHandler), Now: clk.Now, Metrics: metrics.Register(reg)})
	if err != nil {
		t.Fatal(err)
	}
	h := &cacheHarness{t: t, clk: clk, log: l, cache: c, src: gittest.NewSource(t), reg: reg}
	c1 := h.src.Commit("a.txt", "one", "first")
	pack := h.src.Pack(c1)
	ctx := context.Background()
	for i := range n {
		id := fmt.Sprintf("%08d-0000-4000-8000-000000000000", i)
		ix, err := l.CreateRepo(ctx, wal.Meta{ID: id, Owner: "acme", Slug: fmt.Sprintf("r%d", i)}, "main")
		if err != nil {
			t.Fatal(err)
		}
		e := wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{Ref: "refs/heads/main", Old: wal.ZeroSHA, New: c1}}, Pack: wal.BytesBody(pack)}
		if _, err := l.Commit(ctx, id, ix, e, func(context.Context, *wal.Index) error { return nil }); err != nil {
			t.Fatal(err)
		}
		h.ids = append(h.ids, id)
	}
	return h
}

// acquire materializes or refreshes one copy and releases it.
func (h *cacheHarness) acquire(id string) {
	h.t.Helper()
	_, release, err := h.cache.Acquire(context.Background(), id, false)
	if err != nil {
		h.t.Fatal(err)
	}
	release()
}

func (h *cacheHarness) held(id string) bool {
	_, ok := h.cache.Held(id)
	return ok
}

func (h *cacheHarness) evictor(ceiling int64) *Evictor {
	h.t.Helper()
	e, err := NewEvictor(EvictorOptions{Cache: h.cache, Ceiling: ceiling, Now: h.clk.Now, Metrics: h.reg, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// TestEvictionIsLRUAndNeverInUse is spec 005's pressure rule: filling
// the cache past the ceiling with 20 repositories evicts the least
// recently acquired ones, never one holding a lock, never one under
// the floor, and a subsequent read materializes an evicted one with
// identical rev-list.
func TestEvictionIsLRUAndNeverInUse(t *testing.T) {
	h := newCacheHarness(t, 20)
	ctx := context.Background()
	// Acquired one minute apart, oldest first.
	for _, id := range h.ids {
		h.acquire(id)
		h.clk.Advance(time.Minute)
	}
	total, repos := h.cache.Stats()
	if repos != 20 || total <= 0 {
		t.Fatalf("Stats = %d %d", total, repos)
	}
	per := total / 20
	// Repository 3 is in use for the whole sweep.
	inUse, release, err := h.cache.Acquire(ctx, h.ids[3], false)
	if err != nil {
		t.Fatal(err)
	}
	// Now: 20 minutes after the first acquire, so 0..9 are past the
	// floor and 10..19 (acquired in the last 10 minutes) are under it.
	e := h.evictor(per*12 + 1)
	pressure, idle := e.Sweep(ctx)
	// 20 copies over a ceiling of 12: 8 must go, from the oldest, but 3
	// is in use and 10..19 are under the floor, so only 0, 1, 2, 4..9
	// are candidates: 8 of those 9 go, oldest first, leaving 9.
	if pressure != 8 || idle != 0 {
		t.Fatalf("evicted %d for pressure, %d idle", pressure, idle)
	}
	for i, id := range h.ids {
		want := i == 3 || i == 9 || i >= 10
		if h.held(id) != want {
			t.Errorf("repository %d held %v, want %v", i, h.held(id), want)
		}
	}
	release()
	if _, _, err := h.cache.Acquire(ctx, inUse.ID, false); err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	h.reg.WritePrometheus(&text)
	for _, want := range []string{`origo_evictions_total{reason="pressure"} 8`, `origo_evictions_total{reason="idle"} 0`, "origo_cache_repos 12", "origo_cache_bytes "} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, text.String())
		}
	}
	// A subsequent read materializes an evicted one with the same history.
	r, release, err := h.cache.Acquire(ctx, h.ids[0], false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := gittest.RevList(t, r.Dir); got != gittest.RevList(t, h.src.Dir) {
		t.Fatalf("history differs:\n%s\n%s", got, gittest.RevList(t, h.src.Dir))
	}
	// The copy just materialized puts 13 over the ceiling again: the
	// oldest candidate past the floor goes, and the fresh one stays.
	if p, i := e.Sweep(ctx); p != 1 || i != 0 || !h.held(h.ids[0]) {
		t.Fatalf("over the ceiling by one: %d %d, held %v", p, i, h.held(h.ids[0]))
	}
	// Under the ceiling nothing goes.
	roomy, err := NewEvictor(EvictorOptions{Cache: h.cache, Ceiling: 1 << 40, Now: h.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if p, i := roomy.Sweep(ctx); p != 0 || i != 0 {
		t.Fatalf("under the ceiling: %d %d", p, i)
	}
	// The options are checked.
	if _, err := NewEvictor(EvictorOptions{Cache: h.cache}); err == nil {
		t.Fatal("no ceiling")
	}
	if _, err := NewEvictor(EvictorOptions{Ceiling: 1}); err == nil {
		t.Fatal("no cache")
	}
}

// TestIdleEviction is spec 005's idle rule: a repository not acquired
// for 24 hours is evicted at the next evictor run with a fake clock,
// whatever the pressure, and one acquired since is kept.
func TestIdleEviction(t *testing.T) {
	h := newCacheHarness(t, 2)
	ctx := context.Background()
	h.acquire(h.ids[0])
	h.acquire(h.ids[1])
	e := h.evictor(1 << 40)
	h.clk.Advance(Idle - time.Second)
	if p, i := e.Sweep(ctx); p != 0 || i != 0 {
		t.Fatalf("one second under idle: %d %d", p, i)
	}
	h.acquire(h.ids[1])
	h.clk.Advance(time.Second)
	if p, i := e.Sweep(ctx); p != 0 || i != 1 || h.held(h.ids[0]) || !h.held(h.ids[1]) {
		t.Fatalf("at idle: %d %d, held %v %v", p, i, h.held(h.ids[0]), h.held(h.ids[1]))
	}
	if !strings.Contains(metricsText(h.reg), `origo_evictions_total{reason="idle"} 1`) {
		t.Fatal("idle eviction not counted")
	}
	// Run sweeps on its interval until the context ends.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Run(runCtx) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	// The holder over the cache catches up and reports what is held.
	holder := CacheHolder{Cache: h.cache}
	if seq, ok := holder.Held(h.ids[1]); !ok || seq != 1 {
		t.Fatalf("Held = %d %v", seq, ok)
	}
	if err := holder.CatchUp(ctx, h.ids[0]); err != nil {
		t.Fatal(err)
	}
	if !h.held(h.ids[0]) {
		t.Fatal("the catch-up did not materialize the copy")
	}
	if err := holder.CatchUp(ctx, "00000000-0000-4000-8000-000000000099"); err == nil {
		t.Fatal("a catch-up of an unknown repository succeeded")
	}
}

func metricsText(reg *pkgmetrics.Registry) string {
	var text bytes.Buffer
	reg.WritePrometheus(&text)
	return text.String()
}
