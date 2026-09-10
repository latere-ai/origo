// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
)

// mintForm is a submission of the token form with everything filled in.
func mintForm(repo, scope string, ttl int) url.Values {
	return url.Values{"repo": {repo}, "scope": {scope}, "ttl": {strconv.Itoa(ttl)}}
}

// TestATokenIsShownOnceAndNowhereElse is the assertion the screen's own
// sentence makes. The mint answers with the token in the page; every later
// page, including the one the person is sent back to, does not have it,
// because nothing here and nothing at the installation kept a copy.
func TestATokenIsShownOnceAndNowhereElse(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	rec := h.post("/tokens", mintForm(h.fake.repo.ID, "write", 3600), c)
	if rec.Code != http.StatusOK {
		t.Fatalf("the mint answered %d:\n%s", rec.Code, rec.Body.String())
	}
	const token = "orig.a.signed.token.write"
	if !strings.Contains(rec.Body.String(), token) {
		t.Fatalf("the token is not on the page it was minted on:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "You will not see it again") {
		t.Error("the page does not say the token is shown once")
	}

	// Back to the screen, with the same session and the same repository.
	for _, path := range []string{"/tokens", "/tokens?repo=" + h.fake.repo.ID, "/"} {
		if body := h.get(path, c).Body.String(); strings.Contains(body, token) {
			t.Errorf("%s still carries the token", path)
		}
	}
}

// TestTheMintSendsExactlyWhatWasChosen asserts the form's three controls
// reach the installation unchanged, and that a lifetime nobody offered is
// replaced by the default rather than sent.
func TestTheMintSendsExactlyWhatWasChosen(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	h.post("/tokens", mintForm(h.fake.repo.ID, "read", 900), c)
	h.post("/tokens", mintForm(h.fake.repo.ID, "write", 300), c)
	// A lifetime past the cap, which no control offers and a hand-written
	// submission could still carry.
	h.post("/tokens", mintForm(h.fake.repo.ID, "read", 90*24*3600), c)

	got := h.fake.Minted()
	want := []mintCall{
		{Repo: h.fake.repo.ID, Scope: "read", TTL: 900},
		{Repo: h.fake.repo.ID, Scope: "write", TTL: 300},
		{Repo: h.fake.repo.ID, Scope: "read", TTL: 3600},
	}
	if len(got) != len(want) {
		t.Fatalf("the installation was asked %d times, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mint %d was %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestTheLifetimeControlOffersNothingTheInstallationRefuses asserts the
// screen never presents a choice that would come back as a refusal, and that
// the longest it offers is the longest that can be signed.
func TestTheLifetimeControlOffersNothingTheInstallationRefuses(t *testing.T) {
	for _, l := range lifetimes {
		ttl := time.Duration(l.Seconds) * time.Second
		if ttl < origo.MinTokenTTL || ttl > origo.MaxTokenTTL {
			t.Errorf("the control offers %s, which the installation will not sign", l.Label)
		}
	}
	longest := lifetimes[len(lifetimes)-1]
	if time.Duration(longest.Seconds)*time.Second != origo.MaxTokenTTL {
		t.Errorf("the longest lifetime offered is %s, not the cap", longest.Label)
	}
	if !strings.Contains(longest.Label, "longest") {
		t.Errorf("the longest lifetime does not say it is the longest: %q", longest.Label)
	}

	// And the page says so where a person reads it, rather than only in a
	// select nobody opens.
	h := newHarness(t)
	body := h.get("/tokens", h.signedIn("alice")).Body.String()
	if !strings.Contains(body, "1 hour") {
		t.Errorf("the screen does not name the longest lifetime:\n%s", body)
	}
	if strings.Contains(body, "90 days") || strings.Contains(body, "30 days") {
		t.Error("the screen offers a lifetime the installation would refuse")
	}
}

// TestTheTokenScreenSaysThereIsNoRegistry asserts the degradation this
// installation actually has: no list, no revoke link anywhere, and a
// sentence a person can act on rather than an empty table.
func TestTheTokenScreenSaysThereIsNoRegistry(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	rec := h.get("/tokens?repo="+h.fake.repo.ID, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("the screen answered %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "keeps no list of tokens") {
		t.Errorf("the screen does not say it keeps no list:\n%s", body)
	}
	if !strings.Contains(body, "cannot be withdrawn") {
		t.Error("the screen does not say a token cannot be withdrawn")
	}

	page := doc(t, body)
	if n := len(elements(page, "table")); n != 0 {
		t.Errorf("the screen renders %d tables with nothing to put in them", n)
	}
	for _, a := range elements(page, "a") {
		if strings.Contains(attr(a, "href"), "revoke") {
			t.Errorf("the screen offers a revoke this installation cannot do: %q", attr(a, "href"))
		}
	}
	// The one form on the screen is the one that works.
	forms := elements(page, "form")
	if len(forms) != 2 { // the mint form and the masthead's sign-out
		t.Fatalf("the screen carries %d forms", len(forms))
	}
}

// TestTheTokenTableAppearsWhenTheInstallationKeepsOne asserts the other half:
// the screen is written against a capability, so an installation that grows a
// token record renders it here with no change to this interface.
func TestTheTokenTableAppearsWhenTheInstallationKeepsOne(t *testing.T) {
	h := newHarness(t)
	at := time.Now().Add(-12 * time.Minute)
	soon := time.Now().Add(30 * time.Minute)
	h.fake.registry = []origo.TokenRecord{
		{ID: "7Kd2", Scope: origo.ScopeWrite, IssuedAt: &at, ExpiresAt: &soon, LastUsed: &at},
		{ID: "Pz9x", Scope: origo.ScopeRead, IssuedAt: &at, ExpiresAt: &soon},
	}
	body := h.get("/tokens?repo="+h.fake.repo.ID, h.signedIn("alice")).Body.String()
	if strings.Contains(body, "keeps no list of tokens") {
		t.Error("the screen still says there is no list")
	}
	page := doc(t, body)
	tables := elements(page, "table")
	if len(tables) == 0 {
		t.Fatalf("no table was rendered:\n%s", body)
	}
	rows := elements(tables[0], "tr")
	if len(rows) != 3 { // one header, two tokens
		t.Errorf("the table has %d rows, want a header and two tokens", len(rows))
	}
	for _, want := range []string{"7Kd2", "Pz9x", "12 minutes ago", "never used"} {
		if !strings.Contains(text(tables[0]), want) {
			t.Errorf("the table does not carry %q: %s", want, text(tables[0]))
		}
	}
}

// TestTheRepositoryControlDegradesLikeTheHomeScreen asserts the token screen
// asks the same question the home screen asks and answers a "no" the same
// way: a box to name a repository, and a selector the day there is a list.
func TestTheRepositoryControlDegradesLikeTheHomeScreen(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	page := doc(t, h.get("/tokens", c).Body.String())
	if len(elements(page, "select")) != 1 { // the lifetime alone
		t.Error("the screen offers a repository selector on an installation with no list")
	}
	var named bool
	for _, in := range elements(page, "input") {
		if attr(in, "name") == "repo" {
			named = true
		}
	}
	if !named {
		t.Error("the screen offers no way to name a repository")
	}
}

// TestMintingIsRefusedInTheOneSentence asserts a reader who may not mint for
// a repository, and a repository that is not there, read the same, because
// the installation refuses to say which of the two it meant.
func TestMintingIsRefusedInTheOneSentence(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		h := newHarness(t)
		h.fake.mintStatus = status
		rec := h.post("/tokens", mintForm(h.fake.repo.ID, "write", 3600), h.signedIn("alice"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("a %d from the mint answered %d, want 404", status, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "cannot mint tokens for it") {
			t.Errorf("a %d is not the one sentence:\n%s", status, body)
		}
		// The choice is kept, so nothing is retyped.
		if !strings.Contains(body, `value="`+h.fake.repo.ID+`"`) {
			t.Error("the refusal lost the repository that was typed")
		}
		if !strings.Contains(body, `value="write" checked`) {
			t.Error("the refusal lost the scope that was chosen")
		}
	}

	// A repository this reader cannot even read is refused before the mint
	// route is reached at all.
	h := newHarness(t)
	h.fake.status["/v1/repos/1f2e3d"] = http.StatusForbidden
	rec := h.post("/tokens", mintForm(h.fake.repo.ID, "read", 900), h.signedIn("alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a repository that cannot be read answered %d", rec.Code)
	}
	if len(h.fake.Minted()) != 0 {
		t.Error("a repository that cannot be read was still minted against")
	}
}

// TestAnIncompleteMintIsRefusedWithoutACall asserts the form's own rules are
// held here: an empty repository or a scope that is neither never reaches the
// installation.
func TestAnIncompleteMintIsRefusedWithoutACall(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	for _, form := range []url.Values{
		mintForm("", "read", 900),
		mintForm(h.fake.repo.ID, "admin", 900),
		mintForm(h.fake.repo.ID, "", 900),
	} {
		rec := h.post("/tokens", form, c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v answered %d, want 400", form, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Choose a repository") {
			t.Errorf("%v did not say what was missing", form)
		}
	}
	if len(h.fake.Minted()) != 0 {
		t.Errorf("an incomplete form still asked the installation: %+v", h.fake.Minted())
	}
}

// TestAnUnreachableInstallationDoesNotMint asserts the two other refusals of
// the mint reach the reader as the sentences part one fixed for them.
func TestAnUnreachableInstallationDoesNotMint(t *testing.T) {
	h := newHarness(t)
	h.fake.mintStatus = http.StatusServiceUnavailable
	rec := h.post("/tokens", mintForm(h.fake.repo.ID, "read", 900), h.signedIn("alice"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("an installation that is not answering gave %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not answering right now") {
		t.Errorf("the sentence is not the one every other screen uses:\n%s", rec.Body.String())
	}

	// A credential the installation refuses signs the reader out, once.
	h = newHarness(t)
	h.fake.mintStatus = http.StatusUnauthorized
	rec = h.post("/tokens", mintForm(h.fake.repo.ID, "read", 900), h.signedIn("alice"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a refused credential gave %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/auth/start") {
		t.Error("the page offers no way to sign in again")
	}
}

// TestTheTokenScreenNeedsASession asserts a signed-out visitor is shown the
// front door and not a form that could not work.
func TestTheTokenScreenNeedsASession(t *testing.T) {
	h := newHarness(t)
	if body := h.get("/tokens").Body.String(); !strings.Contains(body, "/auth/start") {
		t.Errorf("the token screen signed out is not the sign-in page:\n%s", body)
	}
	if len(h.fake.Calls()) != 0 {
		t.Errorf("the signed-out screen still asked the installation: %v", h.fake.Calls())
	}
}

// TestTheSecretPanelCarriesNoControlThatNeedsAScript asserts the one place a
// design would reach for a clipboard button. There is no script on any page,
// so a copy button could never work, and a control that cannot work is worse
// than none.
func TestTheSecretPanelCarriesNoControlThatNeedsAScript(t *testing.T) {
	h := newHarness(t)
	rec := h.post("/tokens", mintForm(h.fake.repo.ID, "read", 900), h.signedIn("alice"))
	page := doc(t, rec.Body.String())

	if n := len(elements(page, "script")); n != 0 {
		t.Errorf("the token page carries %d scripts", n)
	}
	for _, b := range elements(page, "button") {
		if attr(b, "type") == "button" {
			t.Errorf("the token page carries a control only a script could act on: %q", text(b))
		}
	}
	for _, a := range elements(page, "a") {
		if attr(a, "href") == "" {
			t.Errorf("the token page carries a link with no address: %q", text(a))
		}
	}
	// The value is in a field a person can select, and the page says how.
	var field bool
	for _, in := range elements(page, "input") {
		if attr(in, "id") == "token" && hasAttr(in, "readonly") {
			field = true
		}
	}
	if !field {
		t.Error("the token is not in a field that can be selected and copied")
	}
	if !strings.Contains(rec.Body.String(), "no copy button") {
		t.Error("the page does not say why there is no copy button")
	}
	// The clone and read lines are the installation's own addresses.
	for _, want := range []string{"x-access-token:$ORIGO_TOKEN@git.example/infra/origo.git", "/v1/repos/1f2e3d"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the page does not carry %q", want)
		}
	}
}

// TestTheDocumentationSaysWhatIsBuiltAndWhatIsNot asserts the page does not
// promise a tool server that does not exist, and that it says the things the
// installation actually does.
func TestTheDocumentationSaysWhatIsBuiltAndWhatIsNot(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/docs/agents")
	if rec.Code != http.StatusOK {
		t.Fatalf("the documentation answered %d", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Planned, not built") {
		t.Error("the page does not mark the tool server as unbuilt")
	}
	for _, wrong := range []string{
		"retryable",                // the refusal document carries no such field
		"X-RateLimit",              // the installation sends one header, not three
		"token_scope_insufficient", // not one of the codes
		"branch_protected",         // there is no branch protection
		"repo_locked",              // not a code either
		"90 days",                  // not a lifetime that can be signed
		".patch",                   // there is no format suffix on any address
		"/api/v1",                  // nor a version prefix of that shape
	} {
		if strings.Contains(body, wrong) {
			t.Errorf("the page claims %q, which is not true of this installation", wrong)
		}
	}
	for _, want := range []string{
		"RateLimit-Limit",  // the one header that is sent
		"Origo-Contract",   // on every answer
		"Origo-Truncated",  // how a cut body says so
		"/v1/repos/",       // the shape of every address
		"rate_limited",     // a code that exists
		"non_fast_forward", // and another
		"one hour",         // the lifetime cap
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not carry %q", want)
		}
	}

}

// TestTheDocumentationIsSelfContained asserts every section the contents
// lists is on the page, that every address on it comes from configuration,
// and that it reaches nothing off this installation.
func TestTheDocumentationIsSelfContained(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.CloneHost = mustURL(t, "https://git.elsewhere")
	})
	body := h.get("/docs/agents").Body.String()
	page := doc(t, body)

	ids := map[string]bool{}
	find(page, func(e *html.Node) {
		if id := attr(e, "id"); id != "" {
			ids[id] = true
		}
	})
	var anchors int
	for _, a := range elements(page, "a") {
		href := attr(a, "href")
		if href == "" {
			t.Errorf("a link with no address: %q", text(a))
			continue
		}
		switch {
		case strings.HasPrefix(href, "#"):
			anchors++
			if !ids[strings.TrimPrefix(href, "#")] {
				t.Errorf("the contents points at %q, which is not on the page", href)
			}
		case strings.HasPrefix(href, "/"):
		default:
			t.Errorf("the page links off this installation: %q", href)
		}
	}
	if anchors != 6 {
		t.Errorf("the contents lists %d sections, want six", anchors)
	}

	// The clone line is the configured git host and not a host from a
	// design, and the read line is the configured installation.
	if !strings.Contains(body, "git.elsewhere") {
		t.Errorf("the clone line does not come from configuration:\n%s", body)
	}
	if strings.Contains(body, "git.example.com") {
		t.Error("the page carries an address nobody configured")
	}
	if !strings.Contains(body, h.cfg.OrigoURL.String()+"/v1/repos/") {
		t.Error("the read line does not point at the installation")
	}
}
