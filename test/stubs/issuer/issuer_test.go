// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package issuer_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/stubs/issuer"
)

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	return out
}

func post(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body)) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// decode splits a token into its header, claims, and the signed bytes
// with the signature.
func decode(t *testing.T, token string) (header, claims map[string]any, signed, sig []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	for i, into := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	return header, claims, []byte(parts[0] + "." + parts[1]), sig
}

// keyOf reads the JWKS and returns the public key named kid.
func keyOf(t *testing.T, s *issuer.Server, kid string) any {
	t.Helper()
	for _, k := range getJSON(t, s.URL()+"/jwks")["keys"].([]any) {
		key := k.(map[string]any)
		if key["kid"] != kid {
			continue
		}
		switch key["kty"] {
		case "EC":
			x, _ := base64.RawURLEncoding.DecodeString(key["x"].(string))
			y, _ := base64.RawURLEncoding.DecodeString(key["y"].(string))
			pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
			if err != nil {
				t.Fatal(err)
			}
			return pub
		case "RSA":
			n, _ := base64.RawURLEncoding.DecodeString(key["n"].(string))
			e, _ := base64.RawURLEncoding.DecodeString(key["e"].(string))
			return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		}
	}
	return nil
}

func verify(t *testing.T, pub any, signed, sig []byte) bool {
	t.Helper()
	digest := sha256.Sum256(signed)
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		return len(sig) == 64 && ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) == nil
	}
	return false
}

func TestServesDiscoveryAndMintsWithDefaults(t *testing.T) {
	fixed := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	s := issuer.New(t, issuer.WithClock(func() time.Time { return fixed }))
	disc := getJSON(t, s.URL()+"/.well-known/openid-configuration")
	if disc["issuer"] != s.URL() || disc["jwks_uri"] != s.URL()+"/jwks" {
		t.Fatalf("discovery: %v", disc)
	}
	header, claims, signed, sig := decode(t, s.Mint(issuer.Claims{}))
	if header["alg"] != "ES256" || header["kid"] != s.KID() || header["typ"] != "JWT" {
		t.Fatalf("header: %v", header)
	}
	aud, _ := claims["aud"].([]any)
	if claims["iss"] != s.URL() || claims["sub"] != issuer.DefaultSubject || len(aud) != 1 || aud[0] != issuer.DefaultAudience {
		t.Fatalf("claims: %v", claims)
	}
	if claims["iat"] != float64(fixed.Unix()) || claims["exp"] != float64(fixed.Add(issuer.DefaultLifetime).Unix()) || claims["nbf"] != nil || claims["act"] != nil {
		t.Fatalf("time claims: %v", claims)
	}
	if !verify(t, keyOf(t, s, s.KID()), signed, sig) {
		t.Fatal("the JWKS key does not verify the token")
	}
	// Every field set, over the control endpoint, with aud as a string.
	status, raw := post(t, s.URL()+"/mint", `{"sub":"svc","act":"alice","aud":"other","exp":1,"nbf":2,"iat":3,"kid":"nope","alg":"HS256"}`)
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil || status != 200 || minted.Token == "" {
		t.Fatalf("POST /mint: %d %s %v", status, raw, err)
	}
	header, claims, _, _ = decode(t, minted.Token)
	aud, _ = claims["aud"].([]any)
	if header["alg"] != "HS256" || header["kid"] != "nope" || claims["sub"] != "svc" || claims["act"] != "alice" || aud[0] != "other" {
		t.Fatalf("explicit claims: %v %v", header, claims)
	}
	if claims["exp"] != 1.0 || claims["nbf"] != 2.0 || claims["iat"] != 3.0 {
		t.Fatalf("explicit times: %v", claims)
	}
	if status, _ := post(t, s.URL()+"/mint", `{"aud":7}`); status != 400 {
		t.Fatalf("malformed mint: %d", status)
	}
	if status, _ := post(t, s.URL()+"/mint", `{"aud":["a","b"]}`); status != 200 {
		t.Fatalf("list aud: %d", status)
	}
}

func TestRotateDropsTheOldKey(t *testing.T) {
	s := issuer.New(t)
	old := s.KID()
	before := s.Mint(issuer.Claims{})
	if status, _ := post(t, s.URL()+"/rotate", ""); status != 204 {
		t.Fatalf("POST /rotate: %d", status)
	}
	if s.KID() == old || keyOf(t, s, old) != nil || keyOf(t, s, s.KID()) == nil {
		t.Fatal("the key set still holds the old key, or not the new one")
	}
	header, _, signed, sig := decode(t, before)
	if header["kid"] != old || verify(t, keyOf(t, s, s.KID()), signed, sig) {
		t.Fatal("the old token verifies with the new key")
	}
	_, _, signed, sig = decode(t, s.Mint(issuer.Claims{}))
	if !verify(t, keyOf(t, s, s.KID()), signed, sig) {
		t.Fatal("the new key does not verify a new token")
	}
}

func TestHangHoldsDiscoveryAndTheJWKS(t *testing.T) {
	s := issuer.New(t)
	if status, _ := post(t, s.URL()+"/hang", ""); status != 204 {
		t.Fatalf("POST /hang: %d", status)
	}
	s.Hang() // idempotent
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for _, path := range []string{"/.well-known/openid-configuration", "/jwks"} {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", s.URL()+path, nil)
		if _, err := client.Do(req); err == nil {
			t.Fatalf("%s answered while hung", path)
		}
	}
	if s.Mint(issuer.Claims{}) == "" {
		t.Fatal("minting stopped")
	}
	if status, _ := post(t, s.URL()+"/resume", ""); status != 204 {
		t.Fatalf("POST /resume: %d", status)
	}
	s.Resume() // idempotent
	if getJSON(t, s.URL()+"/jwks")["keys"] == nil {
		t.Fatal("no keys after resume")
	}
	// A request hung at Close is released rather than leaked.
	s.Hang()
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", s.URL()+"/jwks", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond)
	s.Close()
	if code := <-done; code != 503 && code != 0 {
		t.Fatalf("released with %d", code)
	}
}

func TestOptions(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := issuer.New(t, issuer.WithKey(key), issuer.WithIssuer("http://issuer.example/"))
	if s.URL() != "http://issuer.example" {
		t.Fatalf("issuer %q", s.URL())
	}
	_, claims, signed, sig := decode(t, s.Mint(issuer.Claims{}))
	if claims["iss"] != "http://issuer.example" || !verify(t, &key.PublicKey, signed, sig) {
		t.Fatal("the given key did not sign, or iss is not the option's")
	}
	rs := issuer.New(t, issuer.WithRS256())
	header, _, signed, sig := decode(t, rs.Mint(issuer.Claims{}))
	pub := keyOf(t, rs, rs.KID())
	if _, ok := pub.(*rsa.PublicKey); !ok || header["alg"] != "RS256" || !verify(t, pub, signed, sig) {
		t.Fatal("RS256 key does not verify")
	}
	rs.Rotate()
	if _, ok := keyOf(t, rs, rs.KID()).(*rsa.PublicKey); !ok {
		t.Fatal("rotation changed the algorithm")
	}
	h := issuer.NewHandler(issuer.WithIssuer("http://stubs.example:8081"))
	if h.Handler() == nil || h.URL() != "http://stubs.example:8081" {
		t.Fatal("handler")
	}
	h.Close()
	var list issuer.StringList
	if err := json.Unmarshal([]byte(`"one"`), &list); err != nil || len(list) != 1 || list[0] != "one" {
		t.Fatalf("string form: %v %v", list, err)
	}
	if err := json.Unmarshal([]byte(`[1]`), &list); err == nil {
		t.Fatal("a list of numbers decoded")
	}
}

// TestMintsEachFailure mints one token per row of spec 007's verification
// table by setting one field wrong, and checks that the token carries
// exactly that wrong value; spec 007's verifier test asserts the reason
// each one produces.
func TestMintsEachFailure(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	s := issuer.New(t, issuer.WithClock(func() time.Time { return now }))
	rows := []struct {
		reason string
		claims issuer.Claims
		check  func(header, claims map[string]any, token string) bool
	}{
		{"size", issuer.Claims{Sub: strings.Repeat("s", 8<<10)}, func(_, _ map[string]any, token string) bool { return len(token) > 8<<10 }},
		{"signature (alg)", issuer.Claims{Alg: "HS256"}, func(h, _ map[string]any, _ string) bool { return h["alg"] == "HS256" }},
		{"unknown_key", issuer.Claims{Kid: "nope"}, func(h, _ map[string]any, _ string) bool { return h["kid"] == "nope" }},
		{"audience", issuer.Claims{Aud: issuer.StringList{"other"}}, func(_, c map[string]any, _ string) bool {
			aud, _ := c["aud"].([]any)
			return len(aud) == 1 && aud[0] == "other"
		}},
		{"expired", issuer.Claims{Exp: now.Add(-2 * time.Minute).Unix()}, func(_, c map[string]any, _ string) bool { return c["exp"] == float64(now.Add(-2*time.Minute).Unix()) }},
		{"nbf", issuer.Claims{Nbf: now.Add(2 * time.Minute).Unix()}, func(_, c map[string]any, _ string) bool { return c["nbf"] == float64(now.Add(2*time.Minute).Unix()) }},
		{"iat", issuer.Claims{Iat: now.Add(-25 * time.Hour).Unix()}, func(_, c map[string]any, _ string) bool { return c["iat"] == float64(now.Add(-25*time.Hour).Unix()) }},
	}
	for _, row := range rows {
		token := s.Mint(row.claims)
		header, claims, signed, sig := decode(t, token)
		if !row.check(header, claims, token) {
			t.Errorf("%s: the minted token does not carry the wrong value: %v %v", row.reason, header, claims)
		}
		// Every one is signed by the current key: the verifier refuses it
		// for the claim, never for the signature.
		if !verify(t, keyOf(t, s, s.KID()), signed, sig) {
			t.Errorf("%s: not signed by the current key", row.reason)
		}
	}
	// issuer: a second issuer's token names another iss.
	other := issuer.New(t, issuer.WithIssuer("http://other.example"))
	if _, claims, _, _ := decode(t, other.Mint(issuer.Claims{})); claims["iss"] != "http://other.example" {
		t.Fatalf("issuer row: %v", claims)
	}
	// unknown_key after a rotation: the old kid is gone from the set.
	old := s.KID()
	before := s.Mint(issuer.Claims{})
	s.Rotate()
	if header, _, _, _ := decode(t, before); header["kid"] != old || keyOf(t, s, old) != nil {
		t.Fatal("rotation kept the old key")
	}
	// issuer_unavailable: the key set stops answering.
	s.Hang()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", s.URL()+"/jwks", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("the JWKS answered while hung")
	}
	if s.Mint(issuer.Claims{}) == "" {
		t.Fatal("minting stopped while hung")
	}
}
