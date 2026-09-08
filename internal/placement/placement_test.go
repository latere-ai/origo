// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package placement

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"
)

const (
	repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

var secret = bytes.Repeat([]byte("k"), 32)

// clock is a fake clock shared by a test's sets and gossips.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeHolder holds a table of sequences and counts catch-ups.
type fakeHolder struct {
	mu       sync.Mutex
	held     map[string]uint64
	catchUps atomic.Int64
	err      error
}

func (h *fakeHolder) Held(id string) (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq, ok := h.held[id]
	return seq, ok
}

func (h *fakeHolder) CatchUp(context.Context, string) error {
	h.catchUps.Add(1)
	return h.err
}

func counter(reg *metrics.Registry, name string, labels string) int {
	var text bytes.Buffer
	reg.WritePrometheus(&text)
	for line := range strings.SplitSeq(text.String(), "\n") {
		if strings.HasPrefix(line, name+"{"+labels+"} ") {
			var n int
			_, _ = fmt.Sscanf(strings.TrimPrefix(line, name+"{"+labels+"} "), "%d", &n)
			return n
		}
	}
	return -1
}

// TestRendezvousAgreesAcrossNodes is spec 005's placement: three sets
// with the same members, heard in different orders, rank a repository
// the same, k is bounded by the node count, and a single-node set is
// preferred for everything.
func TestRendezvousAgreesAcrossNodes(t *testing.T) {
	clk := newClock()
	names := []string{"origod-0", "origod-1", "origod-2"}
	sets := make([]*Set, 3)
	for i, self := range names {
		sets[i] = NewSet(self, clk.Now)
		for k := 1; k < len(names); k++ {
			sets[i].Heard(names[(i+k)%len(names)], clk.Now())
		}
	}
	for _, id := range []string{repoA, repoB} {
		first := sets[0].Prefer(id, 3)
		if len(first) != 3 {
			t.Fatalf("Prefer(3) = %v", first)
		}
		for _, s := range sets[1:] {
			if got := s.Prefer(id, 3); !slices.Equal(got, first) {
				t.Fatalf("%s ranks %s as %v, %s as %v", s.Self(), id, got, sets[0].Self(), first)
			}
			if got := s.Prefer(id, 1); len(got) != 1 || got[0] != first[0] {
				t.Fatalf("%s prefers %v for k=1, want %s", s.Self(), got, first[0])
			}
		}
		// The order is by score, highest first.
		for i := 1; i < len(first); i++ {
			if Score(first[i-1], id) < Score(first[i], id) {
				t.Fatalf("%v is not ordered by score for %s", first, id)
			}
		}
		if got := sets[0].Prefer(id, 8); len(got) != 3 {
			t.Fatalf("Prefer(8) with 3 nodes = %v", got)
		}
		if got := sets[0].Prefer(id, 0); len(got) != 1 {
			t.Fatalf("Prefer(0) = %v", got)
		}
	}
	// Two repositories do not all land on one node: the hash spreads.
	firsts := map[string]bool{}
	for i := range 64 {
		firsts[sets[0].Prefer(fmt.Sprintf("%08d-0000-4000-8000-000000000000", i), 1)[0]] = true
	}
	if len(firsts) != 3 {
		t.Fatalf("64 repositories prefer only %v", firsts)
	}
	// A single-node set prefers itself for everything, whatever k.
	alone := NewSet("solo", clk.Now)
	if got := alone.Prefer(repoA, 3); len(got) != 1 || got[0] != "solo" {
		t.Fatalf("alone: %v", got)
	}
	// Ties break by name, so the order is total.
	if r := Rank([]string{"b", "a"}, repoA); Score("a", repoA) == Score("b", repoA) && r[0] != "a" {
		t.Fatal("tie-break")
	}
	// The header carries the names, and nothing without a placer or an id.
	h := http.Header{}
	SetHeader(h, sets[0], repoA, 2)
	if got := h.Get(Header); got != strings.Join(sets[0].Prefer(repoA, 2), ",") {
		t.Fatalf("header %q", got)
	}
	h = http.Header{}
	SetHeader(h, nil, repoA, 1)
	SetHeader(h, sets[0], "", 1)
	if h.Get(Header) != "" {
		t.Fatal("header without a placer or an id")
	}
}

// newGossip starts one gossip on a loopback socket with the peers given
// as a list and returns it with its address.
func newGossip(t *testing.T, clk *clock, self string, holder Holder, peers ...string) (*Gossip, string, *metrics.Registry) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewRegistry()
	g, err := NewGossip(GossipOptions{
		Set: NewSet(self, clk.Now), Secret: secret, Peers: strings.Join(peers, ","), Holder: holder,
		Now: clk.Now, Logger: slog.New(slog.DiscardHandler), Metrics: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.Bind(ctx, conn)
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		<-done
	})
	return g, conn.LocalAddr().String(), reg
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 10s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMembershipByHeartbeat is spec 005's live set: three nodes
// exchanging heartbeats over loopback agree on the set within the
// window of a node joining, a node is dropped 60 seconds after its last
// datagram with a fake clock, and a node's own name is in its set with
// no peers.
func TestMembershipByHeartbeat(t *testing.T) {
	clk := newClock()
	holder := &fakeHolder{held: map[string]uint64{}}
	// Ports are chosen first so each node lists the other two.
	conns := make([]net.PacketConn, 3)
	addrs := make([]string, 3)
	for i := range conns {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		conns[i], addrs[i] = c, c.LocalAddr().String()
	}
	names := []string{"origod-0", "origod-1", "origod-2"}
	gossips := make([]*Gossip, 3)
	ctx, cancel := context.WithCancel(context.Background())
	var runs sync.WaitGroup
	for i := range names {
		peers := slices.Concat(addrs[:i], addrs[i+1:])
		g, err := NewGossip(GossipOptions{
			Set: NewSet(names[i], clk.Now), Secret: secret, Peers: strings.Join(peers, ","), Holder: holder,
			Now: clk.Now, Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		gossips[i] = g
	}
	start := func(i int) {
		gossips[i].Bind(ctx, conns[i])
		runs.Go(func() { _ = gossips[i].Run(ctx) })
	}
	if err := gossips[0].Run(ctx); err == nil {
		t.Fatal("Run before Bind")
	}
	t.Cleanup(func() {
		cancel()
		for _, c := range conns {
			_ = c.Close()
		}
		runs.Wait()
	})
	// Two nodes up: each hears the other from the heartbeat Run sends
	// at start; the third is not in either set.
	start(0)
	start(1)
	waitFor(t, "two nodes agreeing", func() bool {
		return slices.Equal(gossips[0].set.Live(), []string{"origod-0", "origod-1"}) && slices.Equal(gossips[1].set.Live(), []string{"origod-0", "origod-1"})
	})
	if len(gossips[0].Peers()) != 2 {
		t.Fatalf("peers %v", gossips[0].Peers())
	}
	// The third joins: its first heartbeat reaches both, and its own
	// set fills from their next heartbeats, inside one window.
	start(2)
	clk.Advance(HeartbeatEvery)
	gossips[0].Heartbeat(ctx)
	gossips[1].Heartbeat(ctx)
	waitFor(t, "three nodes agreeing", func() bool {
		for _, g := range gossips {
			if !slices.Equal(g.set.Live(), names) {
				return false
			}
		}
		return true
	})
	if at, ok := gossips[0].set.LastHeard("origod-2"); !ok || !at.Equal(clk.Now()) {
		t.Fatalf("LastHeard(origod-2) = %v %v", at, ok)
	}
	if at, ok := gossips[0].set.LastHeard("origod-0"); !ok || !at.Equal(clk.Now()) {
		t.Fatalf("LastHeard(self) = %v %v", at, ok)
	}
	// Node 3 goes quiet: 59 seconds later it is live, at 60 it is not.
	clk.Advance(Window - time.Second)
	gossips[0].Heartbeat(ctx)
	gossips[1].Heartbeat(ctx)
	waitFor(t, "nodes 1 and 2 refreshed", func() bool {
		at, _ := gossips[0].set.LastHeard("origod-1")
		return at.Equal(clk.Now())
	})
	if !slices.Equal(gossips[0].set.Live(), names) {
		t.Fatalf("59 seconds quiet: %v", gossips[0].set.Live())
	}
	clk.Advance(time.Second)
	if got := gossips[0].set.Live(); !slices.Equal(got, []string{"origod-0", "origod-1"}) {
		t.Fatalf("60 seconds quiet: %v", got)
	}
	if _, ok := gossips[0].set.LastHeard("origod-2"); ok {
		t.Fatal("a dropped node is still heard")
	}
	// A node with no peers holds its own name and nothing else, sends
	// nothing, and drops what arrives.
	alone, err := NewGossip(GossipOptions{Set: NewSet("solo", clk.Now), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	alone.Heartbeat(ctx)
	alone.Announce(repoA, 1)
	if got := alone.set.Live(); !slices.Equal(got, []string{"solo"}) {
		t.Fatalf("alone: %v", got)
	}
	datagram, _ := Seal(nil, payload{V: version, Node: "x", At: clk.Now()})
	if err := alone.Handle(ctx, datagram); !errors.Is(err, ErrDropped) {
		t.Fatalf("a node without a secret accepted a datagram: %v", err)
	}
	// The options are checked.
	if _, err := NewGossip(GossipOptions{}); err == nil {
		t.Fatal("no set")
	}
	if _, err := NewGossip(GossipOptions{Set: NewSet("x", nil), Peers: "origod-gossip"}); err == nil {
		t.Fatal("peers without a secret")
	}
}

// TestGossipResolvesTheDNSForm: the DNS name form resolves to every
// address on the socket's port, a list entry that does not resolve is
// skipped, and a failed lookup keeps the last addresses.
func TestGossipResolvesTheDNSForm(t *testing.T) {
	clk := newClock()
	holder := &fakeHolder{held: map[string]uint64{}}
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var fail atomic.Bool
	reg := metrics.NewRegistry()
	g, err := NewGossip(GossipOptions{
		Set: NewSet("origod-0", clk.Now), Secret: secret, Peers: "origod-gossip", Holder: holder, Now: clk.Now, Metrics: reg,
		Logger: slog.New(slog.DiscardHandler),
		LookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			if fail.Load() {
				return nil, errors.New("no such host")
			}
			if host != "origod-gossip" {
				t.Errorf("looked up %q", host)
			}
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.Bind(ctx, conn)
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	defer func() { cancel(); _ = conn.Close(); <-done }()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if p := g.Peers(); p[0] != fmt.Sprintf("127.0.0.1:%d", port) || p[1] != fmt.Sprintf("127.0.0.2:%d", port) || g.Port() != port {
		t.Fatalf("peers %v port %d", p, g.Port())
	}
	// The heartbeat at start reached this socket through 127.0.0.1.
	waitFor(t, "own heartbeat received", func() bool { return counter(reg, "origo_gossip_packets_total", `direction="received"`) >= 1 })
	if sent := counter(reg, "origo_gossip_packets_total", `direction="sent"`); sent < 2 {
		t.Fatalf("sent %d", sent)
	}
	fail.Store(true)
	g.Resolve(ctx)
	if len(g.Peers()) != 2 {
		t.Fatalf("a failed lookup changed the peers: %v", g.Peers())
	}
	// The list form skips an entry that does not resolve and keeps one
	// that does.
	other, err := NewGossip(GossipOptions{Set: NewSet("x", clk.Now), Secret: secret, Peers: "127.0.0.1:1,nosuchhost.invalid:1", Holder: holder, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	other.Resolve(ctx)
	if p := other.Peers(); !slices.Equal(p, []string{"127.0.0.1:1"}) {
		t.Fatalf("list form: %v", p)
	}
	for raw, want := range map[string]string{"a:1,b:2": "list", "a:1": "list", "origod-gossip": "name", "": "none", " origod-gossip ": "name"} {
		entries, name := parsePeers(raw)
		got := "none"
		switch {
		case len(entries) > 0:
			got = "list"
		case name != "":
			got = "name"
		}
		if got != want {
			t.Errorf("parsePeers(%q) = %v %q, want %s", raw, entries, name, want)
		}
	}
	if PortOf(":7946") != 7946 || PortOf("nope") != 0 {
		t.Fatal("PortOf")
	}
}

// TestGossipDropsABadMAC is spec 005's authentication of membership: a
// datagram with no tag, a tag under another secret, a tag over a
// changed payload, or an at 61 seconds old is dropped and counted
// without entering the live set or scheduling a catch-up, and the same
// payload under the right secret is accepted.
func TestGossipDropsABadMAC(t *testing.T) {
	clk := newClock()
	holder := &fakeHolder{held: map[string]uint64{repoA: 3}}
	reg := metrics.NewRegistry()
	g, err := NewGossip(GossipOptions{Set: NewSet("origod-0", clk.Now), Secret: secret, Peers: "127.0.0.1:1", Holder: holder, Now: clk.Now, Metrics: reg, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	announce := payload{V: version, Node: "origod-1", Repo: repoA, Seq: 4, At: clk.Now()}
	good, _ := Seal(secret, announce)
	other, _ := Seal(bytes.Repeat([]byte("o"), 32), announce)
	changed := slices.Clone(good)
	changed[len(changed)-3] ^= 1
	old := announce
	old.At = clk.Now().Add(-61 * time.Second)
	stale, _ := Seal(secret, old)
	future := announce
	future.At = clk.Now().Add(61 * time.Second)
	ahead, _ := Seal(secret, future)
	v2 := announce
	v2.V = 2
	otherVersion, _ := Seal(secret, v2)
	unnamed := announce
	unnamed.Node = ""
	noNode, _ := Seal(secret, unnamed)
	garbage := append(hmacOf(secret, []byte("{")), []byte("{")...)
	unheld := announce
	unheld.Repo = repoB
	notHeld, _ := Seal(secret, unheld)
	cases := []struct {
		name     string
		datagram []byte
		member   bool
	}{
		{"no tag", good[tagSize:], false},
		{"empty", nil, false},
		{"tag alone", good[:tagSize], false},
		{"another secret", other, false},
		{"changed payload", changed, false},
		{"61 seconds old", stale, false},
		{"61 seconds ahead", ahead, false},
		{"another version", otherVersion, false},
		{"no node", noNode, false},
		{"garbage", garbage, false},
		{"repository not held", notHeld, true},
	}
	for i, c := range cases {
		if err := g.Handle(ctx, c.datagram); !errors.Is(err, ErrDropped) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := counter(reg, "origo_gossip_packets_total", `direction="dropped"`); got != i+1 {
			t.Fatalf("%s: dropped = %d", c.name, got)
		}
		if _, ok := g.set.LastHeard("origod-1"); ok != c.member {
			t.Fatalf("%s: member %v", c.name, ok)
		}
		g.Wait()
		if holder.catchUps.Load() != 0 {
			t.Fatalf("%s scheduled a catch-up", c.name)
		}
	}
	if got := counter(reg, "origo_gossip_packets_total", `direction="received"`); got != 0 {
		t.Fatalf("received = %d before the good datagram", got)
	}
	// The same payload under the right secret: received, the sender in
	// the set, one catch-up.
	if err := g.Handle(ctx, good); err != nil {
		t.Fatal(err)
	}
	g.Wait()
	if holder.catchUps.Load() != 1 || counter(reg, "origo_gossip_packets_total", `direction="received"`) != 1 {
		t.Fatalf("catch-ups %d received %d", holder.catchUps.Load(), counter(reg, "origo_gossip_packets_total", `direction="received"`))
	}
	if !slices.Equal(g.set.Live(), []string{"origod-0", "origod-1"}) {
		t.Fatalf("live %v", g.set.Live())
	}
	// An announcement at or below the held sequence is received and
	// schedules nothing; a heartbeat is received and schedules nothing.
	clk.Advance(2 * time.Second)
	behind, _ := Seal(secret, payload{V: version, Node: "origod-2", Repo: repoA, Seq: 3, At: clk.Now()})
	beat, _ := Seal(secret, payload{V: version, Node: "origod-2", At: clk.Now()})
	for _, d := range [][]byte{behind, beat} {
		if err := g.Handle(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	g.Wait()
	if holder.catchUps.Load() != 1 || len(g.set.Live()) != 3 {
		t.Fatalf("after a behind announcement and a heartbeat: %d catch-ups, %v", holder.catchUps.Load(), g.set.Live())
	}
	// A catch-up that fails is logged, not returned.
	holder.err = errors.New("bucket down")
	clk.Advance(2 * time.Second)
	again, _ := Seal(secret, payload{V: version, Node: "origod-1", Repo: repoA, Seq: 5, At: clk.Now()})
	if err := g.Handle(ctx, again); err != nil {
		t.Fatal(err)
	}
	g.Wait()
	if holder.catchUps.Load() != 2 {
		t.Fatal("the failing catch-up did not run")
	}
	// Seal refuses nothing the payload type can carry; the JSON is what
	// a receiver of another version reads.
	var p payload
	if err := json.Unmarshal(good[tagSize:], &p); err != nil || p.Repo != repoA || p.Seq != 4 || p.V != 1 {
		t.Fatalf("payload %+v %v", p, err)
	}
}

func hmacOf(key, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return m.Sum(nil)
}

// TestGossipCatchUpIsRateLimited is spec 005's bound on amplification:
// 10 000 announcements for one repository in one second cause at most
// one catch-up on the receiver, and the next second admits one more.
func TestGossipCatchUpIsRateLimited(t *testing.T) {
	clk := newClock()
	holder := &fakeHolder{held: map[string]uint64{repoA: 1, repoB: 1}}
	g, err := NewGossip(GossipOptions{Set: NewSet("origod-0", clk.Now), Secret: secret, Peers: "127.0.0.1:1", Holder: holder, Now: clk.Now, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range 10000 {
		d, _ := Seal(secret, payload{V: version, Node: "origod-1", Repo: repoA, Seq: uint64(i + 2), At: clk.Now()})
		if err := g.Handle(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	g.Wait()
	if got := holder.catchUps.Load(); got != 1 {
		t.Fatalf("%d catch-ups in one second", got)
	}
	// Another repository has a budget of its own.
	d, _ := Seal(secret, payload{V: version, Node: "origod-1", Repo: repoB, Seq: 2, At: clk.Now()})
	if err := g.Handle(ctx, d); err != nil {
		t.Fatal(err)
	}
	g.Wait()
	if got := holder.catchUps.Load(); got != 2 {
		t.Fatalf("%d catch-ups with two repositories", got)
	}
	clk.Advance(CatchUpEvery + time.Millisecond)
	d, _ = Seal(secret, payload{V: version, Node: "origod-1", Repo: repoA, Seq: 20000, At: clk.Now()})
	if err := g.Handle(ctx, d); err != nil {
		t.Fatal(err)
	}
	g.Wait()
	if got := holder.catchUps.Load(); got != 3 {
		t.Fatalf("%d catch-ups after the second", got)
	}
}

// TestAnnounceReachesEveryPeer: an announcement is sent three times to
// every peer and a peer that holds the repository below the sequence
// catches up once.
func TestAnnounceReachesEveryPeer(t *testing.T) {
	clk := newClock()
	holderB := &fakeHolder{held: map[string]uint64{repoA: 1}}
	holderC := &fakeHolder{held: map[string]uint64{}}
	b, addrB, regB := newGossip(t, clk, "origod-1", holderB)
	c, addrC, regC := newGossip(t, clk, "origod-2", holderC)
	a, _, regA := newGossip(t, clk, "origod-0", &fakeHolder{held: map[string]uint64{}}, addrB, addrC)
	a.Announce(repoA, 2)
	a.Wait()
	waitFor(t, "the heartbeat and the three announcements sent to both peers", func() bool {
		return counter(regA, "origo_gossip_packets_total", `direction="sent"`) == 2+AnnounceTimes*2
	})
	waitFor(t, "b caught up", func() bool { return holderB.catchUps.Load() == 1 })
	waitFor(t, "c dropped the three", func() bool {
		return counter(regC, "origo_gossip_packets_total", `direction="dropped"`) == AnnounceTimes
	})
	waitFor(t, "b received the three", func() bool {
		return counter(regB, "origo_gossip_packets_total", `direction="received"`) == AnnounceTimes+1
	})
	b.Wait()
	c.Wait()
	if !slices.Equal(b.set.Live(), []string{"origod-0", "origod-1"}) || !slices.Equal(c.set.Live(), []string{"origod-0", "origod-2"}) {
		t.Fatalf("live sets %v %v", b.set.Live(), c.set.Live())
	}
}
