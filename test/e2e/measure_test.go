// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

// TestMeasure records the three numbers spec 004's Outcome carries:
// pushes per second one node sustains for 60 seconds with 1 KiB
// commits, the latency of the HEAD currency check, and the time to
// materialize a repository of 1000 entries onto an empty disk. It runs
// only with ORIGO_E2E_MEASURE=1 and prints its results.
func TestMeasure(t *testing.T) {
	if os.Getenv("ORIGO_E2E_MEASURE") != "1" {
		t.Skip("ORIGO_E2E_MEASURE is not set")
	}
	s := requireStack(t)
	t.Run("pushes per second", func(t *testing.T) { measurePushes(t, s) })
	t.Run("head currency check", func(t *testing.T) { measureHead(t, s) })
	t.Run("materialize 1000 entries", func(t *testing.T) { measureMaterialize(t, s) })
}

func percentiles(d []time.Duration) (p50, p99 time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)*50/100], d[min(len(d)*99/100, len(d)-1)]
}

// measurePushes runs four clients, each pushing 1 KiB commits to its own
// branch of one repository, for 60 seconds.
func measurePushes(t *testing.T, s *stack) {
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "bench", "pushes-"+id[:8])
	const clients = 4
	const window = 60 * time.Second
	var total atomic.Int64
	var mu sync.Mutex
	var latencies []time.Duration
	var wg sync.WaitGroup
	deadline := time.Now().Add(window)
	for c := range clients {
		dir := clone(t, n.url(id))
		mustGit(t, dir, "checkout", "-q", "-b", fmt.Sprintf("client-%d", c))
		wg.Go(func() {
			buf := make([]byte, 1024)
			for i := 0; time.Now().Before(deadline); i++ {
				_, _ = rand.Read(buf)
				if err := os.WriteFile(filepath.Join(dir, "payload"), buf, 0o644); err != nil {
					t.Error(err)
					return
				}
				mustGit(t, dir, "add", "payload")
				mustGit(t, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
				start := time.Now()
				if out, err := git(t, dir, "push", "-q", "origin", fmt.Sprintf("HEAD:refs/heads/client-%d", c)); err != nil {
					t.Errorf("push: %v\n%s", err, out)
					return
				}
				mu.Lock()
				latencies = append(latencies, time.Since(start))
				mu.Unlock()
				total.Add(1)
			}
		})
	}
	wg.Wait()
	p50, p99 := percentiles(latencies)
	t.Logf("MEASURE pushes: %d in %s by %d clients = %.1f pushes/s; push latency p50 %s p99 %s",
		total.Load(), window, clients, float64(total.Load())/window.Seconds(), p50, p99)
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil || ix.Seq != uint64(total.Load()) {
		t.Fatalf("newest index seq %d, pushes %d, %v", ix.Seq, total.Load(), err)
	}
}

// measureHead times the currency check as a node performs it: HEAD on
// the index object after the newest, which answers 404.
func measureHead(t *testing.T, s *stack) {
	id := newID(t)
	n := startNode(t, s, "", nil)
	n.createRepo(id, "bench", "head-"+id[:8])
	const samples = 500
	miss := make([]time.Duration, 0, samples)
	hit := make([]time.Duration, 0, samples)
	for range samples {
		start := time.Now()
		if ok, err := s.log.HasIndex(context.Background(), id, 1); err != nil || ok {
			t.Fatalf("HEAD index/1: %v %v", ok, err)
		}
		miss = append(miss, time.Since(start))
		start = time.Now()
		if ok, err := s.log.HasIndex(context.Background(), id, 0); err != nil || !ok {
			t.Fatalf("HEAD index/0: %v %v", ok, err)
		}
		hit = append(hit, time.Since(start))
	}
	m50, m99 := percentiles(miss)
	h50, h99 := percentiles(hit)
	t.Logf("MEASURE head: 404 (current) p50 %s p99 %s; 200 (newer exists) p50 %s p99 %s; %d samples each", m50, m99, h50, h99, samples)
}

// measureMaterialize writes 1000 entries through the log, then times a
// node with an empty disk serving the first advertisement, which is the
// materialization, and a full clone.
func measureMaterialize(t *testing.T, s *stack) {
	id := newID(t)
	n := startNode(t, s, "", nil)
	n.createRepo(id, "bench", "materialize-"+id[:8])
	src := gittest.NewSource(t)
	base, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	held := base
	prev := ""
	start := time.Now()
	for i := range 1000 {
		buf := make([]byte, 1024)
		_, _ = rand.Read(buf)
		c := src.Commit("payload", string(buf), fmt.Sprintf("c%d", i))
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
	t.Logf("MEASURE wrote 1000 entries in %s", time.Since(start))
	n.stop()
	fresh := startNode(t, s, "", nil)
	start = time.Now()
	if code, _ := fresh.get(fresh.public, "/r/"+id+".git/info/refs?service=git-upload-pack"); code != 401 {
		t.Fatalf("advertisement: %d", code)
	}
	// The advertisement above needs the bearer; measure through git.
	dir := filepath.Join(t.TempDir(), "ls")
	mustGit(t, t.TempDir(), "init", "-q", dir)
	start = time.Now()
	mustGit(t, dir, "ls-remote", "-q", fresh.url(id))
	materialize := time.Since(start)
	start = time.Now()
	cl := clone(t, fresh.url(id))
	cloneTime := time.Since(start)
	if mustGit(t, cl, "rev-list", "--count", "HEAD") != "1000" {
		t.Fatal("clone is missing commits")
	}
	t.Logf("MEASURE materialize 1000 entries onto an empty disk: %s (first ls-remote); full clone afterwards: %s", materialize, cloneTime)
}
