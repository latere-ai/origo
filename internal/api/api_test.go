// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/httpgit"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sink"
)

const (
	repoA   = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	unknown = "00000000-0000-4000-8000-000000000000"
	issuer  = "https://git.example.com"
)

type harness struct {
	t       *testing.T
	store   *wal.MemStore
	log     *wal.Log
	cache   *repo.Cache
	srv     *httptest.Server
	authz   *authorizer.Server
	guard   *auth.Guard
	key     *ecdsa.PrivateKey
	signer  *auth.Signer
	limits  *limits.Limits
	handler *Handler

	mu        sync.Mutex
	principal auth.Principal
}

// harnessOption changes how a harness is built.
type harnessOption func(*harnessConfig)

type harnessConfig struct {
	store       *wal.MemStore
	wrap        func(wal.Store) wal.Store
	gitBin      string
	readTimeout time.Duration
	sink        *sink.Server
	placement   placement.Placer
	limits      *limits.Options
	egress      *Egress
	loopback    bool
}

// withEgress gives the handler the egress rules of spec 016 and, when
// loopback is set, the AllowLoopback seam that admits an in-process
// source; the seam is written here and in no file that is not a test.
func withEgress(e *Egress, loopback bool) harnessOption {
	return func(c *harnessConfig) { c.egress, c.loopback = e, loopback }
}

// withSink runs an event dispatcher delivering to the stub sink.
func withSink(s *sink.Server) harnessOption {
	return func(c *harnessConfig) { c.sink = s }
}

// withPlacement answers Origo-Prefer from the set.
func withPlacement(p placement.Placer) harnessOption {
	return func(c *harnessConfig) { c.placement = p }
}

// withStore shares a store between harnesses, two nodes over one log.
func withStore(store *wal.MemStore) harnessOption {
	return func(c *harnessConfig) { c.store = store }
}

// withWrap puts a wrapper, the breaker store of spec 015, between the
// log and the store.
func withWrap(wrap func(wal.Store) wal.Store) harnessOption {
	return func(c *harnessConfig) { c.wrap = wrap }
}

// withGit runs every subprocess through the binary at path.
func withGit(path string) harnessOption {
	return func(c *harnessConfig) { c.gitBin = path }
}

// withReadTimeout lowers the read API's budget.
// withLimits gives the handler the bounds of spec 012, so a test drives
// a subprocess semaphore of one slot.
func withLimits(o limits.Options) harnessOption {
	return func(c *harnessConfig) { c.limits = &o }
}

func withReadTimeout(d time.Duration) harnessOption {
	return func(c *harnessConfig) { c.readTimeout = d }
}

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	var cfg harnessConfig
	for _, o := range opts {
		o(&cfg)
	}
	store := cfg.store
	if store == nil {
		store = wal.NewMemStore()
	}
	logger := slog.New(slog.DiscardHandler)
	var logStore wal.Store = store
	if cfg.wrap != nil {
		logStore = cfg.wrap(store)
	}
	l := wal.New(wal.Options{Store: logStore, Logger: logger})
	cache, err := repo.New(repo.Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: logger, GitBin: cfg.gitBin})
	if err != nil {
		t.Fatal(err)
	}
	authz := authorizer.New(t)
	client, err := auth.NewClient(auth.ClientOptions{URL: authz.URL(), Token: authz.Token(), HTTP: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: store, log: l, cache: cache, authz: authz, key: key, principal: auth.Principal{Subject: "alice"}}
	h.guard = auth.NewGuard(client, logger)
	h.signer = auth.NewSigner(key, issuer, nil)
	var dispatcher *events.Dispatcher
	if cfg.sink != nil {
		dispatcher, err = events.New(events.Options{Log: l, Node: "n1", URL: cfg.sink.URL(), Secret: cfg.sink.Secret(), Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() { _ = dispatcher.Run(ctx) }()
	}
	if cfg.limits != nil {
		cfg.limits.Log, cfg.limits.Logger = l, logger
		h.limits = limits.New(*cfg.limits)
	}
	mux := http.NewServeMux()
	h.handler = New(Options{Cache: cache, Logger: logger, Guard: h.guard, Signer: h.signer, ReadTimeout: cfg.readTimeout, Events: dispatcher, Placement: cfg.placement, Limits: h.limits, Egress: cfg.egress, AllowLoopback: cfg.loopback})
	h.handler.Register(mux)
	httpgit.New(httpgit.Options{Cache: cache, Logger: logger, Guard: h.guard}).Register(mux)
	// The verifier is spec 007's own; here the principal is set on the
	// request the way the middleware does.
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		p := h.principal
		h.mu.Unlock()
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// as makes the following requests carry the principal.
func (h *harness) as(p auth.Principal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.principal = p
}

func (h *harness) do(method, path, body string) (int, map[string]any) {
	h.t.Helper()
	status, out, _ := h.doHeader(method, path, body)
	return status, out
}

// doHeader is do with the response headers.
func (h *harness) doHeader(method, path, body string) (int, map[string]any, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, h.srv.URL+path, strings.NewReader(body))
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

func details(out map[string]any) map[string]any {
	if e, ok := out["error"].(map[string]any); ok {
		d, _ := e["details"].(map[string]any)
		return d
	}
	return nil
}

func code(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

func TestRepositoryLifecycle(t *testing.T) {
	h := newHarness(t)
	status, out := h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app","default_branch":"trunk"}`)
	if status != 201 || out["id"] != repoA || out["default_branch"] != "trunk" || out["head"] != "" || out["size_bytes"] != float64(0) {
		t.Fatalf("create: %d %v", status, out)
	}
	if status, out := h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"other"}`); status != 409 || code(out) != contract.CodeRepoExists || details(out)["field"] != "id" {
		t.Fatalf("duplicate id: %d %v", status, out)
	}
	if status, out := h.do("POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"acme","slug":"app"}`); status != 409 || code(out) != contract.CodeRepoExists || details(out)["field"] != "name" {
		t.Fatalf("duplicate name: %d %v", status, out)
	}
	// Every message is the contract's sentence, with the reason in the
	// developer detail.
	if _, out := h.do("POST", "/v1/repos", `{"id":"x","owner":"a","slug":"b"}`); out["error"].(map[string]any)["message"] != contract.Sentence(contract.CodeInvalid) || details(out)["field"] != "id" || details(out)["reason"] == nil {
		t.Fatalf("invalid envelope: %v", out)
	}
	for name, body := range map[string]string{
		"not json":       `{`,
		"unknown field":  `{"id":"` + repoA + `","owner":"a","slug":"b","x":1}`,
		"bad id":         `{"id":"x","owner":"a","slug":"b"}`,
		"reserved owner": `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"r","slug":"b"}`,
		"bad slug":       `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"b c"}`,
		"bad branch":     `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"b","default_branch":"a..b"}`,
	} {
		if status, out := h.do("POST", "/v1/repos", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	if status, out := h.do("GET", "/v1/repos/"+repoA, ""); status != 200 || out["owner"] != "acme" || out["slug"] != "app" {
		t.Fatalf("get: %d %v", status, out)
	}
	if status, out := h.do("GET", "/v1/repos/00000000-0000-4000-8000-000000000000", ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("get unknown: %d %v", status, out)
	}
	if status, out := h.do("GET", "/v1/repos/nope", ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("get invalid: %d %v", status, out)
	}

	// Rename, then the default branch through the log.
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"slug":"app2"}`); status != 200 || out["slug"] != "app2" || out["owner"] != "acme" {
		t.Fatalf("rename: %d %v", status, out)
	}
	if id, err := h.log.Resolve(context.Background(), "acme", "app2"); err != nil || id != repoA {
		t.Fatal("new name does not resolve")
	}
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"owner":"acme2","default_branch":"main"}`); status != 200 || out["owner"] != "acme2" || out["default_branch"] != "main" {
		t.Fatalf("rename owner and branch: %d %v", status, out)
	}
	ix, _, _ := h.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 1 || ix.Refs["HEAD"] != "ref: refs/heads/main" {
		t.Fatalf("HEAD not moved through the log: %+v", ix)
	}
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"default_branch":"main"}`); status != 200 {
		t.Fatalf("same branch: %d %v", status, out)
	}
	if ix, _, _ := h.log.Newest(context.Background(), repoA, 0, false); ix.Seq != 1 {
		t.Fatal("a no-op patch moved the log")
	}
	h.do("POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"acme","slug":"taken"}`)
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"owner":"acme","slug":"taken"}`); status != 409 || code(out) != contract.CodeRepoExists {
		t.Fatalf("rename onto taken: %d %v", status, out)
	}
	for name, body := range map[string]string{
		"not json":   `{`,
		"bad label":  `{"slug":"a b"}`,
		"reserved":   `{"owner":"v1"}`,
		"bad branch": `{"default_branch":"a..b"}`,
	} {
		if status, out := h.do("PATCH", "/v1/repos/"+repoA, body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("patch %s: %d %v", name, status, out)
		}
	}
	if status, out := h.do("PATCH", "/v1/repos/00000000-0000-4000-8000-000000000000", `{"slug":"x"}`); status != 404 {
		t.Fatalf("patch unknown: %d %v", status, out)
	}

	// The subject and the actor of the caller are in the entry a patch
	// of the default branch commits.
	h.as(auth.Principal{Subject: "bob", Actor: "svc"})
	if status, _ := h.do("PATCH", "/v1/repos/"+repoA, `{"default_branch":"trunk"}`); status != 200 {
		t.Fatal("patch as bob")
	}
	ix, _, _ = h.log.Newest(context.Background(), repoA, 0, false)
	rc, _, _ := h.store.Get(context.Background(), h.log.RepoPrefix(repoA)+ix.Entry, "")
	hdr, _, _, err := wal.ReadEntryHead(rc)
	_ = rc.Close()
	if err != nil || hdr.Subject != "bob" || hdr.Actor != "svc" {
		t.Fatalf("entry header: %+v, %v", hdr, err)
	}
	h.as(auth.Principal{Subject: "alice"})

	// Delete holds the objects and answers 404 until an undelete.
	if status, out := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 || out["deleted_at"] == nil || out["purge_after"] == nil {
		t.Fatalf("delete: %d %v", status, out)
	}
	if status, _ := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 {
		t.Fatalf("second delete: %d", status)
	}
	if status, out := h.do("GET", "/v1/repos/"+repoA, ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("get deleted: %d %v", status, out)
	}
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"slug":"x"}`); status != 404 {
		t.Fatalf("patch deleted: %d %v", status, out)
	}
	if _, _, err := h.cache.Acquire(context.Background(), repoA, false); !errors.Is(err, repo.ErrDeleted) {
		t.Fatalf("cache after delete: %v", err)
	}
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 || out["id"] != repoA {
		t.Fatalf("undelete: %d %v", status, out)
	}
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 {
		t.Fatal("second undelete")
	}
	if status, _ := h.do("GET", "/v1/repos/"+repoA, ""); status != 200 {
		t.Fatal("get after undelete")
	}
	if status, _ := h.do("POST", "/v1/repos/00000000-0000-4000-8000-000000000000/undelete", ""); status != 404 {
		t.Fatal("undelete unknown")
	}
	if status, _ := h.do("DELETE", "/v1/repos/00000000-0000-4000-8000-000000000000", ""); status != 404 {
		t.Fatal("delete unknown")
	}
}

func TestStorageFailuresAnswer503(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app"}`)
	fail := func(ops ...string) {
		h.store.SetFault(func(op, key string) error {
			for _, o := range ops {
				if o == op || (strings.HasPrefix(o, "!") && op != o[1:]) {
					return errors.New("storage down")
				}
			}
			return nil
		})
	}
	check := func(name, method, path, body string) {
		t.Helper()
		if status, out := h.do(method, path, body); status != 503 || code(out) != contract.CodeStorageUnavailable {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	fail("Create")
	check("create", "POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"b"}`)
	fail("Get")
	check("get meta", "GET", "/v1/repos/"+repoA, "")
	check("patch", "PATCH", "/v1/repos/"+repoA, `{"slug":"x"}`)
	check("delete", "DELETE", "/v1/repos/"+repoA, "")
	check("undelete", "POST", "/v1/repos/"+repoA+"/undelete", "")
	fail("Head")
	check("get index", "GET", "/v1/repos/"+repoA, "")
	fail("Put")
	check("patch branch", "PATCH", "/v1/repos/"+repoA, `{"default_branch":"dev"}`)
	check("delete entry", "DELETE", "/v1/repos/"+repoA, "")
	check("rename meta", "PATCH", "/v1/repos/"+repoA, `{"slug":"y"}`)
	h.store.SetFault(nil)
	h.do("DELETE", "/v1/repos/"+repoA, "")
	fail("Put")
	check("undelete entry", "POST", "/v1/repos/"+repoA+"/undelete", "")
	h.store.SetFault(nil)
	// A repository whose metadata exists but whose index vanished is
	// not found, and a create whose read-back fails is 503.
	_ = h.store.Delete(context.Background(), h.log.RepoPrefix(repoA)+wal.LatestKey)
	for _, k := range h.store.Keys() {
		if strings.Contains(k, "/index/") {
			_ = h.store.Delete(context.Background(), k)
		}
	}
	if status, out := h.do("GET", "/v1/repos/"+repoA, ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("no index: %d %v", status, out)
	}
	h.store.SetFault(func(op, key string) error {
		if op == "Get" && strings.HasSuffix(key, "/meta") {
			return errors.New("meta unreadable")
		}
		return nil
	})
	check("create read-back", "POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"c"}`)
}

// TestDenyBeforeLookup: for every route that names a repository, the
// authorizer is asked with what the path names before anything of the
// repository is read, so a denied caller learns nothing and a 404 goes
// only to an allowed one.
func TestDenyBeforeLookup(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app"}`)
	h.authz.Deny(authorizer.Rule{Subject: "eve"}, "eve is denied")
	h.authz.ClearRequests()

	// Every store operation is recorded with the number of authorizer
	// requests seen at that moment.
	var mu sync.Mutex
	var ops []string
	h.store.SetFault(func(op, key string) error {
		mu.Lock()
		defer mu.Unlock()
		ops = append(ops, op+" "+key)
		return nil
	})
	reset := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := ops
		ops = nil
		return out
	}
	type route struct {
		method, path, body string
		id, owner, slug    string
	}
	byID := []route{
		{"GET", "/v1/repos/" + unknown, "", unknown, "", ""},
		{"PATCH", "/v1/repos/" + unknown, `{"slug":"x"}`, unknown, "", ""},
		{"DELETE", "/v1/repos/" + unknown, "", unknown, "", ""},
		{"POST", "/v1/repos/" + unknown + "/undelete", "", unknown, "", ""},
		{"POST", "/v1/repos/" + unknown + "/tokens", `{"scope":"read","ttl":60}`, unknown, "", ""},
		{"GET", "/r/" + unknown + ".git/info/refs?service=git-upload-pack", "", unknown, "", ""},
		{"GET", "/r/" + unknown + ".git/info/refs?service=git-receive-pack", "", unknown, "", ""},
		{"POST", "/r/" + unknown + ".git/git-upload-pack", "0000", unknown, "", ""},
		{"POST", "/r/" + unknown + ".git/git-receive-pack", "0000", unknown, "", ""},
		{"GET", "/v1/repos/" + repoA, "", repoA, "", ""},
		{"POST", "/v1/repos", `{"id":"` + unknown + `","owner":"nobody","slug":"nothing"}`, unknown, "nobody", "nothing"},
	}
	byName := []route{
		{"GET", "/nobody/nothing.git/info/refs?service=git-upload-pack", "", "", "nobody", "nothing"},
		{"POST", "/nobody/nothing.git/git-upload-pack", "0000", "", "nobody", "nothing"},
		{"POST", "/nobody/nothing.git/git-receive-pack", "0000", "", "nobody", "nothing"},
		{"GET", "/acme/app.git/info/refs?service=git-upload-pack", "", repoA, "acme", "app"},
	}
	// The denied caller: 403 everywhere, one authorizer request carrying
	// what the path names, and no store read but the name resolution. A
	// distinct actor per route keeps the client's cache out of the count.
	for i, r := range append(byID, byName...) {
		h.as(auth.Principal{Subject: "eve", Actor: fmt.Sprintf("run-%d", i)})
		reset()
		before := len(h.authz.Requests())
		status, out := h.do(r.method, r.path, r.body)
		if status != 403 || code(out) != contract.CodeForbidden || details(out)["reason"] != "eve is denied" || details(out)["subject"] != "eve" {
			t.Errorf("eve %s %s: %d %v", r.method, r.path, status, out)
		}
		reqs := h.authz.Requests()
		if len(reqs) != before+1 || reqs[before].Repo.ID != r.id || reqs[before].Repo.Owner != r.owner || reqs[before].Repo.Slug != r.slug {
			t.Errorf("eve %s %s: authorizer saw %+v", r.method, r.path, reqs[len(reqs)-1])
		}
		got := reset()
		want := 0
		if r.owner != "" && r.id == "" || r.owner == "acme" {
			want = 1 // the name lookup, and nothing of the repository
		}
		if len(got) != want || (want == 1 && !strings.HasPrefix(got[0], "Get origo/names/")) {
			t.Errorf("eve %s %s: store operations %v", r.method, r.path, got)
		}
	}
	// The allowed caller: 404 on the same unknown paths, the authorizer
	// asked before the first store read.
	h.authz.ClearRequests()
	for i, r := range append(byID[:9], byName[:3]...) {
		h.as(auth.Principal{Subject: "alice", Actor: fmt.Sprintf("run-%d", i)})
		var seenAtFirstOp int
		h.store.SetFault(func(op, key string) error {
			mu.Lock()
			defer mu.Unlock()
			if len(ops) == 0 {
				seenAtFirstOp = len(h.authz.Requests())
			}
			ops = append(ops, op+" "+key)
			return nil
		})
		reset()
		before := len(h.authz.Requests())
		status, out := h.do(r.method, r.path, r.body)
		if status != 404 || code(out) != contract.CodeRepoNotFound {
			t.Errorf("alice %s %s: %d %v", r.method, r.path, status, out)
		}
		if r.id != "" && details(out)["id"] != r.id || r.id == "" && details(out)["owner"] != r.owner {
			t.Errorf("alice %s %s: details %v", r.method, r.path, details(out))
		}
		if got := reset(); len(got) == 0 || seenAtFirstOp != before+1 {
			// The name form's lookup precedes the authorizer; the id form
			// reads nothing before it.
			if r.id != "" || seenAtFirstOp != before {
				t.Errorf("alice %s %s: authorizer at %d, requests before %d, ops %v", r.method, r.path, seenAtFirstOp, before, got)
			}
		}
	}
	// The cache: a second denied request within 5 seconds is no call.
	h.as(auth.Principal{Subject: "eve", Actor: "cached"})
	n := len(h.authz.Requests())
	h.do("GET", "/v1/repos/"+repoA, "")
	h.do("GET", "/v1/repos/"+repoA, "")
	if len(h.authz.Requests()) != n+1 {
		t.Fatalf("the deny was not cached: %d calls", len(h.authz.Requests())-n)
	}
	// An authorizer outage is 503 authorizer_unavailable, never an allow.
	h.authz.Fail(500)
	if status, out := h.do("GET", "/v1/repos/"+unknown, ""); status != 503 || code(out) != contract.CodeAuthorizerUnavailable || details(out)["status"] != 500.0 {
		t.Fatalf("outage: %d %v", status, out)
	}
	// A storage failure on the name lookup is 503 before the authorizer.
	h.authz.Resume()
	h.store.SetFault(func(op, key string) error { return errors.New("down") })
	if status, out := h.do("GET", "/x/y.git/info/refs?service=git-upload-pack", ""); status != 503 || code(out) != contract.CodeStorageUnavailable {
		t.Fatalf("name lookup down: %d %v", status, out)
	}
}

func TestTokensEndpointMintsRepositoryBoundTokens(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app"}`)
	h.as(auth.Principal{Subject: "alice", Actor: "svc"})
	status, out := h.do("POST", "/v1/repos/"+repoA+"/tokens", `{"scope":"read","ttl":600}`)
	token, _ := out["token"].(string)
	if status != 201 || token == "" || out["expires_at"] == nil {
		t.Fatalf("mint: %d %v", status, out)
	}
	expires, err := time.Parse(time.RFC3339, out["expires_at"].(string))
	if err != nil || time.Until(expires) > 600*time.Second || time.Until(expires) < 590*time.Second {
		t.Fatalf("expires_at %v: %v", out["expires_at"], err)
	}
	// The token verifies against the node's key and names the minter's
	// subject and actor, the repository, and the scope.
	v, err := auth.NewVerifier(auth.VerifierOptions{LocalIssuer: issuer, LocalKey: &h.key.PublicKey, Client: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), token)
	if err != nil || p.Subject != "alice" || p.Actor != "svc" || p.Bound == nil || p.Bound.Repo != repoA || p.Bound.Scope != auth.ScopeRead {
		t.Fatalf("verified: %+v, %v", p, err)
	}
	// Minting is an admin action on the repository, and a bound token
	// never mints.
	if reqs := h.authz.Requests(); reqs[len(reqs)-1].Action != "admin" || reqs[len(reqs)-1].Repo.ID != repoA || reqs[len(reqs)-1].Actor != "svc" {
		t.Fatalf("authorizer: %+v", reqs[len(reqs)-1])
	}
	h.as(p)
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/tokens", `{"scope":"read","ttl":60}`); status != 403 || details(out)["reason"] != auth.ReasonScope {
		t.Fatalf("bound token minting: %d %v", status, out)
	}
	h.as(auth.Principal{Subject: "alice"})
	for name, body := range map[string]string{
		"not json":    `{`,
		"admin scope": `{"scope":"admin","ttl":60}`,
		"no scope":    `{"ttl":60}`,
		"zero ttl":    `{"scope":"read","ttl":0}`,
		"long ttl":    `{"scope":"read","ttl":3601}`,
		"unknown":     `{"scope":"read","ttl":60,"x":1}`,
	} {
		if status, out := h.do("POST", "/v1/repos/"+repoA+"/tokens", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	if status, _ := h.do("POST", "/v1/repos/"+unknown+"/tokens", `{"scope":"write","ttl":1}`); status != 404 {
		t.Fatalf("unknown repository: %d", status)
	}
	h.do("DELETE", "/v1/repos/"+repoA, "")
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/tokens", `{"scope":"write","ttl":3600}`); status != 404 {
		t.Fatalf("deleted repository: %d", status)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a handler without a guard")
		}
	}()
	New(Options{Cache: h.cache})
}

// TestLifecycleEventsAreEmitted is the wiring of spec 008 in this
// package: a PATCH of default_branch enqueues one push event with the
// single HEAD update and kind_detail default_branch, and an undelete
// emits undeleted once and no push event, both with the caller as the
// pusher.
func TestLifecycleEventsAreEmitted(t *testing.T) {
	s := sink.New(t)
	h := newHarness(t, withSink(s))
	h.as(auth.Principal{Subject: "alice", Actor: "svc"})
	if status, _ := h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app"}`); status != 201 {
		t.Fatalf("create: %d", status)
	}
	if status, _ := h.do("PATCH", "/v1/repos/"+repoA, `{"default_branch":"dev"}`); status != 200 {
		t.Fatalf("patch: %d", status)
	}
	got, ok := s.Wait(repoA, events.KindPush, 1, 10*time.Second)
	if !ok {
		t.Fatal("no push event for the default_branch change")
	}
	var p events.Push
	if err := json.Unmarshal(got[0].Body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Seq != 1 || p.KindDetail != events.DetailDefaultBranch || len(p.Updates) != 1 || p.Updates[0] != (events.Update{Ref: "HEAD", Before: "ref: refs/heads/main", After: "ref: refs/heads/dev"}) || p.Pusher != (events.Pusher{Sub: "alice", Actor: "svc"}) || !got[0].Verified {
		t.Fatalf("event %+v", p)
	}
	if status, _ := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 {
		t.Fatalf("delete: %d", status)
	}
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 {
		t.Fatalf("undelete: %d", status)
	}
	und, ok := s.Wait(repoA, "undeleted", 1, 10*time.Second)
	if !ok {
		t.Fatal("undeleted not delivered")
	}
	var body map[string]any
	_ = json.Unmarshal(und[0].Body, &body)
	if body["kind"] != "undeleted" || body["owner"] != "acme" || body["pusher"].(map[string]any)["sub"] != "alice" || und[0].ID != events.EmitID(repoA, "undeleted", time.Time{}) && und[0].ID != body["id"] {
		t.Fatalf("undeleted %s", und[0].Body)
	}
	// A second undelete of a live repository commits nothing and emits
	// nothing; the push event count stays at one.
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 {
		t.Fatal("second undelete")
	}
	time.Sleep(50 * time.Millisecond)
	if len(s.Deliveries(repoA, events.KindPush)) != 1 || len(s.Deliveries(repoA, "undeleted")) != 1 {
		t.Fatalf("deliveries %+v", s.Deliveries(repoA, ""))
	}
}

// TestOrigoPreferOnEveryRepositoryResponse is spec 005's header on the
// API: on every response to a request that names a repository,
// whatever the status, with the allow's replicas, and absent on
// POST /v1/repos, which names none.
func TestOrigoPreferOnEveryRepositoryResponse(t *testing.T) {
	set := placement.NewSet("origod-0", nil)
	set.Heard("origod-1", time.Now())
	h := newHarness(t, withPlacement(set))
	h.authz.Allow(authorizer.Rule{Subject: "alice", Replicas: 2})
	h.authz.Deny(authorizer.Rule{Subject: "eve"}, "no")
	two := strings.Join(set.Prefer(repoA, 2), ",")
	one := set.Prefer(repoA, 1)[0]
	status, _, header := h.doHeader("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app"}`)
	if status != 201 || header.Get(placement.Header) != "" {
		t.Fatalf("create: %d %q", status, header.Get(placement.Header))
	}
	for _, path := range []string{"/v1/repos/" + repoA, "/v1/repos/" + repoA + "/refs", "/v1/repos/" + repoA + "/commits"} {
		if status, _, header := h.doHeader("GET", path, ""); status != 200 || header.Get(placement.Header) != two {
			t.Fatalf("%s: %d %q, want %q", path, status, header.Get(placement.Header), two)
		}
	}
	if status, _, header := h.doHeader("GET", "/v1/repos/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", ""); status != 404 || header.Get(placement.Header) == "" {
		t.Fatalf("unknown id: %d %q", status, header.Get(placement.Header))
	}
	h.as(auth.Principal{Subject: "eve"})
	if status, _, header := h.doHeader("GET", "/v1/repos/"+repoA+"/refs", ""); status != 403 || header.Get(placement.Header) != one {
		t.Fatalf("refused: %d %q, want %q", status, header.Get(placement.Header), one)
	}
}
