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

import (
	"context"

	"latere.ai/x/pkg/authz"
)

// Caller is what the authorizer learns about the request itself: the
// node's request id, the peer address, and the user agent (Origo spec
// 028). It is the shared package's type, carried on the request context
// so the guard reads it whether the request arrived over HTTP or SSH.
type Caller = authz.Caller

type callerKey struct{}

// WithCaller returns a context carrying the caller block; the HTTP
// middleware and the SSH server set it.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFromContext reports the caller block of a request, or the zero
// value when none was set.
func CallerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// Principal is the verified caller of a request: the subject, and the
// repository the token is bound to when Origo minted it. A token names
// one caller and nobody else: a service that acts for a person presents
// the token its issuer minted for that person (the family's one hop),
// and the verifier refuses a token that carries a delegation claim.
type Principal struct {
	// Subject is who the call is for, rendered as the authorizer contract
	// names it: <iss>|<sub> for an issuer's token, and the sub alone for a
	// repository-bound token the node minted, whose sub is already a
	// rendered subject (Origo spec 028). It is what an entry header, an
	// event, and origo's output record from contract 2 on.
	Subject string
	// Issuer is the token's iss, trailing slash removed, and Sub its bare
	// sub. They travel in the authorizer envelope apart from the rendered
	// Subject so a policy reads either. Empty for the anonymous principal.
	Issuer string
	Sub    string
	// Claims is every verified claim of the token, verbatim, forwarded to
	// the authorizer (Origo spec 028); the node reads none of it. Nil for
	// the anonymous principal.
	Claims map[string]any
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

// Subject reports the subject of a request, or "".
func Subject(ctx context.Context) string { return FromContext(ctx).Subject }
