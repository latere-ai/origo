// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sink"
)

// The stubs of spec 013 are driven over their control APIs, so the
// same cases run against the in-process stubs and against the stack's
// host ports.

// mint asks the stub issuer for a token of the subject, acting for act
// when it is not empty.
func (s *session) mint(t testing.TB, sub, act string) string {
	t.Helper()
	body := fmt.Sprintf(`{"sub":%q}`, sub)
	if act != "" {
		body = fmt.Sprintf(`{"sub":%q,"act":%q}`, sub, act)
	}
	r := s.as(t, "", "POST", s.target.Issuer+"/mint", body)
	token, _ := r.json["token"].(string)
	failIf(t, r.status != http.StatusOK || token == "", "mint at %s: %d %s", s.target.Issuer, r.status, r.body)
	return token
}

// setRules replaces the stub authorizer's rules and clears them when
// the test ends.
func (s *session) setRules(t testing.TB, rules ...authorizer.Rule) {
	t.Helper()
	put := func(rules []authorizer.Rule) {
		if r := s.as(t, "", "PUT", s.target.Authorizer+"/rules", mustJSON(t, map[string]any{"rules": rules})); r.status != http.StatusNoContent {
			t.Fatalf("rules at %s: %d %s", s.target.Authorizer, r.status, r.body)
		}
	}
	put(rules)
	t.Cleanup(func() { put([]authorizer.Rule{}) })
}

// failAuthorizer makes the stub authorizer answer the status to every
// decision until the test ends.
func (s *session) failAuthorizer(t testing.TB, status int) {
	t.Helper()
	set := func(status int) {
		if r := s.as(t, "", "PUT", s.target.Authorizer+"/fail", fmt.Sprintf(`{"status":%d}`, status)); r.status != http.StatusNoContent {
			t.Fatalf("fail at %s: %d %s", s.target.Authorizer, r.status, r.body)
		}
	}
	set(status)
	t.Cleanup(func() { set(0) })
}

// deliveries reads the stub sink's deliveries of a kind for the
// repository.
func (s *session) deliveries(t testing.TB, repo, kind string) []sink.Delivery {
	t.Helper()
	r := s.as(t, "", "GET", s.target.EventsSink+"/deliveries?repo="+repo+"&kind="+kind, "")
	failIf(t, r.status != http.StatusOK, "deliveries at %s: %d %s", s.target.EventsSink, r.status, r.body)
	var out []sink.Delivery
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("deliveries: %v: %s", err, r.body)
	}
	return out
}

// expectEvent waits for the n-th delivery of a kind for the repository
// and answers it verified, or records the delivery unverified when the
// target has no sink to read.
func (s *session) expectEvent(t *testing.T, repo, kind string, n int) (sink.Delivery, bool) {
	t.Helper()
	if s.target.EventsSink == "" {
		s.unverifiable(t, "the "+kind+" event of "+repo, "no EventsSink")
		return sink.Delivery{}, false
	}
	var got []sink.Delivery
	waitFor(t, 30*time.Second, "delivery "+fmt.Sprint(n)+" of "+kind, func() bool {
		got = s.deliveries(t, repo, kind)
		return len(got) >= n
	})
	d := got[n-1]
	failIf(t, !d.Verified || d.Kind != kind || d.Repo != repo || d.ID == "" || d.Headers.Get(sink.HeaderDelivery) != d.ID || d.Headers.Get(sink.HeaderEvent) != kind, "delivery of %s: %+v", kind, d)
	return d, true
}

// expectNoEvent asserts that no delivery of the kind beyond n arrives
// within a few seconds, when the target has a sink.
func (s *session) expectNoEvent(t *testing.T, repo, kind string, n int) {
	t.Helper()
	if s.target.EventsSink == "" {
		s.unverifiable(t, "the absence of a "+kind+" event of "+repo, "no EventsSink")
		return
	}
	time.Sleep(3 * time.Second)
	if got := s.deliveries(t, repo, kind); len(got) != n {
		t.Fatalf("%d deliveries of %s, want %d: %+v", len(got), kind, n, got)
	}
}

// event decodes a delivery's body.
func event(t testing.TB, d sink.Delivery) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(d.Body, &out); err != nil {
		t.Fatalf("event body: %v: %s", err, d.Body)
	}
	return out
}
