// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/sink"
)

// eventSink is the sink the nodes of a test deliver to: the stack's
// stub at the sink's host port of spec 013's ports table when it
// answers, else one started in-process, so the test also runs against
// a bare MinIO. list reads the deliveries either way.
func eventSink(t *testing.T) (url, secret string, list func(repo, kind string) []sink.Delivery) {
	t.Helper()
	stack := fmt.Sprintf("http://localhost:%d", portSink)
	if status, _ := httpGet(http.DefaultClient, stack+"/deliveries"); status == 200 {
		return stack, sink.DefaultSecret, func(repo, kind string) []sink.Delivery {
			_, raw := httpGet(http.DefaultClient, stack+"/deliveries?repo="+repo+"&kind="+kind)
			var out []sink.Delivery
			_ = json.Unmarshal(raw, &out)
			return out
		}
	}
	s := sink.New(t)
	return s.URL(), s.Secret(), s.Deliveries
}

// TestSlowEventRepairAfterKill is spec 008's stack criterion: a node
// killed at ORIGO_FAILPOINT=events.before-enqueue after the index
// create and the verdict, on its second push to a repository its first
// push put in its journal, yields one event for the second push from
// another node's repair sweep within one ORIGO_REPAIR_INTERVAL once the
// dead node is ORIGO_REPAIR_UNHEARD unheard, with updates equal to the
// entry's transaction and id equal to the UUID v5 of the payload table,
// and the dead node's journal names the repository once. Two nodes of
// the test's own run against the stack's MinIO with the sink at its
// host port; the node that carries the failpoint is the first node
// restarted under its name and data directory, so its first push
// enqueued and its second is the one the failpoint ends.
func TestSlowEventRepairAfterKill(t *testing.T) {
	s := requireStack(t)
	sinkURL, secret, list := eventSink(t)
	gossipA, gossipB := freePort(t), freePort(t)
	nameA, nameB := "e2e-a-"+newID(t)[:8], "e2e-b-"+newID(t)[:8]
	common := map[string]string{
		"ORIGO_EVENTS_URL": sinkURL, "ORIGO_EVENTS_SECRET": secret,
		"ORIGO_GOSSIP_SECRET":  strings.Repeat("s", 32),
		"ORIGO_REPAIR_UNHEARD": "5s", "ORIGO_REPAIR_INTERVAL": "10s",
	}
	env := func(name, addr, peer string, extra map[string]string) map[string]string {
		out := map[string]string{"ORIGO_NODE_NAME": name, "ORIGO_GOSSIP_ADDR": addr, "ORIGO_GOSSIP_PEERS": peer}
		for k, v := range common {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	a := startNode(t, s, "", env(nameA, gossipA, gossipB, nil))
	b := startNode(t, s, "", env(nameB, gossipB, gossipA, nil))
	id := newID(t)
	a.createRepo(id, "acme", "events-"+id[:8])

	// The first push through A: enqueued, delivered, journalled.
	work := clone(t, a.url(id))
	commitFile(t, work, "a.txt", "one", "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	waitFor(t, 30*time.Second, "the first push's event", func() bool { return len(list(id, events.KindPush)) == 1 })

	// A restarts under its name and data directory with the failpoint,
	// and the second push ends it after the index create and the
	// verdict, before the event object is written.
	a.stop()
	dead := startNode(t, s, a.dataDir, env(nameA, gossipA, gossipB, map[string]string{"ORIGO_FAILPOINT": events.FailpointBeforeEnqueue}))
	mustGit(t, work, "remote", "set-url", "origin", dead.url(id))
	second := commitFile(t, work, "b.txt", "two", "second")
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		_, _ = git(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	}()
	if err := dead.wait(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("the node did not exit at the failpoint: %v\n%s", err, dead.logs.String())
	}
	<-pushed
	ix, _, err := s.log.Newest(context.Background(), id, 0, false)
	if err != nil || ix.Seq != 2 {
		t.Fatalf("the second push is not in the log: %+v %v", ix, err)
	}
	if keys := s.eventKeys(t, id); len(keys) != 1 || !strings.HasSuffix(keys[0], "/cursor") {
		t.Fatalf("event objects after the kill: %v", keys)
	}

	// B's sweep rebuilds the event from the index once the entry is a
	// minute old and A is unheard: within the lag, one interval, and
	// the sweep's offset.
	waitFor(t, 3*time.Minute, "the repaired event", func() bool { return len(list(id, events.KindPush)) >= 2 })
	got := list(id, events.KindPush)
	if len(got) != 2 {
		t.Fatalf("%d push events, want 2: %+v", len(got), got)
	}
	var p events.Push
	if err := json.Unmarshal(got[1].Body, &p); err != nil {
		t.Fatal(err)
	}
	if !got[1].Verified || got[1].ID != p.ID || p.ID != events.PushID(id, 2) || p.Seq != 2 || p.Pusher.Sub != "dev" {
		t.Fatalf("repaired event %+v", got[1])
	}
	rc, _, err := s.store.Get(context.Background(), s.log.RepoPrefix(id)+ix.Entry, "")
	if err != nil {
		t.Fatal(err)
	}
	hdr, refs, _, err := wal.ReadEntryHead(rc)
	_ = rc.Close()
	if err != nil || len(refs) != 1 || len(p.Updates) != 1 || p.Updates[0] != (events.Update{Ref: refs[0].Ref, Before: refs[0].Old, After: refs[0].New}) || refs[0].New != second || !p.At.Equal(hdr.At) {
		t.Fatalf("updates %+v, entry %+v %+v, %v", p.Updates, hdr, refs, err)
	}
	// The dead node's journal names the repository once, and the
	// repaired object is gone once delivered.
	var lines int
	for _, day := range []time.Time{time.Now(), time.Now().Add(-24 * time.Hour)} {
		key := "origo/events/nodes/" + nameA + "/" + day.UTC().Format(time.DateOnly) + ".log"
		rc, _, err := s.store.Get(context.Background(), key, "")
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		t.Cleanup(func() { _ = s.store.Delete(context.Background(), key) })
		lines += strings.Count(string(raw), id+" ")
	}
	if lines != 1 {
		t.Fatalf("the journal names the repository %d times", lines)
	}
	waitFor(t, 30*time.Second, "the repaired object's deletion", func() bool {
		keys := s.eventKeys(t, id)
		return len(keys) == 1 && strings.HasSuffix(keys[0], "/cursor")
	})
	b.stop()
}

// eventKeys lists every key under origo/events/<id>/ and removes them
// when the test ends.
func (s *stack) eventKeys(t *testing.T, id string) []string {
	t.Helper()
	res, err := s.store.List(context.Background(), wal.ListOptions{Prefix: "origo/events/" + id + "/", Max: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range res.Objects {
		out = append(out, o.Key)
	}
	t.Cleanup(func() {
		for _, k := range out {
			_ = s.store.Delete(context.Background(), k)
		}
	})
	return out
}

// waitFor polls cond until it holds or the budget ends.
func waitFor(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s did not arrive within %v", what, budget)
}
