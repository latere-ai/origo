// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/compact"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

// TestE2EPushThenCloneFromAnEmptyDisk is the phase 1 exit criterion: a
// repository that exists only in the bucket is cloned by a node with an
// empty disk, and the clone's history equals the pushed history.
func TestE2EPushThenCloneFromAnEmptyDisk(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "acme", "app-"+id[:8])

	work := clone(t, n.url(id))
	commitFile(t, work, "a.txt", "one", "first")
	commitFile(t, work, "b.txt", "two", "second")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	mustGit(t, work, "checkout", "-q", "-b", "dev")
	commitFile(t, work, "c.txt", "three", "third")
	mustGit(t, work, "tag", "-a", "-m", "v1", "v1")
	mustGit(t, work, "push", "-q", "origin", "dev", "refs/tags/v1")
	want := gittest.RevList(t, work)

	// The node dies and its disk is wiped; a fresh process serves the
	// clone from the log alone.
	n.kill()
	if err := os.RemoveAll(n.dataDir); err != nil {
		t.Fatal(err)
	}
	fresh := startNode(t, s, "", nil)
	cl := clone(t, fresh.url(id))
	if got := gittest.RevList(t, cl); got != want {
		t.Fatalf("history differs:\n%s\nwant\n%s", got, want)
	}
	mustGit(t, cl, "fsck", "--no-progress")
	if mustGit(t, cl, "rev-parse", "refs/tags/v1^{commit}") != mustGit(t, work, "rev-parse", "dev") {
		t.Fatal("tag")
	}
	// A further push and fetch through the fresh node round-trip too.
	commitFile(t, cl, "d.txt", "four", "fourth")
	mustGit(t, cl, "push", "-q", "origin", "HEAD:refs/heads/main")
	mustGit(t, work, "remote", "set-url", "origin", fresh.url(id))
	mustGit(t, work, "fetch", "-q", "origin")
	if mustGit(t, work, "rev-parse", "origin/main") != mustGit(t, cl, "rev-parse", "HEAD") {
		t.Fatal("fetch after push")
	}
	if status, out := fresh.api("GET", "/v1/repos/"+id, ""); status != 200 || out["head"] != mustGit(t, cl, "rev-parse", "HEAD") {
		t.Fatalf("api: %d %v", status, out)
	}
	// Every push made exactly one entry and one index object.
	if entries, indexes := s.keys(t, id, "wal/"), s.keys(t, id, "index/0"); len(entries) != 3 || len(indexes) != 4 {
		t.Fatalf("%d entries, %d index objects", len(entries), len(indexes))
	}
}

// TestE2EConcurrentPushesToDifferentBranchesOnTwoNodes: two nodes accept
// pushes to different branches of one repository at the same time; both
// land, the newest index has both, and every sequence has one index
// object.
func TestE2EConcurrentPushesToDifferentBranchesOnTwoNodes(t *testing.T) {
	s := requireStack(t)
	a := startNode(t, s, "", nil)
	b := startNode(t, s, "", nil)
	id := newID(t)
	a.createRepo(id, "acme", "app-"+id[:8])
	base := clone(t, a.url(id))
	commitFile(t, base, "base.txt", "base", "base")
	mustGit(t, base, "push", "-q", "origin", "HEAD:refs/heads/main")

	left := clone(t, a.url(id))
	right := clone(t, b.url(id))
	commitFile(t, left, "left.txt", "left", "left")
	commitFile(t, right, "right.txt", "right", "right")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, p := range []struct{ dir, branch string }{{left, "refs/heads/left"}, {right, "refs/heads/right"}} {
		wg.Go(func() {
			if out, err := git(t, p.dir, "push", "-q", "origin", "HEAD:"+p.branch); err != nil {
				errs <- errors.New(out)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil || ix.Seq != 3 || ix.Refs["refs/heads/left"] != mustGit(t, left, "rev-parse", "HEAD") || ix.Refs["refs/heads/right"] != mustGit(t, right, "rev-parse", "HEAD") {
		t.Fatalf("newest index: %+v, %v", ix, err)
	}
	indexes := s.keys(t, id, "index/0")
	if strings.Join(indexes, ",") != "index/000000000000,index/000000000001,index/000000000002,index/000000000003" {
		t.Fatalf("index objects: %v", indexes)
	}
	// A third node sees both branches.
	c := startNode(t, s, "", nil)
	cl := clone(t, c.url(id))
	if mustGit(t, cl, "rev-parse", "origin/left") == "" || mustGit(t, cl, "rev-parse", "origin/right") == "" {
		t.Fatal("branches missing on the third node")
	}
}

// TestE2EKillMidPush kills the node between the entry write and the index
// commit: the client sees a failure, nothing is visible, the orphaned
// entry is swept, and a retry lands.
func TestE2EKillMidPush(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", map[string]string{"ORIGO_FAILPOINT": wal.FailpointBeforeIndex})
	id := newID(t)
	n.createRepo(id, "acme", "app-"+id[:8])
	work := clone(t, n.url(id))
	commitFile(t, work, "a.txt", "one", "first")
	if out, err := git(t, work, "push", "-q", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("push acknowledged although the node died:\n%s", out)
	}
	if err := n.wait(); err == nil {
		t.Fatal("node did not exit at the failpoint")
	}
	entries, indexes := s.keys(t, id, "wal/"), s.keys(t, id, "index/0")
	if len(entries) != 1 || len(indexes) != 1 {
		t.Fatalf("after the kill: entries %v indexes %v", entries, indexes)
	}
	orphan := entries[0]

	// A node without the failpoint sees no change, and the retry lands
	// at sequence 1 under a fresh nonce.
	fresh := startNode(t, s, n.dataDir, map[string]string{"ORIGO_SWEEP_INTERVAL": "500ms", "ORIGO_SWEEP_MIN_AGE": "0s"})
	empty := clone(t, fresh.url(id))
	if out, err := git(t, empty, "rev-parse", "--verify", "-q", "origin/main"); err == nil {
		t.Fatalf("the unacknowledged push is visible: %s", out)
	}
	mustGit(t, work, "remote", "set-url", "origin", fresh.url(id))
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil || ix.Seq != 1 || ix.Entry == orphan {
		t.Fatalf("after the retry: %+v, %v", ix, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		entries = s.keys(t, id, "wal/")
		if len(entries) == 1 && entries[0] == ix.Entry {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan not swept: %v", entries)
		}
		time.Sleep(200 * time.Millisecond)
	}
	cl := clone(t, fresh.url(id))
	if gittest.RevList(t, cl) != gittest.RevList(t, work) {
		t.Fatal("history after the retry")
	}
}

// TestE2EHundredConcurrentPushesFromEightClients is spec 004's
// concurrency criterion: 100 pushes to distinct branches, driven by 8
// clients at once against one node, all land; the newest index lists
// 100 entries in sequence order and carries every branch at the commit
// its client pushed; and every sequence from 0 to 100 has exactly one
// index object, which is what the create-if-absent commit linearizes.
//
// The 100 branches are prepared before the clients start, so the window
// the test measures is the commit path and not git's own object
// writing. Each client owns a disjoint slice of the branches and pushes
// them one after another, so 8 pushes are in flight at any moment and
// the writers race for index/<n+1> a hundred times.
func TestE2EHundredConcurrentPushesFromEightClients(t *testing.T) {
	const branches, clients = 100, 8
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "acme", "app-"+id[:8])

	// One working copy per client, each with its own commits, so no two
	// clients share a git directory while they push.
	type work struct {
		dir   string
		heads map[string]string
	}
	works := make([]*work, clients)
	for c := range clients {
		dir := clone(t, n.url(id))
		w := &work{dir: dir, heads: map[string]string{}}
		for b := c; b < branches; b += clients {
			branch := fmt.Sprintf("refs/heads/b%d", b)
			w.heads[branch] = commitFile(t, dir, "payload", fmt.Sprintf("commit for %s", branch), fmt.Sprintf("b%d", b))
		}
		works[c] = w
	}

	var wg sync.WaitGroup
	errs := make(chan error, branches)
	for _, w := range works {
		wg.Go(func() {
			for branch, head := range w.heads {
				if out, err := git(t, w.dir, "push", "-q", "origin", head+":"+branch); err != nil {
					errs <- fmt.Errorf("push %s: %v\n%s", branch, err, out)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// index/0 is created with the repository and lists no entry, so 100
	// pushes leave the newest index at sequence 100 with 100 entries.
	if ix.Seq != branches || len(ix.Entries) != branches {
		t.Fatalf("newest index: seq %d, %d entries, want %d and %d", ix.Seq, len(ix.Entries), branches, branches)
	}
	for i, e := range ix.Entries {
		if e.Seq != uint64(i+1) {
			t.Fatalf("entry %d of the newest index is at sequence %d, so the list is not in sequence order", i, e.Seq)
		}
	}
	for _, w := range works {
		for branch, head := range w.heads {
			if got := ix.Refs[branch]; got != head {
				t.Errorf("the newest index has %s at %q, the client pushed %q", branch, got, head)
			}
		}
	}
	// One index object per sequence: a second winner at any sequence
	// would be an object the list below does not have room for.
	indexes := s.keys(t, id, "index/0")
	if len(indexes) != branches+1 {
		t.Fatalf("%d index objects for %d pushes", len(indexes), branches)
	}
	for i, key := range indexes {
		if want := fmt.Sprintf("index/%012d", i); key != want {
			t.Fatalf("index object %d is %s, want %s", i, key, want)
		}
	}
}

// TestSlowMaterializeTenThousandEntries is spec 004's materialization
// ceiling: a repository whose newest index lists 10 000 entries and the
// packs of three compactions is materialized onto an empty disk, and
// git fsck passes on the result.
//
// The entries are written by the harness through Log.Commit, not by git
// push, because the criterion is the size of the log a node must apply
// and not the throughput of the receive path; writeEntries is the same
// builder TestE2EMaterializeThousandEntriesUnderBudget uses, at ten
// times the count. The packs come from compaction, which a node
// schedules only on the push path when the entries an index lists cross
// compact.MaxEntries, so each round writes that many entries through
// the log and then pushes one commit through the node to cross it. How
// many packs a run leaves is what `git repack --geometric=2` decides,
// so the rounds run until the index lists the three the criterion
// names.
//
// The node is stopped before the 10 000 entries are written, so nothing
// compacts them away and the fresh node below applies every one.
func TestSlowMaterializeTenThousandEntries(t *testing.T) {
	const entries, packs = 10000, 3
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "bench", "ceiling-"+id[:8])

	start := time.Now()
	for round := 0; len(newestIndexOf(t, s, id).Packs) < packs; round++ {
		if round == 10 {
			t.Fatalf("%d compaction rounds left %d packs", round, len(newestIndexOf(t, s, id).Packs))
		}
		writeEntriesTo(t, s, id, fmt.Sprintf("refs/heads/fill-%d", round), compact.MaxEntries+1)
		through := newestIndexOf(t, s, id).CompactedThrough
		work := clone(t, n.url(id))
		commitFile(t, work, "round", fmt.Sprintf("round %d", round), fmt.Sprintf("round %d", round))
		mustGit(t, work, "push", "-q", "origin", fmt.Sprintf("HEAD:refs/heads/round-%d", round))
		waitForCompactionPast(t, s, id, through)
	}
	held := newestIndexOf(t, s, id)
	t.Logf("MEASURE %d compaction packs after %s", len(held.Packs), time.Since(start).Round(time.Millisecond))

	// Nothing compacts while the ceiling is written: compaction is
	// scheduled on the push path alone, and no push reaches a node that
	// is not running.
	n.stop()
	// The last compaction left the entries it did not fold, the round's
	// own push among them, so the fill is the ceiling less what the
	// index already lists and the total is the count the criterion
	// names.
	kept := len(held.Entries)
	start = time.Now()
	writeEntries(t, s, id, entries-kept)
	t.Logf("MEASURE wrote %d entries in %s", entries-kept, time.Since(start).Round(time.Millisecond))
	ix := newestIndexOf(t, s, id)
	if len(ix.Entries) != entries {
		t.Fatalf("the newest index lists %d entries, want %d", len(ix.Entries), entries)
	}
	if len(ix.Packs) < packs {
		t.Fatalf("the newest index lists %d packs, want at least %d", len(ix.Packs), packs)
	}

	fresh := startNode(t, s, "", nil)
	if dir, _ := os.ReadDir(filepath.Join(fresh.dataDir, "repos")); len(dir) != 0 {
		t.Fatalf("the fresh node's disk holds %d entries", len(dir))
	}
	ls := filepath.Join(t.TempDir(), "ls")
	mustGit(t, t.TempDir(), "init", "-q", ls)
	start = time.Now()
	mustGit(t, ls, "ls-remote", "-q", fresh.url(id))
	t.Logf("MEASURE materialize %d entries and %d packs onto an empty disk: %s",
		entries, len(ix.Packs), time.Since(start).Round(time.Millisecond))
	if got := fresh.metric("origo_repo_entries_applied_total", ""); got != entries {
		t.Fatalf("the fresh node applied %v entries, want %d", got, entries)
	}

	cl := clone(t, fresh.url(id))
	// writeEntries commits a chain of its own, one commit per entry, and
	// the fill above is what refs/heads/main names.
	if got := mustGit(t, cl, "rev-list", "--count", "HEAD"); got != fmt.Sprint(entries-kept) {
		t.Fatalf("the clone holds %s commits, want %d", got, entries-kept)
	}
	mustGit(t, cl, "fsck", "--no-progress")
}

// newestIndexOf is the newest index of a repository read straight from
// the log, with no node in the path.
func newestIndexOf(t *testing.T, s *stack, id string) *wal.Index {
	t.Helper()
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// waitForCompactionPast waits for the compaction the round's push
// scheduled: the newest index names a compacted_through above the one
// the round started from, within five minutes.
func waitForCompactionPast(t *testing.T, s *stack, id string, through uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		ix := newestIndexOf(t, s, id)
		if ix.CompactedThrough > through {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no compaction within five minutes: %d entries, compacted_through %d, %d packs",
				len(ix.Entries), ix.CompactedThrough, len(ix.Packs))
		}
		time.Sleep(time.Second)
	}
}
