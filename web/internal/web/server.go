// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package web is the interface itself: the routes, the screens, and the
// templates that render them.
//
// Every screen is a document a browser can render with scripting off. There
// is no script anywhere in this package, and the Content-Security-Policy
// below says so, so a page that grew one would stop working rather than
// quietly become required.
package web

import (
	"net/http"
	"strings"

	"latere.ai/x/pkg/health"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
	"github.com/latere-ai/origo-web/internal/session"
)

// Options is what a server needs to run.
type Options struct {
	Config   config.Config
	Sessions *session.Manager
	API      *origo.Client

	Version   string
	Commit    string
	BuildTime string
}

// Server serves the interface.
type Server struct {
	cfg      config.Config
	sessions *session.Manager
	api      *origo.Client
	mux      *http.ServeMux
}

// Route is one entry of the route table.
type Route struct {
	Method  string
	Pattern string
}

// Routes is the whole route table, in one place so it can be asserted
// against. Everything here is a GET except sign-out, the key screen and the
// mint form, the three routes that change anything, and none of them touches
// repository content: this interface has no write path to a repository of
// any kind. Minting writes nothing either: the token it asks for is signed
// and kept nowhere.
func Routes(keys bool) []Route {
	rs := []Route{
		{"GET", "/{$}"},
		{"GET", "/sign-in"},
		{"GET", "/open"},
		{"GET", "/auth/start"},
		{"GET", "/auth/callback"},
		{"POST", "/sign-out"},
		{"GET", "/assets/{file}"},
		{"GET", "/tokens"},
		{"POST", "/tokens"},
		{"GET", "/r/{id}"},
		{"GET", "/r/{id}/refs"},
		{"GET", "/r/{id}/log"},
		{"GET", "/r/{id}/commit/{sha}"},
		{"GET", "/r/{id}/patch/{sha}"},
		{"GET", "/r/{id}/compare"},
		{"GET", "/r/{id}/tree/{path...}"},
		{"GET", "/r/{id}/blob/{path...}"},
		{"GET", "/r/{id}/raw/{path...}"},
	}
	if keys {
		rs = append(rs, Route{"GET", "/keys"}, Route{"POST", "/keys"})
	}
	return rs
}

// New builds the server.
func New(o Options) *Server {
	s := &Server{cfg: o.Config, sessions: o.Sessions, api: o.API, mux: http.NewServeMux()}

	handlers := map[string]http.HandlerFunc{
		"GET /{$}":                   s.handleHome,
		"GET /sign-in":               s.handleSignIn,
		"GET /open":                  s.handleOpen,
		"GET /auth/start":            s.handleAuthStart,
		"GET /auth/callback":         s.handleAuthCallback,
		"POST /sign-out":             s.handleSignOut,
		"GET /assets/{file}":         s.handleAsset,
		"GET /tokens":                s.handleTokens,
		"POST /tokens":               s.handleTokensPost,
		"GET /r/{id}":                s.handleOverview,
		"GET /r/{id}/refs":           s.handleRefs,
		"GET /r/{id}/log":            s.handleLog,
		"GET /r/{id}/commit/{sha}":   s.handleCommit,
		"GET /r/{id}/patch/{sha}":    s.handlePatch,
		"GET /r/{id}/compare":        s.handleCompare,
		"GET /r/{id}/tree/{path...}": s.handleTree,
		"GET /r/{id}/blob/{path...}": s.handleBlob,
		"GET /r/{id}/raw/{path...}":  s.handleRaw,
		"GET /keys":                  s.handleKeys,
		"POST /keys":                 s.handleKeysPost,
	}
	for _, rt := range Routes(o.Config.KeysURL != "") {
		pattern := rt.Method + " " + rt.Pattern
		h, listed := handlers[pattern]
		if !listed {
			panic("origoweb: route " + pattern + " has no handler")
		}
		s.mux.HandleFunc(pattern, h)
	}

	// The probes and the metric registry go on the same listener, because
	// this service has one. They are not screens and carry no session.
	s.mux.Handle("/livez", health.Handler(health.Options{Version: o.Version, Commit: o.Commit, BuildTime: o.BuildTime}))
	s.mux.Handle("/readyz", health.Handler(health.Options{Version: o.Version, Commit: o.Commit, BuildTime: o.BuildTime}))
	s.mux.Handle("/version", health.Handler(health.Options{Version: o.Version, Commit: o.Commit, BuildTime: o.BuildTime}))
	return s
}

// ServeHTTP answers one request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()

	// No script, no frame, no third party. The policy is what makes "every
	// page works without JavaScript" a property of the deployment and not
	// only of the templates.
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; "+
		"style-src 'self'; font-src 'self'; img-src 'self' data:; "+
		"form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")

	// A rendered page belongs to the subject it was rendered for and to
	// nobody else. Origo's ETag is the repository's index sequence, the
	// same for every reader, so any shared cache keyed on it would hand
	// one person's private repository to the next visitor. This service
	// keeps no cache and asks every cache in front of it to keep none.
	if !strings.HasPrefix(r.URL.Path, "/assets/") {
		h.Set("Cache-Control", "private, no-store")
	}
	s.mux.ServeHTTP(w, r)
}

// handleAsset serves the stylesheet and the font out of the binary. They are
// this service's own bytes, the same for every reader, and nothing else is
// served from here.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	switch name {
	case "app.css", "inter-latin.woff2":
	default:
		http.NotFound(w, r)
		return
	}
	body, err := assetFS.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "font/woff2")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
}
