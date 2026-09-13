// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package authorizer is Origo's stub authorizer: the shared package's
// stub (latere.ai/x/pkg/authz/stub) with Origo's rule table (Origo spec
// 028). The shared stub carries the envelope, the rule table, the record
// of every request, and the outage modes; this package adds Origo's
// action vocabulary (repo.read, repo.write, repo.admin, repo.list), its
// resource shape (a rule names a repository by id or owner/slug), and
// spec 026's directory, which the shared stub does not know.
//
// A test drives it through the embedded shared server (SetRules, Allow,
// Deny, Fail, Hang, Resume, Requests) and, for the directory, through
// SetDirectory. The control API is HTTP as well as methods (spec 013):
// PUT /rules, GET and DELETE /requests, PUT /fail, POST /hang, POST
// /resume are the shared stub's; PUT /directory is Origo's, mounted on a
// mux that wraps the shared handler.
//
// One rule of the contract binds every authorizer, this one included:
// the probe id is denied for every subject and action, because origod
// check treats an allow on it as a misconfigured authorizer.
package authorizer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
)

// ProbeID is the reserved repository id every authorizer denies.
const ProbeID = authz.ProbeID

// DefaultToken is the bearer the stub expects unless WithToken sets
// another; the kind overlay and make dev use it.
const DefaultToken = stub.DefaultToken

// Rule is one row of the table, the shared stub's row: Subject, Action,
// and Resource are `*` or a value, Resource an id or the owner/slug this
// package renders, and Limits the figures object spec 007 names. Action
// is the contract-2 vocabulary, repo.read and its siblings.
type Rule = stub.Rule

// Option configures the stub. WithToken and WithAllow are the shared
// stub's; a test passes them through.
type Option = stub.Option

// WithToken sets the bearer the endpoint requires.
func WithToken(token string) Option { return stub.WithToken(token) }

// WithAllow sets the subjects allowed when no rule matches, `*` for all.
func WithAllow(subjects ...string) Option { return stub.WithAllow(subjects...) }

// DirectoryEntry is one repository of the stub's directory, as spec
// 026's list answer names it.
type DirectoryEntry struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Slug  string `json:"slug"`
}

// resourceName renders a repository resource as a rule's owner/slug form.
// A resource with no owner, which is a directory request's, renders as
// the empty string so it matches only a rule that names everything, not
// a rule named "/" (Origo spec 028).
func resourceName(r authz.Resource) string {
	owner, slug := r.String("owner"), r.String("slug")
	if owner == "" {
		return ""
	}
	return owner + "/" + slug
}

// Server is one stub authorizer: the shared server with Origo's
// directory.
type Server struct {
	*stub.Server
	mu        sync.Mutex
	supported bool
	directory []DirectoryEntry
	mux       *http.ServeMux
	srv       *httptest.Server
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
// Handler itself (spec 013's stub host).
func NewHandler(opts ...Option) *Server {
	s := &Server{}
	opts = append(opts,
		stub.WithResourceName(resourceName),
		stub.WithAction(listAction, s.answerDirectory),
	)
	s.Server = stub.NewHandler(opts...)
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("PUT /directory", s.putDirectory)
	s.mux.Handle("/", s.Server.Handler())
	return s
}

// Handler serves the endpoint, the shared control API, and Origo's
// directory route.
func (s *Server) Handler() http.Handler { return s.mux }

// The contract-2 action vocabulary the stub answers, kept as plain
// strings so the stub depends on no product package.
const (
	listAction = "repo.list"
	readAction = "repo.read"
)

// URL is the endpoint, the value of ORIGO_AUTHORIZER_URL. It is the
// wrapping listener's, so a request reaches both the directory route and
// the shared handler.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the listener and releases every hung request.
func (s *Server) Close() {
	s.Server.Close()
	if s.srv != nil {
		s.srv.Close()
	}
}

// SetDirectory sets the directory spec 026's list action answers with.
// supported false answers {"directory": false} whatever the entries.
func (s *Server) SetDirectory(supported bool, repos ...DirectoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.supported, s.directory = supported, slices.Clone(repos)
}

// answerDirectory builds spec 026's list answer: the entries the rule
// table allows this subject to read, cut at limit from cursor. It runs
// the same table a read request runs, so a rule that denies a subject a
// repository denies it in the directory too. It is the Answer the shared
// stub calls for the list action, after the table, the record, and the
// outage modes.
func (s *Server) answerDirectory(req authz.Request) any {
	s.mu.Lock()
	held, supported := slices.Clone(s.directory), s.supported
	s.mu.Unlock()
	if !supported {
		return map[string]any{"directory": false}
	}
	if d := s.Decide(req); !d.Allow {
		return map[string]any{"allow": false, "reason": d.Reason}
	}
	limit := req.Resource.Int("limit")
	if limit <= 0 {
		limit = 50
	}
	cursor := req.Resource.String("cursor")
	entries := []DirectoryEntry{}
	started := cursor == ""
	for _, e := range held {
		if !started {
			started = e.ID == cursor
			continue
		}
		probe := authz.Request{
			Subject:  req.Subject,
			Issuer:   req.Issuer,
			Sub:      req.Sub,
			Action:   readAction,
			Resource: authz.NewResource(resourceKind, e.ID, map[string]any{"owner": e.Owner, "slug": e.Slug}),
		}
		if !s.Decide(probe).Allow {
			continue
		}
		if len(entries) == limit {
			return map[string]any{"repos": entries, "next_cursor": entries[len(entries)-1].ID}
		}
		entries = append(entries, e)
	}
	return map[string]any{"repos": entries}
}

// resourceKind is the kind every repository envelope names.
const resourceKind = "Repository"

// putDirectory reads {"supported": <bool>, "repos": [...]} and calls
// SetDirectory with it, so a stack run sets a directory through the host
// port the way it sets the rules.
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
