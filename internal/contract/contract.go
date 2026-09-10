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
	"slices"
	"sort"

	"latere.ai/x/pkg/httpjson"
)

// Version is the value of the Origo-Contract header. Additive changes
// keep it; a removal or a semantic change bumps it.
const Version = "1"

// Header is the response header naming the contract version.
const Header = "Origo-Contract"

// HeaderStale is the header of a response served from the local copy
// without a currency check while the bucket is unreachable (spec 015):
// the whole seconds since the last check that answered; absent on every
// consistent response, so a consumer that must not read stale refuses
// the response by this header.
const HeaderStale = "Origo-Stale"

// HeaderRateLimit is the requests one effective subject may send a node
// in a minute (spec 012), the RateLimit-Limit field of the IETF draft
// "RateLimit header fields for HTTP"
// (draft-ietf-httpapi-ratelimit-headers). It is on every response of
// the rate-limited surface, so a client reads the figure in force
// before it meets it; absent when the limit is off.
const HeaderRateLimit = "RateLimit-Limit"

// HeaderRateRemaining is the tokens left in the effective subject's
// bucket on the node that answered, rounded down (spec 012), the
// RateLimit-Remaining field of the same IETF draft. It is on every
// response HeaderRateLimit is on and absent wherever that one is, so a
// client reads what it has left rather than counting its own requests,
// and spec 021's rate_limited case reads the limit in force without
// exhausting it.
const HeaderRateRemaining = "RateLimit-Remaining"

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
	CodeGone                  = "gone"                   // spec 019
	CodeRepoFrozen            = "repo_frozen"            // spec 019
	CodeRepoImporting         = "repo_importing"         // spec 019
	CodeRepoNotEmpty          = "repo_not_empty"         // spec 019
	CodeImportNotFound        = "import_not_found"       // spec 019
	CodeMergeConflict         = "merge_conflict"         // spec 020
	CodeInvalidChange         = "invalid_change"         // spec 020
	CodeDirectoryUnsupported  = "directory_unsupported"  // spec 026
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
	CodeGone:                  "This repository was deleted and its hold has passed. It cannot be restored.",
	CodeRepoFrozen:            "This repository is frozen and does not accept pushes.",
	CodeRepoImporting:         "This repository is importing and does not accept pushes until the import finishes.",
	CodeRepoNotEmpty:          "This repository already has history; import into an empty repository.",
	CodeImportNotFound:        "No import has been started for this repository.",
	CodeMergeConflict:         "The change conflicts with the branch. Resolve it in a clone and push.",
	CodeInvalidChange:         "A change in the request is not valid.",
	CodeDirectoryUnsupported:  "This installation does not list repositories.",
}

// statuses is the other half of the code table: every HTTP status the
// Status column of the owning spec's Code table lists for the code, in
// the column's order. Most codes have one; repo_frozen has two (403 on
// a write, 409 on a second freeze, spec 019); non_fast_forward, whose
// column names the sideband and one JSON status, holds the JSON status;
// invalid_request carries 416 beside 400 for the one refusal spec 009's
// blob endpoint answers with Content-Range, a Range past the end of the
// blob. A call site sends a code under one of its statuses and nothing
// else, which TestEveryCodeHasOneSentence holds.
var statuses = map[string][]int{
	CodeInvalid:               {http.StatusBadRequest, http.StatusRequestedRangeNotSatisfiable},
	CodeUnauthenticated:       {http.StatusUnauthorized},
	CodeForbidden:             {http.StatusForbidden},
	CodeRepoNotFound:          {http.StatusNotFound},
	CodeRefNotFound:           {http.StatusNotFound},
	CodeRepoExists:            {http.StatusConflict},
	CodeNonFastForward:        {http.StatusConflict},
	CodeOverQuota:             {http.StatusRequestEntityTooLarge},
	CodeRateLimited:           {http.StatusTooManyRequests},
	CodeStorageUnavailable:    {http.StatusServiceUnavailable},
	CodeAuthorizerUnavailable: {http.StatusServiceUnavailable},
	CodeBlobTooLarge:          {http.StatusRequestEntityTooLarge},
	CodeOperationTimeout:      {http.StatusGatewayTimeout},
	CodeLFSObjectMismatch:     {http.StatusUnprocessableEntity},
	CodeLFSObjectNotStored:    {http.StatusNotFound},
	CodeLFSLocksUnsupported:   {http.StatusNotImplemented},
	CodeRepositoryUnavailable: {http.StatusServiceUnavailable},
	CodeGone:                  {http.StatusGone},
	CodeRepoFrozen:            {http.StatusForbidden, http.StatusConflict},
	CodeRepoImporting:         {http.StatusConflict},
	CodeRepoNotEmpty:          {http.StatusConflict},
	CodeImportNotFound:        {http.StatusNotFound},
	CodeMergeConflict:         {http.StatusConflict},
	CodeInvalidChange:         {http.StatusBadRequest},
	CodeDirectoryUnsupported:  {http.StatusNotImplemented},
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

// Status is the first HTTP status of a code's row. Like Sentence it
// panics on a code the table lacks.
func Status(code string) int {
	s, ok := statuses[code]
	if !ok || len(s) == 0 {
		panic("contract: no status for code " + code)
	}
	return s[0]
}

// Statuses is every status of a code's row, in the order the owning
// spec lists them, for the test that holds every call site to the row.
func Statuses(code string) []int {
	return slices.Clone(statuses[code])
}

// Codes lists every code of the table, sorted, for a test that checks
// the table against the specs.
func Codes() []string {
	out := make([]string, 0, len(sentences))
	for c := range sentences {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Line is the form of a code on git's sideband, in an ERR pkt-line, and
// in a hook verdict (spec 021): the code, a colon, a space, and the
// sentence, with nothing appended or substituted.
func Line(code string) string {
	return code + ": " + Sentence(code)
}

// Refusal is one row's answer prepared away from the response writer:
// the status, the code, and the developer details, for a handler that
// decides a refusal in one place and renders it in another. Refuse
// builds one from a Code constant and a status of its row, so the call
// site the table test reads is where the code is chosen.
type Refusal struct {
	Status  int
	Code    string
	Details map[string]any
}

// Refuse prepares the envelope of a code under a status of its row.
func Refuse(status int, code string, details map[string]any) Refusal {
	return Refusal{Status: status, Code: code, Details: details}
}

// Sentence is the user sentence of the refusal's code.
func (r Refusal) Sentence() string { return Sentence(r.Code) }

// Line is the refusal's sideband form, Line of its code.
func (r Refusal) Line() string { return Line(r.Code) }

// Write sends the refusal's envelope.
func (r Refusal) Write(w http.ResponseWriter) { Write(w, r.Status, r.Code, r.Details) }

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
