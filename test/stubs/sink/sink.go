// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package sink is the stub event sink of spec 013: spec 008's endpoint,
// which verifies Origo-Signature over the body with the shared secret,
// records every delivery in order, and answers the status a test
// configured, so a test of the delivery loop reads what arrived and
// drives the retry schedule with a failing sink.
package sink

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultSecret is the HMAC key the stub expects unless WithSecret sets
// another; the kind overlay sets it as ORIGO_EVENTS_SECRET.
const DefaultSecret = "stub-sink-secret"

// The headers of spec 008 the sink reads.
const (
	HeaderSignature = "Origo-Signature"
	HeaderEvent     = "Origo-Event"
	HeaderDelivery  = "Origo-Delivery"
)

// Delivery is one POST the sink saw. ID is Origo-Delivery, Kind is
// Origo-Event, Repo is the body's repo field, Verified says whether
// Origo-Signature matched, Status is what the sink answered, and Body
// is the bytes as sent when they are JSON, or a JSON string of them.
type Delivery struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Repo     string          `json:"repo"`
	Verified bool            `json:"verified"`
	Status   int             `json:"status"`
	At       time.Time       `json:"at"`
	Headers  http.Header     `json:"headers"`
	Body     json.RawMessage `json:"body"`
}

// Option configures a Server.
type Option func(*Server)

// WithSecret sets the HMAC key, the value of ORIGO_EVENTS_SECRET.
func WithSecret(secret string) Option {
	return func(s *Server) { s.secret = secret }
}

// Server is one stub sink.
type Server struct {
	mu         sync.Mutex
	secret     string
	deliveries []Delivery
	// The configured answer: status with body for the next count
	// deliveries, every one while count is 0, 200 when status is 0.
	status  int
	body    json.RawMessage
	count   int
	changed chan struct{}
	srv     *httptest.Server
	mux     *http.ServeMux
}

// New starts a stub sink for the test and stops it with the test.
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
	s := &Server{secret: DefaultSecret, changed: make(chan struct{}), mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	s.mux.HandleFunc("POST /{$}", s.deliver)
	s.mux.HandleFunc("PUT /status", s.putStatus)
	s.mux.HandleFunc("GET /deliveries", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Deliveries(r.URL.Query().Get("repo"), r.URL.Query().Get("kind")))
	})
	s.mux.HandleFunc("DELETE /deliveries", func(w http.ResponseWriter, _ *http.Request) { s.Clear(); w.WriteHeader(http.StatusNoContent) })
	return s
}

// Handler serves the endpoint and the control API.
func (s *Server) Handler() http.Handler { return s.mux }

// Close stops the listener.
func (s *Server) Close() {
	if s.srv != nil {
		s.srv.Close()
	}
}

// URL is the endpoint, the value of ORIGO_EVENTS_URL.
func (s *Server) URL() string { return s.srv.URL }

// Secret is the HMAC key, the value of ORIGO_EVENTS_SECRET.
func (s *Server) Secret() string { return s.secret }

// Deliveries lists every delivery seen, in order, of the repository and
// kind; an empty repo or kind matches every one.
func (s *Server) Deliveries(repo, kind string) []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Delivery
	for _, d := range s.deliveries {
		if (repo == "" || d.Repo == repo) && (kind == "" || d.Kind == kind) {
			out = append(out, d)
		}
	}
	return out
}

// Clear empties the list, what a DELETE of /deliveries does.
func (s *Server) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = nil
}

// Fail makes the next n deliveries answer status, every one while n is
// 0, then 200 again; a status of 0 restores 200 at once. It is
// SetStatus without a body.
func (s *Server) Fail(n, status int) { s.SetStatus(status, nil, n) }

// SetStatus is what a PUT of /status does: the next count deliveries
// are answered status with body, every one while count is 0, then 200
// with no body again; a status of 0 restores 200 at once.
func (s *Server) SetStatus(status int, body json.RawMessage, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body, s.count = status, slices.Clone(body), count
}

// Wait blocks until at least n deliveries of the repository and kind
// have been seen, or the timeout passes, and returns them with whether
// the count was reached.
func (s *Server) Wait(repo, kind string, n int, timeout time.Duration) ([]Delivery, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()
		if got := s.Deliveries(repo, kind); len(got) >= n {
			return got, true
		}
		select {
		case <-changed:
		case <-deadline.C:
			return s.Deliveries(repo, kind), false
		}
	}
}

// Sign computes the Origo-Signature value for a body under the secret,
// so a test sends a delivery the way the node does.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) deliver(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	d := Delivery{
		ID: r.Header.Get(HeaderDelivery), Kind: r.Header.Get(HeaderEvent),
		At: time.Now(), Headers: r.Header.Clone(),
	}
	if json.Valid(raw) {
		d.Body = raw
	} else if quoted, err := json.Marshal(string(raw)); err == nil {
		d.Body = quoted
	}
	var fields struct {
		Repo string `json:"repo"`
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &fields)
	d.Repo = fields.Repo
	if d.Kind == "" {
		d.Kind = fields.Kind
	}
	given := r.Header.Get(HeaderSignature)
	d.Verified = hmac.Equal([]byte(given), []byte(Sign(s.secret, raw)))

	s.mu.Lock()
	var body json.RawMessage
	switch {
	case !d.Verified:
		d.Status = http.StatusUnauthorized
	case s.status != 0:
		d.Status, body = s.status, s.body
		if s.count > 0 {
			if s.count--; s.count == 0 {
				s.status, s.body = 0, nil
			}
		}
	default:
		d.Status = http.StatusOK
	}
	s.deliveries = append(s.deliveries, d)
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()

	if !d.Verified {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if body != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(d.Status)
		_, _ = w.Write(body)
		return
	}
	w.WriteHeader(d.Status)
}

func (s *Server) putStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
		Count  int             `json:"count"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Status != 0 && (body.Status < 100 || body.Status > 599) {
		http.Error(w, "status must be 0 or between 100 and 599, got "+strconv.Itoa(body.Status), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(string(body.Body)) == "null" {
		body.Body = nil
	}
	s.SetStatus(body.Status, body.Body, body.Count)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
