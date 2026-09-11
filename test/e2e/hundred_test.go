// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestE2EHundredConcurrentPushesFromEightClients is spec 004's eighth
// criterion: 100 pushes to distinct branches from 8 clients at once,
// against one node. Every push lands, the newest index lists the 100
// entries in sequence order, and exactly one index object exists per
// sequence. A lost round leaves an orphan entry the sweeper removes
// later, so the entry count is not asserted; the index chain is.
func TestE2EHundredConcurrentPushesFromEightClients(t *testing.T) {
	const clients, pushes = 8, 100
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "acme", "app-"+id[:8])

	// Each client clones the empty repository once and prepares its
	// share of commits on a chain of its own, so the concurrent phase is
	// pushes and nothing else. Client i pushes commit k to branch
	// c<i>-<i+8k>, which names 100 distinct branches.
	type client struct {
		dir  string
		shas []string
	}
	cs := make([]client, clients)
	for i := range cs {
		cs[i].dir = clone(t, n.url(id))
		for j := i; j < pushes; j += clients {
			cs[i].shas = append(cs[i].shas, commitFile(t, cs[i].dir, fmt.Sprintf("c%d-%d.txt", i, j), "x", fmt.Sprintf("client %d push %d", i, j)))
		}
	}
	branch := func(i, k int) string { return fmt.Sprintf("refs/heads/c%d-%d", i, i+k*clients) }

	var wg sync.WaitGroup
	errs := make(chan error, pushes)
	for i, c := range cs {
		wg.Go(func() {
			for k, sha := range c.shas {
				if out, err := git(t, c.dir, "push", "-q", "origin", sha+":"+branch(i, k)); err != nil {
					errs <- fmt.Errorf("%s: %v\n%s", branch(i, k), err, out)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// One index object per sequence, 000000000000 to 000000000100, and
	// the newest lists the 100 entries in order with every branch at
	// the commit its client pushed.
	indexes := s.keys(t, id, "index/0")
	if len(indexes) != pushes+1 {
		t.Fatalf("%d index objects, want %d", len(indexes), pushes+1)
	}
	for seq, key := range indexes {
		if want := fmt.Sprintf("index/%012d", seq); key != want {
			t.Fatalf("index object %d is %q, want %q", seq, key, want)
		}
	}
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil || ix.Seq != pushes || len(ix.Entries) != pushes {
		t.Fatalf("newest index: seq %d, %d entries, %v", ix.Seq, len(ix.Entries), err)
	}
	for k, e := range ix.Entries {
		if e.Seq != uint64(k+1) || e.Kind != "push" {
			t.Fatalf("entry %d is seq %d kind %q", k, e.Seq, e.Kind)
		}
	}
	for i, c := range cs {
		for k, sha := range c.shas {
			if got := ix.Refs[branch(i, k)]; got != sha {
				t.Errorf("%s = %q, want %s", branch(i, k), got, sha)
			}
		}
	}
	// A fresh clone advertises all 100 branches.
	heads := mustGit(t, t.TempDir(), "ls-remote", "--heads", n.url(id))
	if got := strings.Count(heads, "\n") + 1; got != pushes {
		t.Fatalf("ls-remote advertises %d heads, want %d", got, pushes)
	}
}
