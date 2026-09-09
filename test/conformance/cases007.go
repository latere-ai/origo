// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// The rows of spec 007: the key set, repository-bound tokens, the
// authorizer's deny and outage, and delegation with act.

func cases007() []testCase {
	return []testCase{
		{name: "jwks", run: case007JWKS},
		{name: "tokens", run: case007Tokens},
		{name: "forbidden", group: GroupDeny, run: case007Forbidden},
		{name: "authorizer_unavailable", group: GroupDeny, run: case007AuthorizerUnavailable},
		{name: "delegation", group: GroupDelegation, run: case007Delegation},
	}
}

func case007JWKS(t *testing.T, s *session) {
	r := s.as(t, "", "GET", "/.well-known/jwks.json", "")
	expectStatus(t, r, http.StatusOK)
	if keys, _ := r.json["keys"].([]any); len(keys) == 0 {
		t.Fatalf("no key: %s", r.body)
	}
}

func case007Tokens(t *testing.T, s *session) {
	id := s.create(t, "tokens")
	work := clone(t, s.repoURL(id))
	commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	r := s.call(t, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"read","ttl":300}`)
	expectStatus(t, r, http.StatusCreated)
	read, _ := r.json["token"].(string)
	failIf(t, read == "" || r.json["expires_at"] == nil, "read token: %s", r.body)
	// The read token clones and cannot push; a write token pushes.
	byRead := clone(t, s.repoURLAs(read, id))
	commitFile(t, byRead, "b.txt", []byte("b"), "second")
	if out, err := git(t, byRead, "push", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("a read token pushed:\n%s", redact(out))
	}
	expectError(t, s.as(t, read, "GET", "/r/"+id+".git/info/refs?service=git-receive-pack", ""), http.StatusForbidden, contract.CodeForbidden)
	r = s.call(t, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"write","ttl":300}`)
	expectStatus(t, r, http.StatusCreated)
	write, _ := r.json["token"].(string)
	mustGit(t, byRead, "remote", "set-url", "origin", s.repoURLAs(write, id))
	mustGit(t, byRead, "push", "-q", "origin", "HEAD:refs/heads/main")
	// A bound token is bound: another repository refuses it, and admin
	// is outside both scopes.
	expectError(t, s.as(t, write, "GET", "/v1/repos/"+s.fixture.id, ""), http.StatusForbidden, contract.CodeForbidden)
	expectError(t, s.as(t, write, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"read","ttl":60}`), http.StatusForbidden, contract.CodeForbidden)
	d := expectError(t, s.call(t, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"admin","ttl":60}`), http.StatusBadRequest, contract.CodeInvalid)
	failIf(t, d["field"] != "scope", "bad scope: %v", d)
	d = expectError(t, s.call(t, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"read","ttl":0}`), http.StatusBadRequest, contract.CodeInvalid)
	failIf(t, d["field"] != "ttl", "bad ttl: %v", d)
}

// case007Forbidden flips the authorizer to deny: 403 with the reason,
// and 403 before lookup for an id that does not exist.
func case007Forbidden(t *testing.T, s *session) {
	id := s.create(t, "deny")
	unknown := newID(t)
	s.setRules(t,
		authorizer.Rule{Repo: id, Action: "read", Allow: false, Reason: "not welcome"},
		authorizer.Rule{Repo: unknown, Allow: false, Reason: "not welcome"},
	)
	d := expectError(t, s.call(t, "GET", "/v1/repos/"+id, ""), http.StatusForbidden, contract.CodeForbidden)
	failIf(t, d["reason"] != "not welcome" || d["action"] != "read" || d["subject"] == nil, "deny details: %v", d)
	expectError(t, s.call(t, "GET", "/r/"+id+".git/info/refs?service=git-upload-pack", ""), http.StatusForbidden, contract.CodeForbidden)
	// A deny answers before the repository is looked up.
	expectError(t, s.call(t, "GET", "/v1/repos/"+unknown, ""), http.StatusForbidden, contract.CodeForbidden)
	// Admin is still allowed on the denied repository.
	expectStatus(t, s.call(t, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"read","ttl":60}`), http.StatusCreated)
}

func case007AuthorizerUnavailable(t *testing.T, s *session) {
	s.failAuthorizer(t, http.StatusInternalServerError)
	id := newID(t)
	d := expectError(t, s.call(t, "GET", "/v1/repos/"+id, ""), http.StatusServiceUnavailable, contract.CodeAuthorizerUnavailable)
	failIf(t, d["url"] == nil || d["status"] != float64(500), "outage details: %v", d)
	if out, err := git(t, t.TempDir(), "ls-remote", s.repoURL(id)); err == nil || !strings.Contains(out, "503") {
		t.Fatalf("git under the outage: %v\n%s", err, redact(out))
	}
}

// case007Delegation mints a service token acting for a subject: the
// request is decided for the subject with the service as the actor,
// which the push event's pusher shows.
func case007Delegation(t *testing.T, s *session) {
	token := s.mint(t, "conformance-service", "conformance-user")
	id := s.create(t, "act")
	expectStatus(t, s.as(t, token, "GET", "/v1/repos/"+id, ""), http.StatusOK)
	work := clone(t, s.repoURLAs(token, id))
	commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if d, ok := s.expectEvent(t, id, "push", 1); ok {
		pusher, _ := event(t, d)["pusher"].(map[string]any)
		failIf(t, pusher["sub"] != "conformance-user" || pusher["actor"] != "conformance-service", "pusher: %v", pusher)
	}
	// A repository-bound token minted by the delegate carries the same
	// subject and actor.
	r := s.as(t, token, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"read","ttl":60}`)
	expectStatus(t, r, http.StatusCreated)
	bound, _ := r.json["token"].(string)
	expectStatus(t, s.as(t, bound, "GET", "/v1/repos/"+id, ""), http.StatusOK)
}
