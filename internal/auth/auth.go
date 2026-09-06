// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package auth guards the public listener.
//
// Phase 1 stand-in: one static bearer read from ORIGO_DEV_TOKEN is the
// whole of authentication, and every caller it admits is one subject.
// Spec 007 replaces this file with OIDC verification, the consumer's
// authorizer, and delegation; the request contract it implements, a
// Bearer header or git's basic auth with the token as the password, is
// the one spec 003 fixes and does not change.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// DevSubject is the subject every request carries in phase 1.
const DevSubject = "dev"

type subjectKey struct{}

// Subject reports the authenticated subject of a request, or "".
func Subject(ctx context.Context) string {
	s, _ := ctx.Value(subjectKey{}).(string)
	return s
}

// WithSubject returns a context carrying the subject; handlers' tests
// use it.
func WithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectKey{}, subject)
}

// StaticBearer admits exactly one token.
type StaticBearer struct {
	Token string
	// Deny writes the refusal. It receives the code from spec 003's
	// error table, always "unauthenticated" here.
	Deny func(w http.ResponseWriter, r *http.Request, code, message string)
}

// Middleware refuses every request that does not carry the token.
func (s *StaticBearer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" || !s.admit(r) {
			// git prompts for credentials on a Basic challenge; a JSON
			// client reads the envelope.
			w.Header().Set("WWW-Authenticate", `Basic realm="origo"`)
			s.Deny(w, r, "unauthenticated", "a bearer token is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), DevSubject)))
	})
}

func (s *StaticBearer) admit(r *http.Request) bool {
	if raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return equal(strings.TrimSpace(raw), s.Token)
	}
	if user, pass, ok := r.BasicAuth(); ok {
		// Any username with the token as the password, or the token as
		// the username with an empty password.
		if pass == "" {
			return equal(user, s.Token)
		}
		return equal(pass, s.Token)
	}
	return false
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
