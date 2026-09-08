// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/compact"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/wal"
)

// stackNodes are the three node names of spec 013's ports table, in the
// order of the table: node 1 is origod-0.
var stackNodes = []string{"origod-0", "origod-1", "origod-2"}

// idPreferring draws repository ids until the rendezvous of spec 005
// puts node first, which makes that node the compaction primary. The
// score is a pure function of the name and the id, so the test computes
// it rather than creating repositories until one lands; the stack is
// then asked to confirm it agrees. Compaction runs on the primary alone,
// so a test that wants the node it pushes through to compact picks its
// id here rather than waiting up to a sweep interval for the primary to
// pick up a request object.
func idPreferring(t *testing.T, node string) string {
	t.Helper()
	for range 200 {
		id := newID(t)
		if placement.Rank(stackNodes, id)[0] == node {
			return id
		}
	}
	t.Fatalf("no id of 200 prefers %s", node)
	return ""
}

// newestIndex reads the newest index object of a repository straight
// out of the bucket the stack runs on.
func newestIndex(t *testing.T, s *stack, id string) *wal.Index {
	t.Helper()
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// pushCommits makes n commits of size bytes each and pushes each one
// through port, one push per commit, which is what a busy repository
// does to the log.
func pushCommits(t *testing.T, work string, port int, token, id string, n, size int) {
	t.Helper()
	url := repoURL(port, token, id)
	for i := range n {
		commitFile(t, work, "f.txt", strings.Repeat("x", size)+fmt.Sprint(i), fmt.Sprintf("commit %d", i))
		mustGit(t, work, "push", "-q", url, "HEAD:refs/heads/main")
	}
}

// waitForCompaction waits until the newest index lists at most
// compact.MaxEntries entries. A push whose trigger found a run already
// in flight schedules nothing, and that run ends stale when the push
// lands inside it, so the repository can be left with no run scheduled
// until the next push or the primary's sweep; the wait therefore pushes
// once more between rounds, which is what the next push on a live
// repository does.
func waitForCompaction(t *testing.T, s *stack, work string, port int, token, id string) *wal.Index {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for round := 0; ; round++ {
		ix := newestIndex(t, s, id)
		if len(ix.Entries) <= compact.MaxEntries && ix.CompactedThrough > 0 {
			t.Logf("compacted through %d at seq %d after %d extra pushes: %d entries, %d packs",
				ix.CompactedThrough, ix.Seq, round, len(ix.Entries), len(ix.Packs))
			return ix
		}
		if time.Now().After(deadline) {
			t.Fatalf("no compaction within 5 minutes: %d entries, compacted_through %d at seq %d",
				len(ix.Entries), ix.CompactedThrough, ix.Seq)
		}
		time.Sleep(5 * time.Second)
		if round > 0 && round%6 == 0 {
			pushCommits(t, work, port, token, id, 1, 16)
		}
	}
}

// TestClusterFiveHundredPushesStayUnder64EntriesAnd6Packs is spec 006's
// first criterion: 500 pushes of 1 KiB commits through node 1 of spec
// 013's ports table leave the newest index with at most 64 entries and
// at most 6 packs once the run the last threshold crossing scheduled
// completes, and a clone through node 2 passes git fsck.
func TestClusterFiveHundredPushesStayUnder64EntriesAnd6Packs(t *testing.T) {
	requireNodes(t)
	s := requireStack(t)
	token := adminToken(t)
	id := idPreferring(t, stackNodes[0])
	if status, body := stackAPI(t, stackURL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"compaction","slug":"five-hundred-%s"}`, id, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	s.cleanup(t, id)
	waitUntil(t, "node 1 named as the preferred node", placementWindow, func() bool {
		return preferHeader(t, portNode1, token, id) == stackNodes[0]
	})

	work := clone(t, repoURL(portNode1, token, id))
	started := time.Now()
	pushCommits(t, work, portNode1, token, id, 500, 1<<10)
	t.Logf("MEASURE 500 pushes through node 1 in %s", time.Since(started).Round(time.Second))

	ix := waitForCompaction(t, s, work, portNode1, token, id)
	if len(ix.Entries) > compact.MaxEntries {
		t.Fatalf("the newest index lists %d entries", len(ix.Entries))
	}
	if len(ix.Packs) > 6 {
		t.Fatalf("the newest index lists %d packs", len(ix.Packs))
	}
	// A clone through node 2 materializes from the packs and the entries
	// the newest index lists, and the copy it builds is sound.
	bare := filepath.Join(t.TempDir(), "node2.git")
	mustGit(t, t.TempDir(), "clone", "-q", "--bare", repoURL(portNode1+1, token, id), bare)
	mustGit(t, bare, "fsck", "--connectivity-only", "--no-progress")
	if got := strings.TrimSpace(mustGit(t, bare, "rev-list", "--count", "HEAD")); got != "500" {
		t.Fatalf("the clone holds %s commits, want 500", got)
	}
}

// cloneP50 clones the repository n times through port, one after
// another, and answers the median wall time.
func cloneP50(t *testing.T, port int, token, id string, n int) time.Duration {
	t.Helper()
	root := t.TempDir()
	var times []time.Duration
	for i := range n {
		dir := filepath.Join(root, fmt.Sprintf("c%d.git", i))
		started := time.Now()
		mustGit(t, root, "clone", "-q", "--bare", repoURL(port, token, id), dir)
		times = append(times, time.Since(started))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	return times[len(times)/2]
}

// fetchLatencyIsFlat is the body of the fetch-latency criterion, with
// the fixture size and the push count as arguments so TestMeasure runs
// the same routine on a larger repository without asserting. It answers
// the two medians.
func fetchLatencyIsFlat(t *testing.T, token string, blobs, pushes int) (early, late time.Duration) {
	t.Helper()
	id := idPreferring(t, stackNodes[0])
	if status, body := stackAPI(t, stackURL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"compaction","slug":"latency-%s"}`, id, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	s := requireStack(t)
	s.cleanup(t, id)
	waitUntil(t, "node 1 named as the preferred node", placementWindow, func() bool {
		return preferHeader(t, portNode1, token, id) == stackNodes[0]
	})
	// The fixture: one commit of blobs mebibytes, then the ten pushes
	// the first measurement is taken after.
	src := gittest.NewSource(t)
	for i := range blobs {
		src.Write(fmt.Sprintf("blob-%d.bin", i), gittest.Bytes(1<<20, uint64(i+7))) //nolint:gosec // a fixture seed
	}
	src.CommitAll("the fixture")
	mustGit(t, src.Dir, "push", "-q", repoURL(portNode1, token, id), "HEAD:refs/heads/main")
	work := clone(t, repoURL(portNode1, token, id))
	pushCommits(t, work, portNode1, token, id, 10, 1<<10)
	early = cloneP50(t, portNode1, token, id, 10)

	started := time.Now()
	pushCommits(t, work, portNode1, token, id, pushes, 1<<10)
	t.Logf("MEASURE %d pushes onto a %d MiB repository in %s", pushes, blobs, time.Since(started).Round(time.Second))
	ix := waitForCompaction(t, s, work, portNode1, token, id)
	t.Logf("MEASURE after %d pushes: %d entries, %d packs, %d bytes", pushes, len(ix.Entries), len(ix.Packs), ix.SizeBytes)
	late = cloneP50(t, portNode1, token, id, 10)
	return early, late
}

// TestClusterCompactionKeepsFetchLatencyFlat is spec 006's tuning
// target: the p50 of ten clones of a repository through node 1 of spec
// 013's ports table is within 25% after many pushes of what it was after
// ten, because compaction takes the entry count off the fetch path. The
// fixture is 20 MiB and 200 pushes, sized for the e2e job's budget;
// TestMeasure runs the same routine on a 1 GiB fixture and 1 000 pushes
// under ORIGO_E2E_MEASURE=1 and asserts nothing.
func TestClusterCompactionKeepsFetchLatencyFlat(t *testing.T) {
	requireNodes(t)
	early, late := fetchLatencyIsFlat(t, adminToken(t), 20, 200)
	t.Logf("MEASURE clone p50: %s after 10 pushes, %s after 200", early.Round(time.Millisecond), late.Round(time.Millisecond))
	if late > early*5/4 {
		t.Fatalf("clone p50 rose from %s to %s, more than 25%%", early, late)
	}
}

// measureCompaction is TestMeasure's compaction subtest: the same
// figures on a 1 GiB fixture and 1 000 pushes, printed and not
// asserted.
func measureCompaction(t *testing.T) {
	requireNodes(t)
	early, late := fetchLatencyIsFlat(t, adminToken(t), 1024, 1000)
	t.Logf("MEASURE clone p50 of a 1 GiB repository: %s after 10 pushes, %s after 1000", early.Round(time.Millisecond), late.Round(time.Millisecond))
}
