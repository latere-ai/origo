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
	"slices"
	"strings"
	"sync"
	"time"
)

// Fetch budgets of spec 007: the discovery fetch and the JWKS fetch each
// have five seconds, and the key set is refreshed every hour. Until an
// issuer has answered once, a failed attempt is retried after
// FirstRetryInterval, doubled per consecutive failure up to
// RetryInterval, on the request path as well as by the minute loop: a
// node that started before its issuer answered would otherwise refuse
// every token of that issuer for a minute after the issuer came up, with
// issuer_unavailable, while the fetch a request could trigger stood
// pinned by the start-up failure. Once an issuer has answered, a token
// naming an unknown kid forces one refresh of the set, which the shared
// verifier bounds to one every fifteen seconds.
const (
	DefaultFetchTimeout = 5 * time.Second
	RefreshInterval     = time.Hour
	RetryInterval       = time.Minute
	FirstRetryInterval  = time.Second
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
		if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
			return nil
		}
		// The uncompressed point; the parser checks it is on the curve.
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), slices.Concat([]byte{4}, x, y))
		if err != nil {
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

// keySet is one configured OIDC issuer and the back-off the node asks it
// under. It holds no keys: the shared verifier fetched them and holds
// them. What is here is spec 007's pacing, which that package does not
// carry, and the record of the probe the refresh loop runs.
type keySet struct {
	url string

	mu sync.Mutex
	// failures counts the consecutive attempts that found the issuer out
	// of reach; lastFailure is when the last of them was made.
	failures    int
	lastFailure time.Time
	// fetched records that the issuer answered the probe at least once,
	// and lastFetched when it last did, so the loop refreshes on the hour.
	fetched     bool
	lastFetched time.Time
}

// due reports whether the issuer may be asked now: always, until an
// attempt finds it out of reach, and then not before the back-off of the
// failures so far has elapsed.
func (i *keySet) due(now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.failures == 0 || now.Sub(i.lastFailure) >= i.retryAfter()
}

// failed records an attempt that found the issuer out of reach.
func (i *keySet) failed(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failures, i.lastFailure = i.failures+1, now
}

// answered records an attempt that read the issuer's key set.
func (i *keySet) answered(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failures, i.fetched, i.lastFetched = 0, true, now
}

// probed reports whether the issuer has ever answered.
func (i *keySet) probed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fetched
}

// retryAfter is the pause between attempts: FirstRetryInterval doubled
// per consecutive failure and capped at RetryInterval. Called with mu
// held, and only with a failure recorded.
func (i *keySet) retryAfter() time.Duration {
	// Six doublings pass the cap; the shift stays small whatever the
	// count.
	return min(RetryInterval, FirstRetryInterval<<min(i.failures-1, 6))
}

// stale reports whether the last probe of the issuer is older than
// RefreshInterval.
func (i *keySet) stale(now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fetched && now.Sub(i.lastFetched) >= RefreshInterval
}

// FetchKeys reads <iss>/.well-known/openid-configuration for jwks_uri and
// then the key set. It is exported so `origod check` proves an issuer
// through the same two requests the node makes rather than through a
// second reading of the same documents.
func FetchKeys(ctx context.Context, client *http.Client, iss string, timeout time.Duration) (map[string]crypto.PublicKey, error) {
	doc, err := getDocument(ctx, client, iss+"/.well-known/openid-configuration", timeout)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(doc, &discovery); err != nil || discovery.JWKSURI == "" {
		return nil, errors.New("discovery: no jwks_uri")
	}
	// OIDC Discovery 4.3: the document's issuer must be the URL it was
	// fetched under. A document that names another issuer publishes keys
	// for tokens whose iss the node would never match, so its keys are
	// not this issuer's and the fetch fails like an unreachable one.
	if strings.TrimRight(discovery.Issuer, "/") != strings.TrimRight(iss, "/") {
		return nil, fmt.Errorf("discovery: issuer %q is not %q", discovery.Issuer, iss)
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
