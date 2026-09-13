// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package issuer is the stub OIDC issuer of spec 013, as Origo's tests use
// it: the family's latere.ai/x/pkg/authkit/issuertest with Origo's
// defaults, ES256 keys and the audience "origo". The stub itself, its
// control API and its /actor-tokens hop live in the shared package, so
// every repository's tests and the conformance suite run one issuer.
//
// It serves plain HTTP. A node that reaches it on a host other than a
// loopback address lists it in ORIGO_OIDC_INSECURE_ISSUERS.
package issuer

import (
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
)

// The shared stub's types, under the names Origo's tests have used.
type (
	Server     = issuertest.Server
	Claims     = issuertest.Claims
	StringList = issuertest.StringList
	Option     = issuertest.Option
)

// DefaultAudience is the aud a minted token carries when Claims names none.
const DefaultAudience = "origo"

// The shared stub's options, re-exported.
var (
	WithIssuer = issuertest.WithIssuer
	WithKey    = issuertest.WithKey
	WithRS256  = issuertest.WithRS256
	WithClock  = issuertest.WithClock
)

// origoDefaults are applied before the caller's options, so a caller can
// still ask for RS256 or another audience.
func origoDefaults(opts []Option) []Option {
	return append([]Option{issuertest.WithES256(), issuertest.WithDefaultAudience(DefaultAudience)}, opts...)
}

// New starts a stub on a loopback listener and closes it when the test ends.
func New(t testing.TB, opts ...Option) *Server { return issuertest.New(t, origoDefaults(opts)...) }

// NewHandler builds a stub without a listener, for the origo-stubs binary.
func NewHandler(opts ...Option) *Server { return issuertest.NewHandler(origoDefaults(opts)...) }
