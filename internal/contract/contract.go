// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package contract holds what every response of the public surface
// shares under spec 003: the contract version header and the stable
// error codes. The envelope the codes travel in is the family's,
// httpjson.Error, rendered by httpjson.WriteError.
package contract

import "net/http"

// Version is the value of the Origo-Contract header. Additive changes
// keep it; a removal or a semantic change bumps it.
const Version = "1"

// Header is the response header naming the contract version.
const Header = "Origo-Contract"

// The stable error codes.
const (
	CodeUnauthenticated    = "unauthenticated"
	CodeForbidden          = "forbidden"
	CodeRepoNotFound       = "repo_not_found"
	CodeRepoExists         = "repo_exists"
	CodeRefNotFound        = "ref_not_found"
	CodeNonFastForward     = "non_fast_forward"
	CodeOverQuota          = "over_quota"
	CodeRateLimited        = "rate_limited"
	CodeStorageUnavailable = "storage_unavailable"
	CodeInvalid            = "invalid_request"
)

// Middleware stamps the contract version on every response.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(Header, Version)
		next.ServeHTTP(w, r)
	})
}
