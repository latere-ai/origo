// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package auth is spec 007: who is calling and whether they may. The
// Verifier checks a token against the configured OIDC issuers or the
// node's own key, Middleware puts the Principal it names on the request,
// the Client asks the consumer's authorizer and caches its answers, the
// Guard decides one request from either the token's scope or the
// authorizer, and the Signer mints the repository-bound tokens the
// authorizer never sees. Origo stores no user and no permission.
package auth

import "context"

// Principal is the verified caller of a request: the effective subject,
// the actor when the call is made on someone's behalf, and the
// repository the token is bound to when Origo minted it.
type Principal struct {
	// Subject is who the call is for: sub, or act when the token carries
	// one.
	Subject string
	// Actor is who made the call on Subject's behalf: sub when act is
	// present, empty otherwise.
	Actor string
	// Bound is set on a repository-bound token.
	Bound *Bound
}

// Bound is what a repository-bound token allows: one repository and one
// scope.
type Bound struct {
	Repo  string
	Scope Scope
}

// Scope is the scope of a repository-bound token.
type Scope string

// The scopes spec 007 mints.
const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
)

type principalKey struct{}

// FromContext reports the principal of a request, or the zero value.
func FromContext(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// WithPrincipal returns a context carrying the principal; the
// middleware sets it and handlers' tests use it.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Subject reports the effective subject of a request, or "".
func Subject(ctx context.Context) string { return FromContext(ctx).Subject }

// Actor reports the actor of a request, or "".
func Actor(ctx context.Context) string { return FromContext(ctx).Actor }
