// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
