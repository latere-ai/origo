// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/oidc"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
	"github.com/latere-ai/origo-web/internal/session"
)

// fakeOrigo is an installation that records what a screen asked it. It
// answers from fixtures, so a test asserts against the calls the spec's
// table lists and against nothing else.
type fakeOrigo struct {
	mu    sync.Mutex
	calls []string

	repo    origo.Repo
	heads   []origo.Ref
	tags    []origo.Ref
	commits []origo.Commit
	trees   map[string][]origo.Entry
	blobs   map[string]fakeBlob
	diff    string

	// headers and status are per-path overrides, keyed by the path alone.
	headers map[string]http.Header
	status  map[string]int
	// tokens records the Authorization header of every call, and ranges
	// the Range header of every blob call.
	tokens []string
	ranges []string
	// nextCursor is returned by every paging read when set.
	nextCursor string
}

type fakeBlob struct {
	body        string
	contentType string
}

func newFakeOrigo() *fakeOrigo {
	at := time.Date(2026, 9, 10, 9, 41, 0, 0, time.UTC)
	return &fakeOrigo{
		repo: origo.Repo{
			ID: "1f2e3d", Owner: "infra", Slug: "origo",
			DefaultBranch: "main", Head: "9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d",
			SizeBytes: 4404019, PushedAt: &at, UpdatedAt: &at,
		},
		heads: []origo.Ref{
			{Name: "refs/heads/main", SHA: "9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"},
			{Name: "refs/heads/next", SHA: "4c02f7e10b4d3a1e8f6b2c9d05a7e3f1b8c4d6e2"},
		},
		tags: []origo.Ref{
			{Name: "refs/tags/v1.4.2", SHA: "aaa1111", Peeled: "b71d998c0a1f2e3d4c5b6a7988990011aabbccdd"},
		},
		commits: []origo.Commit{{
			SHA:       "9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d",
			Parents:   []string{"4c02f7e10b4d3a1e8f6b2c9d05a7e3f1b8c4d6e2"},
			Author:    origo.Person{Name: "a.hoshino", Email: "aki@example.com", At: at},
			Committer: origo.Person{Name: "a.hoshino", Email: "aki@example.com", At: at},
			Message:   "web: collapse diffs over the render budget\n\nThe pathological case, handled.\n",
			Trailers:  []origo.Trailer{{Key: "Reviewed-by", Value: "m.okonkwo"}},
			Stats:     &origo.Stats{Files: 2, Additions: 3, Deletions: 1},
		}},
		trees: map[string][]origo.Entry{
			"": {
				{Path: "internal", Mode: "040000", Type: "tree", SHA: "t1"},
				{Path: "README.md", Mode: "100644", Type: "blob", SHA: "b1", Size: 42},
				{Path: "logo.png", Mode: "100644", Type: "blob", SHA: "b2", Size: 49152},
				{Path: "huge.json", Mode: "100644", Type: "blob", SHA: "b3", Size: 60 << 20},
				{Path: "big.txt", Mode: "100644", Type: "blob", SHA: "b5", Size: 10 << 20},
			},
			"internal": {
				{Path: "internal/diff.go", Mode: "100644", Type: "blob", SHA: "b4", Size: 120},
			},
		},
		blobs: map[string]fakeBlob{
			"b1": {body: "# Origo\n\nA git server.\n", contentType: "text/plain; charset=utf-8"},
			"b2": {body: "\x89PNG\r\n\x1a\n\x00\x00", contentType: "image/png"},
			"b4": {body: "package diff\n\nfunc F() {}\n", contentType: "text/plain; charset=utf-8"},
			"b5": {body: strings.Repeat("a line of text\n", 74898), contentType: "text/plain; charset=utf-8"},
		},
		diff: "diff --git a/internal/diff.go b/internal/diff.go\n" +
			"index 111..222 100644\n--- a/internal/diff.go\n+++ b/internal/diff.go\n" +
			"@@ -1,3 +1,5 @@ func F\n package diff\n-old line\n+new line\n+another\n+third\n",
		headers: map[string]http.Header{},
		status:  map[string]int{},
	}
}

func (f *fakeOrigo) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.URL.Path
	if q := r.URL.Query(); len(q) > 0 {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		call += "?" + strings.Join(sorted(keys), "&")
	}
	f.calls = append(f.calls, call)
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))
	if strings.Contains(r.URL.Path, "/blob/") {
		f.ranges = append(f.ranges, r.Header.Get("Range"))
	}
}

func sorted(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// Calls returns the paths a screen asked for, with the query keys but not
// their values, which is the shape the spec's table names.
func (f *fakeOrigo) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeOrigo) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.tokens, f.ranges = nil, nil, nil
}

// blobRanges is the Range header of every blob call since the last reset.
func (f *fakeOrigo) blobRanges() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

func (f *fakeOrigo) Tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

func (f *fakeOrigo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	p := r.URL.Path

	f.mu.Lock()
	for k, v := range f.headers[p] {
		w.Header()[k] = v
	}
	code := f.status[p]
	f.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "denied"}})
		return
	}

	switch {
	case p == "/v1/repos":
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "no_directory"}})
	case p == "/v1/repos/"+f.repo.ID:
		writeJSON(w, f.repo)
	case strings.HasSuffix(p, "/refs"):
		if strings.HasPrefix(r.URL.Query().Get("prefix"), "refs/tags/") {
			writeJSON(w, f.tags)
			return
		}
		writeJSON(w, f.heads)
	case strings.Contains(p, "/commits/"):
		writeJSON(w, f.commits[0])
	case strings.HasSuffix(p, "/commits"):
		writeJSON(w, map[string]any{"commits": f.commits, "next_cursor": f.nextCursor})
	case strings.Contains(p, "/compare/"):
		w.Header().Set("Content-Type", "text/x-diff")
		_, _ = w.Write([]byte(f.diff))
	case strings.Contains(p, "/tree/"):
		writeJSON(w, map[string]any{"entries": f.trees[r.URL.Query().Get("path")], "next_cursor": f.nextCursor})
	case strings.Contains(p, "/blob/"):
		sha := p[strings.LastIndex(p, "/")+1:]
		b, ok := f.blobs[sha]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ref_not_found"}})
			return
		}
		w.Header().Set("Content-Type", b.contentType)
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write([]byte(b.body))
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "repo_not_found"}})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// harness is a server wired to a fake installation.
type harness struct {
	t       *testing.T
	server  *Server
	fake    *fakeOrigo
	oidc    *oidc.Client
	cfg     config.Config
	backend *httptest.Server
}

const testCookieKey = "0123456789abcdef0123456789abcdef"

func newHarness(t *testing.T, opts ...func(*config.Config)) *harness {
	t.Helper()
	fake := newFakeOrigo()
	backend := httptest.NewServer(fake)
	t.Cleanup(backend.Close)

	base, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	oc := oidc.Config{
		AuthURL: "https://issuer.example", ClientID: "origoweb",
		RedirectURL: "https://code.example/auth/callback", CookieKey: testCookieKey,
		Audience: "origo",
	}
	cfg := config.Config{
		Addr: ":0", OrigoURL: base, PublicURL: mustURL(t, "https://code.example"),
		CloneHost: mustURL(t, "https://git.example"), OIDC: oc,
	}
	for _, o := range opts {
		o(&cfg)
	}
	sessions, err := session.New(cfg.OIDC)
	if err != nil {
		t.Fatal(err)
	}
	sealer := oc
	sealer.CookieName = session.CookieName
	sealer.SessionTTL = session.Lifetime
	return &harness{
		t: t, fake: fake, backend: backend, cfg: cfg, oidc: oidc.New(sealer),
		server: New(Options{Config: cfg, Sessions: sessions, API: origo.New(base, backend.Client())}),
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// signedIn returns the cookie a request carries to be somebody.
func (h *harness) signedIn(subject string) *http.Cookie {
	h.t.Helper()
	rec := httptest.NewRecorder()
	if err := h.oidc.SetSession(rec, &oidc.Session{
		AccessToken: "token-for-" + subject,
		Expiry:      time.Now().Add(time.Hour),
		User:        oidc.User{Sub: subject},
	}); err != nil {
		h.t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == session.CookieName {
			return c
		}
	}
	h.t.Fatalf("no session cookie was written")
	return nil
}

// get issues one request, with the cookies given.
func (h *harness) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	return rec
}

// repoPath is the address of the fixture repository in this interface.
func (h *harness) repoPath(suffix string) string {
	return "/r/" + h.fake.repo.ID + suffix
}
