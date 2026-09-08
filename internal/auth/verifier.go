// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/cache"
	"latere.ai/x/pkg/wait"
)

// Verification values of spec 007.
const (
	// AudienceOrigo is the audience every accepted token carries.
	AudienceOrigo = "origo"
	// ClockSkew is the tolerance on exp and nbf of an issuer's token,
	// for the difference between the issuer's clock and the node's. A
	// repository-bound token was minted on the node's own clock, so it
	// gets none: it is expired one second after its exp.
	ClockSkew = 60 * time.Second
	// MaxTokenAge is how old iat may be.
	MaxTokenAge = 24 * time.Hour
	// TokenCacheTTL caps how long a verified token is remembered; a
	// token that expires sooner is remembered until it expires.
	TokenCacheTTL = 5 * time.Minute
	// CacheEntries bounds the verified-token cache and each authorizer
	// cache; the least recently used entry is evicted past it.
	CacheEntries = 65536
)

// VerifierOptions configures a Verifier.
type VerifierOptions struct {
	// Issuers are the URLs of ORIGO_OIDC_ISSUERS, trailing slash removed.
	Issuers []string
	// LocalIssuer is ORIGO_PUBLIC_URL: the iss of a repository-bound
	// token, verified against LocalKey with no fetch.
	LocalIssuer string
	// LocalKey is the public half of ORIGO_TOKEN_KEY.
	LocalKey *ecdsa.PublicKey
	// Client fetches discovery documents and key sets. Required; the
	// node passes its instrumented client.
	Client *http.Client
	// FetchTimeout bounds one discovery or JWKS fetch. DefaultFetchTimeout
	// when zero.
	FetchTimeout time.Duration
	// Now is the clock; a test substitutes a fake.
	Now    func() time.Time
	Logger *slog.Logger
}

// Verifier checks tokens against the configured issuers and the local
// key, and remembers what it verified.
type Verifier struct {
	issuers  map[string]*keySet
	order    []*keySet
	local    string
	localKey *ecdsa.PublicKey
	localKID string
	client   *http.Client
	timeout  time.Duration
	now      func() time.Time
	logger   *slog.Logger
	cache    *cache.TTLCache[[32]byte, cached]
}

// cached is a verified principal and the instant it stops being valid.
type cached struct {
	principal Principal
	until     time.Time
}

// NewVerifier builds a verifier. It fetches nothing: Run fetches every
// issuer's keys and keeps them fresh, and a token that arrives before
// the first fetch triggers one.
func NewVerifier(o VerifierOptions) (*Verifier, error) {
	if o.Client == nil {
		return nil, errors.New("auth: the verifier needs an HTTP client")
	}
	if o.LocalIssuer == "" || o.LocalKey == nil {
		return nil, errors.New("auth: the verifier needs the local issuer and its key")
	}
	v := &Verifier{
		issuers: make(map[string]*keySet, len(o.Issuers)), local: strings.TrimRight(o.LocalIssuer, "/"),
		localKey: o.LocalKey, localKID: KeyID(o.LocalKey), client: o.Client, timeout: o.FetchTimeout, now: o.Now, logger: o.Logger,
	}
	if v.timeout == 0 {
		v.timeout = DefaultFetchTimeout
	}
	if v.now == nil {
		v.now = time.Now
	}
	if v.logger == nil {
		v.logger = slog.Default()
	}
	for _, url := range o.Issuers {
		url = strings.TrimRight(url, "/")
		if _, ok := v.issuers[url]; ok || url == "" {
			continue
		}
		i := &keySet{url: url}
		v.issuers[url] = i
		v.order = append(v.order, i)
	}
	v.cache = cache.New[[32]byte, cached](TokenCacheTTL, cache.WithMaxSize[[32]byte, cached](CacheEntries), cache.WithClock[[32]byte, cached](v.now))
	return v, nil
}

// LocalKID is the kid of the node's own key, the one a repository-bound
// token must name.
func (v *Verifier) LocalKID() string { return v.localKID }

// Run fetches every issuer's key set now and then once a minute retries
// any issuer whose set was never fetched and refreshes any set older
// than an hour, until ctx ends. An issuer that does not answer is
// logged and retried; it never stops the node.
func (v *Verifier) Run(ctx context.Context) error {
	v.refresh(ctx, true)
	wait.Every(ctx, RetryInterval, func(ctx context.Context) { v.refresh(ctx, false) })
	return ctx.Err()
}

// refresh fetches the issuers that are due: every one on the first
// pass, then the unfetched and the stale ones.
func (v *Verifier) refresh(ctx context.Context, all bool) {
	now := v.now()
	for _, i := range v.order {
		_, fetched := i.keyFor("")
		if !all && fetched && !i.stale(now) {
			continue
		}
		if !i.due(now) {
			continue
		}
		v.fetchIssuer(ctx, i, now)
	}
}

// fetchIssuer runs one claimed attempt and logs its failure.
func (v *Verifier) fetchIssuer(ctx context.Context, i *keySet, now time.Time) {
	if err := i.fetch(ctx, v.client, v.timeout, now); err != nil {
		v.logger.WarnContext(ctx, "issuer keys not fetched", "issuer", i.url, "error", err)
		return
	}
	v.logger.InfoContext(ctx, "issuer keys fetched", "issuer", i.url)
}

// Verify runs the rows of spec 007's verification table in order and
// returns the principal, or a *Refusal naming the first row that failed.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if raw == "" {
		return Principal{}, refuse(ReasonMissing)
	}
	key := tokenKey(raw)
	now := v.now()
	if c, ok := v.cache.Get(key); ok && now.Before(c.until) {
		return c.principal, nil
	}
	t, err := ParseToken(raw)
	if err != nil {
		return Principal{}, err
	}
	if t.Alg != "RS256" && t.Alg != "ES256" {
		return Principal{}, refuse(ReasonSignature)
	}
	iss := strings.TrimRight(t.Claims.Iss, "/")
	local := iss == v.local
	if local {
		if t.KID != v.localKID {
			return Principal{}, refuse(ReasonUnknownKey)
		}
		if !t.verifySignature(v.localKey) {
			return Principal{}, refuse(ReasonSignature)
		}
	} else {
		i, ok := v.issuers[iss]
		if !ok {
			return Principal{}, refuse(ReasonIssuer)
		}
		pub, err := v.issuerKey(ctx, i, t.KID, now)
		if err != nil {
			return Principal{}, err
		}
		if !t.verifySignature(pub) {
			return Principal{}, refuse(ReasonSignature)
		}
	}
	c := t.Claims
	if !c.HasAudience(AudienceOrigo) {
		return Principal{}, refuse(ReasonAudience)
	}
	skew := ClockSkew
	if local {
		skew = 0
	}
	if c.Exp == nil || !now.Before(unix(*c.Exp).Add(skew)) {
		return Principal{}, refuse(ReasonExpired)
	}
	if c.Nbf != nil && now.Before(unix(*c.Nbf).Add(-skew)) {
		return Principal{}, refuse(ReasonNBF)
	}
	if c.Iat == nil || now.Sub(unix(*c.Iat)) > MaxTokenAge {
		return Principal{}, refuse(ReasonIAT)
	}
	p := Principal{Subject: c.Sub}
	if c.Act != "" {
		p.Subject, p.Actor = c.Act, c.Sub
	}
	if local {
		p.Bound = &Bound{Repo: c.Repo, Scope: Scope(c.Scope)}
	}
	until := unix(*c.Exp)
	if limit := now.Add(TokenCacheTTL); until.After(limit) {
		until = limit
	}
	v.cache.Set(key, cached{principal: p, until: until})
	return p, nil
}

// issuerKey finds the key kid of an issuer: refusing with
// issuer_unavailable until the set was fetched once, and with
// unknown_key after one refresh when the set does not hold it. A fetch
// from here is bounded to one a minute per issuer.
func (v *Verifier) issuerKey(ctx context.Context, i *keySet, kid string, now time.Time) (any, error) {
	pub, fetched := i.keyFor(kid)
	if pub != nil {
		return pub, nil
	}
	if i.due(now) {
		v.fetchIssuer(ctx, i, now)
		pub, fetched = i.keyFor(kid)
		if pub != nil {
			return pub, nil
		}
	}
	if !fetched {
		return nil, refuse(ReasonIssuerUnavailable)
	}
	return nil, refuse(ReasonUnknownKey)
}

// CacheLen reports the verified-token cache's size, for tests.
func (v *Verifier) CacheLen() int { return v.cache.Len() }

func unix(seconds float64) time.Time { return time.Unix(int64(seconds), 0) }

// tokenKey is the cache key of a token: the SHA-256 of its bytes, so the
// token itself is never retained.
func tokenKey(raw string) [32]byte { return sha256.Sum256([]byte(raw)) }
