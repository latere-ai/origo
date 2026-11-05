// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"latere.ai/x/origo/internal/contract"
	"latere.ai/x/origo/internal/tracing"
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
	return v.MiddlewareBehind(nil, next)
}

// MiddlewareBehind is Middleware for a node behind the proxies whose
// ranges trusted names, ORIGO_TRUSTED_PROXIES: the caller's address in
// the request block is read through them as clientIP describes. With
// trusted empty it is Middleware, and X-Forwarded-For is never read.
func (v *Verifier) MiddlewareBehind(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := v.Verify(r.Context(), Credential(r))
		if err != nil {
			if !v.anonymousRead || Credential(r) != "" || !AnonymousEligible(r) {
				Unauthenticated(w, err)
				return
			}
			p = Principal{Subject: AnonymousSubject}
		}
		ctx := WithCaller(WithPrincipal(r.Context(), p), callerOf(r, trusted))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// callerOf reads the request block the authorizer envelope carries
// (Origo spec 028): the request's trace id, the caller's address, and
// the user agent. The trace id is the same id spec 011 logs, so an
// operator's authorizer and the node's log name one request.
func callerOf(r *http.Request, trusted []netip.Prefix) Caller {
	return Caller{ID: tracing.ID(r.Context()), IP: clientIP(r, trusted), UserAgent: r.UserAgent()}
}

// clientIP is the caller's address: the peer with its port removed,
// unless the peer is inside one of the trusted ranges. Then it is the
// rightmost X-Forwarded-For entry outside them. Each proxy appends the
// address it heard from, so the walk runs right to left and stops at
// the first hop no trusted proxy is: everything left of it was written
// by that hop and may be anything it chose.
//
// The header from a peer outside the ranges is never read, so a caller
// that reaches the node directly cannot name its own address. An entry
// that is not an address ends the walk, and so does running out of
// entries, and both keep the peer: the answer is always an address a
// trusted hop vouched for, never a string from the header.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if addr, err := netip.ParseAddr(peer); err != nil || !inside(addr.Unmap(), trusted) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for _, entry := range slices.Backward(hops) {
		hop, ok := forwardedAddr(entry)
		if !ok {
			break
		}
		if !inside(hop, trusted) {
			return hop.String()
		}
	}
	return peer
}

// forwardedAddr parses one X-Forwarded-For entry: an address, or an
// address with a port, which some proxies append, bracketed for IPv6.
// An IPv4 address mapped into IPv6 is reported as IPv4, so it meets an
// IPv4 range.
func forwardedAddr(entry string) (netip.Addr, bool) {
	entry = strings.TrimSpace(entry)
	if ap, err := netip.ParseAddrPort(entry); err == nil {
		return ap.Addr().Unmap(), true
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// inside reports whether addr is in one of the ranges.
func inside(addr netip.Addr, ranges []netip.Prefix) bool {
	return slices.ContainsFunc(ranges, func(p netip.Prefix) bool { return p.Contains(addr) })
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
