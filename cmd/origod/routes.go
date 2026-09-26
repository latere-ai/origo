// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/url"
	"strings"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/origo/internal/httpgit"
)

// router names a request by the route that serves it. The name is the
// root span's name after the method, http.route on that span and on the
// OpenTelemetry request metrics, the route label of origo_requests_total
// and origo_request_duration_seconds, and the route field of the log
// line, so every signal of one request carries the same name.
//
// The name is the pattern of the mux that serves the request, without
// its method, with two refinements for smart HTTP: the label form's
// single wildcard is named by the operation it dispatches to, and an
// info/refs request carries the service it advertises. The names are:
//
//   - the public mux's own routes: /readyz, /version,
//     /.well-known/jwks.json, /openapi.yaml, /{$} (the landing page),
//     and /favicon.ico;
//   - smart HTTP in each URL form, /r/{id} and /{owner}/{slug}, with or
//     without .git: <form>/info/refs?service=git-upload-pack,
//     <form>/info/refs?service=git-receive-pack, <form>/info/refs for
//     any other service, <form>/git-upload-pack, and
//     <form>/git-receive-pack;
//   - LFS and the repository API by their patterns, such as
//     /{owner}/{slug}/info/lfs/objects/batch and /v1/repos/{id}/refs.
//
// A request no route serves, the application's own fallback, a method
// a route does not take, or a label-form path that names no smart HTTP
// operation, has no name: route returns "", which leaves the span and
// the OpenTelemetry request metrics without http.route and names the
// span by its method alone. The Prometheus label and the log field need
// a value and take it from routeLabel.
//
// A name never carries an id, an owner, a slug, or a reference, only
// the placeholders of a pattern.
//
// route is a function of the method, the path, and the query alone. It
// asks each mux which pattern it would match rather than reading the
// pattern a mux recorded, because the span name formatter calls it
// before the request is routed.
type router struct {
	// public is the listener's outer mux: its named routes and the
	// catch-all that hands every other request to the application.
	public *http.ServeMux
	// app is the application surface behind the verifier.
	app *http.ServeMux
}

func (rt router) route(r *http.Request) string {
	// The catch-all "/" ends in a slash; so does the redirect target a
	// mux reports as the pattern of a CONNECT request, which is a path.
	if _, p := rt.public.Handler(r); p != "" && !strings.HasSuffix(p, "/") {
		return pathOf(p)
	}
	_, p := rt.app.Handler(r)
	if p == "" || strings.HasSuffix(p, "/") {
		return ""
	}
	route := pathOf(p)
	if p == httpgit.NamePattern {
		op := httpgit.Operation(r.Method, serviceOf(r))
		if op == "" {
			return ""
		}
		route = "/{owner}/{slug}/" + op
	}
	if strings.HasSuffix(route, "/info/refs") {
		route += advertised(r)
	}
	return route
}

// routeLabel is a route as the Prometheus request metrics and the log
// line record it: the router's name, or otel.UnmatchedRoute for a
// request no route serves. That is one bucket whatever a client puts in
// the path, and the value every service built on pkg/otel counts such
// requests under.
func routeLabel(route string) string {
	if route == "" {
		return otel.UnmatchedRoute
	}
	return route
}

// pathOf is a mux pattern without its method.
func pathOf(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

// serviceOf is what the label form's wildcard binds to {service...}:
// the escaped path after its first two segments, unescaped as the mux
// unescapes a wildcard's value. An escape that does not decode names
// nothing.
func serviceOf(r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	for range 2 {
		_, rest, _ = strings.Cut(rest, "/")
	}
	service, err := url.PathUnescape(rest)
	if err != nil {
		return ""
	}
	return service
}

// advertised is the query an info/refs name carries: the service when
// it is one of the two smart HTTP services, the only values the handler
// serves, and nothing otherwise, so a client cannot grow the label.
func advertised(r *http.Request) string {
	switch s := r.URL.Query().Get("service"); s {
	case "git-upload-pack", "git-receive-pack":
		return "?service=" + s
	}
	return ""
}
