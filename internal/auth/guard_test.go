// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/issuer"
)

// routes is a public surface in miniature: the verifier in front, then
// each route asks the guard for its action on the repository the path
// names, the way httpgit and api do.
func routes(v *Verifier, g *Guard) http.Handler {
	mux := http.NewServeMux()
	ask := func(action Action) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if g.Allow(w, r, RepoRef{ID: strings.TrimSuffix(r.PathValue("id"), ".git")}, action) {
				w.WriteHeader(http.StatusNoContent)
			}
		}
	}
	mux.HandleFunc("GET /r/{id}/info/refs", func(w http.ResponseWriter, r *http.Request) {
		action := ActionRead
		if r.URL.Query().Get("service") == "git-receive-pack" {
			action = ActionWrite
		}
		ask(action)(w, r)
	})
	mux.HandleFunc("POST /r/{id}/git-upload-pack", ask(ActionRead))
	mux.HandleFunc("POST /r/{id}/git-receive-pack", ask(ActionWrite))
	mux.HandleFunc("POST /v1/repos/{id}/tokens", ask(ActionAdmin))
	return contract.Middleware(v.Middleware(mux))
}

func do(t *testing.T, h http.Handler, method, path, token string) (int, httpjson.Error, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env httpjson.ErrorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env.Error, rec.Header()
}

func TestRepositoryBoundTokenScope(t *testing.T) {
	clk := newClock()
	key := newKey(t)
	stub := authorizer.New(t)
	stub.Deny(authorizer.Rule{Subject: "*"}, "the authorizer must not be asked")
	iss := issuer.New(t, issuer.WithClock(clk.Now))
	v := newVerifier(t, clk, key, iss)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	g := NewGuard(c, slog.New(slog.DiscardHandler))
	h := routes(v, g)
	signer := NewSigner(key, localIssuer, clk.Now)
	read, expires, err := signer.Mint(Principal{Subject: "ci", Actor: "svc"}, repoA, ScopeRead, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// read allows read on A, and nothing else.
	if code, _, _ := do(t, h, "GET", "/r/"+repoA+".git/info/refs?service=git-upload-pack", read); code != 204 {
		t.Fatalf("read on A: %d", code)
	}
	if code, _, _ := do(t, h, "POST", "/r/"+repoA+".git/git-upload-pack", read); code != 204 {
		t.Fatalf("upload-pack on A: %d", code)
	}
	for _, c := range []struct{ method, path, reason string }{
		{"POST", "/r/" + repoA + ".git/git-receive-pack", ReasonScope},
		{"GET", "/r/" + repoA + ".git/info/refs?service=git-receive-pack", ReasonScope},
		{"POST", "/v1/repos/" + repoA + "/tokens", ReasonScope},
		{"GET", "/r/" + repoB + ".git/info/refs?service=git-upload-pack", ReasonOtherRepository},
		{"POST", "/r/" + repoB + ".git/git-receive-pack", ReasonOtherRepository},
	} {
		code, e, _ := do(t, h, c.method, c.path, read)
		if code != 403 || e.Code != contract.CodeForbidden || e.Message != contract.Sentence(contract.CodeForbidden) || e.Details["reason"] != c.reason || e.Details["subject"] != "ci" {
			t.Errorf("%s %s: %d %+v", c.method, c.path, code, e)
		}
	}
	if len(stub.Requests()) != 0 {
		t.Fatal("a repository-bound token asked the authorizer for a read")
	}
	// write allows read and write on A, and not admin. A write asks the
	// authorizer once for the minting subject's quota_bytes (spec 012);
	// the answer decides no access, so this stub's deny leaves the
	// default figure and the push goes on.
	write, _, _ := signer.Mint(Principal{Subject: "ci"}, repoA, ScopeWrite, time.Minute)
	if code, _, _ := do(t, h, "POST", "/r/"+repoA+".git/git-receive-pack", write); code != 204 {
		t.Fatalf("write on A: %d", code)
	}
	if code, _, _ := do(t, h, "GET", "/r/"+repoA+".git/info/refs?service=git-upload-pack", write); code != 204 {
		t.Fatalf("read with write on A: %d", code)
	}
	if code, e, _ := do(t, h, "POST", "/v1/repos/"+repoA+"/tokens", write); code != 403 || e.Details["reason"] != ReasonScope {
		t.Fatalf("admin with write: %d %+v", code, e)
	}
	// One second after its exp the read token is expired, with no skew.
	clk.Advance(expires.Sub(clk.Now()) + time.Second)
	code, e, header := do(t, h, "GET", "/r/"+repoA+".git/info/refs?service=git-upload-pack", read)
	if code != 401 || e.Code != contract.CodeUnauthenticated || e.Details["reason"] != ReasonExpired || header.Get("WWW-Authenticate") != `Basic realm="origo"` || header.Get(contract.Header) != contract.Version {
		t.Fatalf("expired: %d %+v %v", code, e, header)
	}
	// An issuer's token goes to the authorizer, and a deny is 403 with
	// its reason.
	code, e, _ = do(t, h, "GET", "/r/"+repoA+".git/info/refs?service=git-upload-pack", iss.Mint(issuer.Claims{Sub: "alice"}))
	if code != 403 || e.Details["reason"] != "the authorizer must not be asked" || e.Details["action"] != "read" || e.Details["subject"] != "alice" {
		t.Fatalf("authorizer deny: %d %+v", code, e)
	}
	// Two calls: the bound token's write asked for its quota figure
	// (spec 012), and the issuer's token asked for the decision.
	if len(stub.Requests()) != 2 {
		t.Fatalf("%d authorizer calls, want the quota call and the decision", len(stub.Requests()))
	}
	// A bound token with an unknown scope allows nothing.
	if err := g.Authorize(context.Background(), Principal{Subject: "x", Bound: &Bound{Repo: repoA, Scope: "admin"}}, RepoRef{ID: repoA}, ActionRead); err == nil {
		t.Fatal("an unknown scope allowed read")
	}
}

func TestGuardRendersEveryRefusal(t *testing.T) {
	clk := newClock()
	stub := authorizer.New(t)
	stub.Fail(502)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	g := NewGuard(c, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil).WithContext(WithPrincipal(context.Background(), Principal{Subject: "alice"}))
	if g.Allow(rec, req, RepoRef{ID: repoA}, ActionRead) {
		t.Fatal("allowed under a 502")
	}
	var env httpjson.ErrorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != 503 || env.Error.Code != contract.CodeAuthorizerUnavailable || env.Error.Message != contract.Sentence(contract.CodeAuthorizerUnavailable) || env.Error.Details["status"] != 502.0 || env.Error.Details["url"] != stub.URL() || env.Error.Details["error"] != nil {
		t.Fatalf("%d %+v", rec.Code, env.Error)
	}
	// A transport failure carries the error and no status.
	rec = httptest.NewRecorder()
	WriteRefusal(rec, req, &Unavailable{URL: "u", Err: errors.New("dial failed")}, nil)
	env = httpjson.ErrorEnvelope{}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != 503 || env.Error.Details["error"] != "dial failed" || env.Error.Details["status"] != nil {
		t.Fatalf("transport: %d %+v", rec.Code, env.Error)
	}
	// Any other error is unavailable too: never fail open.
	rec = httptest.NewRecorder()
	WriteRefusal(rec, req, errors.New("odd"), slog.New(slog.DiscardHandler))
	env = httpjson.ErrorEnvelope{}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != 503 || env.Error.Details["error"] != "odd" {
		t.Fatalf("other: %d %+v", rec.Code, env.Error)
	}
	if (&Denied{Reason: "r"}).Error() == "" {
		t.Fatal("Denied.Error")
	}
}

func TestMiddlewareReadsEveryCredentialForm(t *testing.T) {
	clk := newClock()
	iss := issuer.New(t, issuer.WithClock(clk.Now))
	v := newVerifier(t, clk, newKey(t), iss)
	var seen Principal
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	token := iss.Mint(issuer.Claims{Sub: "svc", Act: "alice"})
	cases := []struct {
		name string
		set  func(*http.Request)
		code int
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }, 204},
		{"bearer with spaces", func(r *http.Request) { r.Header.Set("Authorization", "Bearer  "+token+" ") }, 204},
		{"basic any user", func(r *http.Request) { r.SetBasicAuth("x", token) }, 204},
		{"basic token as user", func(r *http.Request) { r.SetBasicAuth(token, "") }, 204},
		{"nothing", func(*http.Request) {}, 401},
		{"empty bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }, 401},
		{"empty basic", func(r *http.Request) { r.SetBasicAuth("", "") }, 401},
		{"other scheme", func(r *http.Request) { r.Header.Set("Authorization", "Token "+token) }, 401},
	}
	for _, c := range cases {
		seen = Principal{}
		req := httptest.NewRequest("GET", "/", nil)
		c.set(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%s: %d", c.name, rec.Code)
		}
		if c.code == 204 && (seen.Subject != "alice" || seen.Actor != "svc") {
			t.Errorf("%s: principal %+v", c.name, seen)
		}
		if c.code == 401 {
			var env httpjson.ErrorEnvelope
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Details["reason"] != ReasonMissing || rec.Header().Get("WWW-Authenticate") == "" || seen.Subject != "" {
				t.Errorf("%s: %+v", c.name, env.Error)
			}
		}
	}
	// An error that is not a refusal renders as malformed.
	rec := httptest.NewRecorder()
	Unauthenticated(rec, errors.New("odd"))
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"reason":"malformed"`) {
		t.Fatalf("%s", body)
	}
	if Subject(context.Background()) != "" || Actor(context.Background()) != "" {
		t.Fatal("principal without a context value")
	}
}

// TestDecideCarriesTheQuota is what the LFS batch of spec 010 reads:
// the authorizer's decision behind an allow, and the defaults of spec
// 007's table for a repository-bound token the authorizer never sees.
func TestDecideCarriesTheQuota(t *testing.T) {
	clk := newClock()
	stub := authorizer.New(t)
	stub.Allow(authorizer.Rule{Subject: "alice", QuotaBytes: 4096, TTL: 30})
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	g := NewGuard(c, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	d, err := g.Decide(ctx, Principal{Subject: "alice"}, RepoRef{ID: repoA}, ActionWrite)
	if err != nil || !d.Allow || d.QuotaBytes != 4096 || d.TTL != 30*time.Second {
		t.Fatalf("Decide = %+v, %v", d, err)
	}
	// A repository-bound token is decided by its own scope, so the
	// decision it yields is the table's defaults.
	bound := Principal{Subject: "build", Bound: &Bound{Repo: repoA, Scope: ScopeWrite}}
	d, err = g.Decide(ctx, bound, RepoRef{ID: repoA}, ActionWrite)
	if err != nil || !d.Allow || d.QuotaBytes != DefaultQuotaBytes || d.Replicas != DefaultReplicas || d.TTL != DefaultTTL {
		t.Fatalf("bound Decide = %+v, %v", d, err)
	}
	// Its refusals carry no decision.
	for _, tc := range []struct {
		name   string
		p      Principal
		repo   RepoRef
		action Action
		reason string
	}{
		{"another repository", bound, RepoRef{ID: repoB}, ActionWrite, ReasonOtherRepository},
		{"no id", bound, RepoRef{Owner: "acme", Slug: "app"}, ActionWrite, ReasonOtherRepository},
		{"scope", Principal{Subject: "build", Bound: &Bound{Repo: repoA, Scope: ScopeRead}}, RepoRef{ID: repoA}, ActionWrite, ReasonScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := g.Decide(ctx, tc.p, tc.repo, tc.action)
			denied, ok := errors.AsType[*Denied](err)
			if !ok || denied.Reason != tc.reason || d.Allow {
				t.Fatalf("Decide = %+v, %v", d, err)
			}
		})
	}
	// A deny and an outage each yield the zero decision.
	stub.Deny(authorizer.Rule{Subject: "mallory"}, "no")
	if d, err := g.Decide(ctx, Principal{Subject: "mallory"}, RepoRef{ID: repoA}, ActionRead); d.Allow || err == nil {
		t.Fatalf("deny = %+v, %v", d, err)
	}
	stub.Fail(502)
	if d, err := g.Decide(ctx, Principal{Subject: "carol"}, RepoRef{ID: repoB}, ActionRead); d.Allow || err == nil {
		t.Fatalf("outage = %+v, %v", d, err)
	}
}

// TestDecideCarriesTheReplicas is spec 005's reading of the guard: an
// allow from the authorizer carries its replicas value, a
// repository-bound token, which never asks the authorizer, carries the
// default of 1, and a deny carries no decision.
func TestDecideCarriesTheReplicas(t *testing.T) {
	stub := authorizer.New(t)
	stub.Allow(authorizer.Rule{Subject: "alice", Repo: repoA, Action: "read", Replicas: 3})
	stub.Deny(authorizer.Rule{Subject: "bob", Repo: repoA, Action: "read"}, "no")
	client, err := NewClient(ClientOptions{URL: stub.URL(), Token: stub.Token(), HTTP: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	g := NewGuard(client, nil)
	ctx := context.Background()
	d, err := g.Decide(ctx, Principal{Subject: "alice"}, RepoRef{ID: repoA}, ActionRead)
	if err != nil || !d.Allow || d.Replicas != 3 {
		t.Fatalf("alice: %+v %v", d, err)
	}
	if _, err := g.Decide(ctx, Principal{Subject: "bob"}, RepoRef{ID: repoA}, ActionRead); err == nil {
		t.Fatal("bob was allowed")
	}
	d, err = g.Decide(ctx, Principal{Subject: "ci", Bound: &Bound{Repo: repoA, Scope: ScopeRead}}, RepoRef{ID: repoA}, ActionRead)
	if err != nil || !d.Allow || d.Replicas != DefaultReplicas || len(stub.Requests()) != 2 {
		t.Fatalf("bound: %+v %v, %d authorizer calls", d, err, len(stub.Requests()))
	}
	req := httptest.NewRequest("GET", "/", nil).WithContext(WithPrincipal(ctx, Principal{Subject: "alice"}))
	rec := httptest.NewRecorder()
	if d, ok := g.Admit(rec, req, RepoRef{ID: repoA}, ActionRead); !ok || d.Replicas != 3 {
		t.Fatalf("Admit: %+v %v", d, ok)
	}
	req = httptest.NewRequest("GET", "/", nil).WithContext(WithPrincipal(ctx, Principal{Subject: "bob"}))
	rec = httptest.NewRecorder()
	if _, ok := g.Admit(rec, req, RepoRef{ID: repoA}, ActionRead); ok || rec.Code != 403 {
		t.Fatalf("Admit of a deny: %v %d", ok, rec.Code)
	}
}

// TestBoundTokenWriteTakesTheMintersQuota is spec 012's end to spec
// 010's interim rule: a repository-bound token carries no quota claim,
// so a write asks the authorizer for the minting subject's figure with
// the token's own subject and actor, and reads it off the allow. A read
// asks nothing, and an authorizer that gives no figure leaves the
// default rather than refusing a write the scope allows.
func TestBoundTokenWriteTakesTheMintersQuota(t *testing.T) {
	clk := newClock()
	stub := authorizer.New(t)
	stub.SetRules(authorizer.Rule{Subject: "ci", Repo: repoA, Action: "write", Allow: true, QuotaBytes: 4096})
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	g := NewGuard(c, slog.New(slog.DiscardHandler))
	bound := Principal{Subject: "ci", Actor: "svc", Bound: &Bound{Repo: repoA, Scope: ScopeWrite}}

	d, err := g.Decide(context.Background(), bound, RepoRef{ID: repoA}, ActionWrite)
	if err != nil || d.QuotaBytes != 4096 {
		t.Fatalf("write: %+v %v", d, err)
	}
	seen := stub.Requests()
	if len(seen) != 1 || seen[0].Subject != "ci" || seen[0].Actor != "svc" || seen[0].Action != "write" || seen[0].Repo.ID != repoA {
		t.Fatalf("the quota call: %+v", seen)
	}
	// The read path asks nothing and keeps the default.
	d, err = g.Decide(context.Background(), bound, RepoRef{ID: repoA}, ActionRead)
	if err != nil || d.QuotaBytes != DefaultQuotaBytes {
		t.Fatalf("read: %+v %v", d, err)
	}
	if len(stub.Requests()) != 1 {
		t.Fatalf("a bound read asked the authorizer: %+v", stub.Requests())
	}
	// A deny, and an allow with no figure, leave the default: the scope,
	// not the authorizer, decides a bound token's access.
	stub.Deny(authorizer.Rule{Subject: "ci", Repo: repoB, Action: "write"}, "not the minter")
	other := Principal{Subject: "ci", Bound: &Bound{Repo: repoB, Scope: ScopeWrite}}
	d, err = g.Decide(context.Background(), other, RepoRef{ID: repoB}, ActionWrite)
	if err != nil || d.QuotaBytes != DefaultQuotaBytes {
		t.Fatalf("deny: %+v %v", d, err)
	}
	// An authorizer that produces no answer is the one exception: the
	// write fails closed, TestBoundTokenWriteFailsClosedDuringAuthorizerOutage.
	// A repository the cache has no answer for, so the outage is asked.
	const repoC = "2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6e"
	stub.Fail(503)
	third := Principal{Subject: "ci", Bound: &Bound{Repo: repoC, Scope: ScopeWrite}}
	if _, err := g.Decide(context.Background(), third, RepoRef{ID: repoC}, ActionWrite); !errors.As(err, new(*Unavailable)) {
		t.Fatalf("outage: %v", err)
	}
}

// TestBoundTokenWriteFailsClosedDuringAuthorizerOutage is spec 012's
// rule, fixed by spec 016's build: a bound token's write during an
// authorizer outage is refused with authorizer_unavailable, the way
// every unbound write on the node is, and never falls back to the
// default quota; a read under the same token still answers, because it
// asks nothing; and the write is allowed again once the authorizer
// answers.
func TestBoundTokenWriteFailsClosedDuringAuthorizerOutage(t *testing.T) {
	clk := newClock()
	stub := authorizer.New(t)
	stub.SetRules(authorizer.Rule{Subject: "ci", Repo: repoA, Action: "write", Allow: true, QuotaBytes: 4096})
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	g := NewGuard(c, slog.New(slog.DiscardHandler))
	bound := Principal{Subject: "ci", Bound: &Bound{Repo: repoA, Scope: ScopeWrite}}
	ctx := context.Background()

	stub.Fail(503)
	_, err := g.Decide(ctx, bound, RepoRef{ID: repoA}, ActionWrite)
	var u *Unavailable
	if !errors.As(err, &u) || u.Status != 503 {
		t.Fatalf("write during the outage: %v", err)
	}
	req := httptest.NewRequest("POST", "/", nil).WithContext(WithPrincipal(ctx, bound))
	rec := httptest.NewRecorder()
	if _, ok := g.Admit(rec, req, RepoRef{ID: repoA}, ActionWrite); ok || rec.Code != 503 || !strings.Contains(rec.Body.String(), contract.CodeAuthorizerUnavailable) {
		t.Fatalf("Admit during the outage: %v %d %s", ok, rec.Code, rec.Body)
	}
	if d, err := g.Decide(ctx, bound, RepoRef{ID: repoA}, ActionRead); err != nil || !d.Allow {
		t.Fatalf("read during the outage: %+v %v", d, err)
	}
	stub.Fail(0)
	if d, err := g.Decide(ctx, bound, RepoRef{ID: repoA}, ActionWrite); err != nil || d.QuotaBytes != 4096 {
		t.Fatalf("write after the outage: %+v %v", d, err)
	}
}
