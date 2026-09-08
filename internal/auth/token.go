// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"slices"
	"strings"
)

// MaxTokenBytes is the size above which a token is refused before it is
// parsed.
const MaxTokenBytes = 8 << 10

// The reasons of spec 003's unauthenticated table, one per row of spec
// 007's verification table.
const (
	ReasonMissing           = "missing"
	ReasonSize              = "size"
	ReasonMalformed         = "malformed"
	ReasonSignature         = "signature"
	ReasonIssuer            = "issuer"
	ReasonIssuerUnavailable = "issuer_unavailable"
	ReasonUnknownKey        = "unknown_key"
	ReasonAudience          = "audience"
	ReasonExpired           = "expired"
	ReasonNBF               = "nbf"
	ReasonIAT               = "iat"
)

// Refusal is why a credential was refused: one reason of the table,
// sent as details.reason of a 401.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return "auth: token refused: " + r.Reason }

func refuse(reason string) error { return &Refusal{Reason: reason} }

// Audience decodes the aud claim in both forms a JWT allows: one string
// or a list of strings.
type Audience []string

// UnmarshalJSON accepts both forms.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Claims are the claims the verifier reads. Times are Unix seconds and
// nil when absent. Unknown claims are ignored: an issuer's token carries
// many the node has no use for.
type Claims struct {
	Iss   string   `json:"iss"`
	Sub   string   `json:"sub"`
	Act   string   `json:"act"`
	Aud   Audience `json:"aud"`
	Exp   *float64 `json:"exp"`
	Nbf   *float64 `json:"nbf"`
	Iat   *float64 `json:"iat"`
	Repo  string   `json:"repo"`
	Scope string   `json:"scope"`
	JTI   string   `json:"jti"`
}

// Token is a parsed and not yet verified JWT.
type Token struct {
	Alg    string
	KID    string
	Claims Claims

	signingInput []byte
	signature    []byte
}

// ParseToken checks the size and the shape of a compact JWT: three
// base64url segments whose header and claims parse as JSON objects. It
// verifies nothing.
func ParseToken(raw string) (*Token, error) {
	if len(raw) > MaxTokenBytes {
		return nil, refuse(ReasonSize)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, refuse(ReasonMalformed)
	}
	var header struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return nil, refuse(ReasonMalformed)
	}
	t := &Token{Alg: header.Alg, KID: header.KID}
	if err := decodeSegment(parts[1], &t.Claims); err != nil {
		return nil, refuse(ReasonMalformed)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, refuse(ReasonMalformed)
	}
	t.signingInput = []byte(parts[0] + "." + parts[1])
	t.signature = sig
	return t, nil
}

// decodeSegment decodes one base64url segment into a JSON object.
func decodeSegment(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	if len(raw) == 0 || raw[0] != '{' {
		return refuse(ReasonMalformed)
	}
	return json.Unmarshal(raw, v)
}

// verifySignature checks the token's signature with key under the alg
// the header names. A key of the wrong kind for the alg fails like a bad
// signature.
func (t *Token) verifySignature(key crypto.PublicKey) bool {
	digest := sha256.Sum256(t.signingInput)
	switch t.Alg {
	case "RS256":
		pub, ok := key.(*rsa.PublicKey)
		return ok && rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], t.signature) == nil
	case "ES256":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok || len(t.signature) != 64 {
			return false
		}
		r := new(big.Int).SetBytes(t.signature[:32])
		s := new(big.Int).SetBytes(t.signature[32:])
		return ecdsa.Verify(pub, digest[:], r, s)
	}
	return false
}

// HasAudience reports whether aud contains the value.
func (c Claims) HasAudience(want string) bool {
	return slices.Contains(c.Aud, want)
}
