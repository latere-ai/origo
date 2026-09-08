// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// Fetch budgets of spec 007: the discovery fetch and the JWKS fetch each
// have five seconds, the key set is refreshed every hour, and a fetch is
// attempted at most once a minute per issuer while it is unavailable or
// a token names an unknown kid.
const (
	DefaultFetchTimeout = 5 * time.Second
	RefreshInterval     = time.Hour
	RetryInterval       = time.Minute
)

// maxDocumentBytes bounds a discovery document or a JWKS.
const maxDocumentBytes = 1 << 20

// jwk is one key of a JWKS in the two families the verifier accepts.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// parseJWKS reads a key set into public keys by kid. A key the verifier
// cannot use (another family, another curve, no kid) is skipped rather
// than refused: an issuer may publish keys for other consumers.
func parseJWKS(raw []byte) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kid == "" {
			continue
		}
		if pub := k.public(); pub != nil {
			keys[k.Kid] = pub
		}
	}
	return keys, nil
}

func (k jwk) public() crypto.PublicKey {
	switch k.Kty {
	case "EC":
		if k.Crv != "P-256" {
			return nil
		}
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if errX != nil || errY != nil {
			return nil
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil
		}
		return pub
	case "RSA":
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(n) == 0 || len(e) == 0 {
			return nil
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() < 3 {
			return nil
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}
	}
	return nil
}

// keySet is one configured OIDC issuer and the key set fetched from it.
type keySet struct {
	url string

	mu          sync.Mutex
	keys        map[string]crypto.PublicKey
	fetched     bool
	fetching    bool
	lastAttempt time.Time
	lastFetched time.Time
}

// keyFor reports the key named kid and whether the set was ever fetched.
func (i *keySet) keyFor(kid string) (crypto.PublicKey, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.keys[kid], i.fetched
}

// due reports whether an attempt may start now: none is in flight and
// the last one is at least RetryInterval old. A true answer claims the
// attempt, so the caller must fetch.
func (i *keySet) due(now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.fetching || (!i.lastAttempt.IsZero() && now.Sub(i.lastAttempt) < RetryInterval) {
		return false
	}
	i.fetching, i.lastAttempt = true, now
	return true
}

// stale reports whether the set is older than RefreshInterval.
func (i *keySet) stale(now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fetched && now.Sub(i.lastFetched) >= RefreshInterval
}

// fetch runs discovery and the JWKS fetch, each under its own timeout,
// and stores the keys. The caller has claimed the attempt through due.
func (i *keySet) fetch(ctx context.Context, client *http.Client, timeout time.Duration, now time.Time) error {
	keys, err := fetchKeys(ctx, client, i.url, timeout)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.fetching = false
	if err != nil {
		return err
	}
	i.keys, i.fetched, i.lastFetched = keys, true, now
	return nil
}

// fetchKeys reads <iss>/.well-known/openid-configuration for jwks_uri and
// then the key set.
func fetchKeys(ctx context.Context, client *http.Client, iss string, timeout time.Duration) (map[string]crypto.PublicKey, error) {
	doc, err := getDocument(ctx, client, iss+"/.well-known/openid-configuration", timeout)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(doc, &discovery); err != nil || discovery.JWKSURI == "" {
		return nil, errors.New("discovery: no jwks_uri")
	}
	set, err := getDocument(ctx, client, discovery.JWKSURI, timeout)
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	return parseJWKS(set)
}

func getDocument(ctx context.Context, client *http.Client, url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
}
