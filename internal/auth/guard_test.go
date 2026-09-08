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
		t.Fatal("a repository-bound token asked the authorizer")
	}
	// write allows read and write on A, and not admin.
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
	if len(stub.Requests()) != 1 {
		t.Fatal("the issuer's token did not ask the authorizer")
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
