// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/base64"
	"encoding/json/v2"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A minted token's signature covers its payload bytes, so the claims are
// written in one fixed order.
func TestMintedPayloadKeysAreSorted(t *testing.T) {
	clk := newClock()
	s := NewSigner(newKey(t), localIssuer, "", clk.Now)
	tok, _, err := s.Mint(Principal{Subject: "alice"}, repoA, ScopeWrite, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"alg":"ES256","kid":"` + s.KID() + `","typ":"JWT"}`; string(head) != want {
		t.Fatalf("header %s, want %s", head, want)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	keys := regexp.MustCompile(`"([a-z]+)":`).FindAllStringSubmatch(string(payload), -1)
	var got []string
	for _, k := range keys {
		got = append(got, k[1])
	}
	if want := []string{"aud", "exp", "iat", "iss", "jti", "repo", "scope", "sub"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("payload %s: keys %v, want %v", payload, got, want)
	}
}

// The aud claim comes from an issuer: either form and invalid UTF-8 in a
// value are read as before.
func TestAudienceReadsForeignForms(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want Audience
	}{
		"one string": {`"a"`, Audience{"a"}},
		"a list":     {`["a","b"]`, Audience{"a", "b"}},
		"bad utf-8":  {"\"a\xff\"", Audience{"a�"}},
	} {
		var got Audience
		if err := got.UnmarshalJSON([]byte(tc.in)); err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

// A key set and a discovery document are the issuer's: duplicate names
// and mixed-case names are read, the last duplicate winning.
func TestForeignDocumentsAreReadLeniently(t *testing.T) {
	var disc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	in := `{"Issuer":"a","issuer":"b","JWKS_URI":"u"}`
	if err := json.Unmarshal([]byte(in), &disc, foreignJSON); err != nil || disc.Issuer != "b" || disc.JWKSURI != "u" {
		t.Fatalf("%+v %v", disc, err)
	}
}
