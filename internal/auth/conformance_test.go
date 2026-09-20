// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/json"
	"net/http"
	"testing"

	"latere.ai/x/pkg/authkit"
	"latere.ai/x/pkg/authkit/conformance"
)

// identity adapts the node's verifier to the family's
// authkit.Authenticator, the shape the shared audience suite speaks. Two
// steps and no third: Credential and Verify are the pair Middleware
// calls on every request, so the verdict is the node's own, and the
// verified payload is read into authkit.Identity by authkit's claim
// names. The node reads none of those claims itself, because it renders
// a subject and forwards the payload to the authorizer verbatim (spec
// 028), so the mapping here is the family's and not one this test
// invented.
type identity struct{ v *Verifier }

func (a identity) Authenticate(r *http.Request) (authkit.Identity, error) {
	p, err := a.v.Verify(r.Context(), Credential(r))
	if err != nil {
		return authkit.Identity{}, err
	}
	payload, err := json.Marshal(p.Claims)
	if err != nil {
		return authkit.Identity{}, err
	}
	var id authkit.Identity
	if err := json.Unmarshal(payload, &id); err != nil {
		return authkit.Identity{}, err
	}
	return id, nil
}

// installed is ORIGO_OIDC_AUDIENCE as deploy/prod sets it (spec 029):
// the core's own name first, the platform origin the node is published
// at second. The suite runs once per entry over a verifier holding both,
// so each name is proved to be a whole audience and not a string the
// other one carries.
var installed = []string{DefaultAudience, "api.latere.ai"}

// TestConformance runs the family's rule R2 against the verifier origod
// installs in production: a token addressed to ORIGO_OIDC_AUDIENCE is
// admitted and yields one Identity, a token addressed to the issuer
// itself or to another service is refused, a token that names nobody is
// refused, the issuer is called for its key set alone, and the retired
// is_superadmin flag grants no platform role. The verifier is built with
// the same NewVerifier the node calls, carrying the local issuer and its
// key beside the issuer list, because one verifier serves both and the
// suite must run against that one. The suite hands the constructor a
// stub issuer, so the test needs nothing running.
//
// The second run is spec 029's second row: a node that answers at
// api.latere.ai admits that audience under every rule the first one is
// held to, and refuses another-service under both. Widening the set
// widens nothing else.
func TestConformance(t *testing.T) {
	for _, audience := range installed {
		t.Run(audience, func(t *testing.T) {
			conformance.Run(t, conformance.Service{
				Audience: audience,
				New: func(tb testing.TB, issuerURL, _ string) authkit.Authenticator {
					v, err := NewVerifier(VerifierOptions{
						Issuers: []string{issuerURL}, Audiences: installed,
						LocalIssuer: localIssuer, LocalKey: &newKey(tb).PublicKey,
						Client: testClient(),
					})
					if err != nil {
						tb.Fatal(err)
					}
					return identity{v}
				},
			})
		})
	}
}
