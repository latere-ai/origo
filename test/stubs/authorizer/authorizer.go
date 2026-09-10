// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package authorizer is the stub of spec 007's authorizer endpoint, to
// the control API of spec 013's stub table: a rule table a test chooses
// the answers from, a record of every request in order, and two
// failure modes for the outage cases. Spec 007 builds it because its
// own criteria need it; spec 013's binary runs it beside the other
// stubs and adds the three outage paths, PUT /fail, POST /hang, and
// POST /resume, so a stack run sets the outage through the host port.
//
// One rule of the contract binds every authorizer, this one included:
// the probe id 00000000-0000-0000-0000-000000000001 is denied for every
// subject and action, because `origod check` (spec 018) treats an allow
// on it as a misconfigured authorizer.
package authorizer

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// ProbeID is the reserved repository id every authorizer denies.
const ProbeID = "00000000-0000-0000-0000-000000000001"

// DefaultToken is the bearer the stub expects unless WithToken sets
// another; the kind overlay and make dev use it.
const DefaultToken = "stub-authorizer-token"

// Request is what spec 007's client sends, and what Requests records.
type Request struct {
	Subject string `json:"subject"`
	Actor   string `json:"actor"`
	Repo    struct {
		ID    string `json:"id"`
		Owner string `json:"owner"`
		Slug  string `json:"slug"`
	} `json:"repo"`
	Action string `json:"action"`
}

// DirectoryEntry is one repository of the stub's directory, as spec
// 026's list answer names it.
type DirectoryEntry struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Slug  string `json:"slug"`
}

// Rule is one row of the table. Subject, Actor, Repo, and Action are `*`
// or a value; Repo is an id or `owner/slug`. An empty field matches
// everything, the same as `*`. TTL, Replicas, QuotaBytes, and
// RequestsPerMinute are sent only when set, so the node applies its
// defaults otherwise.
type Rule struct {
	Subject    string `json:"subject"`
	Actor      string `json:"actor"`
	Repo       string `json:"repo"`
	Action     string `json:"action"`
	Allow      bool   `json:"allow"`
	Reason     string `json:"reason,omitempty"`
	TTL        int    `json:"ttl,omitempty"`
	Replicas   int    `json:"replicas,omitempty"`
	QuotaBytes int64  `json:"quota_bytes,omitempty"`
	// RequestsPerMinute is spec 007's optional figure: the rate this
	// subject alone is bucketed at (spec 012, read by spec 020).
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
}

func (r Rule) matches(req Request) bool {
	return star(r.Subject, req.Subject) && star(r.Actor, req.Actor) && star(r.Action, req.Action) &&
		(star(r.Repo, req.Repo.ID) || (req.Repo.Owner != "" && r.Repo == req.Repo.Owner+"/"+req.Repo.Slug))
}

func star(pattern, value string) bool {
	return pattern == "" || pattern == "*" || pattern == value
}

// Option configures a Server.
type Option func(*Server)

// WithToken sets the bearer the endpoint requires.
func WithToken(token string) Option {
	return func(s *Server) { s.token = token }
}

// WithAllow sets the subjects allowed when no rule matches, `*` for
// all. The default is `*`; WithAllow() with no subject allows nobody by
// default.
func WithAllow(subjects ...string) Option {
	return func(s *Server) { s.allow = subjects }
}

// Server is one stub authorizer.
type Server struct {
	mu       sync.Mutex
	token    string
	allow    []string
	rules    []Rule
	requests []Request
	fail     int
	// directory is spec 026's answer to a list request, and supported
	// says whether the stub has one at all. An unset directory answers
	// {"directory": false}, so an installation and a test that never
	// mention one see an authorizer with no directory, which is what
	// every deployment of this stub was before spec 026.
	supported bool
	directory []DirectoryEntry
	hung      chan struct{}
	closed    chan struct{}
	srv       *httptest.Server
	mux       *http.ServeMux
}

// New starts a stub authorizer for the test and stops it with the test.
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
	s := &Server{token: DefaultToken, allow: []string{"*"}, closed: make(chan struct{}), mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	s.mux.HandleFunc("POST /{$}", s.decide)
	s.mux.HandleFunc("PUT /rules", s.putRules)
	s.mux.HandleFunc("PUT /directory", s.putDirectory)
	s.mux.HandleFunc("GET /requests", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Requests()) })
	s.mux.HandleFunc("DELETE /requests", func(w http.ResponseWriter, _ *http.Request) { s.ClearRequests(); w.WriteHeader(http.StatusNoContent) })
	// The outage is set over HTTP as well as by method (spec 013), so a
	// stack run drives it through the host port: each path calls the
	// method of the same name.
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

// URL is the endpoint, the value of ORIGO_AUTHORIZER_URL.
func (s *Server) URL() string { return s.srv.URL }

// Token is the bearer the endpoint requires, the value of
// ORIGO_AUTHORIZER_TOKEN.
func (s *Server) Token() string { return s.token }

// Allow adds an allow rule. A later rule wins over an earlier one that
// matches the same request.
func (s *Server) Allow(rule Rule) {
	rule.Allow = true
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, rule)
}

// Deny adds a deny rule with the reason.
func (s *Server) Deny(rule Rule, reason string) {
	rule.Allow, rule.Reason = false, reason
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, rule)
}

// SetRules replaces the table, what a PUT of /rules does.
func (s *Server) SetRules(rules ...Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = slices.Clone(rules)
}

// SetDirectory sets the directory spec 026's list action answers with.
// supported false answers {"directory": false} whatever the entries.
func (s *Server) SetDirectory(supported bool, repos ...DirectoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.supported, s.directory = supported, slices.Clone(repos)
}

// Directory answers one list request: the entries the rule table allows
// this subject and actor to read, cut at limit from cursor. It runs the
// same table a read request runs, so a rule that denies a subject one
// repository denies it in the directory too and there is no second
// table to keep in step.
func (s *Server) Directory(req Request, cursor string, limit int) (entries []DirectoryEntry, next string, supported bool) {
	entries = []DirectoryEntry{}
	s.mu.Lock()
	held, supported := slices.Clone(s.directory), s.supported
	s.mu.Unlock()
	if !supported {
		return nil, "", false
	}
	if limit <= 0 {
		limit = 50
	}
	started := cursor == ""
	for _, e := range held {
		if !started {
			started = e.ID == cursor
			continue
		}
		probe := Request{Subject: req.Subject, Actor: req.Actor, Action: string(actionRead)}
		probe.Repo.ID, probe.Repo.Owner, probe.Repo.Slug = e.ID, e.Owner, e.Slug
		if !s.Decide(probe).Allow {
			continue
		}
		if len(entries) == limit {
			return entries, entries[len(entries)-1].ID, true
		}
		entries = append(entries, e)
	}
	return entries, "", true
}

// The two actions the stub names itself: the directory question, and
// the read it puts each entry through.
const (
	actionList = "list"
	actionRead = "read"
)

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

// Fail makes every answer the given status; 0 restores the rule table.
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
// rule table.
func (s *Server) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = 0
	if s.hung != nil {
		close(s.hung)
		s.hung = nil
	}
}

// Decide answers one request from the table, the way the endpoint does.
func (s *Server) Decide(req Request) Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Repo.ID == ProbeID {
		return Rule{Allow: false, Reason: "the probe id is reserved"}
	}
	for _, v := range slices.Backward(s.rules) {
		if v.matches(req) {
			return v
		}
	}
	if slices.Contains(s.allow, "*") || slices.Contains(s.allow, req.Subject) {
		return Rule{Allow: true}
	}
	return Rule{Allow: false, Reason: "no rule allows " + req.Subject}
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(raw)), []byte(s.token)) != 1 {
		http.Error(w, "bearer required", http.StatusUnauthorized)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A list request carries a cursor and a limit and no repo object
	// (spec 026); the three actions carry neither.
	var listQuery struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	_ = json.Unmarshal(payload, &listQuery)
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
	if req.Action == actionList {
		entries, next, supported := s.Directory(req, listQuery.Cursor, listQuery.Limit)
		switch {
		case !supported:
			// An authorizer with no directory says so whatever the rule
			// table holds: it cannot answer the question at all.
			writeJSON(w, map[string]any{"directory": false})
			return
		case !s.Decide(req).Allow:
			writeJSON(w, map[string]any{"allow": false, "reason": s.Decide(req).Reason})
			return
		}
		out := map[string]any{"repos": entries}
		if next != "" {
			out["next_cursor"] = next
		}
		writeJSON(w, out)
		return
	}
	rule := s.Decide(req)
	if !rule.Allow {
		writeJSON(w, map[string]any{"allow": false, "reason": rule.Reason})
		return
	}
	out := map[string]any{"allow": true}
	if rule.TTL != 0 {
		out["ttl"] = rule.TTL
	}
	if rule.Replicas != 0 {
		out["replicas"] = rule.Replicas
	}
	if rule.QuotaBytes != 0 {
		out["quota_bytes"] = rule.QuotaBytes
	}
	if rule.RequestsPerMinute != 0 {
		out["requests_per_minute"] = rule.RequestsPerMinute
	}
	writeJSON(w, out)
}

// putDirectory reads {"supported": <bool>, "repos": [...]} and calls
// SetDirectory with it, so a stack run sets a directory through the
// host port the way it sets the rules.
func (s *Server) putDirectory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Supported bool             `json:"supported"`
		Repos     []DirectoryEntry `json:"repos"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.SetDirectory(body.Supported, body.Repos...)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.SetRules(body.Rules...)
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
