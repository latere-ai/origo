// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package placement is spec 005: which node a request should prefer so
// the local copy is warm, how nodes learn about new entries without a
// round trip, and when a local copy is thrown away. The live node set
// is kept by heartbeat; placement is rendezvous hashing over it; gossip
// carries sequence announcements under an HMAC; the evictor bounds the
// cache. None of it decides what is served: the currency check of spec
// 004 does, and a lost datagram costs one HEAD that answers 200.
package placement

import (
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Header is the response header naming the preferred nodes of a
// repository, highest score first, on every response to a request that
// names one. It is a hint for an ingress or a client that can route by
// pod, never a redirect.
const Header = "Origo-Prefer"

// Window is how long a node stays in the live set after its last valid
// datagram; a datagram whose at is further than it from the receiver's
// clock is dropped.
const Window = 60 * time.Second

// Score is the rendezvous score of node n for repository id: the first
// 8 bytes of SHA-256(n || "\n" || id) as a big-endian integer.
func Score(node, id string) uint64 {
	sum := sha256.Sum256([]byte(node + "\n" + id))
	return binary.BigEndian.Uint64(sum[:8])
}

// Rank orders nodes for id by score, highest first, with the name as
// the tie-break so every node computes one order.
func Rank(nodes []string, id string) []string {
	out := slices.Clone(nodes)
	sort.Slice(out, func(i, j int) bool {
		si, sj := Score(out[i], id), Score(out[j], id)
		if si != sj {
			return si > sj
		}
		return out[i] < out[j]
	})
	return out
}

// Set is the live node set: the names heard, by heartbeat or by an
// announcement that carried a valid MAC, in the last Window, plus the
// node's own name, which is always in it.
type Set struct {
	self string
	now  func() time.Time

	mu    sync.Mutex
	heard map[string]time.Time
}

// NewSet starts a set holding self alone. now is the clock the window
// runs on; nil is the wall clock.
func NewSet(self string, now func() time.Time) *Set {
	if now == nil {
		now = time.Now
	}
	return &Set{self: self, now: now, heard: map[string]time.Time{}}
}

// Self is the node's own name.
func (s *Set) Self() string { return s.self }

// Heard records that name sent a valid datagram at the time.
func (s *Set) Heard(name string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.heard[name]; !ok || at.After(prev) {
		s.heard[name] = at
	}
}

// LastHeard reports when name was last heard. The node's own name is
// heard now; a name outside the window is reported with false.
func (s *Set) LastHeard(name string) (time.Time, bool) {
	if name == s.self {
		return s.now(), true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.heard[name]
	if !ok || s.now().Sub(at) >= Window {
		return time.Time{}, false
	}
	return at, true
}

// Live lists the live names in lexical order, the node's own first
// among equals only by that order.
func (s *Set) Live() []string {
	now := s.now()
	s.mu.Lock()
	out := []string{s.self}
	for name, at := range s.heard {
		if name == s.self {
			continue
		}
		if now.Sub(at) < Window {
			out = append(out, name)
		} else {
			delete(s.heard, name)
		}
	}
	s.mu.Unlock()
	sort.Strings(out)
	return out
}

// Prefer is the placement of id: the replicas live nodes with the
// highest scores, highest first, at most the node count and at least
// one. The compaction primary of spec 006 is the first name.
func (s *Set) Prefer(id string, replicas int) []string {
	ranked := Rank(s.Live(), id)
	return ranked[:min(max(replicas, 1), len(ranked))]
}

// Placer answers the preferred nodes of a repository; *Set is one.
type Placer interface {
	Prefer(id string, replicas int) []string
}

// SetHeader writes Origo-Prefer for id with the replicas value of the
// request's allow, 1 for a request that made no authorizer call. A nil
// placer, which no node has, writes nothing.
func SetHeader(h http.Header, p Placer, id string, replicas int) {
	if p == nil || id == "" {
		return
	}
	h.Set(Header, strings.Join(p.Prefer(id, replicas), ","))
}
