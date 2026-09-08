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
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := v.Verify(r.Context(), Credential(r))
		if err != nil {
			Unauthenticated(w, err)
			return
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
