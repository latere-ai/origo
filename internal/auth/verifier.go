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

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/cache"
	"latere.ai/x/pkg/wait"
)

// Verification values of spec 007.
const (
	// DefaultAudience is the audience a token carries when the operator
	// sets no ORIGO_OIDC_AUDIENCE (Origo spec 028). The family chose the
	// consolidated model, so the default stays the service's own name and
	// does not move to a shared constant.
	DefaultAudience = "origo"
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
	// Audience is ORIGO_OIDC_AUDIENCE: the aud a token must carry.
	// DefaultAudience when empty.
	Audience string
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
	// AnonymousRead is ORIGO_ANONYMOUS_READ (spec 027). When true, the
	// middleware admits a request that carries no credential on one of
	// AnonymousRoutes with an empty subject, and the authorizer decides
	// it. False in every installation that does not set the variable, and
	// the node then behaves exactly as it did before that spec.
	AnonymousRead bool
	// Now is the clock; a test substitutes a fake.
	Now    func() time.Time
	Logger *slog.Logger
}

// Verifier checks tokens against the configured issuers and the local
// key, and remembers what it verified. The check itself is the family's
// latere.ai/x/pkg/authkit/jwt: shared holds the parser, the signature,
// the key sets and the claims window, all on Origo's clock and Origo's
// values of spec 007. What stays here is what spec 007 asks and that
// package does not carry, named row by row in Verify.
type Verifier struct {
	shared   *jwt.Validator
	issuers  map[string]*keySet
	order    []*keySet
	local    string
	localKID string
	audience string
	client   *http.Client
	timeout  time.Duration
	now      func() time.Time
	logger   *slog.Logger
	cache    *cache.TTLCache[[32]byte, cached]
	// anonymousRead is VerifierOptions.AnonymousRead (spec 027).
	anonymousRead bool
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
		localKID: KeyID(o.LocalKey), audience: o.Audience, client: o.Client, timeout: o.FetchTimeout, now: o.Now, logger: o.Logger,
		anonymousRead: o.AnonymousRead,
	}
	if v.audience == "" {
		v.audience = DefaultAudience
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
	urls := make([]string, 0, len(v.order))
	for _, i := range v.order {
		urls = append(urls, i.url)
	}
	// Origo's values of spec 007, handed to the shared verifier. Audiences
	// is deliberately not among them: spec 007's table checks aud before
	// exp and the package checks it after, so that one row is weighed in
	// Verify rather than here. The clock is Origo's, so every window the
	// package reads, and its key-set cache and its back-off, run on the
	// clock a test moves. The client is the node's, with the fetch budget
	// as its timeout: the package bounds a fetch no other way.
	v.shared = jwt.New(jwt.Config{
		Issuers:         urls,
		LocalIssuer:     v.local,
		LocalKey:        o.LocalKey,
		LocalKeyID:      v.localKID,
		ClockSkew:       ClockSkew,
		MaxTokenBytes:   MaxTokenBytes,
		MaxTokenAge:     MaxTokenAge,
		RequireIssuedAt: true,
		CacheTTL:        RefreshInterval,
		HTTPClient:      &http.Client{Transport: o.Client.Transport, Timeout: v.timeout},
		Now:             v.now,
	})
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

// refresh probes the issuers that are due: every one on the first pass,
// then the unprobed and the stale ones.
func (v *Verifier) refresh(ctx context.Context, all bool) {
	now := v.now()
	for _, i := range v.order {
		if !all && i.probed() && !i.stale(now) {
			continue
		}
		if !i.due(now) {
			continue
		}
		v.fetchIssuer(ctx, i, now)
	}
}

// fetchIssuer reads one issuer's two documents and records what it
// learned. The keys are the shared verifier's to hold, so the probe
// keeps none: what the node needs from it is the reachability the
// back-off below is paced by, and the line an operator reads at start-up
// when an issuer does not answer. The package exposes no way to warm its
// key set, so this is the same pair of requests `origod check` makes.
func (v *Verifier) fetchIssuer(ctx context.Context, i *keySet, now time.Time) {
	if _, err := FetchKeys(ctx, v.client, i.url, v.timeout); err != nil {
		i.failed(now)
		v.logger.WarnContext(ctx, "issuer keys not fetched", "issuer", i.url, "error", err)
		return
	}
	i.answered(now)
	v.logger.InfoContext(ctx, "issuer keys fetched", "issuer", i.url)
}

// Verify runs the rows of spec 007's verification table in order and
// returns the principal, or a *Refusal naming the first row that failed.
// Most rows are the shared verifier's; the ones named below are Origo's,
// and each is here because spec 007 asks something the package does not
// carry, not because the package's answer was rewritten.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if raw == "" {
		return Principal{}, refuse(ReasonMissing)
	}
	key := tokenKey(raw)
	now := v.now()
	if c, ok := v.cache.Get(key); ok && now.Before(c.until) {
		return c.principal, nil
	}
	// The size and the algorithm rows are weighed here as well as by the
	// shared verifier, because the issuer that paces the fetch below is
	// read out of the payload and the table puts both rows above it: a
	// token too large is never decoded, and one signed under an algorithm
	// nothing verifies never reaches an issuer's back-off.
	if len(raw) > MaxTokenBytes {
		return Principal{}, refuse(ReasonSize)
	}
	header, err := jwt.ParseHeader(raw)
	if err != nil {
		return Principal{}, refuse(ReasonMalformed)
	}
	if header.Alg != "RS256" && header.Alg != "ES256" {
		return Principal{}, refuse(ReasonSignature)
	}
	var c Claims
	if err := jwt.DecodePayload(raw, &c); err != nil {
		return Principal{}, refuse(ReasonMalformed)
	}
	iss := strings.TrimRight(c.Iss, "/")
	local := iss == v.local
	var set *keySet
	if !local {
		var known bool
		if set, known = v.issuers[iss]; !known {
			return Principal{}, refuse(ReasonIssuer)
		}
		// Spec 007's back-off, which the package does not carry: until an
		// issuer answers once, an attempt is made after FirstRetryInterval,
		// doubled per consecutive failure and capped at RetryInterval, and
		// a token arriving before the pause elapses is refused without a
		// fetch. The package attempts a fetch on every request instead, so
		// the node would hammer an issuer that is down.
		if !set.due(now) {
			return Principal{}, refuse(ReasonIssuerUnavailable)
		}
	}
	verified, err := v.shared.Validate(raw)
	if set != nil {
		v.settle(set, now, err)
	}
	if err != nil {
		if r := v.refusalOf(err, c); r != "" {
			return Principal{}, refuse(r)
		}
	}
	// The table's tail, in its order. The shared verifier reaches exp only
	// once the signature has verified, so its verdict is what says the aud
	// row below is due at all: aud is checked before exp here and after it
	// there, and spec 007's table pins the order.
	if !c.HasAudience(v.audience) {
		return Principal{}, refuse(ReasonAudience)
	}
	if err != nil {
		// exp, nbf and iat carry the package's words, which are the
		// table's. A token that names nobody is the row below them: it
		// would otherwise verify to the empty subject, which is the
		// anonymous principal of spec 027, and an issuer's token would
		// reach the authorizer as an anonymous request whether or not the
		// switch is on. The package refuses it too, as malformed, which is
		// the shape row and not the word spec 007 gives this one.
		if c.Sub == "" {
			return Principal{}, refuse(ReasonSubject)
		}
		return Principal{}, refuse(string(jwt.ReasonOf(err)))
	}
	// Every claim of the payload, verbatim, for the authorizer envelope
	// (Origo spec 028): the node reads the typed Claims above, the
	// authorizer reads whatever its policy needs. It carries the same
	// object the signature covered, because the token verified.
	var claims map[string]any
	if err := jwt.DecodePayload(raw, &claims); err != nil {
		return Principal{}, refuse(ReasonMalformed)
	}
	// A token that carries an act claim names two parties, and no token
	// carries a chain: the caller is the token's sub and nobody else.
	if _, delegated := claims["act"]; delegated {
		return Principal{}, refuse(ReasonDelegation)
	}
	// The rendered subject the authorizer, the entry header, and the event
	// record (Origo spec 028). A repository-bound token's sub is the
	// minter's rendered subject already, and its iss is ORIGO_PUBLIC_URL,
	// so rendering it again would nest: the node renders a local token's
	// subject as the sub it carries. An issuer's token renders <iss>|<sub>.
	subject := verified.Sub
	if !local {
		subject = authz.Subject(iss, verified.Sub)
	}
	p := Principal{Subject: subject, Issuer: iss, Sub: verified.Sub, Claims: claims}
	if local {
		p.Bound = &Bound{Repo: c.Repo, Scope: Scope(c.Scope)}
	}
	// The verified token is remembered, which the package's key-set cache
	// is not: it caches the keys a signature is checked against, not the
	// verdict, so without this every request re-runs the signature and the
	// whole window. Bounded by TokenCacheTTL and by the token's own exp,
	// whichever is sooner, so a cache entry never outlives the token.
	until := unix(c.Exp)
	if limit := now.Add(TokenCacheTTL); until.After(limit) {
		until = limit
	}
	v.cache.Set(key, cached{principal: p, until: until})
	return p, nil
}

// settle records what one attempt learned about an issuer. Only a verdict
// the shared verifier could reach after it read a key says anything: the
// rows it refuses before that, the shape and the size and the algorithm,
// are about the token and not about the issuer.
func (v *Verifier) settle(set *keySet, now time.Time, err error) {
	switch {
	case errors.Is(err, jwt.ErrIssuerUnavailable), errors.Is(err, jwt.ErrBadDiscovery):
		set.failed(now)
	case errors.Is(err, jwt.ErrTokenTooLarge), errors.Is(err, jwt.ErrUnsupportedAlg),
		errors.Is(err, jwt.ErrInvalidIssuer), errors.Is(err, jwt.ErrMalformedToken):
	default:
		set.answered(now)
	}
}

// refusalOf is the row a shared refusal belongs to, or "" when the row is
// one Verify weighs itself below. Two words differ from the package's and
// are translated here rather than in the package, because spec 007 and
// the family's table disagree on them and neither is wrong:
//
//   - a discovery document naming another issuer fails the fetch like an
//     unreachable issuer (spec 007), where the package calls the issuer
//     itself bad. An operator reads issuer_unavailable either way, so the
//     node keeps the word it always wrote.
//   - a token whose sub is empty is the subject row, weighed after iat.
//     The package calls it malformed, which is the shape row.
func (v *Verifier) refusalOf(err error, c Claims) string {
	switch reason := jwt.ReasonOf(err); {
	case errors.Is(err, jwt.ErrBadDiscovery):
		return ReasonIssuerUnavailable
	case reason == jwt.ReasonExpired, reason == jwt.ReasonNotYetValid, reason == jwt.ReasonTooOld,
		reason == jwt.ReasonMalformed && c.Sub == "":
		// Rows at or below aud in the table: Verify weighs aud first and
		// then writes the word itself.
		return ""
	case reason == "":
		return ReasonMalformed
	default:
		return string(reason)
	}
}

// CacheLen reports the verified-token cache's size, for tests.
func (v *Verifier) CacheLen() int { return v.cache.Len() }

func unix(seconds float64) time.Time { return time.Unix(int64(seconds), 0) }

// tokenKey is the cache key of a token: the SHA-256 of its bytes, so the
// token itself is never retained.
func tokenKey(raw string) [32]byte { return sha256.Sum256([]byte(raw)) }
