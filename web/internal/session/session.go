// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package session holds the signed-in person's identity for the length of
// one browsing session.
//
// It is a thin layer over latere.ai/x/pkg/authkit/oidc, which is the relying
// party every Latere web surface uses: authorization code with PKCE, an
// encrypted __Host- cookie, and refresh. What this package adds is the three
// rules spec 023 fixes and the library leaves to its caller: a twelve hour
// window that a refresh never extends, a sign-in that preserves the path the
// person asked for, and the short list of repositories this session has
// opened, which lives in a cookie and in no server-side store.
//
// The access token is in the encrypted cookie and nowhere else. It is never
// in a URL, never in the rendered HTML, and never in a log line.
package session

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit"
	"latere.ai/x/pkg/authkit/oidc"
)

const (
	// CookieName is the encrypted session cookie. The __Host- prefix binds
	// it to this origin and to a secure connection, which a browser
	// enforces whatever a response says.
	CookieName = "__Host-origoweb-session"

	// RecentCookieName holds the repository ids this session has opened.
	// It carries no credential: it is the home screen's substitute for a
	// directory Origo cannot serve.
	RecentCookieName = "__Host-origoweb-recent"

	// CSRFCookieName seeds the token the two POST routes require.
	CSRFCookieName = "__Host-origoweb-csrf"

	// Lifetime is how long a session lives from sign-in. A refresh renews
	// the access token inside the window and never moves the window, so a
	// stolen cookie has a bounded life.
	Lifetime = 12 * time.Hour

	// recentLimit is how many repositories the home screen remembers.
	recentLimit = 12
)

// ErrNoSession means the request carries no usable session: no cookie, a
// cookie this key does not decrypt, an elapsed window, or a refresh that
// failed. Every one of them ends the same way, at the sign-in screen.
var ErrNoSession = errors.New("session: no usable session")

// Manager reads and writes sessions.
type Manager struct {
	client *oidc.Client
	secure bool

	// The three cookie names in force. A __Host- prefix binds a cookie to
	// one origin and to a secure connection, and a browser rejects such a
	// cookie outright when the connection is not secure, so a local run
	// over plain HTTP carries the same names without the prefix. The
	// library does this to the session cookie; these are the two this
	// service writes itself, plus the effective session name, which Load
	// needs before it asks the library for anything.
	sessionName string
	recentName  string
	csrfName    string
}

// New returns a manager over the configured relying party. The session
// cookie name and the twelve hour window are this service's, not the
// caller's, so they cannot be configured away.
func New(cfg oidc.Config) (*Manager, error) {
	cfg.CookieName = CookieName
	cfg.SessionTTL = Lifetime
	c := oidc.New(cfg)
	if c == nil {
		return nil, errors.New("session: the identity provider is not configured")
	}
	secure := !cfg.InsecureCookies
	return &Manager{
		client:      c,
		secure:      secure,
		sessionName: hostPrefixed(CookieName, secure),
		recentName:  hostPrefixed(RecentCookieName, secure),
		csrfName:    hostPrefixed(CSRFCookieName, secure),
	}, nil
}

// hostPrefixed drops the __Host- prefix when the connection is not secure,
// the way authkit does, because a browser rejects a __Host- cookie that is
// not Secure and the cookie would simply never arrive.
func hostPrefixed(name string, secure bool) string {
	if secure {
		return name
	}
	return strings.TrimPrefix(name, "__Host-")
}

// SignIn starts the authorization-code flow and redirects to the issuer.
// returnTo is the path the person asked for; the callback lands there.
func (m *Manager) SignIn(w http.ResponseWriter, r *http.Request, returnTo string) {
	q := url.Values{}
	q.Set("return_to", safePath(returnTo))
	r2 := r.Clone(r.Context())
	r2.URL = &url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	m.client.HandleLogin(w, r2)
}

// Callback exchanges the code, writes the session cookie, and redirects to
// the path SignIn preserved.
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) {
	m.client.HandleCallback(w, r)
}

// Load returns the session on the request, refreshing the access token when
// it is within a minute of expiry and writing the refreshed session back.
//
// Every failure is ErrNoSession with the cookie cleared, so a caller has one
// case to handle and cannot leave a half-dead cookie in place.
func (m *Manager) Load(w http.ResponseWriter, r *http.Request) (*oidc.Session, error) {
	if _, err := r.Cookie(m.sessionName); err != nil {
		return nil, ErrNoSession
	}
	sess, err := m.client.SessionFromRequest(w, r)
	if err != nil {
		m.Clear(w)
		return nil, ErrNoSession
	}
	return sess, nil
}

// Token is the access token of the session on the request, or the empty
// string when there is none. A caller sends it to Origo unchanged, and sends
// no Authorization header at all when it is empty.
func (m *Manager) Token(w http.ResponseWriter, r *http.Request) string {
	sess, err := m.Load(w, r)
	if err != nil {
		return ""
	}
	return sess.AccessToken
}

// Clear removes the session and the recent list.
func (m *Manager) Clear(w http.ResponseWriter) {
	m.client.ClearSession(w)
	http.SetCookie(w, &http.Cookie{
		Name: m.recentName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
	})
}

// CSRFToken is the token the two POST routes require, issued once per
// browser session and not once per page.
//
// A fresh one on every render would rotate the cookie, and the form on a
// page a person left open would then be refused: two tabs of the same
// interface would break each other. So a request that already carries one
// keeps it, and only a request with none is given one.
func (m *Manager) CSRFToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(m.csrfName); err == nil && c.Value != "" {
		return c.Value
	}
	return authkit.CSRFIssue(w, m.csrfName, m.secure)
}

// CSRFValid reports whether a POST carries a token matching its cookie.
func (m *Manager) CSRFValid(r *http.Request) bool {
	return authkit.CSRFValidate(r, m.csrfName)
}

// CSRFCookie is the name of the token cookie in force, which a test and a
// local run need to know because the prefix depends on the connection.
func (m *Manager) CSRFCookie() string { return m.csrfName }

// RecentCookie is the name of the recent-repository cookie in force.
func (m *Manager) RecentCookie() string { return m.recentName }

// SessionCookie is the name of the session cookie in force.
func (m *Manager) SessionCookie() string { return m.sessionName }

// CSRFField is the form field name a template writes the token into.
func CSRFField() string { return authkit.CSRFFieldName() }

// Recent reads the repositories this session has opened, newest first.
func (m *Manager) Recent(r *http.Request) []string {
	c, err := r.Cookie(m.recentName)
	if err != nil || c.Value == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(c.Value, " ") {
		id, err := url.QueryUnescape(part)
		if err != nil || id == "" || !plausibleID(id) {
			continue
		}
		out = append(out, id)
		if len(out) == recentLimit {
			break
		}
	}
	return out
}

// Remember moves one repository to the front of the recent list.
func (m *Manager) Remember(w http.ResponseWriter, r *http.Request, id string) {
	if !plausibleID(id) {
		return
	}
	next := []string{id}
	for _, old := range m.Recent(r) {
		if old != id {
			next = append(next, old)
		}
		if len(next) == recentLimit {
			break
		}
	}
	parts := make([]string, len(next))
	for i, id := range next {
		parts[i] = url.QueryEscape(id)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     m.recentName,
		Value:    strings.Join(parts, " "),
		Path:     "/",
		MaxAge:   int(Lifetime / time.Second),
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// plausibleID keeps a cookie a person edited from reaching a request path.
// A repository id is an opaque identifier, so the shape is all this service
// can check; Origo decides whether it names anything.
func plausibleID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// safePath keeps an open redirect out of the sign-in flow: only a path on
// this origin survives.
func safePath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "/"
	}
	return p
}
