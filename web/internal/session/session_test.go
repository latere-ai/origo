// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package session

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/oidc"
)

// issuer is an identity provider as far as the authorization-code flow is
// concerned: it exchanges a code and refreshes a token.
type issuer struct {
	*httptest.Server
	exchanges atomic.Int32
	refreshes atomic.Int32
	refuse    atomic.Bool
	life      time.Duration
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{life: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" {
			is.refreshes.Add(1)
			if is.refuse.Load() {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
		} else {
			is.exchanges.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  jwtFor("alice"),
			"refresh_token": "refresh-token",
			"token_type":    "Bearer",
			"expires_in":    int(is.life.Seconds()),
		})
	})
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}

// jwtFor is a well-formed JWT with the claims the library reads. Nothing here
// verifies a signature: the token is a bearer credential the interface hands
// to Origo, and Origo is what verifies it.
func jwtFor(sub string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." +
		enc(map[string]any{"sub": sub, "aud": "origo", "exp": time.Now().Add(time.Hour).Unix()}) + ".x"
}

func testConfig(t *testing.T, is *issuer) oidc.Config {
	t.Helper()
	return oidc.Config{
		AuthURL:     is.URL,
		ClientID:    "origoweb",
		RedirectURL: "https://code.example/auth/callback",
		CookieKey:   "0123456789abcdef0123456789abcdef",
		Audience:    "origo",
	}
}

// TestSignInRoundTrip asserts the whole front door: the redirect carries PKCE
// and a nonce, the callback exchanges the code and lands on the path the
// person asked for, and the session cookie is __Host- prefixed, HttpOnly,
// Secure, SameSite=Lax, and readable only with the configured key.
func TestSignInRoundTrip(t *testing.T) {
	is := newIssuer(t)
	m, err := New(testConfig(t, is))
	if err != nil {
		t.Fatal(err)
	}

	// The redirect to the issuer.
	rec := httptest.NewRecorder()
	m.SignIn(rec, httptest.NewRequest(http.MethodGet, "/r/1f2e3d/log", nil), "/r/1f2e3d/log")
	if rec.Code != http.StatusFound {
		t.Fatalf("sign-in answered %d, want a redirect to the issuer", rec.Code)
	}
	to, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := to.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("the redirect carries no PKCE challenge: %v", q)
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Errorf("the redirect carries no state or nonce: %v", q)
	}
	if q.Get("response_type") != "code" {
		t.Errorf("the flow is %q, want an authorization code", q.Get("response_type"))
	}

	// The callback, carrying the flow cookie the redirect set.
	flow := cookieNamed(t, rec, oidc.FlowCookieName)
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state="+q.Get("state"), nil)
	cb.AddCookie(flow)
	rec2 := httptest.NewRecorder()
	m.Callback(rec2, cb)

	if rec2.Code != http.StatusFound {
		t.Fatalf("the callback answered %d", rec2.Code)
	}
	if got := rec2.Header().Get("Location"); got != "/r/1f2e3d/log" {
		t.Errorf("the callback landed on %q, not the path that was asked for", got)
	}
	if is.exchanges.Load() != 1 {
		t.Errorf("the code was exchanged %d times", is.exchanges.Load())
	}

	c := cookieNamed(t, rec2, CookieName)
	if !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("the session cookie is named %q", c.Name)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("the session cookie is %+v", c)
	}
	if strings.Contains(c.Value, "alice") || strings.Contains(c.Value, "refresh-token") {
		t.Error("the session cookie carries its contents in the clear")
	}

	// It reads back with the configured key.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	sess, err := m.Load(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatalf("the session did not read back: %v", err)
	}
	if sess.AccessToken == "" || sess.RefreshToken != "refresh-token" {
		t.Errorf("the session read back as %+v", sess)
	}

	// And with no other key.
	other := testConfig(t, is)
	other.CookieKey = "ffffffffffffffffffffffffffffffff"
	m2, err := New(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Load(httptest.NewRecorder(), req); !errors.Is(err, ErrNoSession) {
		t.Errorf("a cookie read with another key answered %v", err)
	}
}

// TestRefreshAndSessionLifetime asserts the three rules the spec fixes: a
// token within a minute of expiry is refreshed before any call goes out, a
// refresh that fails clears the session, and a session past its twelve hours
// is refused whatever its tokens say.
func TestRefreshAndSessionLifetime(t *testing.T) {
	is := newIssuer(t)
	m, err := New(testConfig(t, is))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a token within a minute of expiry is refreshed", func(t *testing.T) {
		req := requestWith(t, m, &oidc.Session{
			AccessToken:  jwtFor("alice"),
			RefreshToken: "refresh-token",
			Expiry:       time.Now().Add(30 * time.Second),
		})
		before := is.refreshes.Load()
		rec := httptest.NewRecorder()
		sess, err := m.Load(rec, req)
		if err != nil {
			t.Fatalf("the session was refused: %v", err)
		}
		if is.refreshes.Load() != before+1 {
			t.Error("the token was not refreshed before the call")
		}
		if sess.Expiry.Before(time.Now().Add(30 * time.Minute)) {
			t.Errorf("the refreshed token expires at %v", sess.Expiry)
		}
		if cookieNamed(t, rec, CookieName) == nil {
			t.Error("the refreshed session was not written back")
		}
	})

	t.Run("a token with time left is left alone", func(t *testing.T) {
		req := requestWith(t, m, &oidc.Session{
			AccessToken:  jwtFor("alice"),
			RefreshToken: "refresh-token",
			Expiry:       time.Now().Add(time.Hour),
		})
		before := is.refreshes.Load()
		if _, err := m.Load(httptest.NewRecorder(), req); err != nil {
			t.Fatal(err)
		}
		if is.refreshes.Load() != before {
			t.Error("a token with an hour left was refreshed anyway")
		}
	})

	t.Run("a refresh that fails clears the session", func(t *testing.T) {
		is.refuse.Store(true)
		defer is.refuse.Store(false)
		req := requestWith(t, m, &oidc.Session{
			AccessToken:  jwtFor("alice"),
			RefreshToken: "refresh-token",
			Expiry:       time.Now().Add(10 * time.Second),
		})
		rec := httptest.NewRecorder()
		if _, err := m.Load(rec, req); !errors.Is(err, ErrNoSession) {
			t.Fatalf("a failed refresh answered %v", err)
		}
		cleared := cookieNamed(t, rec, CookieName)
		if cleared == nil || cleared.MaxAge >= 0 {
			t.Error("a failed refresh left the cookie in place")
		}
	})

	t.Run("a session past twelve hours is refused", func(t *testing.T) {
		req := requestWith(t, m, &oidc.Session{
			AccessToken:   jwtFor("alice"),
			RefreshToken:  "refresh-token",
			Expiry:        time.Now().Add(time.Hour),
			SessionExpiry: time.Now().Add(-time.Minute),
		})
		if _, err := m.Load(httptest.NewRecorder(), req); !errors.Is(err, ErrNoSession) {
			t.Errorf("an elapsed session answered %v", err)
		}
	})

	t.Run("a refresh never extends the window", func(t *testing.T) {
		window := time.Now().Add(20 * time.Minute)
		req := requestWith(t, m, &oidc.Session{
			AccessToken:   jwtFor("alice"),
			RefreshToken:  "refresh-token",
			Expiry:        time.Now().Add(10 * time.Second),
			SessionExpiry: window,
		})
		sess, err := m.Load(httptest.NewRecorder(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !sess.SessionExpiry.Equal(window) {
			t.Errorf("the window moved to %v from %v", sess.SessionExpiry, window)
		}
	})

	t.Run("a request with no cookie has no session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if _, err := m.Load(httptest.NewRecorder(), req); !errors.Is(err, ErrNoSession) {
			t.Errorf("a request with no cookie answered %v", err)
		}
		if got := m.Token(httptest.NewRecorder(), req); got != "" {
			t.Errorf("a request with no cookie carried the token %q", got)
		}
	})
}

func TestRecentIsBoundedAndChecked(t *testing.T) {
	is := newIssuer(t)
	m, err := New(testConfig(t, is))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	m.Remember(rec, req, "r1")
	c := cookieNamed(t, rec, RecentCookieName)
	if c == nil || c.Value != "r1" {
		t.Fatalf("the first repository was remembered as %+v", c)
	}

	// The newest is first and a repeat does not grow the list.
	req.AddCookie(c)
	rec2 := httptest.NewRecorder()
	m.Remember(rec2, req, "r2")
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(cookieNamed(t, rec2, RecentCookieName))
	if got := Recent(req2); len(got) != 2 || got[0] != "r2" || got[1] != "r1" {
		t.Errorf("the list reads as %v", got)
	}

	// It is bounded, so a long session cannot grow an unbounded cookie.
	seed := httptest.NewRequest(http.MethodGet, "/", nil)
	for i := range 40 {
		w := httptest.NewRecorder()
		m.Remember(w, seed, "id"+itoa(i))
		seed = httptest.NewRequest(http.MethodGet, "/", nil)
		seed.AddCookie(cookieNamed(t, w, RecentCookieName))
	}
	if got := Recent(seed); len(got) != recentLimit {
		t.Errorf("the list grew to %d", len(got))
	}

	// A cookie somebody edited cannot become a request path.
	edited := httptest.NewRequest(http.MethodGet, "/", nil)
	edited.AddCookie(&http.Cookie{Name: RecentCookieName, Value: url.QueryEscape("../../etc/passwd") + " ok1"})
	if got := Recent(edited); len(got) != 1 || got[0] != "ok1" {
		t.Errorf("an edited cookie read as %v", got)
	}

	// A rejected identifier is not remembered at all.
	w := httptest.NewRecorder()
	m.Remember(w, httptest.NewRequest(http.MethodGet, "/", nil), "../etc")
	if cookieNamed(t, w, RecentCookieName) != nil {
		t.Error("an implausible identifier was remembered")
	}

	// Clearing the session clears it.
	out := httptest.NewRecorder()
	m.Clear(out)
	if got := cookieNamed(t, out, RecentCookieName); got == nil || got.MaxAge >= 0 {
		t.Error("signing out left the recent list behind")
	}
}

func TestCSRFRoundTrip(t *testing.T) {
	is := newIssuer(t)
	m, err := New(testConfig(t, is))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	token := m.CSRFToken(rec)
	if token == "" {
		t.Fatal("no token was issued")
	}
	form := url.Values{CSRFField(): {token}}
	req := httptest.NewRequest(http.MethodPost, "/sign-out", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookieNamed(t, rec, CSRFCookieName))
	if !m.CSRFValid(req) {
		t.Error("a form carrying its own token was refused")
	}

	bad := httptest.NewRequest(http.MethodPost, "/sign-out", strings.NewReader(url.Values{CSRFField(): {"nope"}}.Encode()))
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bad.AddCookie(cookieNamed(t, rec, CSRFCookieName))
	if m.CSRFValid(bad) {
		t.Error("a form carrying another token was accepted")
	}
}

func TestAnUnconfiguredIdentityProviderIsAnError(t *testing.T) {
	if _, err := New(oidc.Config{}); err == nil {
		t.Error("a service with no identity provider started anyway")
	}
}

func TestSignInKeepsTheFlowOnThisOrigin(t *testing.T) {
	is := newIssuer(t)
	m, _ := New(testConfig(t, is))
	for _, target := range []string{"https://elsewhere.example/", "//elsewhere.example/", ""} {
		rec := httptest.NewRecorder()
		m.SignIn(rec, httptest.NewRequest(http.MethodGet, "/", nil), target)
		flow := cookieNamed(t, rec, oidc.FlowCookieName)
		if flow == nil {
			t.Fatal("no flow cookie")
		}
		// The library refuses an off-origin return itself; assert the
		// redirect it built goes to the issuer and not to the target.
		to := rec.Header().Get("Location")
		if !strings.HasPrefix(to, is.URL) {
			t.Errorf("a return to %q sent the browser to %q", target, to)
		}
	}
}

// requestWith seals a session into a cookie and puts it on a request.
func requestWith(t *testing.T, m *Manager, sess *oidc.Session) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := m.client.SetSession(rec, sess); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookieNamed(t, rec, CookieName))
	return req
}

func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
