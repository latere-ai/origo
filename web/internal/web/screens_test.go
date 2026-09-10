// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
)

// screens is every screen a signed-in person can reach on the fixture
// repository, named as the criteria name them.
func (h *harness) screens() map[string]string {
	return map[string]string{
		"home":     "/",
		"overview": h.repoPath(""),
		"refs":     h.repoPath("/refs"),
		"log":      h.repoPath("/log"),
		"commit":   h.repoPath("/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"),
		"compare":  h.repoPath("/compare?base=main&head=next"),
		"tree":     h.repoPath("/tree/internal"),
		"file":     h.repoPath("/blob/README.md"),
	}
}

// TestScreenCallsAreExact holds each screen to the calls spec 023's screen
// table lists for it, against a recording installation.
//
// The table addresses a repository by owner and slug, through a route Origo
// does not serve yet; every screen here resolves the repository by its id
// instead, with GET /v1/repos/{id} of spec 003, which is the same one call in
// the same place. No screen's call count grows with the number of rows it
// renders, which the second half of this test measures by rendering a wider
// page and counting again.
func TestScreenCallsAreExact(t *testing.T) {
	repo := "/v1/repos/1f2e3d"
	want := map[string][]string{
		"overview": {
			repo,
			repo + "/refs?prefix", repo + "/refs?prefix",
			repo + "/tree/main",
			repo + "/commits?limit&ref",
			repo + "/blob/b1",
		},
		"refs": {repo, repo + "/refs?prefix", repo + "/refs?prefix"},
		"log":  {repo, repo + "/commits?limit&ref"},
		"commit": {
			repo,
			repo + "/commits/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d",
			repo + "/compare/4c02f7e10b4d3a1e8f6b2c9d05a7e3f1b8c4d6e2...9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d",
		},
		"compare": {repo, repo + "/compare/main...next"},
		"tree":    {repo, repo + "/tree/main?path"},
		"file":    {repo, repo + "/tree/main", repo + "/blob/b1"},
	}

	h := newHarness(t)
	c := h.signedIn("alice")
	for name, path := range h.screens() {
		if name == "home" {
			continue
		}
		h.fake.Reset()
		if rec := h.get(path, c); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, rec.Code)
		}
		got := h.fake.Calls()
		if strings.Join(got, "\n") != strings.Join(want[name], "\n") {
			t.Errorf("%s made\n  %v\nwant\n  %v", name, got, want[name])
		}
	}

	// The same screens over a repository with many more rows make the same
	// number of calls: no screen asks a question per row.
	h.fake.trees["internal"] = manyEntries(500)
	h.fake.commits = manyCommits(200)
	for name, path := range h.screens() {
		if name == "home" || name == "overview" || name == "commit" {
			continue
		}
		h.fake.Reset()
		h.get(path, c)
		if got, wanted := len(h.fake.Calls()), len(want[name]); got != wanted {
			t.Errorf("%s over a wider page made %d calls, want %d", name, got, wanted)
		}
	}
}

// TestRefusalAndAbsenceAreIndistinguishable asserts that a 403 and a 404 from
// Origo reach the reader as one sentence and one status. Origo refuses to say
// which of the two it meant, and this interface refuses to leak the
// difference.
func TestRefusalAndAbsenceAreIndistinguishable(t *testing.T) {
	var forbidden, missing *httptest.ResponseRecorder
	for i, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		h := newHarness(t)
		h.fake.status["/v1/repos/1f2e3d"] = code
		rec := h.get(h.repoPath(""), h.signedIn("alice"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("origo answered %d, the page answered %d, want 404", code, rec.Code)
		}
		if i == 0 {
			forbidden = rec
		} else {
			missing = rec
		}
	}
	// The two pages are the same but for the token each render issues to
	// its own form, which is per response by construction.
	if withoutCSRF(forbidden.Body.String()) != withoutCSRF(missing.Body.String()) {
		t.Error("a refusal and an absence rendered different pages")
	}
	if !strings.Contains(forbidden.Body.String(), "No such repository, or you cannot see it") {
		t.Errorf("the sentence is not the one the spec fixes:\n%s", forbidden.Body.String())
	}
}

// TestAnonymousRendersWhateverOrigoAnswers asserts both halves of the rule:
// today a signed-out visitor is shown the sign-in page, because Origo refuses
// a tokenless read; and the same handler, against an installation that
// answers one, renders the repository, with no branch on "is there a session"
// between them.
func TestAnonymousRendersWhateverOrigoAnswers(t *testing.T) {
	// Half one: the installation refuses a request with no credential.
	h := newHarness(t)
	h.fake.status["/v1/repos/1f2e3d"] = http.StatusUnauthorized
	for _, path := range []string{h.repoPath(""), h.repoPath("/log"), h.repoPath("/tree/")} {
		rec := h.get(path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 with the sign-in page", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "/auth/start") {
			t.Errorf("%s: the page offers no way to sign in", path)
		}
	}

	// Half two: the same handler, the same request, an installation that
	// answers a read carrying no Authorization header.
	h2 := newHarness(t)
	rec := h2.get(h2.repoPath(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the repository", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "origo") {
		t.Error("the repository did not render for a reader with no session")
	}
	for _, got := range h2.fake.Tokens() {
		if got != "" {
			t.Errorf("a tokenless read carried %q; it must carry no Authorization header at all", got)
		}
	}
}

// TestEveryScreenWorksWithoutScript asserts the property the design and the
// spec both fix: no screen contains a script, and every navigation on it is a
// link to a real URL or a form that submits.
func TestEveryScreenWorksWithoutScript(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	for name, path := range h.screens() {
		rec := h.get(path, c)
		page := doc(t, rec.Body.String())

		if got := elements(page, "script"); len(got) > 0 {
			t.Errorf("%s carries %d script elements", name, len(got))
		}
		for _, e := range elements(page, "a") {
			href := attr(e, "href")
			if href == "" {
				t.Errorf("%s: a link with no href: %q", name, text(e))
			}
			if strings.HasPrefix(strings.ToLower(href), "javascript:") {
				t.Errorf("%s: a link that runs a script: %q", name, href)
			}
		}
		for _, e := range elements(page, "form") {
			if attr(e, "action") == "" {
				t.Errorf("%s: a form with no action", name)
			}
			method := strings.ToLower(attr(e, "method"))
			if method != "get" && method != "post" {
				t.Errorf("%s: a form with method %q", name, method)
			}
		}
		for _, e := range elements(page, "button") {
			if attr(e, "type") == "button" {
				t.Errorf("%s: a button that only a script could act on", name)
			}
		}
		for _, k := range []string{"onclick", "onchange", "onsubmit", "onload"} {
			if strings.Contains(rec.Body.String(), k+"=") {
				t.Errorf("%s: an inline %s handler", name, k)
			}
		}
	}
}

// TestNoCacheIsSharedBetweenSubjects asserts the rule the spec writes down so
// nobody optimises it away: a rendered page is never served to a second
// subject. This service keeps no cache, and every page says so, so the
// question cannot arise in a cache in front of it either.
func TestNoCacheIsSharedBetweenSubjects(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.signedIn("alice"), h.signedIn("bob")

	first := h.get(h.repoPath(""), alice)
	if got := first.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control is %q, want private, no-store", got)
	}

	// The installation now refuses the second reader. If any page had been
	// kept, the refusal would be answered from it.
	h.fake.status["/v1/repos/1f2e3d"] = http.StatusForbidden
	second := h.get(h.repoPath(""), bob)
	if second.Code != http.StatusNotFound {
		t.Fatalf("the second reader got %d, want the refusal", second.Code)
	}
	if strings.Contains(second.Body.String(), "Clone") {
		t.Error("the second reader was served the first reader's page")
	}

	// Each reader's own token went out with their own request.
	tokens := h.fake.Tokens()
	if len(tokens) < 2 {
		t.Fatalf("want a call per reader, got %d", len(tokens))
	}
	if tokens[0] == tokens[len(tokens)-1] {
		t.Error("two readers' calls carried the same credential")
	}
}

// TestStaleNotice asserts that Origo-Stale reaches the page as a sentence,
// and that the content is otherwise the same. A machine consumer ignores that
// header; a person reading a commit log must not.
func TestStaleNotice(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	plain := h.get(h.repoPath("/log"), c).Body.String()
	if strings.Contains(plain, "may be a few minutes behind") {
		t.Fatal("a consistent response carried the staleness line")
	}

	h.fake.headers["/v1/repos/1f2e3d/commits"] = http.Header{"Origo-Stale": []string{"180"}}
	stale := h.get(h.repoPath("/log"), c).Body.String()
	if !strings.Contains(stale, "may be a few minutes behind") {
		t.Error("a stale response carried no staleness line")
	}
	if !strings.Contains(stale, "3 minutes") {
		t.Errorf("the line does not say how far behind:\n%s", stale)
	}
	if !strings.Contains(stale, "web: collapse diffs over the render budget") {
		t.Error("a stale response lost its content")
	}
}

// TestRoutesAreReadOnly asserts the route table against a checked-in list.
// This interface has no write path to repository content of any kind, and the
// only routes that are not a GET are sign-out and the key screen, each of
// which requires a token issued to this session.
func TestRoutesAreReadOnly(t *testing.T) {
	want := []string{
		"GET /{$}", "GET /sign-in", "GET /open", "GET /auth/start", "GET /auth/callback",
		"POST /sign-out", "GET /assets/{file}",
		"GET /r/{id}", "GET /r/{id}/refs", "GET /r/{id}/log", "GET /r/{id}/commit/{sha}",
		"GET /r/{id}/patch/{sha}", "GET /r/{id}/compare", "GET /r/{id}/tree/{path...}",
		"GET /r/{id}/blob/{path...}", "GET /r/{id}/raw/{path...}",
		"GET /keys", "POST /keys",
	}
	var got []string
	for _, r := range Routes(true) {
		got = append(got, r.Method+" "+r.Pattern)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the route table is\n%v\nand the checked-in list is\n%v", got, want)
	}
	for _, r := range Routes(true) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			t.Errorf("%s %s: a method that is neither a read nor one of the two forms", r.Method, r.Pattern)
		}
	}

	// Both forms refuse a submission with no token.
	h := newHarness(t, func(c *config.Config) { c.KeysURL = "https://keys.example" })
	for _, path := range []string{"/sign-out", "/keys"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(h.signedIn("alice"))
		rec := httptest.NewRecorder()
		h.server.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with no token: %d, want 403", path, rec.Code)
		}
	}

	// The list above is what the server registered, and the server is what
	// answers: every address of the interface is driven with every method
	// that changes something, and the only two that answer are the two
	// forms. A handler added straight onto the mux would fail here.
	c := h.signedIn("alice")
	addresses := []string{
		"/", "/sign-in", "/open", "/auth/start", "/auth/callback", "/sign-out",
		"/keys", "/assets/app.css", h.repoPath(""), h.repoPath("/refs"),
		h.repoPath("/log"), h.repoPath("/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"),
		h.repoPath("/patch/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"), h.repoPath("/compare"),
		h.repoPath("/tree/internal"), h.repoPath("/blob/README.md"), h.repoPath("/raw/README.md"),
		"/livez", "/readyz", "/version",
	}
	for _, address := range addresses {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if method == http.MethodPost && (address == "/sign-out" || address == "/keys") {
				continue
			}
			req := httptest.NewRequest(method, address, nil)
			req.AddCookie(c)
			rec := httptest.NewRecorder()
			h.server.ServeHTTP(rec, req)
			if rec.Code < 400 {
				t.Errorf("%s %s answered %d; nothing but the two forms may change anything",
					method, address, rec.Code)
			}
		}
	}
}

// TestKeyScreenIsOptional asserts that the one screen with no contract behind
// it is absent by default, together with its navigation entry, and that
// nothing else changes when it appears.
func TestKeyScreenIsOptional(t *testing.T) {
	off := newHarness(t)
	c := off.signedIn("alice")
	if rec := off.get("/keys", c); rec.Code != http.StatusNotFound {
		t.Errorf("the key screen answers %d while no key surface is configured, want 404", rec.Code)
	}
	home := off.get("/", c).Body.String()
	if strings.Contains(home, "ssh keys") {
		t.Error("the navigation offers a screen that is not there")
	}

	on := newHarness(t, func(cfg *config.Config) { cfg.KeysURL = "https://keys.example" })
	c2 := on.signedIn("alice")
	if rec := on.get("/keys", c2); rec.Code != http.StatusNotImplemented {
		t.Errorf("the configured key screen answers %d", rec.Code)
	}
	if !strings.Contains(on.get("/", c2).Body.String(), "ssh keys") {
		t.Error("the configured key screen has no navigation entry")
	}

	// Every other screen renders the same either way.
	for name, path := range off.screens() {
		a := off.get(path, c).Code
		b := on.get(path, c2).Code
		if a != b {
			t.Errorf("%s answers %d without the key screen and %d with it", name, a, b)
		}
	}
}

// TestCloneURLsComeFromConfiguration asserts that the clone addresses come
// from the settings an operator gave the process and never from a request
// header, so a forged Host cannot move them.
func TestCloneURLsComeFromConfiguration(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.SSHCloneHost = "git.example" })
	req := httptest.NewRequest(http.MethodGet, h.repoPath(""), nil)
	req.Host = "attacker.example"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.AddCookie(h.signedIn("alice"))
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "https://git.example/infra/origo.git") {
		t.Errorf("the HTTPS clone address is not the configured one:\n%s", body)
	}
	if !strings.Contains(body, "git@git.example:infra/origo.git") {
		t.Error("the SSH clone address is not the configured one")
	}
	if strings.Contains(body, "attacker.example") {
		t.Error("a request header reached the page")
	}

	// With no SSH surface configured, only the HTTPS form is shown.
	plain := newHarness(t)
	body = plain.get(plain.repoPath(""), plain.signedIn("alice")).Body.String()
	if strings.Contains(body, "git@") {
		t.Error("an installation with no SSH surface showed an SSH clone address")
	}
}

// TestReferenceSelector asserts that the control lists the branches and the
// tags the installation returned, and that choosing one is a form submission
// that lands on the same screen at the new revision.
func TestReferenceSelector(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	page := doc(t, h.get(h.repoPath(""), c).Body.String())

	selects := elements(page, "select")
	if len(selects) != 1 {
		t.Fatalf("want one reference control, got %d", len(selects))
	}
	if attr(selects[0], "name") != "ref" {
		t.Errorf("the control submits %q", attr(selects[0], "name"))
	}
	var options []string
	for _, o := range elements(selects[0], "option") {
		options = append(options, attr(o, "value"))
	}
	want := []string{"main", "next", "v1.4.2"}
	if strings.Join(options, ",") != strings.Join(want, ",") {
		t.Errorf("the control offers %v, the installation returned %v", options, want)
	}

	// The control is a form that submits with a GET, so a chosen reference
	// is a URL that can be shared.
	form := selects[0].Parent
	for form != nil && form.Data != "form" {
		form = form.Parent
	}
	if form == nil {
		t.Fatal("the control is not inside a form")
	}
	if strings.ToLower(attr(form, "method")) != "get" {
		t.Errorf("the control submits with %q", attr(form, "method"))
	}

	// Submitting it lands on the same screen at the new revision.
	rec := h.get(attr(form, "action")+"?ref=next", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("the chosen reference answered %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Files at next") {
		t.Error("the screen did not move to the chosen reference")
	}
}

// TestListDegradesWithoutDirectory asserts that a home screen without a
// collection route is the way in that works without one, and that the rest of
// the interface is unaffected.
func TestListDegradesWithoutDirectory(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	body := h.get("/", c).Body.String()
	if !strings.Contains(body, "does not list repositories") {
		t.Errorf("the home screen does not say it cannot enumerate:\n%s", body)
	}
	page := doc(t, body)
	forms := elements(page, "form")
	var open bool
	for _, f := range forms {
		if attr(f, "action") == "/open" {
			open = true
		}
	}
	if !open {
		t.Error("the home screen offers no way to open a repository")
	}

	// Opening one leaves it in the list for the rest of the session.
	rec := h.get(h.repoPath(""), c)
	var recent *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "__Host-origoweb-recent" {
			recent = ck
		}
	}
	if recent == nil {
		t.Fatal("opening a repository did not remember it")
	}
	if !strings.Contains(h.get("/", c, recent).Body.String(), "Opened in this session") {
		t.Error("the home screen does not show what this session opened")
	}

	// Every other screen is unaffected.
	for name, path := range h.screens() {
		if name == "home" {
			continue
		}
		if rec := h.get(path, c); rec.Code != http.StatusOK {
			t.Errorf("%s answers %d on an installation with no directory", name, rec.Code)
		}
	}
}

// TestTokenNeverLeavesTheCookie asserts that the access token appears in no
// body, no URL, and no header of any screen.
func TestTokenNeverLeavesTheCookie(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	const token = "token-for-alice"
	for name, path := range h.screens() {
		rec := h.get(path, c)
		if strings.Contains(rec.Body.String(), token) {
			t.Errorf("%s: the token is in the body", name)
		}
		for k, vs := range rec.Header() {
			for _, v := range vs {
				if strings.Contains(v, token) && k != "Set-Cookie" {
					t.Errorf("%s: the token is in the %s header", name, k)
				}
			}
		}
		page := doc(t, rec.Body.String())
		for _, a := range elements(page, "a") {
			if strings.Contains(attr(a, "href"), token) {
				t.Errorf("%s: the token is in a URL", name)
			}
		}
	}
	// The cookie itself is encrypted: the token is not readable in it.
	if strings.Contains(c.Value, token) {
		t.Error("the session cookie carries the token in the clear")
	}
}

// manyEntries and manyCommits widen a page so a call count can be measured
// against the number of rows it renders.
func manyEntries(n int) []origo.Entry {
	out := make([]origo.Entry, 0, n)
	for i := range n {
		out = append(out, origo.Entry{
			Path: "internal/f" + itoa(i) + ".go", Mode: "100644", Type: "blob",
			SHA: "b" + itoa(i), Size: int64(100 + i),
		})
	}
	return out
}

func manyCommits(n int) []origo.Commit {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	out := make([]origo.Commit, 0, n)
	for i := range n {
		out = append(out, origo.Commit{
			SHA:     strings.Repeat("a", 39) + itoa(i%10),
			Parents: []string{strings.Repeat("b", 40)},
			Author:  origo.Person{Name: "a.hoshino", Email: "aki@example.com", At: at},
			Message: "a commit " + itoa(i),
		})
	}
	return out
}

// withoutCSRF removes the per-response form token, which differs between any
// two renders and says nothing about what a page shows.
func withoutCSRF(body string) string {
	const marker = `value="`
	i := strings.Index(body, "csrf_token")
	if i < 0 {
		return body
	}
	j := strings.Index(body[i:], marker)
	if j < 0 {
		return body
	}
	start := i + j + len(marker)
	k := strings.Index(body[start:], `"`)
	if k < 0 {
		return body
	}
	return body[:start] + body[start+k:]
}
