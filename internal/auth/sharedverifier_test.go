// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/jwt"

	"github.com/latere-ai/origo/test/stubs/issuer"
)

// Spec 028's design moves the verifier to latere.ai/x/pkg/authkit/jwt.
// The `verifier` waiver of .lateregate.yaml is why it has not moved, and
// this test is that waiver written as an assertion: it holds what the
// shared package cannot do, so the waiver ends by a red test rather than
// by a date.
//
// One rule closed in v0.73.0 and is asserted below rather than waived:
// a `kid` that names no key of the set is `unknown_key` on both paths,
// where v0.72.0's JWKS path tried every key in turn and its local path
// called the miss a signature. That was the row the waiver named, and
// it is a row no longer.
//
// Three hold the verifier here now, and none is composable away:
//
//   - The package reads its own clock. `Validate` calls `time.Now` and
//     `jwt.Config` takes no clock, so Origo cannot hand it the clock its
//     verifier runs on. Every row of spec 007's table that reads a time,
//     `exp`, `nbf` and `iat`, is therefore untestable on a clock a test
//     moves, and those three rows are most of what a move would carry.
//   - The package's discovery does not run OIDC Discovery 4.3. Spec 007
//     reads `<iss>/.well-known/openid-configuration` and then checks the
//     document's `issuer` equals the URL it was fetched under, so a
//     document naming another issuer fails the fetch. The package
//     follows the `jwks_uri` of whatever document answers.
//   - An issuer the node cannot reach has no word. Spec 007 refuses its
//     tokens with `issuer_unavailable` until one fetch succeeds; the
//     package wraps the fetch error unclassified, so `jwt.ReasonOf`
//     reads the empty string and a caller cannot tell an unreachable
//     issuer from anything else.
//
// Smaller gaps are not here, because Origo can close each on its own
// side once the three above close: an empty `sub` reads `malformed`
// where spec 007 says `subject`; `missing` and `delegation` are Origo's
// words for rows the package does not carry; `aud` is checked after
// `exp`, where spec 007 checks it before; and Claims.Iss comes back with
// its trailing slash.
//
// When this test reds, read the row it names: the shared verifier gained
// what spec 007 asks, and internal/auth moves onto it.
func TestTheSharedVerifierCannotCarrySpec007(t *testing.T) {
	t.Run("the kid names the key, which is the row that closed", theKidNamesTheKey)
	t.Run("the package reads its own clock", thePackageReadsItsOwnClock)
	t.Run("discovery does not check the issuer", discoveryDoesNotCheckTheIssuer)
	t.Run("an unreachable issuer has no word", anUnreachableIssuerHasNoWord)
}

// theKidNamesTheKey is spec 007's `kid` row, carried by v0.73.0 on both
// paths. It is asserted and not waived: a regression here puts the row
// back among the reasons the waiver names.
func theKidNamesTheKey(t *testing.T) {
	iss := issuer.New(t, issuer.WithClock(time.Now))
	jwks := jwt.New(sharedConfig(jwt.Config{Issuers: []string{iss.URL()}}))
	if _, err := jwks.Validate(iss.Mint(issuer.Claims{Sub: "alice"})); err != nil {
		t.Fatalf("the JWKS path must verify a plain token: %v", err)
	}
	if got := jwt.ReasonOf(mustNotVerify(t, jwks, iss.Mint(issuer.Claims{Sub: "alice", Kid: "nope"}))); got != jwt.ReasonUnknownKey {
		t.Errorf("an issuer's kid naming no key of the set: %q, want %q", got, jwt.ReasonUnknownKey)
	}

	key := newKey(t)
	local := issuer.New(t, issuer.WithKey(key), issuer.WithIssuer(localIssuer), issuer.WithClock(time.Now))
	one := jwt.New(sharedConfig(jwt.Config{
		LocalIssuer: localIssuer,
		LocalKeys:   []jwt.LocalKey{{KeyID: local.KID(), Key: &key.PublicKey}},
	}))
	if _, err := one.Validate(local.Mint(issuer.Claims{Sub: "ci"})); err != nil {
		t.Fatalf("the local path must verify a plain token: %v", err)
	}
	if got := jwt.ReasonOf(mustNotVerify(t, one, local.Mint(issuer.Claims{Sub: "ci", Kid: "nope"}))); got != jwt.ReasonUnknownKey {
		t.Errorf("a local kid naming no key of the set: %q, want %q", got, jwt.ReasonUnknownKey)
	}
	// The key is chosen before it is asked, so a forged token under the
	// node's own kid is still a signature and not the other word: the two
	// rows Origo tells apart stay apart.
	forged := issuer.New(t, issuer.WithKey(newKey(t)), issuer.WithIssuer(localIssuer), issuer.WithClock(time.Now))
	if got := jwt.ReasonOf(mustNotVerify(t, one, forged.Mint(issuer.Claims{Sub: "ci", Kid: local.KID()}))); got != jwt.ReasonBadSignature {
		t.Errorf("a forged local token: %q, want %q", got, jwt.ReasonBadSignature)
	}
}

// thePackageReadsItsOwnClock is the first blocker: jwt.Config carries no
// clock, and Validate reads time.Now, so a token minted on the clock
// Origo's verifier runs on is expired to the shared one. Spec 007's
// `exp`, `nbf` and `iat` rows cannot move while that holds.
func thePackageReadsItsOwnClock(t *testing.T) {
	for _, f := range reflect.VisibleFields(reflect.TypeFor[jwt.Config]()) {
		if f.Type == reflect.TypeFor[func() time.Time]() {
			t.Errorf("jwt.Config.%s takes a clock now: spec 007's exp, nbf and iat rows "+
				"can be read on Origo's clock, so move the verifier", f.Name)
		}
	}
	clk := newClock()
	iss := issuer.New(t, issuer.WithClock(clk.Now))
	v := jwt.New(sharedConfig(jwt.Config{Issuers: []string{iss.URL()}}))
	// The token is valid on the clock that minted it, which is the clock
	// Origo's verifier is given.
	tok := iss.Mint(issuer.Claims{Sub: "alice"})
	if got := jwt.ReasonOf(mustNotVerify(t, v, tok)); got != jwt.ReasonExpired {
		t.Errorf("a token minted on Origo's clock: %q, want %q", got, jwt.ReasonExpired)
	}
}

// discoveryDoesNotCheckTheIssuer is the second blocker: spec 007 fails
// the fetch when the discovery document names an issuer other than the
// URL it was fetched under (OIDC Discovery 4.3), so keys that are not
// the issuer's never verify its tokens. The package follows the
// document's jwks_uri whatever issuer it names.
func discoveryDoesNotCheckTheIssuer(t *testing.T) {
	// A reachable key set, published by an issuer that is not the one the
	// document below is served under.
	key := newKey(t)
	elsewhere := issuer.New(t, issuer.WithKey(key), issuer.WithClock(time.Now))
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": elsewhere.URL(), "jwks_uri": elsewhere.JWKSURL(),
		})
	}))
	t.Cleanup(front.Close)

	v := jwt.New(sharedConfig(jwt.Config{Issuers: []string{front.URL}}))
	// A token naming the front as its issuer, signed by the key the
	// document pointed at. Spec 007 never fetches that key set.
	header, _, _ := strings.Cut(elsewhere.Mint(issuer.Claims{}), ".")
	now := time.Now()
	tok := signClaims(t, key, header, map[string]any{
		"iss": front.URL, "sub": "alice", "aud": DefaultAudience,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if _, err := v.Validate(tok); err == nil {
		t.Log("a document naming another issuer still publishes the keys the package verifies with")
	} else {
		t.Errorf("discovery checks the document's issuer now (%v): spec 007's OIDC 4.3 row "+
			"holds through the package, so move the verifier", jwt.ReasonOf(err))
	}
}

// anUnreachableIssuerHasNoWord is the third blocker: spec 007 refuses an
// unreachable issuer's tokens with issuer_unavailable until one fetch
// succeeds, and the package's fetch failure carries no row of the table.
func anUnreachableIssuerHasNoWord(t *testing.T) {
	down := issuer.New(t, issuer.WithClock(time.Now))
	url, tok := down.URL(), down.Mint(issuer.Claims{Sub: "alice"})
	down.Close()

	v := jwt.New(sharedConfig(jwt.Config{Issuers: []string{url}}))
	err := mustNotVerify(t, v, tok)
	if got := jwt.ReasonOf(err); got == "" {
		t.Logf("an unreachable issuer is still unclassified: %v", err)
	} else {
		t.Errorf("an unreachable issuer reads %q now: spec 007's issuer_unavailable row "+
			"holds through the package, so move the verifier", got)
	}
}

// sharedConfig is the configuration a move would give the shared
// verifier: Origo's values of spec 007, with the caller adding the key
// source under test.
func sharedConfig(c jwt.Config) jwt.Config {
	c.Audiences = []string{DefaultAudience}
	c.ClockSkew = ClockSkew
	c.MaxTokenBytes = MaxTokenBytes
	c.MaxTokenAge = MaxTokenAge
	c.RequireIssuedAt = true
	return c
}

// mustNotVerify validates a token that must not verify and returns the
// refusal.
func mustNotVerify(t *testing.T, v *jwt.Validator, token string) error {
	t.Helper()
	c, err := v.Validate(token)
	if err == nil {
		t.Fatalf("the token verified as %q", c.Sub)
	}
	return err
}
