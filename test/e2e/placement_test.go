// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

// freeUDP reserves a loopback UDP address for a node's gossip socket.
func freeUDP(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.LocalAddr().String()
}

// metric reads one series from a node's /metrics: name with the exact
// label set, "" for an unlabelled series.
func (n *node) metric(name, labels string) float64 {
	n.t.Helper()
	code, body := n.get(n.internal, "/metrics")
	if code != 200 {
		n.t.Fatalf("metrics: %d", code)
	}
	return parseMetric(n.t, string(body), name, labels)
}

func parseMetric(t *testing.T, text, name, labels string) float64 {
	t.Helper()
	prefix := name + " "
	if labels != "" {
		prefix = name + "{" + labels + "} "
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			var v float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, prefix), "%g", &v); err != nil {
				t.Fatalf("metric %s: %q: %v", name, line, err)
			}
			return v
		}
	}
	t.Fatalf("metric %s{%s} is not served", name, labels)
	return 0
}

// gossipPair starts two nodes of the harness with each other as peers
// under one secret, the way spec 005's two-node tests run.
func gossipPair(t *testing.T, s *stack) (*node, *node) {
	t.Helper()
	secret := strings.Repeat("e2e-gossip-secret-", 2)
	addrA, addrB := freeUDP(t), freeUDP(t)
	a := startNode(t, s, "", map[string]string{"ORIGO_NODE_NAME": "node-a", "ORIGO_GOSSIP_ADDR": addrA, "ORIGO_GOSSIP_PEERS": addrB, "ORIGO_GOSSIP_SECRET": secret})
	b := startNode(t, s, "", map[string]string{"ORIGO_NODE_NAME": "node-b", "ORIGO_GOSSIP_ADDR": addrB, "ORIGO_GOSSIP_PEERS": addrA, "ORIGO_GOSSIP_SECRET": secret})
	return a, b
}

// TestE2EGossipShortensTheCatchUp is spec 005's announcement: a push
// acknowledged on node A is returned by a fetch on node B. With
// gossip, B applies the entry in the background before any request,
// so the fetch's currency check answers 404; without gossip the first
// fetch on B finds the newer index with a 200. The time from A's
// acknowledgement to B's catch-up is recorded.
func TestE2EGossipShortensTheCatchUp(t *testing.T) {
	s := requireStack(t)
	a, b := gossipPair(t, s)
	id := newID(t)
	a.createRepo(id, "acme", "app-"+id[:8])
	work := clone(t, a.url(id))
	commitFile(t, work, "a.txt", "one", "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	// B holds the repository warm at sequence 1.
	warm := clone(t, b.url(id))
	applied := b.metric("origo_repo_entries_applied_total", "")

	pushed := commitFile(t, work, "b.txt", "two", "second")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	acknowledged := time.Now()
	deadline := acknowledged.Add(10 * time.Second)
	for b.metric("origo_repo_entries_applied_total", "") != applied+1 {
		if time.Now().After(deadline) {
			t.Fatalf("B did not catch up on the announcement\n%s", b.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	caughtUp := time.Since(acknowledged)
	t.Logf("MEASURE gossip: B applied the entry %s after A's acknowledgement", caughtUp)

	// The fetch on B: one request (protocol v1's advertisement answers
	// the references; v2 would add an ls-refs POST, a second check)
	// whose currency check answers 404, because the catch-up ran before
	// the request.
	miss, hit := b.metric("origo_wal_head_check_seconds_count", `result="404"`), b.metric("origo_wal_head_check_seconds_count", `result="200"`)
	if out := mustGit(t, warm, "-c", "protocol.version=1", "ls-remote", "-q", b.url(id), "refs/heads/main"); !strings.HasPrefix(out, pushed) {
		t.Fatalf("ls-remote on B: %q, want %s", out, pushed)
	}
	if got := b.metric("origo_wal_head_check_seconds_count", `result="404"`); got != miss+1 {
		t.Fatalf("404 checks rose by %v, want 1", got-miss)
	}
	if got := b.metric("origo_wal_head_check_seconds_count", `result="200"`); got != hit {
		t.Fatalf("200 checks rose by %v, want 0", got-hit)
	}
	mustGit(t, warm, "fetch", "-q", "origin")
	if mustGit(t, warm, "rev-parse", "origin/main") != pushed || b.metric("origo_wal_head_check_seconds_count", `result="200"`) != hit {
		t.Fatal("the fetch on B did not find the commit from the copy")
	}
	if got := b.metric("origo_gossip_packets_total", `direction="received"`); got < 3 {
		t.Fatalf("B received %v datagrams, want the three announcements at least", got)
	}

	// Gossip disabled: two nodes with no peers, the first fetch on the
	// second finds the newer index with one 200.
	c := startNode(t, s, "", nil)
	d := startNode(t, s, "", nil)
	cold := clone(t, d.url(id))
	miss, hit = d.metric("origo_wal_head_check_seconds_count", `result="404"`), d.metric("origo_wal_head_check_seconds_count", `result="200"`)
	mustGit(t, work, "remote", "set-url", "origin", c.url(id))
	third := commitFile(t, work, "c.txt", "three", "third")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if out := mustGit(t, cold, "-c", "protocol.version=1", "ls-remote", "-q", d.url(id), "refs/heads/main"); !strings.HasPrefix(out, third) {
		t.Fatalf("ls-remote on D: %q, want %s", out, third)
	}
	if got := d.metric("origo_wal_head_check_seconds_count", `result="200"`); got != hit+1 {
		t.Fatalf("without gossip: 200 checks rose by %v, want 1", got-hit)
	}
	// A check that found a newer index walks forward until a 404, so
	// the 404 count rises by one as well.
	if got := d.metric("origo_wal_head_check_seconds_count", `result="404"`); got != miss+1 {
		t.Fatalf("without gossip: 404 checks rose by %v, want 1", got-miss)
	}
	if got := d.metric("origo_gossip_packets_total", `direction="received"`); got != 0 {
		t.Fatalf("D received %v datagrams with gossip disabled", got)
	}
}

// TestE2EDrainLosesNoPush is spec 005's drain: a node receiving 100
// concurrent pushes is told to stop; every push is either acknowledged
// and in the newest index or refused and absent from it.
func TestE2EDrainLosesNoPush(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "acme", "app-"+id[:8])
	base := clone(t, n.url(id))
	commitFile(t, base, "base.txt", "base", "base")
	mustGit(t, base, "push", "-q", "origin", "HEAD:refs/heads/main")

	const pushes = 100
	type result struct {
		sha string
		out string
		err error
	}
	dirs := make([]string, pushes)
	shas := make([]string, pushes)
	for i := range pushes {
		dirs[i] = filepath.Join(t.TempDir(), fmt.Sprintf("p%d", i))
		mustGit(t, t.TempDir(), "clone", "-q", "--no-hardlinks", base, dirs[i])
		mustGit(t, dirs[i], "remote", "set-url", "origin", n.url(id))
		shas[i] = commitFile(t, dirs[i], fmt.Sprintf("f%d.txt", i), fmt.Sprintf("%d", i), fmt.Sprintf("push %d", i))
	}
	results := make([]result, pushes)
	var wg sync.WaitGroup
	for i := range pushes {
		wg.Go(func() {
			out, err := git(t, dirs[i], "push", "-q", "origin", fmt.Sprintf("HEAD:refs/heads/b%d", i))
			results[i] = result{sha: shas[i], out: out, err: err}
		})
	}
	// The node is told to stop while the pushes are in flight: unready,
	// the drain delay, then the listeners close and in-flight requests
	// get the grace period.
	time.Sleep(300 * time.Millisecond)
	if err := n.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := n.wait(); err != nil {
		t.Fatalf("origod exited: %v", err)
	}
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledged, refused, transport int
	for i, r := range results {
		ref := fmt.Sprintf("refs/heads/b%d", i)
		got, present := ix.Refs[ref]
		switch {
		case r.err == nil:
			acknowledged++
			if !present || got != r.sha {
				t.Errorf("push %d acknowledged but %s is %q in the newest index", i, ref, got)
			}
		default:
			if present {
				t.Errorf("push %d failed but %s is in the newest index:\n%s", i, ref, r.out)
			}
			if strings.Contains(r.out, "storage_unavailable") {
				refused++
			} else {
				transport++
			}
		}
	}
	t.Logf("MEASURE drain: %d pushes acknowledged, %d refused with storage_unavailable, %d failed to connect; newest index seq %d", acknowledged, refused, transport, ix.Seq)
	if acknowledged == 0 {
		t.Fatal("no push was acknowledged before the drain")
	}
	if int(ix.Seq) != acknowledged+1 {
		t.Fatalf("newest index seq %d, want %d", ix.Seq, acknowledged+1)
	}
}

// writeEntries writes n entries for the repository through Log.Commit,
// one per commit with a thin pack the harness builds, not by git push:
// the fixture of the materialization budget and of TestMeasure.
func writeEntries(t *testing.T, s *stack, id string, n int) {
	t.Helper()
	src := gittest.NewSource(t)
	held, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	prev := ""
	for i := range n {
		c := src.Commit("payload", string(gittest.Bytes(1024, uint64(i+1))), fmt.Sprintf("c%d", i))
		var pack []byte
		old := wal.ZeroSHA
		if prev == "" {
			pack = src.Pack(c)
		} else {
			pack = src.Pack(c, prev)
			old = prev
		}
		e := wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{Ref: "refs/heads/main", Old: old, New: c}}, Pack: wal.BytesBody(pack)}
		committed, err := s.log.Commit(context.Background(), id, held, e, func(context.Context, *wal.Index) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		held = committed.Index
		prev = c
	}
}

// TestE2EMaterializeThousandEntriesUnderBudget is spec 005's
// materialization budget: 1 000 entries, each a thin pack over the
// previous one, materialize onto an empty disk with 4 workers in under
// 30 seconds; the fixture is written under a budget of its own of 5
// minutes.
func TestE2EMaterializeThousandEntriesUnderBudget(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "bench", "materialize-"+id[:8])
	n.stop()
	start := time.Now()
	writeEntries(t, s, id, 1000)
	if took := time.Since(start); took > 5*time.Minute {
		t.Fatalf("writing the fixture took %s, over its 5 minute budget", took)
	}
	t.Logf("MEASURE wrote 1000 entries in %s", time.Since(start).Round(time.Millisecond))

	fresh := startNode(t, s, "", nil)
	if entries, _ := os.ReadDir(filepath.Join(fresh.dataDir, "repos")); len(entries) != 0 {
		t.Fatalf("the fresh node's disk holds %d entries", len(entries))
	}
	dir := filepath.Join(t.TempDir(), "ls")
	mustGit(t, t.TempDir(), "init", "-q", dir)
	start = time.Now()
	mustGit(t, dir, "ls-remote", "-q", fresh.url(id))
	took := time.Since(start)
	t.Logf("MEASURE materialize 1000 entries onto an empty disk: %s", took.Round(time.Millisecond))
	if took > 30*time.Second {
		t.Fatalf("materialization took %s, over the 30 second budget", took)
	}
	if got := fresh.metric("origo_repo_entries_applied_total", ""); got != 1000 {
		t.Fatalf("applied %v entries", got)
	}
	cl := clone(t, fresh.url(id))
	if mustGit(t, cl, "rev-list", "--count", "HEAD") != "1000" {
		t.Fatal("clone is missing commits")
	}
	mustGit(t, cl, "fsck", "--no-progress")
}
