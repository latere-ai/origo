// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/latere-ai/origo/internal/contract"
)

// Credential reads the token from the request in the three forms spec
// 007 accepts: a Bearer header, basic auth with any username and the
// token as the password, and basic auth with the token as the username
// and an empty password. It reports "" when none is present.
func Credential(r *http.Request) string {
	if raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(raw)
	}
	if user, pass, ok := r.BasicAuth(); ok {
		if pass != "" {
			return pass
		}
		return user
	}
	return ""
}

// Middleware refuses every request whose credential the verifier does
// not accept, with 401 unauthenticated, the row's reason in
// details.reason, and a Basic challenge so git prompts for credentials.
// An admitted request carries its Principal.
//
// One request is admitted without a credential: with anonymous read on
// (spec 027), a request that carries no credential at all on one of the
// routes of AnonymousRoutes is admitted with an empty Principal and
// decided by the authorizer like any other. Only an absent credential is
// admitted this way. A credential that is present and does not verify is
// refused with its own reason and is never downgraded to anonymous, which
// is why the eligibility test reads Credential again rather than reading
// the verifier's error.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := v.Verify(r.Context(), Credential(r))
		if err != nil {
			if !v.anonymousRead || Credential(r) != "" || !AnonymousEligible(r) {
				Unauthenticated(w, err)
				return
			}
			p = Principal{Subject: AnonymousSubject}
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// Unauthenticated writes the 401 for a refused credential.
func Unauthenticated(w http.ResponseWriter, err error) {
	reason := ReasonMalformed
	if refusal, ok := errors.AsType[*Refusal](err); ok {
		reason = refusal.Reason
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="origo"`)
	contract.Write(w, http.StatusUnauthorized, contract.CodeUnauthenticated, map[string]any{"reason": reason})
}
