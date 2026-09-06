// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticBearerAdmitsTheTokenInEveryFormGitSends(t *testing.T) {
	var subject string
	denied := ""
	guard := &StaticBearer{Token: "s3cret", Deny: func(w http.ResponseWriter, r *http.Request, code, message string) {
		denied = code
		w.WriteHeader(http.StatusUnauthorized)
	}}
	h := guard.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject = Subject(r.Context())
	}))
	cases := []struct {
		name   string
		set    func(*http.Request)
		admits bool
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret") }, true},
		{"bearer with spaces", func(r *http.Request) { r.Header.Set("Authorization", "Bearer  s3cret ") }, true},
		{"basic any user", func(r *http.Request) { r.SetBasicAuth("x", "s3cret") }, true},
		{"basic token as user", func(r *http.Request) { r.SetBasicAuth("s3cret", "") }, true},
		{"wrong bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, false},
		{"wrong basic", func(r *http.Request) { r.SetBasicAuth("x", "nope") }, false},
		{"wrong basic user only", func(r *http.Request) { r.SetBasicAuth("nope", "") }, false},
		{"nothing", func(*http.Request) {}, false},
		{"other scheme", func(r *http.Request) { r.Header.Set("Authorization", "Token s3cret") }, false},
	}
	for _, c := range cases {
		subject, denied = "", ""
		req := httptest.NewRequest("GET", "/", nil)
		c.set(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if c.admits && (subject != DevSubject || denied != "") {
			t.Errorf("%s: not admitted (denied %q)", c.name, denied)
		}
		if !c.admits && (subject != "" || denied != "unauthenticated" || rec.Header().Get("WWW-Authenticate") == "") {
			t.Errorf("%s: admitted, or no challenge", c.name)
		}
	}
	// An empty configured token admits nobody.
	empty := &StaticBearer{Deny: guard.Deny}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer ")
	empty.Middleware(h).ServeHTTP(httptest.NewRecorder(), req)
	if denied != "unauthenticated" {
		t.Fatal("empty token admitted")
	}
	if Subject(context.Background()) != "" {
		t.Fatal("subject without a context value")
	}
}
