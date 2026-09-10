// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
)

// TestASecondReadThatFailsIsStillOneSentence walks every screen that makes
// more than one call and refuses each call in turn: whichever read fails, the
// reader gets one of the four sentences and never a status the installation
// chose.
func TestASecondReadThatFailsIsStillOneSentence(t *testing.T) {
	repo := "/v1/repos/1f2e3d"
	cases := []struct {
		screen string
		path   string
		fail   string
	}{
		{"overview", "", repo + "/refs"},
		{"overview", "", repo + "/tree/main"},
		{"overview", "", repo + "/commits"},
		{"references", "/refs", repo + "/refs"},
		{"log", "/log", repo + "/commits"},
		{"commit", "/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d", repo + "/commits/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"},
		{"tree", "/tree/internal", repo + "/tree/main"},
		{"file", "/blob/README.md", repo + "/tree/main"},
		{"raw", "/raw/README.md", repo + "/blob/b1"},
		{"patch", "/patch/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d", repo + "/commits/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"},
	}
	for _, tc := range cases {
		h := newHarness(t)
		h.fake.status[tc.fail] = http.StatusForbidden
		rec := h.get(h.repoPath(tc.path), h.signedIn("alice"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with %s refused answered %d, want the one refusal", tc.screen, tc.fail, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "No such repository, or you cannot see it") {
			t.Errorf("%s with %s refused reads as:\n%s", tc.screen, tc.fail, rec.Body.String())
		}
	}
}

// TestACompareThatFailsIsOneSentence covers the second call of the two diff
// screens.
func TestACompareThatFailsIsOneSentence(t *testing.T) {
	for _, path := range []string{
		"/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d",
		"/compare?base=main&head=next",
	} {
		h := newHarness(t)
		for _, p := range []string{
			"/v1/repos/1f2e3d/compare/4c02f7e10b4d3a1e8f6b2c9d05a7e3f1b8c4d6e2...9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d",
			"/v1/repos/1f2e3d/compare/main...next",
		} {
			h.fake.status[p] = http.StatusServiceUnavailable
		}
		rec := h.get(h.repoPath(path), h.signedIn("alice"))
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s answered %d while the installation was degraded", path, rec.Code)
		}
	}
}

// TestAnAnswerThatIsNotTheContractIsAnError asserts that a body this service
// cannot read is one sentence and a 502, not a panic and not a page.
func TestAnAnswerThatIsNotTheContractIsAnError(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("this is not the document the contract names"))
	}))
	defer broken.Close()

	h := newHarness(t)
	h.server = New(Options{Config: h.cfg, Sessions: mustSessions(t, h.cfg), API: apiFor(t, broken)})
	rec := h.get(h.repoPath(""), h.signedIn("alice"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("an unreadable answer became %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "could not be read") {
		t.Errorf("it reads as:\n%s", rec.Body.String())
	}
}

// TestAReadmeThatCannotBeReadDoesNotTakeThePageDown asserts that the one
// optional read of the overview is optional: a readme refused or too large
// leaves the rest of the screen standing.
func TestAReadmeThatCannotBeReadDoesNotTakeThePageDown(t *testing.T) {
	h := newHarness(t)
	h.fake.status["/v1/repos/1f2e3d/blob/b1"] = http.StatusForbidden
	body := h.get(h.repoPath(""), h.signedIn("alice")).Body.String()
	if !strings.Contains(body, "Files at main") {
		t.Errorf("a refused readme took the overview down:\n%s", body)
	}
	if !strings.Contains(body, "README.md") {
		t.Error("the readme's own row vanished with it")
	}

	h2 := newHarness(t)
	h2.fake.trees[""][1].Size = readmeLimit + 1
	body = h2.get(h2.repoPath(""), h2.signedIn("alice")).Body.String()
	if !strings.Contains(body, "too large to show here") {
		t.Errorf("a very large readme reads as:\n%s", body)
	}
}

func TestKeysNeedsASession(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.KeysURL = "https://keys.example" })
	rec := h.get("/keys")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/auth/start") {
		t.Errorf("the key screen with no session answered %d", rec.Code)
	}
}

// TestRenderRefusesAScreenThatIsNotOne asserts the last line of the render
// path: a name that is not a screen, or a screen given the wrong data, is a
// server error and never a half written page.
func TestRenderRefusesAScreenThatIsNotOne(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.server.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "not-a-screen", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("an unknown screen answered %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	h.server.render(rec2, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "home", struct{}{})
	if rec2.Code != http.StatusInternalServerError {
		t.Errorf("a screen given the wrong data answered %d", rec2.Code)
	}
}

func TestReturnToKeepsThePathAndItsQuery(t *testing.T) {
	for in, want := range map[string]string{
		"/r/1/log?ref=next": "/r/1/log?ref=next",
		"/":                 "/",
	} {
		req := httptest.NewRequest(http.MethodGet, in, nil)
		if got := returnTo(req); got != want {
			t.Errorf("returnTo(%q) is %q, want %q", in, got, want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "//elsewhere.example/"
	if got := returnTo(req); got != "/" {
		t.Errorf("an off-origin path returned %q", got)
	}
}

func TestListRowReadsARepositoryWithNoPush(t *testing.T) {
	h := newHarness(t)
	repo := h.fake.repo
	repo.PushedAt = nil
	row := h.server.listRow(repo)
	if row.Pushed != "" || row.PushedExact != "" {
		t.Errorf("a repository that was never pushed reads as %q", row.Pushed)
	}
	if row.URL != "/r/1f2e3d" || row.Size == "" {
		t.Errorf("the row reads as %+v", row)
	}
}

func apiFor(t *testing.T, s *httptest.Server) *origo.Client {
	t.Helper()
	return origo.New(mustURL(t, s.URL), s.Client())
}
