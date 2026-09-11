// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"strings"
)

// Anonymous read (spec 027).
//
// Origo holds no visibility and no ownership: whether a repository may be
// read without a credential is the authorizer's answer, not a fact the
// node stores. What the node owns is the two things the authorizer cannot
// do. Admitting a request that carries no credential is this file. Paying
// for it is internal/limits.
//
// The feature is off unless ORIGO_ANONYMOUS_READ is set. With it unset no
// credential-less request is admitted anywhere and the node behaves
// exactly as it did before this spec, reason token for reason token.

// AnonymousSubject is the effective subject of an admitted anonymous
// request. It is the empty string, because that is what the authorizer
// contract calls a caller with no identity, and every decision path
// already reads it that way.
const AnonymousSubject = ""

// AnonymousRoutes is the maintained list of what a caller with no
// credential may reach. It is a list and not a rule over the action,
// because the authorizer sees only "read" and cannot tell /blob from
// /export.bundle: a read-marked route added elsewhere in the tree does
// not become anonymous by being added, it becomes anonymous by being
// added here.
//
// Withheld on purpose, with the reason in spec 027: /stats, the LFS
// batch, /export.bundle, /import, the directory, and every write and
// administrative route.
//
// The pretty form is the same /{owner}/{slug}/{service...} pattern the
// handler routes on, and not two exact patterns: an exact
// /{owner}/{slug}/info/refs is neither more nor less specific than
// /v1/repos/{id}/refs, and a mux holding both refuses to build. The
// wildcard covers git-receive-pack as well, so anonymousService below
// reads the service out of the path and admits only the fetch half.
var AnonymousRoutes = []string{
	"GET /r/{id}/info/refs",
	"POST /r/{id}/git-upload-pack",
	prettyRoute,
	"GET /v1/repos/{id}",
	"GET /v1/repos/{id}/refs",
	"GET /v1/repos/{id}/commits",
	"GET /v1/repos/{id}/commits/{sha}",
	"GET /v1/repos/{id}/compare/{range}",
	"GET /v1/repos/{id}/tree/{sha}",
	"GET /v1/repos/{id}/blob/{sha}",
	"GET /v1/repos/{id}/archive/{file}",
}

// prettyRoute is the owner/slug form of the git routes, carrying no
// method because the handler's own pattern carries none.
const prettyRoute = "/{owner}/{slug}/{service...}"

// UploadPackService is the only value of the service parameter an
// anonymous info/refs may carry. git-receive-pack is a push.
const UploadPackService = "git-upload-pack"

// anonymousMux matches a request against AnonymousRoutes. It is an
// http.ServeMux holding the same patterns, so Go's own routing decides
// which pattern a path belongs to rather than a hand-rolled prefix test,
// and the precedence between /r/{id}/info/refs and
// /{owner}/{slug}/info/refs is the precedence the application mux uses.
var anonymousMux = func() *http.ServeMux {
	mux := http.NewServeMux()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, pattern := range AnonymousRoutes {
		mux.Handle(pattern, h)
	}
	return mux
}()

// AnonymousEligible reports whether a request with no credential may be
// admitted with an empty subject. It answers the route question only: the
// authorizer still decides whether this repository is readable, and a
// denied anonymous request is refused by WriteRefusal.
func AnonymousEligible(r *http.Request) bool {
	_, pattern := anonymousMux.Handler(r)
	switch {
	case pattern == "":
		return false
	case pattern == prettyRoute:
		switch r.Method + " " + anonymousService(r.URL.Path) {
		case "GET info/refs":
			return fetchRefs(r)
		case "POST " + UploadPackService:
			return true
		}
		return false
	case strings.HasSuffix(pattern, "/info/refs"):
		return fetchRefs(r)
	}
	return true
}

// fetchRefs reports whether an info/refs is the fetch half. The service
// travels in the query, and only git-upload-pack is a read. A push asks
// for git-receive-pack and gets the 401 with the Basic challenge that
// makes git prompt for a credential.
func fetchRefs(r *http.Request) bool {
	return r.URL.Query().Get("service") == UploadPackService
}

// anonymousService is the {service...} segment of the pretty form: the
// path with its owner and slug segments removed. The mux matched the
// pattern but did not populate the request's path values, because the
// request is routed by the application mux and only matched here.
func anonymousService(path string) string {
	rest := strings.TrimPrefix(path, "/")
	for range 2 {
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			return ""
		}
		rest = rest[i+1:]
	}
	return rest
}
