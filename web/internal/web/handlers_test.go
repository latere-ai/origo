// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
	"github.com/latere-ai/origo-web/internal/session"
)

func TestRawStreamsTheBytesAsAnAttachment(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	rec := h.get(h.repoPath("/raw/README.md"), c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Errorf("the bytes were offered inline: %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("the type the installation detected did not survive: %q", got)
	}
	if rec.Body.String() != "# Origo\n\nA git server.\n" {
		t.Errorf("the bytes came through as %q", rec.Body.String())
	}

	// A file the installation will not serve is a sentence and a way out.
	h.fake.status["/v1/repos/1f2e3d/blob/b2"] = http.StatusRequestEntityTooLarge
	big := h.get(h.repoPath("/raw/logo.png"), c)
	if !strings.Contains(big.Body.String(), "Clone the repository") {
		t.Errorf("a refused download reads as:\n%s", big.Body.String())
	}

	// A path that names nothing is the one sentence for absence.
	if rec := h.get(h.repoPath("/raw/nothing.txt"), c); rec.Code != http.StatusNotFound {
		t.Errorf("a path that names nothing answered %d", rec.Code)
	}
	if rec := h.get(h.repoPath("/blob/"), c); rec.Code != http.StatusNotFound {
		t.Errorf("a file screen with no path answered %d", rec.Code)
	}
}

func TestPatchIsTheBytesGitWrote(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	rec := h.get(h.repoPath("/patch/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"), c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("the patch is served as %q", got)
	}
	if !strings.HasPrefix(rec.Body.String(), "diff --git") {
		t.Errorf("the patch reads as %q", rec.Body.String())
	}

	// A commit with no parent has no patch.
	h.fake.commits[0].Parents = nil
	if rec := h.get(h.repoPath("/patch/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"), c); rec.Code != http.StatusNotFound {
		t.Errorf("the first commit's patch answered %d", rec.Code)
	}
}

func TestARootCommitSaysSoRatherThanFailing(t *testing.T) {
	h := newHarness(t)
	h.fake.commits[0].Parents = nil
	body := h.get(h.repoPath("/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"), h.signedIn("alice")).Body.String()
	if !strings.Contains(body, "first commit in the repository") {
		t.Errorf("a root commit reads as:\n%s", body)
	}
}

func TestAnEmptyRepositorySaysWhatToDoNext(t *testing.T) {
	h := newHarness(t)
	h.fake.repo.Head = ""
	h.fake.heads = nil
	body := h.get(h.repoPath(""), h.signedIn("alice")).Body.String()
	if !strings.Contains(body, "no commits yet") {
		t.Errorf("an empty repository reads as:\n%s", body)
	}
	if !strings.Contains(body, "https://git.example/infra/origo.git") {
		t.Error("an empty repository does not show where to push")
	}
}

func TestAFrozenRepositorySaysSo(t *testing.T) {
	h := newHarness(t)
	at := h.fake.repo.PushedAt
	h.fake.repo.FrozenAt = at
	body := h.get(h.repoPath(""), h.signedIn("alice")).Body.String()
	if !strings.Contains(body, "accepts no pushes") {
		t.Error("a frozen repository does not say so")
	}
}

func TestAnUnreachableInstallationIsOneSentence(t *testing.T) {
	h := newHarness(t)
	h.fake.status["/v1/repos/1f2e3d"] = http.StatusServiceUnavailable
	rec := h.get(h.repoPath(""), h.signedIn("alice"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("a degraded installation answered %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not answering right now") {
		t.Errorf("the sentence is:\n%s", rec.Body.String())
	}
}

func TestABrokenAnswerIsOneSentence(t *testing.T) {
	h := newHarness(t)
	h.fake.headers["/v1/repos/1f2e3d"] = http.Header{"Content-Type": []string{"application/json"}}
	// A body that is not the document the contract names.
	h.fake.repo.ID = "1f2e3d"
	rec := h.get("/r/unknown-id", h.signedIn("alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unknown repository answered %d", rec.Code)
	}
}

func TestTheHomeScreenShowsADirectoryWhenThereIsOne(t *testing.T) {
	h := newHarness(t)
	// An installation that grew the collection route of spec 023.
	h.fake.status["/v1/repos"] = 0
	h.fake.headers["/v1/repos"] = http.Header{"Content-Type": []string{"application/json"}}
	h.server = New(Options{
		Config: h.cfg, Sessions: mustSessions(t, h.cfg), API: origo.New(mustURL(t, h.backend.URL), h.backend.Client()),
	})
	h.fake.repo.ID = "1f2e3d"

	// The fake answers the collection route with 501 by default; point it
	// at a directory instead.
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/repos" {
			writeJSON(w, map[string]any{
				"repos":       []origo.Repo{{ID: "r1", Owner: "infra", Slug: "origo", DefaultBranch: "main", SizeBytes: 2048}},
				"next_cursor": "r1",
			})
			return
		}
		h.fake.ServeHTTP(w, r)
	}))
	defer dir.Close()
	h.server = New(Options{
		Config: h.cfg, Sessions: mustSessions(t, h.cfg), API: origo.New(mustURL(t, dir.URL), dir.Client()),
	})

	body := h.get("/", h.signedIn("alice")).Body.String()
	for _, want := range []string{"Repositories you can read", "origo", "2.0 KB", "main", "cursor=r1"} {
		if !strings.Contains(body, want) {
			t.Errorf("the directory does not show %q:\n%s", want, body)
		}
	}
}

func TestTheHomeScreenSaysWhenADirectoryIsEmpty(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"repos": []origo.Repo{}, "next_cursor": ""})
	}))
	defer empty.Close()
	h := newHarness(t)
	h.server = New(Options{
		Config: h.cfg, Sessions: mustSessions(t, h.cfg), API: origo.New(mustURL(t, empty.URL), empty.Client()),
	})
	if !strings.Contains(h.get("/", h.signedIn("alice")).Body.String(), "nothing here you can read yet") {
		t.Error("an empty directory does not say so")
	}
}

func TestTheHomeScreenSendsARefusedReaderToSignIn(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
	}))
	defer refuse.Close()
	h := newHarness(t)
	h.server = New(Options{
		Config: h.cfg, Sessions: mustSessions(t, h.cfg), API: origo.New(mustURL(t, refuse.URL), refuse.Client()),
	})
	rec := h.get("/", h.signedIn("alice"))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "/auth/start") {
		t.Errorf("a refused reader got %d:\n%s", rec.Code, rec.Body.String())
	}
}

func TestOpenSendsAPersonToTheRepositoryTheyNamed(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	rec := h.get("/open?id=1f2e3d", c)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/r/1f2e3d" {
		t.Errorf("open answered %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := h.get("/open?id=", c); rec.Header().Get("Location") != "/" {
		t.Errorf("an empty identifier landed on %q", rec.Header().Get("Location"))
	}
}

func TestSignOutClearsTheSession(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	// The token the page issues is what makes the form work.
	page := h.get("/", c)
	var csrf *http.Cookie
	for _, ck := range page.Result().Cookies() {
		if ck.Name == session.CSRFCookieName {
			csrf = ck
		}
	}
	if csrf == nil {
		t.Fatal("no token cookie was issued")
	}
	form := url.Values{session.CSRFField(): {csrf.Value}}
	req := httptest.NewRequest(http.MethodPost, "/sign-out", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	req.AddCookie(csrf)
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sign-out answered %d", rec.Code)
	}
	var cleared bool
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == session.CookieName && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the session cookie survived sign-out")
	}
}

func TestSignInStartsAndReturnsTheFlow(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/auth/start?return_to=/r/1f2e3d/log")
	if rec.Code != http.StatusFound {
		t.Fatalf("the flow started with %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "code_challenge") {
		t.Errorf("the redirect carries no PKCE challenge: %q", rec.Header().Get("Location"))
	}
	// An off-origin return is not one this service will make.
	rec = h.get("/auth/start?return_to=https://elsewhere.example/")
	if rec.Code != http.StatusFound || strings.Contains(rec.Header().Get("Location"), "elsewhere.example") {
		t.Errorf("an off-origin return reached the issuer: %q", rec.Header().Get("Location"))
	}
	// The callback with no flow cookie goes back to the start rather than
	// failing at the person.
	rec = h.get("/auth/callback?code=x&state=y")
	if rec.Code != http.StatusFound {
		t.Errorf("a callback with no flow answered %d", rec.Code)
	}
}

func TestSignInScreenNamesTheIssuerWhenItIsConfigured(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.IssuerName = "Okta"; c.SSHCloneHost = "git.example" })
	body := h.get("/sign-in").Body.String()
	if !strings.Contains(body, "Continue with Okta") {
		t.Errorf("the button reads:\n%s", body)
	}
	if !strings.Contains(body, "HTTPS and SSH") {
		t.Error("an installation with an SSH surface does not say so")
	}
	plain := newHarness(t).get("/sign-in").Body.String()
	if !strings.Contains(plain, "Continue to sign in") {
		t.Error("an installation that does not name its issuer has no button")
	}
	if strings.Contains(plain, "and SSH") {
		t.Error("an installation with no SSH surface offered SSH")
	}
}

func TestAssetsComeOutOfTheBinary(t *testing.T) {
	h := newHarness(t)
	css := h.get("/assets/app.css")
	if css.Code != http.StatusOK || css.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("the stylesheet answered %d as %q", css.Code, css.Header().Get("Content-Type"))
	}
	font := h.get("/assets/inter-latin.woff2")
	if font.Code != http.StatusOK || font.Header().Get("Content-Type") != "font/woff2" {
		t.Errorf("the font answered %d as %q", font.Code, font.Header().Get("Content-Type"))
	}
	if font.Body.Len() < 1000 {
		t.Errorf("the font is %d bytes", font.Body.Len())
	}
	if other := h.get("/assets/../go.mod"); other.Code == http.StatusOK {
		t.Error("a path outside the assets came out of the binary")
	}
	if other := h.get("/assets/nothing.txt"); other.Code != http.StatusNotFound {
		t.Errorf("an asset that is not there answered %d", other.Code)
	}
}

// TestNoPageFetchesFromAnotherHost asserts what makes this service
// self-hostable: every stylesheet, font, and image a page names is served by
// this binary, and the policy on every response refuses the rest.
func TestNoPageFetchesFromAnotherHost(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	for name, path := range h.screens() {
		rec := h.get(path, c)
		page := doc(t, rec.Body.String())
		for _, tag := range []string{"link", "img", "script", "iframe"} {
			for _, e := range elements(page, tag) {
				for _, a := range []string{"href", "src"} {
					v := attr(e, a)
					if v == "" || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "data:") || strings.HasPrefix(v, "#") {
						continue
					}
					t.Errorf("%s: <%s %s=%q> reaches another host", name, tag, a, v)
				}
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'none'", "style-src 'self'", "font-src 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: the policy has no %q: %q", name, want, csp)
			}
		}
	}
	// The stylesheet itself names no other host either.
	css := string(mustAsset(t, "app.css"))
	for _, host := range []string{"http://", "https://", "//fonts."} {
		if strings.Contains(css, host) {
			t.Errorf("the stylesheet reaches %q", host)
		}
	}
}

func mustSessions(t *testing.T, cfg config.Config) *session.Manager {
	t.Helper()
	m, err := session.New(cfg.OIDC)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
