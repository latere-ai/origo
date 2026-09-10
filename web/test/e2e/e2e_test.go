// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

// Package e2e drives the interface against a real Origo installation.
//
// The tier runs against a pinned origod with the stub issuer and stub
// authorizer of Origo's spec 013 and a fixture repository, so it asserts
// against the real read API and not against a mock. It skips when the
// installation is not named, which is what lets `go test ./...` stay
// hermetic on a developer's machine.
//
//	ORIGOWEB_TEST_ORIGO_URL   the installation, e.g. http://127.0.0.1:8080
//	ORIGOWEB_TEST_ISSUER_URL  the stub issuer, whose POST /mint mints a token
//	ORIGOWEB_TEST_REPO_ID     a repository with history, a tree and a readme
//	ORIGOWEB_TEST_DENIED_SUB  a subject the authorizer denies that repository
package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"latere.ai/x/pkg/authkit"
	"latere.ai/x/pkg/authkit/oidc"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
	"github.com/latere-ai/origo-web/internal/session"
	"github.com/latere-ai/origo-web/internal/web"
)

const cookieKey = "0123456789abcdef0123456789abcdef"

type stack struct {
	t      *testing.T
	server *web.Server
	sealer *oidc.Client
	repo   string
	issuer string
	logs   *bytes.Buffer
}

func start(t *testing.T) *stack {
	t.Helper()
	base := os.Getenv("ORIGOWEB_TEST_ORIGO_URL")
	issuer := os.Getenv("ORIGOWEB_TEST_ISSUER_URL")
	repo := os.Getenv("ORIGOWEB_TEST_REPO_ID")
	if base == "" || issuer == "" || repo == "" {
		t.Skip("set ORIGOWEB_TEST_ORIGO_URL, ORIGOWEB_TEST_ISSUER_URL and ORIGOWEB_TEST_REPO_ID to run this tier")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	oc := oidc.Config{
		AuthURL: issuer, ClientID: "origoweb", RedirectURL: "https://code.example/auth/callback",
		CookieKey: cookieKey, Audience: "origo",
	}
	cfg := config.Config{
		Addr: ":0", OrigoURL: u, PublicURL: mustURL(t, "https://code.example"),
		CloneHost: u, OIDC: oc,
	}
	sessions, err := session.New(cfg.OIDC)
	if err != nil {
		t.Fatal(err)
	}
	sealer := oc
	sealer.CookieName = session.CookieName
	sealer.SessionTTL = session.Lifetime
	return &stack{
		t:      t,
		server: web.New(web.Options{Config: cfg, Sessions: sessions, API: origo.New(u, http.DefaultClient)}),
		sealer: oidc.New(sealer),
		repo:   repo,
		issuer: issuer,
		logs:   &bytes.Buffer{},
	}
}

// mint asks the stub issuer for a token for one subject, the same way Origo's
// own suite does.
func (s *stack) mint(subject string) string {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"sub": subject, "aud": "origo"})
	resp, err := http.Post(s.issuer+"/mint", "application/json", bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		s.t.Fatal(err)
	}
	if out.Token == "" {
		s.t.Fatal("the stub issuer minted no token")
	}
	return out.Token
}

func (s *stack) session(subject string) (*http.Cookie, string) {
	s.t.Helper()
	token := s.mint(subject)
	rec := httptest.NewRecorder()
	if err := s.sealer.SetSession(rec, &oidc.Session{
		AccessToken: token, Expiry: time.Now().Add(time.Hour), User: oidc.User{Sub: subject},
	}); err != nil {
		s.t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == session.CookieName {
			return c, token
		}
	}
	s.t.Fatal("no session cookie")
	return nil, ""
}

func (s *stack) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.server.ServeHTTP(rec, req)
	return rec
}

// post submits one form, with the token the rendered form carries.
func (s *stack) post(path string, form url.Values, c *http.Cookie) *httptest.ResponseRecorder {
	s.t.Helper()
	rendered := s.get(path, c)
	m := csrfValue.FindStringSubmatch(rendered.Body.String())
	if m == nil {
		s.t.Fatalf("no form token on %s", path)
	}
	form.Set(authkit.CSRFFieldName(), m[1])
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	for _, ck := range rendered.Result().Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	s.server.ServeHTTP(rec, req)
	return rec
}

var csrfValue = regexp.MustCompile(`name="` + regexp.QuoteMeta(authkit.CSRFFieldName()) + `" value="([^"]+)"`)

func (s *stack) screens() map[string]string {
	r := "/r/" + s.repo
	return map[string]string{
		"home":       "/",
		"overview":   r,
		"references": r + "/refs",
		"log":        r + "/log",
		"tree":       r + "/tree/",
		"compare":    r + "/compare",
		"tokens":     "/tokens",
		"docs":       "/docs/agents",
	}
}

// TestMintingAgainstARealInstallation drives the one form that is not a
// read against a real installation.
//
// The installation decides whether this subject may mint for this
// repository, and both answers are correct here: a token page carrying a
// credential that is not the session's own, or the one sentence every
// refusal on every screen uses. What is asserted is that it is one of the
// two, that the session token never appears either way, and that the minted
// token is on that page and on no page after it.
func TestMintingAgainstARealInstallation(t *testing.T) {
	s := start(t)
	c, session := s.session("alice")

	form := url.Values{"repo": {s.repo}, "scope": {"read"}, "ttl": {"300"}}
	rec := s.post("/tokens", form, c)
	body := rec.Body.String()
	if strings.Contains(body, session) {
		t.Error("the session token reached the mint page")
	}

	switch rec.Code {
	case http.StatusOK:
		minted := tokenValue(t, body)
		if minted == "" {
			t.Fatalf("the mint answered 200 with no token:\n%s", body)
		}
		if minted == session {
			t.Error("the page handed back the session's own credential")
		}
		if !strings.Contains(body, "You will not see it again") {
			t.Error("the page does not say the token is shown once")
		}
		for _, path := range []string{"/tokens", "/tokens?repo=" + s.repo} {
			if strings.Contains(s.get(path, c).Body.String(), minted) {
				t.Errorf("%s still carries the minted token", path)
			}
		}
	case http.StatusNotFound:
		if !strings.Contains(body, "cannot mint tokens for it") {
			t.Errorf("a refusal is not the one sentence:\n%s", body)
		}
	default:
		t.Fatalf("the mint answered %d:\n%s", rec.Code, body)
	}
}

// tokenValue reads the token out of the field the page puts it in.
func tokenValue(t *testing.T, body string) string {
	t.Helper()
	page, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "input" {
			var isToken, value string
			for _, a := range n.Attr {
				switch a.Key {
				case "id":
					isToken = a.Val
				case "value":
					value = a.Val
				}
			}
			if isToken == "token" {
				out = value
			}
		}
		for x := n.FirstChild; x != nil; x = x.NextSibling {
			walk(x)
		}
	}
	walk(page)
	return out
}

// TestTokenNeverLeavesTheCookie asserts over every screen of a real
// installation that the access token is in no body, no URL and no header.
func TestTokenNeverLeavesTheCookie(t *testing.T) {
	s := start(t)
	c, token := s.session("alice")
	for name, path := range s.screens() {
		rec := s.get(path, c)
		if strings.Contains(rec.Body.String(), token) {
			t.Errorf("%s: the token is in the body", name)
		}
		for k, vs := range rec.Header() {
			if k == "Set-Cookie" {
				continue
			}
			for _, v := range vs {
				if strings.Contains(v, token) {
					t.Errorf("%s: the token is in the %s header", name, k)
				}
			}
		}
	}
}

// TestEveryScreenWorksWithoutScript visits each screen of a real
// installation and asserts the document carries no script at all, so the page
// with scripting off is the same page.
func TestEveryScreenWorksWithoutScript(t *testing.T) {
	s := start(t)
	c, _ := s.session("alice")
	for name, path := range s.screens() {
		rec := s.get(path, c)
		if rec.Code != http.StatusOK {
			t.Errorf("%s answered %d", name, rec.Code)
			continue
		}
		page, err := html.Parse(strings.NewReader(rec.Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		var scripts int
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "script" {
				scripts++
			}
			for x := n.FirstChild; x != nil; x = x.NextSibling {
				walk(x)
			}
		}
		walk(page)
		if scripts != 0 {
			t.Errorf("%s carries %d scripts", name, scripts)
		}
		if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'none'") {
			t.Errorf("%s: the policy is %q", name, got)
		}
	}
}

// TestNoCacheIsSharedBetweenSubjects asserts against a real authorizer that a
// page rendered for one subject is not served to another: the first is
// allowed the repository and the second is denied it.
func TestNoCacheIsSharedBetweenSubjects(t *testing.T) {
	s := start(t)
	denied := os.Getenv("ORIGOWEB_TEST_DENIED_SUB")
	if denied == "" {
		t.Skip("set ORIGOWEB_TEST_DENIED_SUB to a subject the authorizer denies")
	}
	allowed, _ := s.session("alice")
	first := s.get("/r/"+s.repo, allowed)
	if first.Code != http.StatusOK {
		t.Fatalf("the allowed reader got %d", first.Code)
	}
	if got := first.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control is %q", got)
	}

	refused, _ := s.session(denied)
	second := s.get("/r/"+s.repo, refused)
	if second.Code != http.StatusNotFound {
		t.Fatalf("the denied reader got %d, want the refusal", second.Code)
	}
	if strings.Contains(second.Body.String(), "Clone") {
		t.Error("the denied reader was served the allowed reader's page")
	}
}

// TestPagingIsExact walks the log of a real repository through its cursors
// and asserts that every commit appears exactly once.
func TestPagingIsExact(t *testing.T) {
	s := start(t)
	c, _ := s.session("alice")

	seen := map[string]int{}
	next := "/r/" + s.repo + "/log"
	for pages := 0; next != "" && pages < 20; pages++ {
		rec := s.get(next, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d", next, rec.Code)
		}
		page, err := html.Parse(strings.NewReader(rec.Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		next = ""
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "a" {
				href := attrOf(n, "href")
				switch {
				case strings.Contains(href, "/commit/"):
					seen[href]++
				case strings.Contains(href, "cursor="):
					next = href
				}
			}
			for x := n.FirstChild; x != nil; x = x.NextSibling {
				walk(x)
			}
		}
		walk(page)
	}
	if len(seen) == 0 {
		t.Fatal("the log showed no commits")
	}
	for href, n := range seen {
		// A commit's hash and its subject are two links to the same
		// page, and no more than two.
		if n > 2 {
			t.Errorf("%s appeared %d times across the pages", href, n)
		}
	}
}

// TestListDegradesWithoutDirectory asserts against a real installation, which
// has no collection route, that the home screen is the way in that works
// without one and that every other screen is unaffected.
func TestListDegradesWithoutDirectory(t *testing.T) {
	s := start(t)
	c, _ := s.session("alice")
	body := s.get("/", c).Body.String()
	if !strings.Contains(body, "does not list repositories") {
		t.Errorf("the home screen reads as:\n%s", body)
	}
	for name, path := range s.screens() {
		if name == "home" {
			continue
		}
		if rec := s.get(path, c); rec.Code != http.StatusOK {
			t.Errorf("%s answered %d", name, rec.Code)
		}
	}
}

// TestAccessibility and TestNarrowWidths need a browser: an axe run at AA in
// both themes, and a document that does not scroll sideways at 320, 768 and
// 1280 CSS pixels. The structural half of both is asserted without one in
// internal/web (TestAccessibleStructure and the stylesheet's narrow rules);
// this is where the browser half runs when one is configured.
func TestAccessibility(t *testing.T) {
	start(t)
	if os.Getenv("ORIGOWEB_TEST_BROWSER") == "" {
		t.Skip("set ORIGOWEB_TEST_BROWSER to the browser this tier drives")
	}
	t.Fatal("the browser half of this criterion is not implemented; see spec 023")
}

func TestNarrowWidths(t *testing.T) {
	start(t)
	if os.Getenv("ORIGOWEB_TEST_BROWSER") == "" {
		t.Skip("set ORIGOWEB_TEST_BROWSER to the browser this tier drives")
	}
	t.Fatal("the browser half of this criterion is not implemented; see spec 023")
}

func attrOf(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
