// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"latere.ai/x/pkg/authkit"
	"latere.ai/x/pkg/authz"

	"github.com/latere-ai/origo/internal/contract"
	authorizerstub "github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/issuer"
)

// The personal access token rows of the identity epic's id-13. A person
// pushes over HTTPS with a key of theirs; the token it mints carries
// token_use "pat" and the grants the key was created with, as RFC 9396's
// authorization_details. Origo reads neither claim and forwards both to
// the decision point, which intersects its answer with them.

// grant is one authorization_details entry as the claim carries it, built
// from the wire shape so the test reads what a token actually holds.
func grant(action, repo string) map[string]any {
	g := map[string]any{
		"type":      authz.GrantType,
		"actions":   []any{action},
		"datatypes": []any{ResourceKind},
		"locations": []any{"https://api.latere.ai"},
	}
	if repo != "" {
		g["identifier"] = repo
	}
	return g
}

// patClaims are the claims of a token minted from a personal access key:
// the person as the subject, the credential class, and the grants.
func patClaims(sub string, grants ...map[string]any) issuer.Claims {
	entries := make([]any, 0, len(grants))
	for _, g := range grants {
		entries = append(entries, g)
	}
	return issuer.Claims{Sub: sub, Extra: map[string]any{
		"token_use":             authkit.TokenUsePAT,
		"authorization_details": entries,
	}}
}

// TestAPersonalAccessTokenVerifiesWithItsGrants is id-13's verifier row.
// The validator promises to read the claim (jwt.Config.ReadsGrants), so a
// token carrying grants verifies and the identity behind it carries the
// credential class and the entries typed. Without the promise the same
// token is refused grants_unread, which is the closed direction: a
// service that reads a restriction and applies none grants more than the
// person asked for.
func TestAPersonalAccessTokenVerifiesWithItsGrants(t *testing.T) {
	clk := newClock()
	iss := issuer.New(t, issuer.WithClock(clk.Now))
	v := newVerifier(t, clk, newKey(t), iss)
	ctx := context.Background()
	raw := iss.Mint(patClaims("alice", grant("origo:repo.read", repoA)))

	p, err := v.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("a personal access token was refused: %v", err)
	}
	if p.Subject != authz.Subject(iss.URL(), "alice") {
		t.Errorf("subject %q", p.Subject)
	}
	if use, _ := p.Claims["token_use"].(string); use != authkit.TokenUsePAT {
		t.Errorf("token_use %v", p.Claims["token_use"])
	}
	if _, ok := p.Claims["authorization_details"]; !ok {
		t.Error("the claims carry no authorization_details")
	}

	// The identity the shared verifier read, which is what the promise is
	// about: the class typed, and the entries parsed.
	id, err := v.shared.Validate(raw)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if id.TokenUse != authkit.TokenUsePAT {
		t.Errorf("identity token use %q", id.TokenUse)
	}
	if len(id.Grants) != 1 {
		t.Fatalf("identity grants %v", id.Grants)
	}
	if g := id.Grants[0]; g.Type != authz.GrantType || g.Identifier != repoA ||
		len(g.Actions) != 1 || g.Actions[0] != "origo:repo.read" {
		t.Errorf("identity grant %+v", g)
	}
}

// patReq is an envelope carrying the claims a personal access token's
// bearer arrives with: the credential class, and the grants.
func patReq(subject, action, id string, grants ...map[string]any) authz.Request {
	req := ownerReq(subject, action, id)
	entries := make([]any, 0, len(grants))
	for _, g := range grants {
		entries = append(entries, g)
	}
	req.Claims = map[string]any{"token_use": authkit.TokenUsePAT, "authorization_details": entries}
	return req
}

// noGrantsReq is the envelope of a personal access token whose claims
// name no grant at all. An absent claim is not full authority: the
// credential says what it may do, and this says nothing.
func noGrantsReq(subject, action, id string) authz.Request {
	req := ownerReq(subject, action, id)
	req.Claims = map[string]any{"token_use": authkit.TokenUsePAT}
	return req
}

// TestTheOwnerPolicyNarrowsAScopedToken is id-13's rule at the node's own
// decision point. With ORIGO_AUTHORIZER_URL unset the owner policy is the
// only decision point a request meets, so the intersection is applied
// there or a key narrowed to one repository reaches every repository its
// holder owns.
//
// The three properties of the rule are the rows: the answer is the
// ceiling, so a grant on a repository the person does not own reaches
// nothing and reads not_owner; the intersection only ever denies; and a
// request no grant covers is denied `grant`, which needs none of the
// node's tables to decide.
func TestTheOwnerPolicyNarrowsAScopedToken(t *testing.T) {
	const (
		alice = "https://iss|alice"
		bob   = "https://iss|bob"
		mine  = repoA
		also  = "2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6d"
		yours = repoB
	)
	objects := fakeObjects{owners: map[string]string{mine: alice, also: alice, yours: bob}}
	p := NewOwnerPolicy(nil, objects)
	ctx := context.Background()

	readMine := grant("origo:repo.read", mine)
	readAny := grant("origo:repo.read", "")
	for _, row := range []struct {
		name   string
		req    authz.Request
		allow  bool
		reason string
	}{
		{"the granted action on the granted repository", patReq(alice, "repo.read", mine, readMine), true, ""},
		{"another action on the granted repository", patReq(alice, "repo.write", mine, readMine), false, authz.ReasonGrant},
		{"the granted action on another repository", patReq(alice, "repo.read", also, readMine), false, authz.ReasonGrant},
		{"a kind-wide grant covers every repository", patReq(alice, "repo.read", also, readAny), true, ""},
		{"a grant is not authority", patReq(alice, "repo.read", yours, grant("origo:repo.read", yours)), false, authz.ReasonNotOwner},
		{"an unqualified action covers nothing", patReq(alice, "repo.read", mine, grant("repo.read", mine)), false, authz.ReasonGrant},
		{"a token of another class is not narrowed", ownerReq(alice, "repo.write", mine), true, ""},
		{"no grant at all covers nothing", patReq(alice, "repo.read", mine), false, authz.ReasonGrant},
		{"an absent claim covers nothing", noGrantsReq(alice, "repo.read", mine), false, authz.ReasonGrant},
	} {
		t.Run(row.name, func(t *testing.T) {
			d, err := p.Authorize(ctx, row.req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Allow != row.allow || (!d.Allow && d.Reason != row.reason) {
				t.Fatalf("allow=%v reason=%q, want allow=%v reason=%q", d.Allow, d.Reason, row.allow, row.reason)
			}
		})
	}

	// The directory is the same rule read on the action that names no
	// repository: it is covered by a kind-wide selector on repo.list and
	// by nothing else, so a key that grants a read on one repository
	// cannot read the names of the rest.
	for _, row := range []struct {
		name    string
		grant   map[string]any
		allowed bool
	}{
		{"a read grant does not list", readMine, false},
		{"a read grant on every repository does not list", readAny, false},
		{"a kind-wide list grant lists", grant("origo:repo.list", ""), true},
		{"a list grant on one repository does not list", grant("origo:repo.list", mine), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			req := patReq(alice, "repo.list", "", row.grant)
			req.Subject = alice
			dir, err := p.List(ctx, req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dir.Allowed != row.allowed {
				t.Fatalf("allowed=%v reason=%q, want allowed=%v", dir.Allowed, dir.Reason, row.allowed)
			}
			if !row.allowed && dir.Reason != authz.ReasonGrant {
				t.Fatalf("reason %q", dir.Reason)
			}
		})
	}

	// A claim nobody can parse is no verdict: it fails closed the way a
	// lookup that did not answer does.
	bad := ownerReq(alice, "repo.read", mine)
	bad.Claims = map[string]any{"token_use": authkit.TokenUsePAT, "authorization_details": "not an array"}
	if _, err := p.Authorize(ctx, bad); err == nil {
		t.Fatal("a malformed authorization_details decided")
	} else if _, ok := errors.AsType[*Unavailable](err); !ok {
		t.Fatalf("the failure is not an *Unavailable: %v", err)
	}
}

// TestTheGrantRefusalReachesTheClient is id-13's refusal row over HTTPS.
// A key narrowed to some repositories is refused elsewhere, and the
// reason a decision point writes for that refusal, `grant`, reaches the
// person as every other authorizer reason does: the 403 forbidden of
// spec 003 with the reason in details, beside the action and the
// subject. The node adds no word of its own, which is what keeps the
// reason table the decision point's.
func TestTheGrantRefusalReachesTheClient(t *testing.T) {
	clk := newClock()
	iss := issuer.New(t, issuer.WithClock(clk.Now))
	v := newVerifier(t, clk, newKey(t), iss)
	stub := authorizerstub.New(t)
	stub.SetRules(
		authorizerstub.Rule{Subject: "*", Action: "repo.read", Allow: true},
		authorizerstub.Rule{Subject: "*", Action: "repo.write", Allow: false, Reason: authz.ReasonGrant},
	)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	h := routes(v, NewGuard(c, slog.New(slog.DiscardHandler)))
	raw := iss.Mint(patClaims("alice", grant("origo:repo.read", repoA)))
	subject := authz.Subject(iss.URL(), "alice")

	if code, _, _ := do(t, h, "POST", "/r/"+repoA+".git/git-upload-pack", raw); code != 204 {
		t.Fatalf("the granted read: %d", code)
	}
	code, e, _ := do(t, h, "POST", "/r/"+repoA+".git/git-receive-pack", raw)
	if code != http.StatusForbidden || e.Code != contract.CodeForbidden || e.Message != contract.Sentence(contract.CodeForbidden) {
		t.Fatalf("the push: %d %+v", code, e)
	}
	if e.Details["reason"] != authz.ReasonGrant || e.Details["action"] != string(ActionWrite) || e.Details["subject"] != subject {
		t.Errorf("details %+v", e.Details)
	}
}
