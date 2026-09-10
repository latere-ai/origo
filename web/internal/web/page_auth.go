// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/latere-ai/origo-web/internal/origo"
	"github.com/latere-ai/origo-web/internal/session"
)

// req is what one request carries into a screen: the reader's token, which
// is empty when there is none, and the frame every page renders.
type req struct {
	tok string
	v   view
}

// begin reads the session once per request. A refreshed session is written
// back to the cookie here, before any call to Origo, so a call never races
// the expiry boundary.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, section string) req {
	tok := s.sessions.Token(w, r)
	return req{
		tok: tok,
		v: view{
			Section:     section,
			SignedIn:    tok != "",
			KeysEnabled: s.cfg.KeysURL != "",
			CSRF:        s.sessions.CSRFToken(w),
		},
	}
}

// signInData is the front door.
type signInData struct {
	View        view
	Heading     string
	Lead        string
	ButtonLabel string
	CloneHTTPS  string
	CloneSSH    string
	ReturnTo    string
	Refused     bool
}

// signIn renders the sign-in screen. It is what a signed-out visitor sees on
// every repository URL today, because Origo requires a credential on every
// read (spec 007) and has no anonymous path yet. The page keeps the address
// the person asked for, so signing in lands there and not on the home page.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, v view, refused bool) {
	v.Title = "Sign in"
	v.SignedIn = false
	label := "Continue to sign in"
	if s.cfg.IssuerName != "" {
		label = "Continue with " + s.cfg.IssuerName
	}
	status := http.StatusOK
	if refused {
		status = http.StatusUnauthorized
	}
	s.render(w, r, status, "signin", signInData{
		View:        v,
		Heading:     "origo",
		Lead:        "This is a git server. It holds this organisation's repositories and serves them over HTTPS" + sshClause(s.cfg.SSHCloneHost) + ".",
		ButtonLabel: label,
		CloneHTTPS:  s.cfg.CloneHTTPS("<owner>", "<name>"),
		CloneSSH:    s.cfg.CloneSSH("<owner>", "<name>"),
		ReturnTo:    returnTo(r),
		Refused:     refused,
	})
}

func sshClause(host string) string {
	if host == "" {
		return ""
	}
	return " and SSH"
}

// returnTo is the path to come back to after signing in: this request's own
// path, and never a path from another origin.
func returnTo(r *http.Request) string {
	p := r.URL.Path
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "/"
	}
	return p
}

func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	rq := s.begin(w, r, "")
	if rq.tok != "" {
		http.Redirect(w, r, safeReturn(r.URL.Query().Get("return_to")), http.StatusFound)
		return
	}
	v := rq.v
	v.Title = "Sign in"
	label := "Continue to sign in"
	if s.cfg.IssuerName != "" {
		label = "Continue with " + s.cfg.IssuerName
	}
	s.render(w, r, http.StatusOK, "signin", signInData{
		View:        v,
		Heading:     "origo",
		Lead:        "This is a git server. It holds this organisation's repositories and serves them over HTTPS" + sshClause(s.cfg.SSHCloneHost) + ".",
		ButtonLabel: label,
		CloneHTTPS:  s.cfg.CloneHTTPS("<owner>", "<name>"),
		CloneSSH:    s.cfg.CloneSSH("<owner>", "<name>"),
		ReturnTo:    safeReturn(r.URL.Query().Get("return_to")),
	})
}

// handleAuthStart hands the browser to the issuer. It is a GET because it
// changes nothing here: the session is written by the callback.
func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	s.sessions.SignIn(w, r, safeReturn(r.URL.Query().Get("return_to")))
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	s.sessions.Callback(w, r)
}

func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if !s.sessions.CSRFValid(r) {
		http.Error(w, "This form has expired. Go back and try again.", http.StatusForbidden)
		return
	}
	s.sessions.Clear(w)
	http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
}

func safeReturn(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "/"
	}
	return p
}

// homeData is the home screen: the directory when the installation can serve
// one, and the way in without it when it cannot.
type homeData struct {
	View      view
	Repos     []listRow
	Next      string
	Directory bool
	Recent    []listRow
	Query     string
	Stale     string
}

type listRow struct {
	Name          string
	Owner         string
	URL           string
	DefaultBranch string
	Size          string
	Pushed        string
	PushedExact   string
	ID            string
}

// handleHome lists the repositories the reader may see.
//
// Origo cannot answer that question today: it has no collection route, and
// its authorizer contract is a yes/no oracle over one named repository with
// no enumerate verb (spec 023). So the screen asks, and on an installation
// that cannot answer it degrades to the way in that works without a
// directory: the address of a repository, and the ones this session has
// already opened. It does not guess the list from a token claim, which would
// be a second access-control model and would diverge from the authorizer's
// answer the first time the two disagreed.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	rq := s.begin(w, r, "")
	rq.v.Title = "Repositories"

	data := homeData{View: rq.v, Query: r.URL.Query().Get("q")}
	page, err := s.api.List(r.Context(), rq.tok, r.URL.Query().Get("cursor"), 50)
	switch {
	case err == nil:
		data.Directory = true
		data.Next = page.Next
		data.Stale = staleSentence(page.Meta)
		for _, repo := range page.Items {
			data.Repos = append(data.Repos, s.listRow(repo))
		}
	case origo.Unauthenticated(err):
		s.sessions.Clear(w)
		s.signIn(w, r, rq.v, true)
		return
	default:
		// ErrNoDirectory, and every other refusal of a route this
		// installation does not serve, are the same screen: the one
		// that works without a directory.
		data.Directory = false
	}

	for _, id := range session.Recent(r) {
		data.Recent = append(data.Recent, listRow{ID: id, Name: id, URL: "/r/" + url.PathEscape(id)})
	}
	s.render(w, r, http.StatusOK, "home", data)
}

func (s *Server) listRow(repo origo.Repo) listRow {
	row := listRow{
		ID:            repo.ID,
		Name:          repo.Slug,
		Owner:         repo.Owner,
		URL:           "/r/" + url.PathEscape(repo.ID),
		DefaultBranch: repo.DefaultBranch,
		Size:          humanSize(repo.SizeBytes),
	}
	if repo.PushedAt != nil && !repo.PushedAt.IsZero() {
		row.Pushed = relTime(*repo.PushedAt)
		row.PushedExact = absTime(*repo.PushedAt)
	}
	return row
}

// keysData is the one screen with no contract behind it.
type keysData struct {
	View view
	URL  string
}

// handleKeys exists only when a key surface is configured, which is never by
// default. Spec 024 keeps public keys out of Origo, behind a resolver Origo
// only reads, so no component owns a place to add or remove one. When a
// component grows that place, this screen lists the reader's keys with a
// comment and a fingerprint, adds one, and removes one by fingerprint.
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	rq := s.begin(w, r, "")
	if rq.tok == "" {
		s.signIn(w, r, rq.v, false)
		return
	}
	rq.v.Title = "SSH keys"
	s.render(w, r, http.StatusNotImplemented, "keys", keysData{View: rq.v, URL: s.cfg.KeysURL})
}

func (s *Server) handleKeysPost(w http.ResponseWriter, r *http.Request) {
	if !s.sessions.CSRFValid(r) {
		http.Error(w, "This form has expired. Go back and try again.", http.StatusForbidden)
		return
	}
	s.handleKeys(w, r)
}

// handleOpen takes the identifier a person typed on the home screen and
// sends them to it. It is a GET form and a redirect, so the address bar
// carries the repository and the page can be bookmarked.
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/r/"+url.PathEscape(id), http.StatusSeeOther)
}
