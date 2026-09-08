// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// TestOrigoPreferNamesThePreferredNodes is spec 005's header on the
// smart HTTP routes: the preferred nodes for the repository, highest
// score first, as many as the allow's replicas, on every response that
// names a repository by id whatever the status, 1 for a
// repository-bound token, and absent on a 403 or a 404 to a name that
// the caller was not allowed to resolve.
func TestOrigoPreferNamesThePreferredNodes(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	store := wal.NewMemStore()
	l := wal.New(wal.Options{Store: store, Logger: logger})
	cache, err := repo.New(repo.Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	authz := authorizer.New(t)
	authz.Allow(authorizer.Rule{Subject: "alice", Replicas: 2})
	authz.Deny(authorizer.Rule{Subject: "eve"}, "no")
	client, err := auth.NewClient(auth.ClientOptions{URL: authz.URL(), Token: authz.Token(), HTTP: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	set := placement.NewSet("origod-0", nil)
	set.Heard("origod-1", time.Now())
	set.Heard("origod-2", time.Now())
	h := New(Options{Cache: cache, Logger: logger, Guard: auth.NewGuard(client, logger), Placement: set})
	mux := http.NewServeMux()
	h.Register(mux)
	var mu sync.Mutex
	principal := auth.Principal{Subject: "alice"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		p := principal
		mu.Unlock()
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	as := func(p auth.Principal) {
		mu.Lock()
		principal = p
		mu.Unlock()
	}
	get := func(path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+path, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get(placement.Header)
	}
	if _, err := l.CreateRepo(context.Background(), wal.Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main"); err != nil {
		t.Fatal(err)
	}
	two := strings.Join(set.Prefer(repoA, 2), ",")
	one := set.Prefer(repoA, 1)[0]
	if !strings.HasPrefix(two, one+",") {
		t.Fatalf("Prefer(2) %q does not start with Prefer(1) %q", two, one)
	}
	// Allowed with replicas 2, by id and by name.
	if status, got := get("/r/" + repoA + ".git/info/refs?service=git-upload-pack"); status != 200 || got != two {
		t.Fatalf("id form: %d %q, want %q", status, got, two)
	}
	if status, got := get("/acme/app.git/info/refs?service=git-upload-pack"); status != 200 || got != two {
		t.Fatalf("name form: %d %q, want %q", status, got, two)
	}
	// An unknown id is 404 with the header: the id is the path's.
	if status, got := get("/r/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.git/info/refs?service=git-upload-pack"); status != 404 || got == "" {
		t.Fatalf("unknown id: %d %q", status, got)
	}
	// An unknown name is 404 without it: nothing resolved.
	if status, got := get("/acme/nope.git/info/refs?service=git-upload-pack"); status != 404 || got != "" {
		t.Fatalf("unknown name: %d %q", status, got)
	}
	// A refused caller: the header on the id form, k = 1 because no
	// replicas value was decided; none on the name form.
	as(auth.Principal{Subject: "eve"})
	if status, got := get("/r/" + repoA + ".git/info/refs?service=git-upload-pack"); status != 403 || got != one {
		t.Fatalf("refused id form: %d %q, want %q", status, got, one)
	}
	if status, got := get("/acme/app.git/info/refs?service=git-upload-pack"); status != 403 || got != "" {
		t.Fatalf("refused name form: %d %q", status, got)
	}
	// A repository-bound token asks no authorizer and uses k = 1.
	as(auth.Principal{Subject: "ci", Bound: &auth.Bound{Repo: repoA, Scope: auth.ScopeRead}})
	if status, got := get("/r/" + repoA + ".git/info/refs?service=git-upload-pack"); status != 200 || got != one {
		t.Fatalf("bound token: %d %q, want %q", status, got, one)
	}
	// A single node names itself.
	solo := New(Options{Cache: cache, Logger: logger, Guard: auth.NewGuard(client, logger), Placement: placement.NewSet("solo", nil)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/r/"+repoA+".git/info/refs?service=git-upload-pack", nil)
	req.SetPathValue("id", repoA+".git")
	solo.infoRefs(rec, req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Subject: "alice"})))
	if rec.Code != 200 || rec.Header().Get(placement.Header) != "solo" {
		t.Fatalf("single node: %d %q", rec.Code, rec.Header().Get(placement.Header))
	}
	// Without a placer nothing is written.
	if status, got := func() (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/r/"+repoA+".git/info/refs?service=git-upload-pack", nil)
		req.SetPathValue("id", repoA+".git")
		New(Options{Cache: cache, Logger: logger, Guard: auth.NewGuard(client, logger)}).infoRefs(rec, req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Subject: "alice"})))
		return rec.Code, rec.Header().Get(placement.Header)
	}(); status != 200 || got != "" {
		t.Fatalf("no placer: %d %q", status, got)
	}
}
