// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/tracing"
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
		ctx := WithCaller(WithPrincipal(r.Context(), p), callerOf(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// callerOf reads the request block the authorizer envelope carries
// (Origo spec 028): the request's trace id, the peer address with its
// port removed, and the user agent. The trace id is the same id spec 011
// logs, so an operator's authorizer and the node's log name one request.
func callerOf(r *http.Request) Caller {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return Caller{ID: tracing.ID(r.Context()), IP: ip, UserAgent: r.UserAgent()}
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
