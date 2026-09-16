// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/jwt"

	"github.com/latere-ai/origo/test/stubs/issuer"
)

// Spec 028's design moves the verifier to latere.ai/x/pkg/authkit/jwt.
// The `verifier` waiver of .lateregate.yaml is why it has not moved, and
// this test is that waiver written as an assertion: it holds the two
// things the shared package cannot do together, so the waiver ends by a
// red test rather than by a date.
//
// Spec 007's verification table asks for both at once: a token's `kid`
// must name a key of the issuer's set, else `unknown_key`, and an
// issuer's `exp` and `nbf` carry 60 seconds of skew. authkit/jwt v0.71.0
// offers one path with each:
//
//	               names the key strictly   carries ClockSkew
//	LocalIssuer              yes                   no
//	JWKS                     no                    yes
//
// Neither column is spec 007's row, and a caller cannot compose them,
// because Config.LocalKey takes one key and the node has an issuer's set:
// picking the key of a `kid` needs the JOSE header, which the package
// decodes and does not hand back. DecodePayload reads the payload alone.
//
// Four smaller gaps are not here, because Origo can close each on its own
// side once the two above close: an empty `sub` reads `malformed` where
// spec 007 says `subject`; `act` and the reasons `missing`,
// `issuer_unavailable` and `unknown_key` are Origo's words for rows the
// package does not carry; `aud` is checked after `exp`, where spec 007
// checks it before; and Claims.Iss comes back with its trailing slash.
//
// When this test reds, read the row it names: the shared verifier gained
// what spec 007 asks, and internal/auth moves onto it.
func TestTheSharedVerifierCannotCarrySpec007(t *testing.T) {
	// The JWKS path carries the skew, and does not name the key
	// strictly: a token signed by the issuer whose `kid` names no key of
	// the published set verifies, where spec 007's `kid` row refuses it
	// with unknown_key.
	iss := issuer.New(t)
	jwks := jwt.New(jwt.Config{
		JWKSURL: iss.JWKSURL(), Issuer: iss.URL(), Audiences: []string{DefaultAudience},
		ClockSkew: ClockSkew, MaxTokenBytes: MaxTokenBytes, MaxTokenAge: MaxTokenAge,
		RequireIssuedAt: true,
	})
	if _, err := jwks.Validate(iss.Mint(issuer.Claims{Sub: "alice"})); err != nil {
		t.Fatalf("the JWKS path must verify a plain token: %v", err)
	}
	if _, err := jwks.Validate(iss.Mint(issuer.Claims{Sub: "alice", Kid: "nope"})); err == nil {
		t.Log("the JWKS path still tries every key when the kid names none")
	} else {
		t.Errorf("the JWKS path names the key strictly now (%v): with the skew it already "+
			"carries, spec 007's kid and exp rows both hold, so move the verifier", jwt.ReasonOf(err))
	}
	// And it does carry the skew: 59 seconds past exp is read.
	now := time.Now()
	if _, err := jwks.Validate(iss.Mint(issuer.Claims{Sub: "alice", Exp: now.Add(-59 * time.Second).Unix()})); err != nil {
		t.Errorf("the JWKS path must carry ClockSkew: %v", err)
	}

	// The local-issuer path names the key strictly, and zeroes the skew
	// whatever Config.ClockSkew says, so it cannot verify an issuer's
	// token under spec 007's exp and nbf rows.
	key := newKey(t)
	local := issuer.New(t, issuer.WithKey(key), issuer.WithIssuer(localIssuer))
	one := jwt.New(jwt.Config{
		LocalIssuer: localIssuer, LocalKey: &key.PublicKey, LocalKeyID: local.KID(),
		Audiences: []string{DefaultAudience}, ClockSkew: ClockSkew,
		MaxTokenBytes: MaxTokenBytes, MaxTokenAge: MaxTokenAge, RequireIssuedAt: true,
	})
	if _, err := one.Validate(local.Mint(issuer.Claims{Sub: "ci"})); err != nil {
		t.Fatalf("the local path must verify a plain token: %v", err)
	}
	if _, err := one.Validate(local.Mint(issuer.Claims{Sub: "ci", Exp: now.Add(-59 * time.Second).Unix()})); jwt.ReasonOf(err) == "expired" {
		t.Log("the local path still zeroes ClockSkew")
	} else {
		t.Errorf("the local path carries ClockSkew now (%v): with the strict kid it already "+
			"has, spec 007's kid and exp rows both hold, so move the verifier", err)
	}
	// It is strict, and the strictness reads as a signature: the row
	// Origo answers with unknown_key.
	if got := jwt.ReasonOf(mustFail(t, one, local.Mint(issuer.Claims{Sub: "ci", Kid: "nope"}))); got != jwt.ReasonBadSignature {
		t.Errorf("a local kid that names no key: %q", got)
	}
	// A key mismatch under the node's own kid is a signature too, so the
	// two rows Origo tells apart are one word here.
	forged := issuer.New(t, issuer.WithKey(newKey(t)), issuer.WithIssuer(localIssuer))
	if got := jwt.ReasonOf(mustFail(t, one, forged.Mint(issuer.Claims{Sub: "ci", Kid: local.KID()}))); got != jwt.ReasonBadSignature {
		t.Errorf("a forged local token: %q", got)
	}
}

// mustFail validates a token that must not verify and returns the refusal.
func mustFail(t *testing.T, v *jwt.Validator, token string) error {
	t.Helper()
	c, err := v.Validate(token)
	if err == nil {
		t.Fatalf("the token verified as %q", c.Sub)
	}
	return err
}
