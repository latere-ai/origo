// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package contract holds what every response of the public surface
// shares under spec 003: the contract version header, the stable error
// codes, and the one user sentence of each code. The envelope the codes
// travel in is the family's, httpjson.Error, rendered by
// httpjson.WriteError; Write here fills it from the code table so no
// handler carries a sentence of its own.
package contract

import (
	"net/http"

	"latere.ai/x/pkg/httpjson"
)

// Version is the value of the Origo-Contract header. Additive changes
// keep it; a removal or a semantic change bumps it.
const Version = "1"

// Header is the response header naming the contract version.
const Header = "Origo-Contract"

// The stable error codes: spec 003's table and the codes later specs add
// to it.
const (
	CodeUnauthenticated       = "unauthenticated"
	CodeForbidden             = "forbidden"
	CodeRepoNotFound          = "repo_not_found"
	CodeRepoExists            = "repo_exists"
	CodeRefNotFound           = "ref_not_found"
	CodeNonFastForward        = "non_fast_forward"
	CodeOverQuota             = "over_quota"
	CodeRateLimited           = "rate_limited"
	CodeStorageUnavailable    = "storage_unavailable"
	CodeInvalid               = "invalid_request"
	CodeAuthorizerUnavailable = "authorizer_unavailable" // spec 007
	CodeBlobTooLarge          = "blob_too_large"         // spec 009
	CodeOperationTimeout      = "operation_timeout"      // spec 009
	CodeLFSObjectMismatch     = "lfs_object_mismatch"    // spec 010
	CodeLFSObjectNotStored    = "lfs_object_not_stored"  // spec 010
	CodeLFSLocksUnsupported   = "lfs_locks_unsupported"  // spec 010
	CodeRepositoryUnavailable = "repository_unavailable" // spec 015
)

// sentences is the code table: one user sentence per code, the text of
// the Message column of the owning spec. Everything a developer needs
// goes in details, never here.
var sentences = map[string]string{
	CodeInvalid:               "The request is malformed.",
	CodeUnauthenticated:       "A bearer token is required.",
	CodeForbidden:             "You do not have permission to do this.",
	CodeRepoNotFound:          "Repository not found.",
	CodeRefNotFound:           "The reference or object does not exist in this repository.",
	CodeRepoExists:            "A repository with this id or name already exists.",
	CodeNonFastForward:        "The reference moved since you fetched. Fetch, then push again.",
	CodeOverQuota:             "The request exceeds this repository's storage limit.",
	CodeRateLimited:           "Too many requests. Wait and try again.",
	CodeStorageUnavailable:    "The repository is temporarily unavailable. Nothing was lost. Try again in a few minutes.",
	CodeAuthorizerUnavailable: "Permissions cannot be checked right now. Nothing was lost. Try again in a few minutes.",
	CodeBlobTooLarge:          "This file is larger than 50 MiB. Request it in ranges of at most 50 MiB.",
	CodeOperationTimeout:      "The operation took too long and nothing was changed.",
	CodeLFSObjectMismatch:     "The uploaded object does not match its declared size.",
	CodeLFSObjectNotStored:    "This object is not stored.",
	CodeLFSLocksUnsupported:   "Locking is not supported.",
	CodeRepositoryUnavailable: "This repository cannot be served until an operator restores it. Other repositories are not affected.",
}

// Sentence is the one user sentence of a code. A code without a sentence
// is a programming error: the table is the contract, so it panics rather
// than inventing text.
func Sentence(code string) string {
	s, ok := sentences[code]
	if !ok {
		panic("contract: no sentence for code " + code)
	}
	return s
}

// Codes lists every code of the table, for a test that checks the table
// against the specs.
func Codes() []string {
	out := make([]string, 0, len(sentences))
	for c := range sentences {
		out = append(out, c)
	}
	return out
}

// Error builds the envelope body of a code: its sentence and the
// developer details, which are omitted when empty.
func Error(code string, details map[string]any) httpjson.Error {
	e := httpjson.Error{Code: code, Message: Sentence(code)}
	if len(details) > 0 {
		e.Details = details
	}
	return e
}

// Write sends the envelope of a code with the status and the details.
func Write(w http.ResponseWriter, status int, code string, details map[string]any) {
	httpjson.WriteError(w, status, Error(code, details))
}

// Middleware stamps the contract version on every response.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(Header, Version)
		next.ServeHTTP(w, r)
	})
}
