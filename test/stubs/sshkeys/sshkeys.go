// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package sshkeys is the stub of spec 024's key resolution endpoint, to
// the control API of spec 013's stub table: a map from SHA-256
// fingerprint to subject that a test writes over the control endpoint, a
// record of every request in order, and the two failure modes the outage
// cases need.
//
// It is the reference implementation of the contract, which is small on
// purpose: a file of fingerprints behind a bearer satisfies it in full.
// It is not for production. It holds its table in memory, it never
// expires a key on its own, and it authenticates nothing but the bearer.
package sshkeys

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// DefaultToken is the bearer the stub expects unless WithToken sets
// another; the kind overlay and the install document use it.
const DefaultToken = "stub-sshkeys-token"

// Request is what spec 024's resolver sends, and what Requests records.
type Request struct {
	Fingerprint string `json:"fingerprint"`
	Type        string `json:"type"`
	PublicKey   string `json:"public_key"`
}

// Key is one row of the table: the fingerprint a subject registered,
// with the store's own id for it and how long an answer may be cached.
// One subject per fingerprint is the contract's third rule, and the
// table is a map keyed by fingerprint, so it cannot hold two.
type Key struct {
	Fingerprint string `json:"fingerprint"`
	Subject     string `json:"subject"`
	KeyID       string `json:"key_id,omitempty"`
	TTL         int    `json:"ttl,omitempty"`
}

// Option configures a Server.
type Option func(*Server)

// WithToken sets the bearer the endpoint requires.
func WithToken(token string) Option {
	return func(s *Server) { s.token = token }
}

// Server is one stub key resolver.
type Server struct {
	mu       sync.Mutex
	token    string
	keys     map[string]Key
	requests []Request
	fail     int
	hung     chan struct{}
	closed   chan struct{}
	srv      *httptest.Server
	mux      *http.ServeMux
}

// New starts a stub for the test and stops it with the test.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := NewHandler(opts...)
	s.srv = httptest.NewServer(s.mux)
	t.Cleanup(s.Close)
	return s
}

// NewHandler builds a stub without a listener, for a binary that serves
// Handler itself.
func NewHandler(opts ...Option) *Server {
	s := &Server{token: DefaultToken, keys: map[string]Key{}, closed: make(chan struct{}), mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	s.mux.HandleFunc("POST /{$}", s.resolve)
	s.mux.HandleFunc("PUT /keys", s.putKeys)
	s.mux.HandleFunc("GET /requests", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Requests()) })
	s.mux.HandleFunc("DELETE /requests", func(w http.ResponseWriter, _ *http.Request) { s.ClearRequests(); w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("DELETE /keys/{fingerprint}", func(w http.ResponseWriter, r *http.Request) {
		s.Revoke(r.PathValue("fingerprint"))
		w.WriteHeader(http.StatusNoContent)
	})
	s.mux.HandleFunc("PUT /fail", s.putFail)
	s.mux.HandleFunc("POST /hang", func(w http.ResponseWriter, _ *http.Request) { s.Hang(); w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("POST /resume", func(w http.ResponseWriter, _ *http.Request) { s.Resume(); w.WriteHeader(http.StatusNoContent) })
	return s
}

// Handler serves the endpoint and the control API.
func (s *Server) Handler() http.Handler { return s.mux }

// Close stops the listener and releases every hung request.
func (s *Server) Close() {
	s.mu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	s.mu.Unlock()
	if s.srv != nil {
		s.srv.Close()
	}
}

// URL is the endpoint, the value of ORIGO_SSH_KEYS_URL.
func (s *Server) URL() string { return s.srv.URL }

// Token is the bearer the endpoint requires, the value of
// ORIGO_SSH_KEYS_TOKEN.
func (s *Server) Token() string { return s.token }

// Register puts a key in the table. A fingerprint already registered is
// replaced, which is the one shape a map can hold: the contract's rule
// that a store refuses a fingerprint registered to someone else lives in
// the store's own management API, which this stub has none of.
func (s *Server) Register(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.Fingerprint] = k
}

// Revoke removes a key, which is how a test makes an authenticating key
// stop authenticating.
func (s *Server) Revoke(fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, fingerprint)
}

// SetKeys replaces the table, what a PUT of /keys does.
func (s *Server) SetKeys(keys ...Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = map[string]Key{}
	for _, k := range keys {
		s.keys[k.Fingerprint] = k
	}
}

// Requests lists every request seen, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ClearRequests empties the list.
func (s *Server) ClearRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// Fail makes every answer the given status; 0 restores the table.
func (s *Server) Fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = status
}

// Hang makes the endpoint never answer until Resume or Close.
func (s *Server) Hang() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hung == nil {
		s.hung = make(chan struct{})
	}
}

// Resume clears Hang and Fail: the next request is answered from the
// table.
func (s *Server) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = 0
	if s.hung != nil {
		close(s.hung)
		s.hung = nil
	}
}

// Lookup answers one request from the table, the way the endpoint does.
func (s *Server) Lookup(fingerprint string) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[fingerprint]
	return k, ok
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(raw)), []byte(s.token)) != 1 {
		http.Error(w, "bearer required", http.StatusUnauthorized)
		return
	}
	var req Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	fail, hung := s.fail, s.hung
	s.mu.Unlock()
	if hung != nil {
		select {
		case <-hung:
		case <-s.closed:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	if fail != 0 {
		http.Error(w, "failing on request", fail)
		return
	}
	// Both verdicts are 200. An unknown, revoked, expired, or
	// already-registered fingerprint looks the same on the wire.
	k, ok := s.Lookup(req.Fingerprint)
	if !ok {
		writeJSON(w, map[string]any{"found": false})
		return
	}
	out := map[string]any{"found": true, "subject": k.Subject}
	if k.KeyID != "" {
		out["key_id"] = k.KeyID
	}
	if k.TTL != 0 {
		out["ttl"] = k.TTL
	}
	writeJSON(w, out)
}

func (s *Server) putKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []Key `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.SetKeys(body.Keys...)
	w.WriteHeader(http.StatusNoContent)
}

// putFail reads {"status": <int>} and calls Fail with it; 0 clears the
// outage.
func (s *Server) putFail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status int `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.Fail(body.Status)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
