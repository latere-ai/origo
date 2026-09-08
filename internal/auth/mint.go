// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Token lifetimes a mint request may ask for.
const (
	MinTokenTTL = time.Second
	MaxTokenTTL = time.Hour
)

// ParseKey reads a PEM-encoded ECDSA P-256 private key, the value of
// ORIGO_TOKEN_KEY, in the SEC 1 form openssl ecparam writes (skipping
// the EC PARAMETERS block it puts first) or the PKCS #8 form.
func ParseKey(text string) (*ecdsa.PrivateKey, error) {
	rest := []byte(text)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no EC PRIVATE KEY or PRIVATE KEY block")
		}
		var key *ecdsa.PrivateKey
		switch block.Type {
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			key = k
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			ec, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return nil, errors.New("not an ECDSA key")
			}
			key = ec
		default:
			continue
		}
		if key.Curve != elliptic.P256() {
			return nil, errors.New("the curve must be P-256")
		}
		return key, nil
	}
}

// KeyID is spec 007's kid: the first 16 hex characters of the SHA-256
// of the public key's DER encoding.
func KeyID(pub *ecdsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:16]
}

// Signer mints repository-bound tokens with ORIGO_TOKEN_KEY and serves
// the public key set.
type Signer struct {
	key    *ecdsa.PrivateKey
	kid    string
	issuer string
	now    func() time.Time
	jwks   []byte
}

// NewSigner builds a signer whose tokens carry issuer as iss.
func NewSigner(key *ecdsa.PrivateKey, issuer string, now func() time.Time) *Signer {
	if now == nil {
		now = time.Now
	}
	s := &Signer{key: key, kid: KeyID(&key.PublicKey), issuer: strings.TrimRight(issuer, "/"), now: now}
	// The uncompressed point: 0x04, then x and y of 32 bytes each.
	point, err := key.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	s.jwks = mustJSON(map[string][]publishedKey{"keys": {{
		Kty: "EC", Crv: "P-256", Kid: s.kid, Alg: "ES256", Use: "sig",
		X: base64.RawURLEncoding.EncodeToString(point[1:33]), Y: base64.RawURLEncoding.EncodeToString(point[33:65]),
	}}})
	return s
}

// jwkOut is the one key the node publishes.
type publishedKey struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// header is the JOSE header of a minted token.
type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// mustJSON encodes a value of a type that always marshals.
func mustJSON[T any](v T) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// KID is the kid every minted token names.
func (s *Signer) KID() string { return s.kid }

// Public is the key the tokens verify with.
func (s *Signer) Public() *ecdsa.PublicKey { return &s.key.PublicKey }

// Mint signs a token bound to repo with the scope for ttl, on behalf of
// the minter: sub is the minter's effective subject and act its actor.
func (s *Signer) Mint(minter Principal, repo string, scope Scope, ttl time.Duration) (string, time.Time, error) {
	if scope != ScopeRead && scope != ScopeWrite {
		return "", time.Time{}, fmt.Errorf("scope must be %s or %s", ScopeRead, ScopeWrite)
	}
	if ttl < MinTokenTTL || ttl > MaxTokenTTL {
		return "", time.Time{}, fmt.Errorf("ttl must be %d to %d seconds", int(MinTokenTTL.Seconds()), int(MaxTokenTTL.Seconds()))
	}
	now := s.now().UTC().Truncate(time.Second)
	exp := now.Add(ttl)
	claims := map[string]any{
		"iss": s.issuer, "aud": []string{AudienceOrigo}, "sub": minter.Subject,
		"repo": repo, "scope": string(scope), "iat": now.Unix(), "exp": exp.Unix(), "jti": newUUID(),
	}
	if minter.Actor != "" {
		// The minter acted for the subject: the token carries the same
		// sub and act, so its verified subject and actor are the
		// minter's and the entry a build pushes names both.
		claims["sub"], claims["act"] = minter.Actor, minter.Subject
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	signing := base64.RawURLEncoding.EncodeToString(mustJSON(header{Alg: "ES256", Kid: s.kid, Typ: "JWT"})) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(signing))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", time.Time{}, err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), exp, nil
}

// JWKS serves GET /.well-known/jwks.json: the public key set Origo signs
// with, no token required.
func (s *Signer) JWKS() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		_, _ = w.Write(s.jwks)
	})
}

// newUUID draws a random UUID (version 4) for jti.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
