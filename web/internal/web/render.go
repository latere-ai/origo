// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed templates
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// pageNames are the screens. Each is parsed with the shared base, so each
// file defines "title" and "main" and nothing else.
var pageNames = []string{
	"home", "signin", "overview", "refs", "log", "commit",
	"tree", "blob", "keys", "message", "compare", "tokens", "token", "agents",
}

var pages = func() map[string]*template.Template {
	out := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t := template.Must(template.New(name).ParseFS(templateFS,
			"templates/base.gohtml", "templates/"+name+".gohtml"))
		out[name] = t
	}
	return out
}()

// render writes one screen. The status is explicit because a refusal and a
// page are the same document with a different code.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	t, listed := pages[name]
	if !listed {
		s.fail(w, r, "template "+name+" is not a screen")
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		s.fail(w, r, "render "+name+": "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, detail string) {
	slog.ErrorContext(r.Context(), "origoweb: render failed", "detail", detail)
	http.Error(w, "Something went wrong on this page.", http.StatusInternalServerError)
}

// messageData is the one-sentence screen: an empty result, a refusal, an
// installation that is not answering.
type messageData struct {
	View    view
	Repo    *repoView
	Heading string
	Body    string
	Links   []crumb
}

// The four sentences this interface says when it cannot show a repository.
// Each is written for the person reading it: Origo's own message field is
// written for a developer reading an API, and never reaches a screen.
const (
	absentSentence = "No such repository, or you cannot see it. If you expected to find " +
		"something here, ask whoever administers this installation for access."
	unavailableSentence = "This installation is not answering right now. Nothing was lost. " +
		"Try again in a few minutes."
	brokenSentence = "Something went wrong reading this from the installation."
)

// notFound is the answer to both a refusal and an absence.
//
// Origo answers 403 and 404 to the same question and deliberately refuses to
// say which, so that a caller who may not see a repository does not learn it
// exists. This interface refuses too: one sentence, one status, for both.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request, v view) {
	v.Title = "Not found"
	s.render(w, r, http.StatusNotFound, "message", messageData{
		View:    v,
		Heading: "Not found",
		Body:    absentSentence,
	})
}

func (s *Server) unavailable(w http.ResponseWriter, r *http.Request, v view) {
	v.Title = "Unavailable"
	s.render(w, r, http.StatusBadGateway, "message", messageData{
		View:    v,
		Heading: "The installation is not answering",
		Body:    unavailableSentence,
	})
}

func (s *Server) broken(w http.ResponseWriter, r *http.Request, v view, err error) {
	slog.WarnContext(r.Context(), "origoweb: read failed", "error", err)
	v.Title = "Error"
	s.render(w, r, http.StatusBadGateway, "message", messageData{
		View:    v,
		Heading: "This page could not be read",
		Body:    brokenSentence,
	})
}
