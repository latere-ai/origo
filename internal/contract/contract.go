// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package contract holds what every response of the public surface
// shares under spec 003: the contract version header and the error
// envelope with its stable codes.
package contract

import (
	"encoding/json"
	"net/http"
)

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

// Envelope is the body of every error response.
type Envelope struct {
	Error Detail `json:"error"`
}

// Detail is the error inside the envelope.
type Detail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// WriteError sends the envelope with the status.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, Envelope{Error: Detail{Code: code, Message: message}})
}

// WriteJSON sends v with the status. A value that cannot be encoded is
// reported as plain text, which only a programming error produces.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Middleware stamps the contract version on every response.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(Header, Version)
		next.ServeHTTP(w, r)
	})
}
