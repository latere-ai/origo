// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/stubs/issuer"
)

// clock is a fake clock a test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testClient() *http.Client { return &http.Client{Transport: &http.Transport{}} }

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

const localIssuer = "https://git.example.com"

// newVerifier builds a verifier over the issuers with the fake clock and
// the local key, fetching nothing yet.
func newVerifier(t *testing.T, clk *clock, key *ecdsa.PrivateKey, issuers ...*issuer.Server) *Verifier {
	t.Helper()
	urls := make([]string, 0, len(issuers))
	for _, i := range issuers {
		urls = append(urls, i.URL()+"/")
	}
	v, err := NewVerifier(VerifierOptions{
		Issuers: urls, LocalIssuer: localIssuer + "/", LocalKey: &key.PublicKey,
		Client: testClient(), Now: clk.Now, FetchTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// signClaims signs claims as an ES256 token under the header segment of
// a token the stub minted, so the kid is the stub's: for a row the
// stub's Mint cannot produce because it fills the claim in.
func signClaims(t *testing.T, key *ecdsa.PrivateKey, header string, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestDiscoveryIssuerMustMatch is OIDC Discovery 4.3: a document whose
// issuer is not the URL it was fetched under yields no keys, and a token
// naming that URL is issuer_unavailable rather than verified against
// keys that are not the issuer's.
func TestDiscoveryIssuerMustMatch(t *testing.T) {
	clk := newClock()
	wrongKey := newKey(t)
	// The stub serves under front.URL and its document says elsewhere.
	wrong := issuer.NewHandler(issuer.WithKey(wrongKey), issuer.WithIssuer("http://elsewhere.example"), issuer.WithClock(clk.Now))
	front := httptest.NewServer(wrong.Handler())
	t.Cleanup(front.Close)
	ctx := context.Background()
	if _, err := FetchKeys(ctx, testClient(), front.URL, time.Second); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("a document naming another issuer was accepted: %v", err)
	}
	key := newKey(t)
	v, err := NewVerifier(VerifierOptions{
		Issuers: []string{front.URL}, LocalIssuer: localIssuer, LocalKey: &key.PublicKey,
		Client: testClient(), Now: clk.Now, FetchTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(wrong.Mint(issuer.Claims{}), ".")
	now := clk.Now()
	tok := signClaims(t, wrongKey, header, map[string]any{"iss": front.URL, "sub": "alice", "aud": "origo", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	if _, err := v.Verify(ctx, tok); reason(err) != ReasonIssuerUnavailable {
		t.Fatalf("got %v, want issuer_unavailable", err)
	}
}

func reason(err error) string {
	if r, ok := errors.AsType[*Refusal](err); ok {
		return r.Reason
	}
	return ""
}

func TestVerifierAcceptsTwoIssuersAndRefusesEachFailure(t *testing.T) {
	clk := newClock()
	aKey := newKey(t)
	a := issuer.New(t, issuer.WithKey(aKey), issuer.WithClock(clk.Now))
	b := issuer.New(t, issuer.WithRS256(), issuer.WithClock(clk.Now))
	key := newKey(t)
	v := newVerifier(t, clk, key, a, b)
	ctx := context.Background()

	// Both issuers' tokens verify, ES256 and RS256, in the three
	// credential forms, and act sets the effective subject.
	for _, iss := range []*issuer.Server{a, b} {
		p, err := v.Verify(ctx, iss.Mint(issuer.Claims{Sub: "alice"}))
		if err != nil || p.Subject != "alice" || p.Actor != "" || p.Bound != nil {
			t.Fatalf("%s: %+v, %v", iss.URL(), p, err)
		}
	}
	p, err := v.Verify(ctx, a.Mint(issuer.Claims{Sub: "svc", Act: "bob"}))
	if err != nil || p.Subject != "bob" || p.Actor != "svc" {
		t.Fatalf("act: %+v, %v", p, err)
	}
	// A repository-bound token verifies against the node's key with no
	// fetch: no issuer of the list is asked.
	signer := NewSigner(key, localIssuer, clk.Now)
	local, _, err := signer.Mint(Principal{Subject: "ci"}, "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f", ScopeRead, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fresh := newVerifier(t, clk, key, a)
	p, err = fresh.Verify(ctx, local)
	if err != nil || p.Subject != "ci" || p.Bound == nil || p.Bound.Repo != "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f" || p.Bound.Scope != ScopeRead {
		t.Fatalf("local token: %+v, %v", p, err)
	}
	if _, fetched := fresh.issuers[a.URL()].keyFor(""); fetched {
		t.Fatal("the local token fetched the issuer's keys")
	}
	// Each row of the verification table, on the verifier alone.
	other := issuer.New(t, issuer.WithClock(clk.Now))
	parts := strings.Split(a.Mint(issuer.Claims{}), ".")
	badSig := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	now := clk.Now()
	rows := []struct {
		reason string
		token  string
	}{
		{ReasonMissing, ""},
		{ReasonSize, a.Mint(issuer.Claims{Sub: strings.Repeat("s", MaxTokenBytes)})},
		{ReasonMalformed, "not.a.jwt"},
		{ReasonMalformed, "only-one-segment"},
		{ReasonMalformed, parts[0] + "." + parts[1]},
		{ReasonMalformed, parts[0] + "." + parts[1] + ".!!!"},
		{ReasonSignature, "e30." + parts[1] + "." + parts[2]},
		{ReasonSignature, a.Mint(issuer.Claims{Alg: "HS256"})},
		{ReasonSignature, a.Mint(issuer.Claims{Alg: "none"})},
		{ReasonSignature, badSig},
		{ReasonSignature, a.Mint(issuer.Claims{Alg: "RS256"})},
		{ReasonIssuer, other.Mint(issuer.Claims{})},
		{ReasonUnknownKey, a.Mint(issuer.Claims{Kid: "nope"})},
		{ReasonAudience, a.Mint(issuer.Claims{Aud: issuer.StringList{"other"}})},
		{ReasonAudience, a.Mint(issuer.Claims{Aud: issuer.StringList{"other", "another"}})},
		{ReasonExpired, a.Mint(issuer.Claims{Exp: now.Add(-2 * time.Minute).Unix()})},
		{ReasonNBF, a.Mint(issuer.Claims{Nbf: now.Add(2 * time.Minute).Unix()})},
		{ReasonIAT, a.Mint(issuer.Claims{Iat: now.Add(-25 * time.Hour).Unix()})},
		// The stub fills an empty sub in, so the two subject rows are
		// signed by hand with the issuer's own key.
		{ReasonSubject, signClaims(t, aKey, parts[0], map[string]any{"iss": a.URL(), "aud": "origo", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})},
		{ReasonSubject, signClaims(t, aKey, parts[0], map[string]any{"iss": a.URL(), "sub": "", "aud": "origo", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})},
	}
	for _, row := range rows {
		_, err := v.Verify(ctx, row.token)
		if got := reason(err); got != row.reason {
			t.Errorf("%s: got %q (%v)", row.reason, got, err)
		}
	}
	// The skews: 59 seconds past exp and 59 seconds before nbf pass.
	for _, tok := range []string{
		a.Mint(issuer.Claims{Exp: now.Add(-59 * time.Second).Unix()}),
		a.Mint(issuer.Claims{Nbf: now.Add(59 * time.Second).Unix()}),
		a.Mint(issuer.Claims{Iat: now.Add(-23 * time.Hour).Unix()}),
	} {
		if _, err := v.Verify(ctx, tok); err != nil {
			t.Errorf("within the skew: %v", err)
		}
	}
	// A verified token is remembered: the same token after a rotation
	// still verifies from the cache, until it expires.
	cached := a.Mint(issuer.Claims{Sub: "carol", Exp: clk.Now().Add(2 * time.Minute).Unix()})
	if _, err := v.Verify(ctx, cached); err != nil {
		t.Fatal(err)
	}
	a.Rotate()
	if p, err := v.Verify(ctx, cached); err != nil || p.Subject != "carol" {
		t.Fatalf("cached: %+v, %v", p, err)
	}
	clk.Advance(3 * time.Minute)
	if got := reason(func() error { _, err := v.Verify(ctx, cached); return err }()); got != ReasonExpired {
		t.Fatalf("after the lifetime: %q", got)
	}

	// unknown_key after one refresh: a rotation the verifier has not seen
	// is found by the refresh; a kid nobody has is refused after it.
	beforeRotate := a.Mint(issuer.Claims{})
	a.Rotate()
	if _, err := v.Verify(ctx, a.Mint(issuer.Claims{})); err != nil {
		t.Fatalf("rotated key not refreshed: %v", err)
	}
	if got := reason(func() error { _, err := v.Verify(ctx, beforeRotate); return err }()); got != ReasonUnknownKey {
		t.Fatalf("dropped key: %q", got)
	}
	// A second rotation within the minute is not refreshed: the new kid
	// is unknown until the next minute.
	a.Rotate()
	if got := reason(func() error { _, err := v.Verify(ctx, a.Mint(issuer.Claims{})); return err }()); got != ReasonUnknownKey {
		t.Fatalf("refresh not bounded to one a minute: %q", got)
	}
	clk.Advance(RetryInterval)
	if _, err := v.Verify(ctx, a.Mint(issuer.Claims{})); err != nil {
		t.Fatalf("after the minute: %v", err)
	}
	// A local token with another kid is unknown_key, and one signed by
	// another key with the right kid is signature.
	otherKey := newKey(t)
	otherSigner := NewSigner(otherKey, localIssuer, clk.Now)
	tok, _, _ := otherSigner.Mint(Principal{Subject: "x"}, "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f", ScopeRead, time.Minute)
	if got := reason(func() error { _, err := v.Verify(ctx, tok); return err }()); got != ReasonUnknownKey {
		t.Fatalf("local token with another kid: %q", got)
	}
	forged := issuer.New(t, issuer.WithKey(otherKey), issuer.WithIssuer(localIssuer), issuer.WithClock(clk.Now))
	if got := reason(func() error { _, err := v.Verify(ctx, forged.Mint(issuer.Claims{Kid: v.LocalKID()})); return err }()); got != ReasonSignature {
		t.Fatalf("forged local token: %q", got)
	}
	// A token with claims that are not an object, and a missing exp or
	// iat.
	if got := reason(func() error { _, err := v.Verify(ctx, parts[0]+".W10."+parts[2]); return err }()); got != ReasonMalformed {
		t.Fatalf("array claims: %q", got)
	}
	if _, err := NewVerifier(VerifierOptions{}); err == nil {
		t.Fatal("a verifier without a client")
	}
	if _, err := NewVerifier(VerifierOptions{Client: testClient()}); err == nil {
		t.Fatal("a verifier without the local key")
	}
}

func TestIssuerUnavailableIsRetried(t *testing.T) {
	if DefaultFetchTimeout != 5*time.Second || RetryInterval != time.Minute || RefreshInterval != time.Hour {
		t.Fatal("the fetch budgets are not spec 007's")
	}
	clk := newClock()
	up := issuer.New(t, issuer.WithClock(clk.Now))
	down := issuer.New(t, issuer.WithClock(clk.Now))
	down.Hang()
	v := newVerifier(t, clk, newKey(t), up, down)
	ctx := context.Background()
	// The start-up fetch: the hung discovery is abandoned after the
	// timeout and the node is up, serving the other issuer.
	start := time.Now()
	v.refresh(ctx, true)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the hung discovery held the start-up for %v", elapsed)
	}
	if _, err := v.Verify(ctx, up.Mint(issuer.Claims{})); err != nil {
		t.Fatal(err)
	}
	if got := reason(func() error { _, err := v.Verify(ctx, down.Mint(issuer.Claims{})); return err }()); got != ReasonIssuerUnavailable {
		t.Fatalf("hung issuer: %q", got)
	}
	// The issuer answers again, but the start-up attempt failed an
	// instant ago: still refused, with no fetch. A second on, the
	// request path fetches and the token verifies, without a restart
	// and without waiting for the minute loop.
	down.Resume()
	if got := reason(func() error { _, err := v.Verify(ctx, down.Mint(issuer.Claims{})); return err }()); got != ReasonIssuerUnavailable {
		t.Fatalf("before the first back-off: %q", got)
	}
	clk.Advance(FirstRetryInterval)
	if p, err := v.Verify(ctx, down.Mint(issuer.Claims{Sub: "late"})); err != nil || p.Subject != "late" {
		t.Fatalf("after the first back-off: %+v, %v", p, err)
	}
	// An hour on, the refresh sees a rotation.
	down.Rotate()
	clk.Advance(RefreshInterval)
	v.refresh(ctx, false)
	if _, err := v.Verify(ctx, down.Mint(issuer.Claims{})); err != nil {
		t.Fatalf("after the hourly refresh: %v", err)
	}
	// A discovery that answers without jwks_uri, a JWKS that is not JSON,
	// and a JWKS that answers 500 each leave the issuer unavailable.
	for name, handler := range map[string]http.HandlerFunc{
		"no jwks_uri": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"bad jwks": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/jwks") {
				_, _ = w.Write([]byte(`nope`))
				return
			}
			_, _ = w.Write([]byte(`{"jwks_uri":"http://` + r.Host + `/jwks"}`))
		},
		"jwks 500": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/jwks") {
				w.WriteHeader(500)
				return
			}
			_, _ = w.Write([]byte(`{"jwks_uri":"http://` + r.Host + `/jwks"}`))
		},
		"discovery 404": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) },
	} {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		if _, err := FetchKeys(ctx, testClient(), srv.URL, time.Second); err == nil {
			t.Errorf("%s: fetched", name)
		}
	}
	if _, err := FetchKeys(ctx, testClient(), "http://[::1]:namedport", time.Second); err == nil {
		t.Error("a bad URL fetched")
	}
	// Run fetches at start and stops with the context.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- newVerifier(t, clk, newKey(t), up).Run(runCtx) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
}

func TestJWKSParsingSkipsWhatItCannotUse(t *testing.T) {
	keys, err := parseJWKS([]byte(`{"keys":[
		{"kty":"EC","crv":"P-384","kid":"p384","x":"AA","y":"AA"},
		{"kty":"EC","crv":"P-256","kid":"badpoint","x":"AQ","y":"AQ"},
		{"kty":"EC","crv":"P-256","kid":"badb64","x":"!!","y":"AQ"},
		{"kty":"RSA","kid":"smalle","n":"AQ","e":"AQ"},
		{"kty":"RSA","kid":"empty","n":"","e":""},
		{"kty":"RSA","kid":"badb64","n":"!!","e":"AQAB"},
		{"kty":"oct","kid":"oct","k":"AA"},
		{"kty":"RSA","n":"AQ","e":"AQAB"},
		{"kty":"RSA","kid":"ok","n":"AQ","e":"AQAB"}]}`))
	if err != nil || len(keys) != 1 || keys["ok"] == nil {
		t.Fatalf("%v %v", keys, err)
	}
	if _, err := parseJWKS([]byte(`nope`)); err == nil {
		t.Fatal("bad JSON parsed")
	}
}

func FuzzParseToken(f *testing.F) {
	iss := issuer.NewHandler(issuer.WithIssuer("http://fuzz.example"))
	defer iss.Close()
	valid := iss.Mint(issuer.Claims{Sub: "alice", Act: "svc", Nbf: 1})
	f.Add(valid)
	f.Add("")
	f.Add("a.b.c")
	f.Add("e30.e30.")
	parts := strings.Split(valid, ".")
	f.Add(parts[0] + "." + parts[1])
	f.Add(parts[0] + ".W10." + parts[2])
	f.Add(parts[0] + "." + parts[1] + "." + strings.Repeat("A", 100))
	f.Add(strings.Repeat("x", MaxTokenBytes+1))
	f.Add(parts[0] + ".eyJhdWQiOjF9." + parts[2])
	key := newKey(f)
	v, err := NewVerifier(VerifierOptions{LocalIssuer: localIssuer, LocalKey: &key.PublicKey, Client: testClient()})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		tok, err := ParseToken(raw)
		if err != nil {
			if reason(err) == "" {
				t.Fatalf("not a refusal: %v", err)
			}
			return
		}
		_ = tok.verifySignature(&key.PublicKey)
		if _, err := v.Verify(context.Background(), raw); err == nil {
			t.Fatal("a fuzzed token verified")
		}
	})
}

// The start-up fetch of a node that came up before its issuer fails,
// and the fetch a request could trigger was then held for the whole
// minute of the retry interval: every token of the issuer was refused
// with issuer_unavailable for a minute after the issuer answered, which
// is what the kind stack of spec 013 met at start (up-script job). Until
// the first fetch succeeds, the pause after a failure is one second,
// doubled per consecutive failure and capped at the minute; after the
// first success the minute cap of the unknown-kid refresh applies as
// before.
func TestFirstFetchFailureBacksOffFromASecond(t *testing.T) {
	if FirstRetryInterval != time.Second {
		t.Fatal("the first retry is not a second")
	}
	clk := newClock()
	var stub *issuer.Server
	var attempts atomic.Int32
	var down atomic.Bool
	// The issuer behind a front that counts discovery fetches and
	// answers 503 while down; the stub names the front as its issuer.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/openid-configuration") {
			attempts.Add(1)
			if down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		stub.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	stub = issuer.NewHandler(issuer.WithIssuer(front.URL), issuer.WithClock(clk.Now))
	v := newVerifier(t, clk, newKey(t), stub)
	ctx := context.Background()
	verify := func(sub string) (string, string) {
		p, err := v.Verify(ctx, stub.Mint(issuer.Claims{Sub: sub}))
		return p.Subject, reason(err)
	}
	down.Store(true)
	v.refresh(ctx, true)
	if attempts.Load() != 1 {
		t.Fatalf("start-up attempts: %d", attempts.Load())
	}
	// Each pause is met on the request path: a request just before it
	// is refused with no fetch, one at it fetches, fails, and doubles it.
	want := int32(1)
	for _, pause := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, RetryInterval, RetryInterval} {
		clk.Advance(pause - time.Millisecond)
		if _, got := verify("a"); got != ReasonIssuerUnavailable || attempts.Load() != want {
			t.Fatalf("%v before the pause of %v: %q, %d attempts", pause-time.Millisecond, pause, got, attempts.Load())
		}
		clk.Advance(time.Millisecond)
		want++
		if _, got := verify("a"); got != ReasonIssuerUnavailable || attempts.Load() != want {
			t.Fatalf("at the pause of %v: %q, %d attempts, want %d", pause, got, attempts.Load(), want)
		}
	}
	// The issuer is up. Before the pause, still refused with no fetch; at
	// it, the request fetches and verifies.
	down.Store(false)
	clk.Advance(RetryInterval - time.Millisecond)
	if _, got := verify("b"); got != ReasonIssuerUnavailable || attempts.Load() != want {
		t.Fatalf("up, before the pause: %q, %d attempts", got, attempts.Load())
	}
	clk.Advance(time.Millisecond)
	want++
	if sub, got := verify("b"); got != "" || sub != "b" || attempts.Load() != want {
		t.Fatalf("up, at the pause: %q %q, %d attempts, want %d", sub, got, attempts.Load(), want)
	}
	// Fetched once, the minute cap holds: a token naming a new kid is
	// unknown_key without a fetch until the minute, whatever the
	// failures before the success.
	stub.Rotate()
	clk.Advance(FirstRetryInterval)
	if _, got := verify("c"); got != ReasonUnknownKey || attempts.Load() != want {
		t.Fatalf("rotated, a second after the fetch: %q, %d attempts", got, attempts.Load())
	}
	clk.Advance(RetryInterval - FirstRetryInterval)
	want++
	if sub, got := verify("c"); got != "" || sub != "c" || attempts.Load() != want {
		t.Fatalf("rotated, at the minute: %q %q, %d attempts, want %d", sub, got, attempts.Load(), want)
	}
	// A failed refresh of a fetched set keeps the minute cap: the keys
	// held serve, and the next attempt is a minute on.
	down.Store(true)
	stub.Rotate()
	clk.Advance(RetryInterval)
	want++
	if _, got := verify("d"); got != ReasonUnknownKey || attempts.Load() != want {
		t.Fatalf("refresh failed: %q, %d attempts, want %d", got, attempts.Load(), want)
	}
	clk.Advance(32 * time.Second)
	if _, got := verify("d"); got != ReasonUnknownKey || attempts.Load() != want {
		t.Fatalf("fetched set, 32 s after a failure: %q, %d attempts, want %d", got, attempts.Load(), want)
	}
}
