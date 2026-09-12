// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/stubs/issuer"
)

// The route set of spec 027, asserted on the matcher alone. The wiring
// through a running node is cmd/origod's route sweep.

const anonRepo = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

// testVerifier builds a verifier with the anonymous switch in the state
// given, over the stub issuers named and none by default, because most
// requests here either carry no credential or carry one that cannot
// verify.
func testVerifier(t *testing.T, anonymous bool, stubs ...*issuer.Server) *Verifier {
	t.Helper()
	key := newKey(t)
	var issuers []string
	for _, s := range stubs {
		issuers = append(issuers, s.URL())
	}
	v, err := NewVerifier(VerifierOptions{
		Issuers: issuers, LocalIssuer: localIssuer, LocalKey: &key.PublicKey,
		Client: testClient(), AnonymousRead: anonymous,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func eligible(t *testing.T, method, path string) bool {
	t.Helper()
	return AnonymousEligible(httptest.NewRequest(method, path, nil))
}

// TestAnonymousSetAdmitsTheReadRoutes walks the whole set, in both URL
// forms where there are two.
func TestAnonymousSetAdmitsTheReadRoutes(t *testing.T) {
	admitted := []struct{ method, path string }{
		{"GET", "/r/" + anonRepo + ".git/info/refs?service=git-upload-pack"},
		{"POST", "/r/" + anonRepo + ".git/git-upload-pack"},
		{"GET", "/acme/app.git/info/refs?service=git-upload-pack"},
		{"POST", "/acme/app.git/git-upload-pack"},
		{"GET", "/v1/repos/" + anonRepo},
		{"GET", "/v1/repos/" + anonRepo + "/refs"},
		{"GET", "/v1/repos/" + anonRepo + "/commits"},
		{"GET", "/v1/repos/" + anonRepo + "/commits/abc123"},
		{"GET", "/v1/repos/" + anonRepo + "/compare/main...dev"},
		{"GET", "/v1/repos/" + anonRepo + "/tree/main"},
		{"GET", "/v1/repos/" + anonRepo + "/blob/abc123"},
		{"GET", "/v1/repos/" + anonRepo + "/archive/main.tar.gz"},
	}
	for _, c := range admitted {
		if !eligible(t, c.method, c.path) {
			t.Errorf("%s %s is not anonymous and the set says it should be", c.method, c.path)
		}
	}
}

// TestAnonymousSetExcludesReceivePack is the write rule at the route
// layer. A push must reach the 401 with the Basic challenge that makes git
// prompt for a credential, in both URL forms and at both steps.
func TestAnonymousSetExcludesReceivePack(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{"GET", "/r/" + anonRepo + ".git/info/refs?service=git-receive-pack"},
		{"POST", "/r/" + anonRepo + ".git/git-receive-pack"},
		{"GET", "/acme/app.git/info/refs?service=git-receive-pack"},
		{"POST", "/acme/app.git/git-receive-pack"},
		{"GET", "/r/" + anonRepo + ".git/info/refs"},
		{"GET", "/acme/app.git/info/refs"},
	} {
		if eligible(t, c.method, c.path) {
			t.Errorf("%s %s is anonymous and a push must never be", c.method, c.path)
		}
	}
}

// TestAnonymousSetWithholdsTheRest is the list of what spec 027 decided to
// keep behind a credential, with the reason in that spec. A read-marked
// route added elsewhere in the tree must not become anonymous by being
// added; this test is what fails when one does.
func TestAnonymousSetWithholdsTheRest(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/repos/" + anonRepo + "/stats"},
		{"GET", "/v1/repos/" + anonRepo + "/export.bundle"},
		{"GET", "/v1/repos/" + anonRepo + "/import"},
		{"POST", "/v1/repos/" + anonRepo + "/import"},
		{"GET", "/v1/repos"},
		{"POST", "/v1/repos"},
		{"PATCH", "/v1/repos/" + anonRepo},
		{"DELETE", "/v1/repos/" + anonRepo},
		{"POST", "/v1/repos/" + anonRepo + "/tokens"},
		{"POST", "/v1/repos/" + anonRepo + "/freeze"},
		{"POST", "/v1/repos/" + anonRepo + "/gc"},
		{"POST", "/v1/repos/" + anonRepo + "/commits"},
		{"POST", "/v1/repos/" + anonRepo + "/merge"},
		{"POST", "/r/" + anonRepo + ".git/info/lfs/objects/batch"},
		{"POST", "/acme/app.git/info/lfs/objects/batch"},
		{"POST", "/r/" + anonRepo + ".git/info/lfs/verify"},
		{"POST", "/acme/app.git/info/lfs/locks"},
		{"GET", "/nope"},
	} {
		if eligible(t, c.method, c.path) {
			t.Errorf("%s %s is anonymous and spec 027 withholds it", c.method, c.path)
		}
	}
}

// TestBadCredentialIsNotAnonymous asserts the one thing that would turn
// the feature into an escalation: a credential that is present and does
// not verify must be refused with its own reason, never downgraded to the
// anonymous principal.
func TestBadCredentialIsNotAnonymous(t *testing.T) {
	issKey := newKey(t)
	iss := issuer.New(t, issuer.WithKey(issKey))
	v := testVerifier(t, true, iss)
	var reached bool
	h := v.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		if s := Subject(r.Context()); s != AnonymousSubject {
			t.Errorf("subject = %q, want the anonymous subject", s)
		}
	}))

	// No credential at all, on a route of the set: admitted.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/r/"+anonRepo+".git/git-upload-pack", nil))
	if !reached {
		t.Fatalf("a credential-less request on an anonymous route was refused: %d", rec.Code)
	}

	// A credential that is present and does not verify, on the same
	// route: refused, with its own reason and not the anonymous one. The
	// last one verifies in every row but the subject's: a token that
	// names nobody is refused, not admitted as the anonymous principal.
	header, _, _ := strings.Cut(iss.Mint(issuer.Claims{}), ".")
	now := time.Now()
	nobody := signClaims(t, issKey, header, map[string]any{"iss": iss.URL(), "sub": "", "aud": "origo", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	for _, c := range []struct{ cred, reason string }{
		{"Bearer not-a-token", ReasonMalformed},
		{"Bearer " + strings.Repeat("a.", 3), ReasonMalformed},
		{"Bearer " + nobody, ReasonSubject},
	} {
		reached = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/r/"+anonRepo+".git/git-upload-pack", nil)
		req.Header.Set("Authorization", c.cred)
		h.ServeHTTP(rec, req)
		if reached {
			t.Errorf("%q was downgraded to an anonymous request", c.cred)
		}
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"reason":"`+c.reason+`"`) {
			t.Errorf("%q = %d %s, want 401 with reason %q", c.cred, rec.Code, rec.Body.String(), c.reason)
		}
	}

	// Basic auth with a password is a credential too, so it is never an
	// absent one.
	reached = false
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/r/"+anonRepo+".git/git-upload-pack", nil)
	req.SetBasicAuth("git", "not-a-token")
	h.ServeHTTP(rec, req)
	if reached || rec.Code != http.StatusUnauthorized {
		t.Errorf("basic auth with a bad password: reached=%v code=%d", reached, rec.Code)
	}
}

// TestAnonymousReadOffAdmitsNothing is the switch. An installation that
// does not set ORIGO_ANONYMOUS_READ answers exactly as it did before this
// spec, on every route of the set.
func TestAnonymousReadOffAdmitsNothing(t *testing.T) {
	v := testVerifier(t, false)
	h := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a credential-less request reached the handler with the switch off")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/r/"+anonRepo+".git/git-upload-pack", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != `Basic realm="origo"` {
		t.Errorf("challenge = %q", rec.Header().Get("WWW-Authenticate"))
	}
}

// TestAnonymousDenialIsTheSame401Everywhere is the existence-hiding rule
// on the wire. Every refusal an anonymous caller can provoke, including an
// authorizer that answered nothing at all, is the one 401 the node already
// answers to any request with no credential.
func TestAnonymousDenialIsTheSame401Everywhere(t *testing.T) {
	// The refusal a request with no credential gets with the feature off
	// is the reference. Everything below must equal it.
	want := httptest.NewRecorder()
	Unauthenticated(want, refuse(ReasonMissing))

	anon := WithPrincipal(httptest.NewRequest("GET", "/v1/repos/"+anonRepo, nil).Context(),
		Principal{Subject: AnonymousSubject})
	req := httptest.NewRequest("GET", "/v1/repos/"+anonRepo, nil).WithContext(anon)

	errs := []error{
		&Denied{Subject: AnonymousSubject, Action: ActionRead, Reason: "unknown_repository"},
		&Denied{Subject: AnonymousSubject, Action: ActionRead, Reason: "insufficient_role"},
		&Denied{Subject: AnonymousSubject, Action: ActionWrite, Reason: "anonymous_subject"},
		&Unavailable{URL: "http://authorizer.invalid", Status: 502},
	}
	for _, err := range errs {
		rec := httptest.NewRecorder()
		WriteRefusal(rec, req, err, nil)
		if rec.Code != want.Code {
			t.Errorf("%v: code = %d, want %d", err, rec.Code, want.Code)
		}
		if rec.Body.String() != want.Body.String() {
			t.Errorf("%v: body = %s, want %s", err, rec.Body.String(), want.Body.String())
		}
		if rec.Header().Get("WWW-Authenticate") != want.Header().Get("WWW-Authenticate") {
			t.Errorf("%v: challenge = %q", err, rec.Header().Get("WWW-Authenticate"))
		}
	}

	// An authenticated subject still gets the 403 with the reason, so the
	// rule above is about the anonymous principal and not about every
	// refusal.
	named := httptest.NewRequest("GET", "/v1/repos/"+anonRepo, nil)
	named = named.WithContext(WithPrincipal(named.Context(), Principal{Subject: "alice"}))
	rec := httptest.NewRecorder()
	WriteRefusal(rec, named, &Denied{Subject: "alice", Action: ActionRead, Reason: "insufficient_role"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a named subject's deny = %d, want 403", rec.Code)
	}
}
