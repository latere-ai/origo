// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Error is a refusal from Origo, carrying the status and the code Origo's
// error document names. The message Origo sends is deliberately not kept:
// it is written for a developer reading an API, and every sentence a person
// reads on a screen of this service is written for a person.
type Error struct {
	Status int
	Code   string
	cause  error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("origo: %s: %v", e.Code, e.cause)
	}
	return fmt.Sprintf("origo: %d %s", e.Status, e.Code)
}

func (e *Error) Unwrap() error { return e.cause }

// As is [errors.As], named here so a caller of this package needs no second
// import to read a refusal.
func As(err error, target any) bool { return errors.As(err, target) }

// Unauthenticated reports that Origo refused the credential. The caller
// clears the session and sends the person to sign in once.
func Unauthenticated(err error) bool { return statusIs(err, http.StatusUnauthorized) }

// Absent reports that the repository, reference, or object is not there or
// that the reader may not see it. Origo answers 403 and 404 to exactly the
// same question and refuses to say which, so this predicate refuses too, and
// one sentence covers both.
func Absent(err error) bool {
	return statusIs(err, http.StatusForbidden) ||
		statusIs(err, http.StatusNotFound) ||
		statusIs(err, http.StatusGone)
}

// TooLarge reports that Origo would not serve the object because of its size.
func TooLarge(err error) bool { return statusIs(err, http.StatusRequestEntityTooLarge) }

// Unavailable reports that the installation could not answer: it is
// unreachable, its storage is degraded, or its authorizer is not answering.
func Unavailable(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == 0 || e.Status >= 500
}

func statusIs(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// readError turns a refusal into an Error, reading the code from Origo's
// error document when there is one.
func readError(resp *http.Response) *Error {
	e := &Error{Status: resp.StatusCode}
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&doc); err == nil {
		e.Code = doc.Error.Code
		if e.Code == "" {
			e.Code = doc.Code
		}
	}
	if e.Code == "" {
		e.Code = http.StatusText(resp.StatusCode)
	}
	return e
}
