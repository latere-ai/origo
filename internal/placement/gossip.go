// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package placement

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/cache"
	"latere.ai/x/pkg/metrics"
)

// The gossip schedule of spec 005.
const (
	// HeartbeatEvery is how often a node sends a heartbeat to every peer
	// and refreshes the peer addresses.
	HeartbeatEvery = 10 * time.Second
	// AnnounceTimes and AnnounceGap: an announcement is sent this many
	// times, this far apart, with no acknowledgement.
	AnnounceTimes = 3
	AnnounceGap   = 10 * time.Millisecond
	// CatchUpEvery bounds the catch-ups one repository's announcements
	// schedule, whatever the datagram rate.
	CatchUpEvery = time.Second
	// lookupTimeout bounds one resolution of the DNS form of the peers.
	lookupTimeout = 5 * time.Second
	// tagSize is the HMAC-SHA256 in front of every payload.
	tagSize = sha256.Size
	// maxDatagram bounds what the read loop accepts.
	maxDatagram = 4096
	// version is the v of every payload.
	version = 1
)

// payload is the JSON object of one datagram. Repo empty with Seq 0 is
// a heartbeat; anything else announces that the sender created the
// index object of that sequence.
type payload struct {
	V    int       `json:"v"`
	Node string    `json:"node"`
	Repo string    `json:"repo"`
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`
}

// Seal encodes a payload as a datagram: the HMAC-SHA256 of the payload
// bytes under secret, then the bytes.
func Seal(secret []byte, p payload) ([]byte, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return append(mac.Sum(nil), body...), nil
}

// ErrDropped is the reason a datagram was not accepted.
var ErrDropped = errors.New("gossip: datagram dropped")

// open verifies the tag in constant time and returns the payload bytes.
// A datagram shorter than the tag plus one byte, or whose tag differs,
// is dropped before the payload is parsed.
func open(secret, datagram []byte) ([]byte, error) {
	if len(datagram) <= tagSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrDropped, len(datagram))
	}
	body := datagram[tagSize:]
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), datagram[:tagSize]) {
		return nil, fmt.Errorf("%w: bad tag", ErrDropped)
	}
	return body, nil
}

// Holder is what gossip needs of the repository cache: whether the node
// holds a repository and at what sequence, and a catch-up, which is an
// Acquire for writing that returns at once.
type Holder interface {
	Held(id string) (seq uint64, ok bool)
	CatchUp(ctx context.Context, id string) error
}

// GossipOptions configures a Gossip.
type GossipOptions struct {
	// Set is the live set the datagrams feed; its Self is the sender's
	// name. Required.
	Set *Set
	// Secret is ORIGO_GOSSIP_SECRET. Empty with no peers, in which case
	// every datagram is dropped and nothing is sent.
	Secret []byte
	// Peers is ORIGO_GOSSIP_PEERS: a comma separated list of host:port
	// entries, or one DNS name resolved on the port of the socket.
	Peers string
	// Holder decides whether an announcement schedules a catch-up.
	// Required when Peers is set.
	Holder Holder
	// Now is the clock; the wall clock by default.
	Now func() time.Time
	// LookupIP resolves the DNS form of Peers; net.DefaultResolver by
	// default.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
	Logger   *slog.Logger
	Metrics  *metrics.Registry
}

// Gossip sends and receives the datagrams of spec 005 on one socket.
type Gossip struct {
	set      *Set
	secret   []byte
	entries  []string
	name     string
	holder   Holder
	now      func() time.Time
	lookupIP func(context.Context, string) ([]net.IP, error)
	logger   *slog.Logger
	packets  *metrics.Counter

	mu    sync.Mutex
	conn  net.PacketConn
	port  int
	peers []net.Addr

	recent *cache.TTLCache[string, struct{}]
	wg     sync.WaitGroup
}

// NewGossip builds the gossip over the options. It opens no socket:
// Run takes the one the node bound.
func NewGossip(o GossipOptions) (*Gossip, error) {
	if o.Set == nil {
		return nil, errors.New("placement: gossip needs a set")
	}
	if o.Peers != "" && (len(o.Secret) == 0 || o.Holder == nil) {
		return nil, errors.New("placement: gossip with peers needs a secret and a holder")
	}
	g := &Gossip{set: o.Set, secret: o.Secret, holder: o.Holder, now: o.Now, lookupIP: o.LookupIP, logger: o.Logger}
	g.entries, g.name = parsePeers(o.Peers)
	if g.now == nil {
		g.now = time.Now
	}
	if g.lookupIP == nil {
		g.lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, nil
		}
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	reg := o.Metrics
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	g.packets = reg.Counter("origo_gossip_packets_total", "gossip datagrams by direction")
	for _, d := range []string{"sent", "received", "dropped"} {
		g.packets.Add(map[string]string{"direction": d}, 0)
	}
	g.recent = cache.New[string, struct{}](CatchUpEvery, cache.WithClock[string, struct{}](g.now), cache.WithMaxSize[string, struct{}](65536))
	return g, nil
}

// parsePeers reads ORIGO_GOSSIP_PEERS: a value with a comma or a port
// is the list, anything else the DNS name.
func parsePeers(raw string) (entries []string, name string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	if strings.Contains(raw, ",") {
		for e := range strings.SplitSeq(raw, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
		return entries, ""
	}
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return []string{raw}, ""
	}
	return nil, raw
}

// Bind takes the socket the node bound and resolves the peers once,
// before anything is served, so an announcement from the first commit
// has somewhere to go. It opens no socket of its own.
func (g *Gossip) Bind(ctx context.Context, conn net.PacketConn) {
	g.mu.Lock()
	g.conn = conn
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		g.port = a.Port
	}
	g.mu.Unlock()
	g.Resolve(ctx)
}

// Run serves the bound socket until it is closed and ctx is done: the
// read loop handles every datagram, a heartbeat goes to every peer at
// once and then every HeartbeatEvery, with the peers resolved again
// before each. A scheduled catch-up finishes before Run returns.
func (g *Gossip) Run(ctx context.Context) error {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return errors.New("placement: gossip is not bound to a socket")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, maxDatagram)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_ = g.Handle(ctx, buf[:n]) // a dropped datagram is counted, not reported
		}
	}()
	g.Heartbeat()
	ticker := time.NewTicker(HeartbeatEvery)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-ticker.C:
			g.Resolve(ctx)
			g.Heartbeat()
		}
	}
	<-done
	g.wg.Wait()
	return ctx.Err()
}

// Resolve refreshes the peer addresses: each list entry as given, or
// every address the DNS name resolves to on the socket's port. A
// resolution that fails keeps the last addresses and logs.
func (g *Gossip) Resolve(ctx context.Context) {
	var peers []net.Addr
	for _, e := range g.entries {
		a, err := net.ResolveUDPAddr("udp", e)
		if err != nil {
			g.logger.WarnContext(ctx, "gossip peer not resolved", "peer", e, "error", err)
			continue
		}
		peers = append(peers, a)
	}
	if g.name != "" {
		g.mu.Lock()
		port := g.port
		g.mu.Unlock()
		// A lookup that hangs must not hold the node's start-up or a
		// heartbeat; the next tick resolves again.
		lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		ips, err := g.lookupIP(lookupCtx, g.name)
		if err != nil {
			g.logger.WarnContext(ctx, "gossip peers not resolved", "name", g.name, "error", err)
			return
		}
		for _, ip := range ips {
			peers = append(peers, &net.UDPAddr{IP: ip, Port: port})
		}
	}
	g.mu.Lock()
	g.peers = peers
	g.mu.Unlock()
}

// Peers lists the addresses of the last resolution.
func (g *Gossip) Peers() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.peers))
	for _, p := range g.peers {
		out = append(out, p.String())
	}
	return out
}

// Heartbeat sends one heartbeat to every peer.
func (g *Gossip) Heartbeat() {
	g.send(payload{V: version, Node: g.set.Self(), At: g.now().UTC()})
}

// Announce sends the announcement of repo at seq AnnounceTimes times,
// AnnounceGap apart, to every peer, in the background so the commit
// that created the index object is not delayed by it. It is the
// OnCommit of the log.
func (g *Gossip) Announce(repo string, seq uint64) {
	if len(g.secret) == 0 {
		return
	}
	g.wg.Go(func() {
		for i := range AnnounceTimes {
			if i > 0 {
				time.Sleep(AnnounceGap)
			}
			g.send(payload{V: version, Node: g.set.Self(), Repo: repo, Seq: seq, At: g.now().UTC()})
		}
	})
}

// send seals one payload and writes it to every peer, counting each
// datagram sent. It runs outside any request, so it logs without one.
func (g *Gossip) send(p payload) {
	if len(g.secret) == 0 {
		return
	}
	g.mu.Lock()
	conn, peers := g.conn, g.peers
	g.mu.Unlock()
	if conn == nil {
		return
	}
	datagram, err := Seal(g.secret, p)
	if err != nil {
		g.logger.Error("gossip datagram not encoded", "error", err)
		return
	}
	for _, peer := range peers {
		if _, err := conn.WriteTo(datagram, peer); err != nil {
			g.logger.Warn("gossip datagram not sent", "peer", peer.String(), "error", err)
			continue
		}
		g.packets.Inc(map[string]string{"direction": "sent"})
	}
}

// Handle receives one datagram. A datagram with no tag, a tag under
// another secret, a tag over a changed payload, an at outside the
// window, another version, or a repository the node does not hold is
// dropped and counted; a valid heartbeat records the sender and does
// nothing else; a valid announcement whose sequence is above the local
// copy schedules one catch-up, at most one per repository per
// CatchUpEvery. It reports the reason a datagram was dropped, for the
// tests, and nil for one received.
func (g *Gossip) Handle(ctx context.Context, datagram []byte) error {
	drop := func(err error) error {
		g.packets.Inc(map[string]string{"direction": "dropped"})
		return err
	}
	if len(g.secret) == 0 {
		// A node with no peers has no secret and believes nobody.
		return drop(fmt.Errorf("%w: no secret", ErrDropped))
	}
	body, err := open(g.secret, datagram)
	if err != nil {
		return drop(err)
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return drop(fmt.Errorf("%w: %w", ErrDropped, err))
	}
	if p.V != version || p.Node == "" {
		return drop(fmt.Errorf("%w: version %d from %q", ErrDropped, p.V, p.Node))
	}
	now := g.now()
	if d := now.Sub(p.At); d > Window || d < -Window {
		return drop(fmt.Errorf("%w: at %s is %s from now", ErrDropped, p.At.Format(time.RFC3339), d))
	}
	g.set.Heard(p.Node, now)
	if p.Repo == "" && p.Seq == 0 {
		g.packets.Inc(map[string]string{"direction": "received"})
		return nil
	}
	held, ok := g.holder.Held(p.Repo)
	if !ok {
		return drop(fmt.Errorf("%w: repository not held", ErrDropped))
	}
	g.packets.Inc(map[string]string{"direction": "received"})
	if held >= p.Seq {
		return nil
	}
	if _, recent := g.recent.Get(p.Repo); recent {
		return nil
	}
	g.recent.Set(p.Repo, struct{}{})
	g.wg.Go(func() {
		if err := g.holder.CatchUp(ctx, p.Repo); err != nil && ctx.Err() == nil {
			g.logger.WarnContext(ctx, "catch-up after an announcement failed", "repo", p.Repo, "seq", p.Seq, "error", err)
		}
	})
	return nil
}

// Wait blocks until every scheduled catch-up and announcement is done,
// for the tests.
func (g *Gossip) Wait() { g.wg.Wait() }

// Port is the port of the socket, for a test that lists a DNS peer.
func (g *Gossip) Port() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.port
}
