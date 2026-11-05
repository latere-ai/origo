// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// The caller's address behind a proxy (Origo spec 028): the request
// block names the peer, unless the peer is one of ORIGO_TRUSTED_PROXIES,
// and then the rightmost X-Forwarded-For entry outside them.

// ingress is the range the proxies of these tests connect from.
var ingress = []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16"), netip.MustParsePrefix("fd00:10::/64")}

// callerIP sends one credential-less request on an anonymous route from
// peer, with the X-Forwarded-For lines given, through h, and reports the
// address the request block carried to the handler.
func callerIP(t *testing.T, h func(http.Handler) http.Handler, peer string, forwarded ...string) string {
	t.Helper()
	var got string
	var reached bool
	handler := h(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		got = CallerFromContext(r.Context()).IP
	}))
	req := httptest.NewRequest("POST", "/r/"+anonRepo+".git/git-upload-pack", nil)
	req.RemoteAddr = peer
	for _, line := range forwarded {
		req.Header.Add("X-Forwarded-For", line)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !reached {
		t.Fatalf("the request did not reach the handler: %d %s", rec.Code, rec.Body.String())
	}
	return got
}

// behind is the middleware of a node behind the ingress range.
func behind(v *Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return v.MiddlewareBehind(ingress, next) }
}

// TestASpoofedForwardedForFromAnUntrustedPeerIsIgnored: a caller that
// reaches the node directly and sends its own X-Forwarded-For is named
// by the address it connected from, whatever the header says.
func TestASpoofedForwardedForFromAnUntrustedPeerIsIgnored(t *testing.T) {
	v := testVerifier(t, true)
	if got := callerIP(t, behind(v), "198.51.100.7:51234", "203.0.113.4", "10.244.1.9"); got != "198.51.100.7" {
		t.Fatalf("a direct caller with a forged header is named %q, want its own address", got)
	}
}

// TestAChainThroughTwoTrustedProxiesNamesTheClient: the client reaches a
// load balancer, which reaches the ingress controller, which reaches the
// node. Each appends the address it heard from, and the walk from the
// right skips both proxies and stops at the client. An entry the client
// wrote itself, left of its own address, is never read.
func TestAChainThroughTwoTrustedProxiesNamesTheClient(t *testing.T) {
	v := testVerifier(t, true)
	for _, c := range []struct {
		name      string
		forwarded []string
	}{
		{"one header", []string{"203.0.113.4, 10.244.0.5"}},
		{"one line per proxy", []string{"203.0.113.4", "10.244.0.5"}},
		{"the client forged a hop", []string{"192.0.2.1, 10.244.0.6, 203.0.113.4, 10.244.0.5"}},
		{"ports on the entries", []string{"203.0.113.4:40112, 10.244.0.5:443"}},
	} {
		if got := callerIP(t, behind(v), "10.244.3.17:8080", c.forwarded...); got != "203.0.113.4" {
			t.Errorf("%s: the caller is %q, want the client 203.0.113.4", c.name, got)
		}
	}
	// The same over IPv6, the peer written in brackets.
	if got := callerIP(t, behind(v), "[fd00:10::3]:8080", "[2001:db8::7]:443, fd00:10::2"); got != "2001:db8::7" {
		t.Errorf("IPv6: the caller is %q, want 2001:db8::7", got)
	}
}

// TestNoTrustedProxiesKeepsThePeerAddress: a node that sets no trusted
// proxies names the peer, with its port removed, exactly as it did
// before the setting existed, and never reads the header.
func TestNoTrustedProxiesKeepsThePeerAddress(t *testing.T) {
	v := testVerifier(t, true)
	for _, mw := range map[string]func(http.Handler) http.Handler{
		"Middleware":              v.Middleware,
		"MiddlewareBehind no one": func(next http.Handler) http.Handler { return v.MiddlewareBehind(nil, next) },
	} {
		if got := callerIP(t, mw, "10.244.3.17:8080", "203.0.113.4"); got != "10.244.3.17" {
			t.Errorf("the caller is %q, want the peer 10.244.3.17", got)
		}
		if got := callerIP(t, mw, "[2001:db8::9]:443"); got != "2001:db8::9" {
			t.Errorf("an IPv6 peer is %q", got)
		}
		if got := callerIP(t, mw, "@"); got != "@" {
			t.Errorf("a peer address with no port is %q, want it as it is", got)
		}
	}
}

// TestTheForwardedWalkKeepsThePeerWhenNoEntryIsUsable: from a trusted
// peer, a request the walk cannot place keeps the peer rather than a
// string from the header, and an address mapped into IPv6 meets its IPv4
// range on both sides.
func TestTheForwardedWalkKeepsThePeerWhenNoEntryIsUsable(t *testing.T) {
	const peer = "10.244.3.17:8080"
	for _, c := range []struct {
		name      string
		peer      string
		forwarded []string
		want      string
	}{
		{"no header", peer, nil, "10.244.3.17"},
		{"every entry is a proxy", peer, []string{"10.244.0.9, 10.244.0.5"}, "10.244.3.17"},
		{"an entry that is no address", peer, []string{"203.0.113.4, unknown, 10.244.0.5"}, "10.244.3.17"},
		{"an empty entry", peer, []string{"203.0.113.4,,10.244.0.5"}, "10.244.3.17"},
		{"a mapped peer", "[::ffff:10.244.3.17]:8080", []string{"203.0.113.4"}, "203.0.113.4"},
		{"a mapped entry", peer, []string{"::ffff:203.0.113.4, ::ffff:10.244.0.5"}, "203.0.113.4"},
		{"a peer that is no address", "pipe", []string{"203.0.113.4"}, "pipe"},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.peer
		for _, line := range c.forwarded {
			req.Header.Add("X-Forwarded-For", line)
		}
		if got := clientIP(req, ingress); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
