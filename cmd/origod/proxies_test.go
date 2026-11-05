// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"testing"
)

// TestTheAuthorizerHearsTheCallerBehindTheIngress is the wiring of
// ORIGO_TRUSTED_PROXIES (Origo spec 028) through a running node: the
// test client connects from loopback, standing in for an ingress
// controller that forwards for 203.0.113.4. A node that trusts loopback
// sends the authorizer the client's address; a node that trusts nothing
// sends the peer's, as every node did before the setting existed.
func TestTheAuthorizerHearsTheCallerBehindTheIngress(t *testing.T) {
	for _, c := range []struct {
		name, trusted, want string
	}{
		{"behind a trusted ingress", "127.0.0.0/8", "203.0.113.4"},
		{"no trusted proxy", "", "127.0.0.1"},
	} {
		env, id := newEnv(t)
		env["ORIGO_TRUSTED_PROXIES"] = c.trusted
		base := directoryNode(t, env)
		id.authz.ClearRequests()
		resp, body := fetchBody(t, http.MethodGet, base+"/v1/repos", map[string]string{
			"Authorization":   "Bearer " + id.token(),
			"X-Forwarded-For": "198.51.100.1, 203.0.113.4",
		}, "")
		seen := id.authz.Requests()
		if len(seen) == 0 {
			t.Fatalf("%s: the listing never reached the authorizer: %d %s", c.name, resp.StatusCode, body)
		}
		if got := seen[len(seen)-1].Request.IP; got != c.want {
			t.Errorf("%s: the authorizer heard %q, want %q", c.name, got, c.want)
		}
	}
}
