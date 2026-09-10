// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/latere-ai/origo-web/internal/origo"
)

// lifetime is one entry of the lifetime control: the seconds the request
// carries and the words a person reads.
type lifetime struct {
	Seconds int
	Label   string
	Default bool
}

// lifetimes are the lifetimes this screen offers. The longest is the longest
// the installation will sign, so the control never offers a choice the
// server refuses, and there is no unbounded entry because there is no
// unbounded token.
var lifetimes = []lifetime{
	{Seconds: 300, Label: "5 minutes"},
	{Seconds: 900, Label: "15 minutes"},
	{Seconds: int(origo.MaxTokenTTL.Seconds()), Label: "1 hour — the longest this installation signs", Default: true},
}

// tokensData is the token screen: the form that works, and the plain account
// of what this installation cannot do.
type tokensData struct {
	View      view
	Directory bool
	Repos     []listRow
	Recent    []listRow
	Lifetimes []lifetime

	// Repo is the repository the screen is scoped to, when one was named.
	Repo *repoView
	// Registry reports that this installation keeps a record of the tokens
	// it minted. It is false everywhere today.
	Registry bool
	Tokens   []tokenRow

	// Form is what a refused submission carries back, so nothing is retyped.
	Form  tokenForm
	Error string
}

// tokenForm is what the mint form holds between a refusal and the next try.
type tokenForm struct {
	Repo  string
	Scope string
	TTL   int
}

// tokenRow is one row of a token table, on the day there is one to render.
type tokenRow struct {
	ID        string
	Scope     string
	Issued    string
	Expires   string
	LastUsed  string
	Revoked   bool
	Withdrawn string
}

// tokenData is the screen a token is shown on, once.
type tokenData struct {
	View      view
	Token     string
	Repo      *repoView
	Scope     string
	Expires   string
	ExpiresIn string
	CloneLine string
	ReadLine  string
	BackURL   string
}

const (
	// The sentence for a repository whose tokens this reader may not mint.
	// Minting needs administrative access, and the installation refuses a
	// repository you cannot administer and a repository that is not there
	// in the same breath, so this page does too.
	tokenRefusedSentence = "No such repository, or you cannot mint tokens for it. Minting needs " +
		"administrative access to that one repository. Ask whoever administers this " +
		"installation for it, or for a token."
	tokenFormSentence = "Choose a repository, a scope, and a lifetime."
)

// handleTokens renders the token screen.
//
// The screen is one form and one explanation. The form mints, which is the
// whole of what this installation can do with tokens: they are signed and
// carry no record, so nothing is written when one is created and there is
// nothing to list or to withdraw. The explanation says that in the reader's
// words rather than showing an empty table.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	rq := s.begin(w, r, "tokens")
	if rq.tok == "" {
		s.signIn(w, r, rq.v, false)
		return
	}
	rq.v.Title = "Agent tokens"
	s.renderTokens(w, r, rq, http.StatusOK, tokenForm{
		Repo:  strings.TrimSpace(r.URL.Query().Get("repo")),
		Scope: string(origo.ScopeRead),
		TTL:   defaultLifetime(),
	}, "")
}

// renderTokens draws the screen, whether it was asked for or refused.
func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, rq req, status int, form tokenForm, refusal string) {
	rq.v.Title = "Agent tokens"
	data := tokensData{View: rq.v, Lifetimes: lifetimes, Form: form, Error: refusal}

	// The same question the home screen asks, answered the same way: a
	// directory when the installation has one, and a box to name a
	// repository when it does not.
	if page, err := s.api.List(r.Context(), rq.tok, "", 200); err == nil {
		data.Directory = true
		for _, repo := range page.Items {
			data.Repos = append(data.Repos, s.listRow(repo))
		}
	}
	for _, id := range s.sessions.Recent(r) {
		data.Recent = append(data.Recent, listRow{ID: id, Name: id, URL: "/tokens?repo=" + url.QueryEscape(id)})
	}

	// A named repository is resolved so the screen can say its name, and
	// its token record asked for so the table appears the day there is one.
	if form.Repo != "" {
		if repo, _, err := s.api.Repo(r.Context(), rq.tok, form.Repo); err == nil {
			data.Repo = newRepoView(repo, "")
			if records, err := s.api.Tokens(r.Context(), rq.tok, form.Repo); err == nil {
				data.Registry = true
				data.Tokens = tokenRows(records)
			}
		}
	}
	s.render(w, r, status, "tokens", data)
}

func tokenRows(records []origo.TokenRecord) []tokenRow {
	out := make([]tokenRow, 0, len(records))
	for _, rec := range records {
		row := tokenRow{ID: rec.ID, Scope: string(rec.Scope), Revoked: rec.RevokedAt != nil}
		if rec.IssuedAt != nil {
			row.Issued = relTime(*rec.IssuedAt)
		}
		if rec.ExpiresAt != nil {
			row.Expires = absTime(*rec.ExpiresAt)
		}
		if rec.LastUsed != nil {
			row.LastUsed = relTime(*rec.LastUsed)
		}
		if rec.RevokedAt != nil {
			row.Withdrawn = relTime(*rec.RevokedAt)
		}
		out = append(out, row)
	}
	return out
}

// handleTokensPost mints one token and shows it, once.
//
// There is no redirect between the mint and the page: the token exists in
// this one response and nowhere else, so sending the browser to fetch a
// second page would mean holding it somewhere to hand over, and there is
// nowhere this service is willing to hold it.
func (s *Server) handleTokensPost(w http.ResponseWriter, r *http.Request) {
	if !s.sessions.CSRFValid(r) {
		http.Error(w, "This form has expired. Go back and try again.", http.StatusForbidden)
		return
	}
	rq := s.begin(w, r, "tokens")
	if rq.tok == "" {
		s.signIn(w, r, rq.v, false)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderTokens(w, r, rq, http.StatusBadRequest, tokenForm{Scope: string(origo.ScopeRead), TTL: defaultLifetime()}, tokenFormSentence)
		return
	}
	form := tokenForm{
		Repo:  strings.TrimSpace(r.PostFormValue("repo")),
		Scope: r.PostFormValue("scope"),
		TTL:   defaultLifetime(),
	}
	if secs, err := strconv.Atoi(r.PostFormValue("ttl")); err == nil && offered(secs) {
		form.TTL = secs
	}
	scope := origo.Scope(form.Scope)
	if form.Repo == "" || (scope != origo.ScopeRead && scope != origo.ScopeWrite) {
		form.Scope = string(origo.ScopeRead)
		s.renderTokens(w, r, rq, http.StatusBadRequest, form, tokenFormSentence)
		return
	}

	// The repository is read before it is minted against, so the name on
	// the page that follows is the repository's own and a reader who may
	// not see it is refused by the same sentence every other screen uses.
	repo, _, err := s.api.Repo(r.Context(), rq.tok, form.Repo)
	if err != nil {
		s.mintRefused(w, r, rq, form, err)
		return
	}
	token, err := s.api.Mint(r.Context(), rq.tok, form.Repo, scope, time.Duration(form.TTL)*time.Second)
	if err != nil {
		s.mintRefused(w, r, rq, form, err)
		return
	}

	rq.v.Title = "Your new token"
	view := newRepoView(repo, "")
	s.render(w, r, http.StatusOK, "token", tokenData{
		View:      rq.v,
		Token:     token.Value,
		Repo:      view,
		Scope:     form.Scope,
		Expires:   absTime(token.ExpiresAt),
		ExpiresIn: lifetimeLabel(form.TTL),
		CloneLine: "git clone https://x-access-token:$ORIGO_TOKEN@" + hostOf(s.cfg.CloneHTTPS(repo.Owner, repo.Slug)) + "/" + repo.Owner + "/" + repo.Slug + ".git",
		ReadLine:  "curl -H \"Authorization: Bearer $ORIGO_TOKEN\" \\\n  " + s.cfg.RepoAPIURL(repo.ID),
		BackURL:   "/tokens?repo=" + url.QueryEscape(repo.ID),
	})
}

// mintRefused answers a refusal of either call the mint makes. A reader who
// may not administer the repository and a repository that is not there are
// one sentence, because the installation refuses to distinguish them.
func (s *Server) mintRefused(w http.ResponseWriter, r *http.Request, rq req, form tokenForm, err error) {
	switch {
	case origo.Unauthenticated(err):
		s.sessions.Clear(w)
		s.signIn(w, r, rq.v, true)
	case origo.Absent(err):
		s.renderTokens(w, r, rq, http.StatusNotFound, form, tokenRefusedSentence)
	case origo.Unavailable(err):
		s.unavailable(w, r, rq.v)
	default:
		s.renderTokens(w, r, rq, http.StatusBadGateway, form, brokenSentence)
	}
}

func defaultLifetime() int {
	for _, l := range lifetimes {
		if l.Default {
			return l.Seconds
		}
	}
	return lifetimes[len(lifetimes)-1].Seconds
}

func offered(secs int) bool {
	for _, l := range lifetimes {
		if l.Seconds == secs {
			return true
		}
	}
	return false
}

func lifetimeLabel(secs int) string {
	for _, l := range lifetimes {
		if l.Seconds == secs {
			return strings.TrimSuffix(strings.Split(l.Label, " — ")[0], " ")
		}
	}
	return plural(secs, "second")
}

// hostOf is the host of a URL this service built itself.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
