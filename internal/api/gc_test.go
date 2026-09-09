// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/compact"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/wal"
)

// stubCompactor answers what a test fixes, for the states a real
// manager cannot be held in from another package.
type stubCompactor struct {
	res compact.Result
	err error
}

func (s stubCompactor) GC(context.Context, string) (compact.Result, error) { return s.res, s.err }
func (s stubCompactor) Primary(string) string                              { return s.res.Primary }

// threeNodes is a live set of three names with the primary of repoA and
// one other name.
func threeNodes(t *testing.T) (set *placement.Set, primary, other string) {
	t.Helper()
	names := []string{"origod-0", "origod-1", "origod-2"}
	set = placement.NewSet(names[0], nil)
	now := time.Now()
	for _, n := range names[1:] {
		set.Heard(n, now)
	}
	primary = set.Prefer(repoA, 1)[0]
	i := slices.Index(names, primary)
	return set, primary, names[(i+1)%len(names)]
}

// TestGcRoutesToThePrimary is spec 019's gc criterion: a node that is
// not the primary schedules and compacts nothing, the primary runs and
// reports its figures, a run still going is answered with its start
// time, and a second gc within an hour of any compaction is refused.
func TestGcRoutesToThePrimary(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()
	set, primary, other := threeNodes(t)

	// A node that is not the primary answers 202 naming it, leaves the
	// request object of spec 006, and compacts nothing.
	away := newHarness(t, withPlacement(set), withCompaction(other))
	away.seed(f)
	before := mustIndex(t, away.log, repoA)
	status, out := away.do("POST", "/v1/repos/"+repoA+"/gc", "")
	if status != 202 || out["status"] != "scheduled" {
		t.Fatalf("gc away from the primary: %d %v", status, out)
	}
	d, _ := out["details"].(map[string]any)
	if d["primary"] != primary || d["within_seconds"] != float64(ScheduledWithin) {
		t.Fatalf("scheduled details %v", out)
	}
	if _, err := away.store.Head(ctx, away.log.Prefix()+"gc/"+repoA); err != nil {
		t.Fatalf("no request object: %v", err)
	}
	if after := mustIndex(t, away.log, repoA); after.Seq != before.Seq {
		t.Fatalf("a node that is not the primary compacted: %d to %d", before.Seq, after.Seq)
	}

	// The primary runs it and answers the before and after figures.
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	home := newHarness(t, withPlacement(set), withCompaction(primary), withNow(func() time.Time { return now }))
	home.seed(f)
	status, out = home.do("POST", "/v1/repos/"+repoA+"/gc", "")
	if status != 200 {
		t.Fatalf("gc on the primary: %d %v", status, out)
	}
	got, want := out["before"].(map[string]any), out["after"].(map[string]any)
	if got["entries"] != float64(1) || want["entries"] != float64(1) || want["packs"] == float64(0) {
		t.Fatalf("figures %v", out)
	}
	ix := mustIndex(t, home.log, repoA)
	if len(ix.Entries) != 1 || ix.Entries[0].Kind != wal.KindCompact {
		t.Fatalf("the primary did not compact: %+v", ix.Entries)
	}

	// stats reads compacted_at off that entry.
	status, out = home.do("GET", "/v1/repos/"+repoA+"/stats", "")
	if status != 200 || out["compacted_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("stats after the gc: %d %v", status, out)
	}

	// A second gc within the hour is refused, whatever started the
	// compaction, with the repository limit and a Retry-After.
	now = now.Add(30 * time.Minute)
	status, out, header := home.doHeader("POST", "/v1/repos/"+repoA+"/gc", "")
	if status != 429 || code(out) != contract.CodeRateLimited || details(out)["limit"] != limits.LimitRepository {
		t.Fatalf("second gc: %d %v", status, out)
	}
	if header.Get("Retry-After") != "1800" || details(out)["retry_after"] != float64(1800) {
		t.Fatalf("retry after %q %v", header.Get("Retry-After"), details(out))
	}
	// An hour after the compaction it runs again.
	now = now.Add(31 * time.Minute)
	if status, out := home.do("POST", "/v1/repos/"+repoA+"/gc", ""); status != 200 {
		t.Fatalf("gc after the hour: %d %v", status, out)
	}

	// A run still going is 202 with running true and its start time.
	started := time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)
	busy := newHarness(t, withCompactor(stubCompactor{res: compact.Result{Running: true, StartedAt: started}}))
	busy.create(repoB, "acme", "busy")
	status, out = busy.do("POST", "/v1/repos/"+repoB+"/gc", "")
	if status != 202 || out["status"] != "running" {
		t.Fatalf("gc over a running compaction: %d %v", status, out)
	}
	d, _ = out["details"].(map[string]any)
	if d["running"] != true || d["started_at"] != started.Format(time.RFC3339) {
		t.Fatalf("running details %v", out)
	}

	// A failed manager is a 503, and a node without one the same.
	broken := newHarness(t, withCompactor(stubCompactor{err: errors.New("no")}))
	broken.create(repoB, "acme", "broken")
	if status, out := broken.do("POST", "/v1/repos/"+repoB+"/gc", ""); status != 503 || code(out) != contract.CodeStorageUnavailable {
		t.Fatalf("failed manager: %d %v", status, out)
	}
	none := newHarness(t)
	none.create(repoB, "acme", "none")
	if status, out := none.do("POST", "/v1/repos/"+repoB+"/gc", ""); status != 503 {
		t.Fatalf("no manager: %d %v", status, out)
	}
}

func mustIndex(t *testing.T, l *wal.Log, id string) *wal.Index {
	t.Helper()
	ix, _, err := l.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}
