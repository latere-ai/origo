// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit"
	"latere.ai/x/pkg/authz"

	authorizerstub "github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/issuer"
)

// fixedServer serves one body as a 200 and returns its URL, for an
// authorizer that does not read the request.
func fixedServer(t *testing.T, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// The acceptance tests of Origo spec 028. Each proves one row of the
// spec's criteria table; the verifier row is
// TestVerifierAcceptsTwoIssuersAndRefusesEachFailure in verifier_test.go,
// and the conformance-suite row is test/conformance's authorizer group.

// principalCtx is a request context carrying an issuer's principal and a
// caller block, the way the middleware builds it.
func principalCtx(iss, sub string, claims map[string]any) context.Context {
	p := Principal{Subject: authz.Subject(iss, sub), Issuer: iss, Sub: sub, Claims: claims}
	ctx := WithPrincipal(context.Background(), p)
	return WithCaller(ctx, Caller{ID: "req-1", IP: "203.0.113.4", UserAgent: "git/2.47"})
}

// TestAuthorizerEnvelope is the first acceptance row: every operation
// reaches the authorizer with the action, a resource of kind Repository,
// issuer and sub apart, every claim of the token in claims, and a request
// block. It is table-driven over the four actions the guard builds.
func TestAuthorizerEnvelope(t *testing.T) {
	stub := authorizerstub.New(t)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, newClock(), nil)
	g := NewGuard(c, nil)
	iss := "https://auth.example.com"
	claims := map[string]any{"iss": iss, "sub": "0f5c", "roles": []any{"member"}}
	ctx := principalCtx(iss, "0f5c", claims)
	p := FromContext(ctx)
	ref := RepoRef{ID: repoA, Owner: "acme", Slug: "app"}

	for _, action := range []Action{ActionRead, ActionWrite, ActionAdmin} {
		if _, err := g.Decide(ctx, p, ref, action); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if _, err := g.Directory(ctx, p, "cur", 10); err != nil {
		t.Fatalf("list: %v", err)
	}
	reqs := stub.Requests()
	if len(reqs) != 4 {
		t.Fatalf("the four operations made %d calls", len(reqs))
	}
	for i, want := range []string{"repo.read", "repo.write", "repo.admin", "repo.list"} {
		r := reqs[i]
		if r.Action != want {
			t.Errorf("call %d action %q, want %q", i, r.Action, want)
		}
		if r.Resource.Kind != ResourceKind {
			t.Errorf("call %d resource kind %q", i, r.Resource.Kind)
		}
		if r.Subject != authz.Subject(iss, "0f5c") || r.Issuer != iss || r.Sub != "0f5c" {
			t.Errorf("call %d subject/issuer/sub: %q %q %q", i, r.Subject, r.Issuer, r.Sub)
		}
		if r.Claims["roles"] == nil || r.Claims["sub"] != "0f5c" {
			t.Errorf("call %d claims not forwarded verbatim: %v", i, r.Claims)
		}
		if r.Request.ID != "req-1" || r.Request.IP != "203.0.113.4" || r.Request.UserAgent != "git/2.47" {
			t.Errorf("call %d request block: %+v", i, r.Request)
		}
	}
	// The three actions that name a repository carry its id; the list
	// names none and carries the cursor and limit in the resource.
	for i := range 3 {
		if reqs[i].Resource.ID != repoA {
			t.Errorf("call %d resource id %q", i, reqs[i].Resource.ID)
		}
	}
	if reqs[3].Resource.ID != "" || reqs[3].Resource.String("cursor") != "cur" || reqs[3].Resource.Int("limit") != 10 {
		t.Errorf("the list resource is %+v", reqs[3].Resource)
	}
}

// TestContractOneIsGone is the first row's other half: no code path builds
// the contract-1 body. The client's source names neither a repo nor an
// actor key, and the envelope the guard marshals carries a resource and
// no repo or actor.
func TestContractOneIsGone(t *testing.T) {
	for _, file := range []string{"authorizer.go", "guard.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		// The contract-1 envelope keys and their struct tags. A log field
		// named repo is not an envelope key, so the JSON-key forms are what
		// the check names.
		for _, banned := range []string{`"repo":`, `"actor":`, `json:"repo"`, `json:"actor"`} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s still names %s of contract 1", file, banned)
			}
		}
	}
	// The envelope on the wire.
	env := envelope(principalCtx("https://iss", "s", nil), Principal{Subject: "https://iss|s", Issuer: "https://iss", Sub: "s"}, RepoRef{ID: repoA, Owner: "acme", Slug: "app"}, ActionRead)
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["repo"]; ok {
		t.Errorf("the envelope carries a repo key: %s", raw)
	}
	if _, ok := m["actor"]; ok {
		t.Errorf("the envelope carries an actor key: %s", raw)
	}
	if _, ok := m["resource"]; !ok {
		t.Errorf("the envelope carries no resource: %s", raw)
	}
}

// TestFiguresReachTheirConsumers is the second row: the three figures are
// read from limits and reach the Decision the handlers consume.
func TestFiguresReachTheirConsumers(t *testing.T) {
	stub := authorizerstub.New(t)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, newClock(), nil)
	stub.Allow(authorizerstub.Rule{Subject: "*", Resource: repoA, Action: "repo.read",
		Limits: map[string]any{"replicas": 3, "quota_bytes": 4096, "requests_per_minute": 120}})
	d, err := c.Authorize(context.Background(), request("alice", repoA, ActionRead))
	if err != nil || !d.Allow {
		t.Fatalf("allow: %+v, %v", d, err)
	}
	if d.Replicas != 3 || d.QuotaBytes != 4096 || d.RequestsPerMinute != 120 {
		t.Fatalf("figures did not reach the decision: %+v", d)
	}
	// An answer with no limits carries the defaults, and requests_per_minute
	// stays zero, which the node reads as its own configured rate.
	stub.SetRules()
	stub.Allow(authorizerstub.Rule{Subject: "*", Resource: repoB, Action: "repo.read"})
	d, err = c.Authorize(context.Background(), request("alice", repoB, ActionRead))
	if err != nil || d.Replicas != DefaultReplicas || d.QuotaBytes != DefaultQuotaBytes || d.RequestsPerMinute != 0 {
		t.Fatalf("defaults: %+v, %v", d, err)
	}
}

// TestSubjectsAreIssuerQualified is the third row: a subject is <iss>|<sub>,
// and two issuers agreeing on a sub are two subjects.
func TestSubjectsAreIssuerQualified(t *testing.T) {
	clk := newClock()
	a := issuer.New(t, issuer.WithClock(clk.Now))
	b := issuer.New(t, issuer.WithClock(clk.Now))
	v := newVerifier(t, clk, newKey(t), a, b)
	ctx := context.Background()

	pa, err := v.Verify(ctx, a.Mint(issuer.Claims{Sub: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := v.Verify(ctx, b.Mint(issuer.Claims{Sub: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	if pa.Subject == pb.Subject {
		t.Fatalf("two issuers agreeing on a sub are one subject: %q", pa.Subject)
	}
	if pa.Subject != authz.Subject(a.URL(), "alice") || pa.Issuer != a.URL() || pa.Sub != "alice" {
		t.Fatalf("subject a is %q, issuer %q, sub %q", pa.Subject, pa.Issuer, pa.Sub)
	}
	if !strings.HasPrefix(pa.Subject, a.URL()+"|") {
		t.Fatalf("the subject is not <iss>|<sub>: %q", pa.Subject)
	}
}

// TestAudienceIsConfigurable is the fifth row: ORIGO_OIDC_AUDIENCE changes
// the accepted audience and defaults to origo.
func TestAudienceIsConfigurable(t *testing.T) {
	clk := newClock()
	iss := issuer.New(t, issuer.WithClock(clk.Now))
	ctx := context.Background()

	// The default accepts origo and refuses another.
	def := newVerifier(t, clk, newKey(t), iss)
	if _, err := def.Verify(ctx, iss.Mint(issuer.Claims{Sub: "a"})); err != nil {
		t.Fatalf("the default audience rejected origo: %v", err)
	}
	if _, err := def.Verify(ctx, iss.Mint(issuer.Claims{Sub: "a", Aud: issuer.StringList{"code.example"}})); !reasonIs(err, ReasonAudience) {
		t.Fatalf("the default audience accepted another: %v", err)
	}

	// A configured audience accepts its value and refuses origo.
	v, err := NewVerifier(VerifierOptions{
		Issuers: []string{iss.URL()}, Audience: "code.example", LocalIssuer: localIssuer, LocalKey: &newKey(t).PublicKey,
		Client: testClient(), Now: clk.Now, FetchTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, iss.Mint(issuer.Claims{Sub: "a", Aud: issuer.StringList{"code.example"}})); err != nil {
		t.Fatalf("the configured audience rejected its value: %v", err)
	}
	if _, err := v.Verify(ctx, iss.Mint(issuer.Claims{Sub: "a"})); !reasonIs(err, ReasonAudience) {
		t.Fatalf("the configured audience accepted origo: %v", err)
	}
}

// TestCheckProbesTheAuthorizer is the seventh row: origod check sends the
// probe in contract 2 and reads an allow as an endpoint that does not read
// the request.
func TestCheckProbesTheAuthorizer(t *testing.T) {
	ctx := context.Background()

	// A stub denies the probe id, so Check passes.
	good := authorizerstub.New(t)
	c := newClient(t, good.URL(), good.Token(), &http.Transport{}, newClock(), nil)
	if err := c.Check(ctx); err != nil {
		t.Fatalf("a conforming authorizer failed the check: %v", err)
	}
	reqs := good.Requests()
	if len(reqs) != 1 || reqs[0].Resource.ID != authz.ProbeID || reqs[0].Subject != "" || reqs[0].Action != "repo.read" {
		t.Fatalf("the probe was not sent in contract 2: %+v", reqs)
	}

	// An endpoint that allows the probe fails the check.
	bad := newClient(t, fixedServer(t, `{"allow":true}`), "t", &http.Transport{}, newClock(), nil)
	if err := bad.Check(ctx); err == nil {
		t.Fatal("an authorizer that allows the probe passed the check")
	}
	// An unavailable endpoint fails the check too.
	down := newClient(t, "http://127.0.0.1:1/", "t", &http.Transport{}, newClock(), nil)
	if err := down.Check(ctx); err == nil {
		t.Fatal("an unavailable authorizer passed the check")
	}
}

// TestTheEnvelopeCarriesTheTwoClaims is id-13's envelope row. The node
// reads neither claim, so what the decision point is handed has to be
// what the token carried: a push by a key holder sends token_use and
// authorization_details verbatim, and a token that carries neither sends
// neither. The envelope itself does not change, which is why forwarding
// them is inside contract 2 (Origo spec 028) rather than a change to it.
func TestTheEnvelopeCarriesTheTwoClaims(t *testing.T) {
	clk := newClock()
	iss := issuer.New(t, issuer.WithClock(clk.Now))
	v := newVerifier(t, clk, newKey(t), iss)
	stub := authorizerstub.New(t)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	g := NewGuard(c, nil)
	ctx := context.Background()
	ref := RepoRef{ID: repoA, Owner: "acme", Slug: "app"}

	for _, row := range []struct {
		name   string
		raw    string
		grants bool
	}{
		{"a personal access token", iss.Mint(patClaims("alice", grant("origo:repo.write", repoA))), true},
		{"a session token", iss.Mint(issuer.Claims{Sub: "bob"}), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, err := v.Verify(ctx, row.raw)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			stub.ClearRequests()
			if _, err := g.Decide(WithCaller(ctx, Caller{ID: "req-1"}), p, ref, ActionWrite); err != nil {
				t.Fatalf("decide: %v", err)
			}
			reqs := stub.Requests()
			if len(reqs) != 1 {
				t.Fatalf("%d calls", len(reqs))
			}
			claims := reqs[0].Claims
			use, hasUse := claims["token_use"]
			details, hasDetails := claims["authorization_details"]
			if hasUse != row.grants || hasDetails != row.grants {
				t.Fatalf("token_use %v (%v), authorization_details %v (%v)", use, hasUse, details, hasDetails)
			}
			if !row.grants {
				return
			}
			if use != authkit.TokenUsePAT {
				t.Errorf("token_use %v", use)
			}
			// Verbatim: the entry the endpoint reads is the entry the
			// token carried, field for field.
			got, err := json.Marshal(details)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal([]any{grant("origo:repo.write", repoA)})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("authorization_details %s, want %s", got, want)
			}
		})
	}
}
