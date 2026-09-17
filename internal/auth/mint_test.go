// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
)

func TestParseKeyReadsTheFormsOpensslWrites(t *testing.T) {
	key := newKey(t)
	sec1, _ := x509.MarshalECPrivateKey(key)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	for name, text := range map[string]string{
		"sec1":            string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})),
		"ecparam -genkey": string(params) + string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})),
		"pkcs8":           string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
	} {
		got, err := ParseKey(text)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384der, _ := x509.MarshalECPrivateKey(p384)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsaDer, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	for name, text := range map[string]string{
		"garbage":     "not a key",
		"params only": string(params),
		"p384":        string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: p384der})),
		"rsa pkcs8":   string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDer})),
		"bad sec1":    string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1}})),
		"bad pkcs8":   string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}})),
	} {
		if _, err := ParseKey(text); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if kid := KeyID(&key.PublicKey); !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(kid) {
		t.Fatalf("kid %q", kid)
	}
}

func TestSignerMintsAndServesItsKey(t *testing.T) {
	clk := newClock()
	key := newKey(t)
	s := NewSigner(key, localIssuer+"/", "", clk.Now)
	if s.KID() != KeyID(&key.PublicKey) || !s.Public().Equal(&key.PublicKey) {
		t.Fatal("kid or public key")
	}
	tok, exp, err := s.Mint(Principal{Subject: "alice"}, repoA, ScopeWrite, 90*time.Second)
	if err != nil || !exp.Equal(clk.Now().Add(90*time.Second)) {
		t.Fatalf("%v %v", exp, err)
	}
	header, err := jwt.ParseHeader(tok)
	if err != nil {
		t.Fatal(err)
	}
	var c Claims
	if err := jwt.DecodePayload(tok, &c); err != nil {
		t.Fatal(err)
	}
	var extra struct {
		Iat *float64        `json:"iat"`
		JTI string          `json:"jti"`
		Act json.RawMessage `json:"act"`
	}
	if err := jwt.DecodePayload(tok, &extra); err != nil {
		t.Fatal(err)
	}
	if header.Alg != "ES256" || header.KID != s.KID() || c.Iss != localIssuer || !c.HasAudience(DefaultAudience) || c.Sub != "alice" || extra.Act != nil || c.Repo != repoA || c.Scope != "write" {
		t.Fatalf("claims: %+v %+v", header, c)
	}
	if extra.Iat == nil || *extra.Iat != float64(clk.Now().Unix()) || c.Exp != float64(exp.Unix()) {
		t.Fatalf("times: %v %v", extra.Iat, c.Exp)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(extra.JTI) {
		t.Fatalf("jti %q", extra.JTI)
	}
	tok2, _, _ := s.Mint(Principal{Subject: "bob"}, repoA, ScopeRead, time.Hour)
	var c2 Claims
	if err := jwt.DecodePayload(tok2, &c2); err != nil {
		t.Fatal(err)
	}
	if c2.Sub != "bob" || c2.Scope != "read" {
		t.Fatalf("read token: %+v", c2)
	}
	// The JWKS verifies what the signer minted: the key set the node
	// serves, read back through the shared verifier as any client reads
	// it, names the same subject as the minter had.
	rec := httptest.NewRecorder()
	s.JWKS().ServeHTTP(rec, httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	keys, err := parseJWKS(rec.Body.Bytes())
	if err != nil || len(keys) != 1 || keys[s.KID()] == nil || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("jwks: %s, %v", rec.Body.String(), err)
	}
	served := jwt.New(jwt.Config{
		LocalIssuer: localIssuer, LocalKey: keys[s.KID()], LocalKeyID: s.KID(),
		Audiences: []string{DefaultAudience}, Now: clk.Now,
	})
	if _, err := served.Validate(tok); err != nil {
		t.Fatalf("the published key does not verify what the signer minted: %v", err)
	}
	v := newVerifier(t, clk, key)
	p, err := v.Verify(context.Background(), tok)
	if err != nil || p.Subject != "alice" || p.Bound.Repo != repoA || p.Bound.Scope != ScopeWrite {
		t.Fatalf("verified: %+v, %v", p, err)
	}
	// The body's limits.
	for name, c := range map[string]struct {
		scope Scope
		ttl   time.Duration
	}{
		"admin scope": {"admin", time.Minute},
		"empty scope": {"", time.Minute},
		"zero ttl":    {ScopeRead, 0},
		"long ttl":    {ScopeRead, MaxTokenTTL + time.Second},
	} {
		if _, _, err := s.Mint(Principal{Subject: "x"}, repoA, c.scope, c.ttl); err == nil {
			t.Errorf("%s: minted", name)
		}
	}
	if MinTokenTTL != time.Second || MaxTokenTTL != time.Hour {
		t.Fatal("the ttl bounds are not spec 007's")
	}
	if u := newUUID(); len(u) != 36 || strings.Count(u, "-") != 4 {
		t.Fatalf("uuid %q", u)
	}
	if s2 := NewSigner(key, localIssuer, "", nil); s2.now == nil {
		t.Fatal("default clock")
	}
}
