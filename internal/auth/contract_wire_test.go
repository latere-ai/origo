// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/json"
	"testing"
)

// The authorizer wire contract (spec 007), pinned as bytes.
//
// Origo and an authorizer are separate programs in separate repositories
// with no shared type. Each side has thorough tests against its own idea
// of the other: this repository's TestAnonymousClone clones against a
// stub that allows, and latere-ai/auth's TestAnonymousDecisionOverHTTP
// answers allow for a real public row. Both pass while agreeing on
// nothing, because neither ever sees the other's bytes.
//
// The field names and the action words are the whole contract, and they
// are declared twice: `Action` here, `RepoAction` in auth's
// internal/authz/gitplane.go, which casts rather than parses, so a drift
// in either would not fail to compile and would not fail a test. It would
// deny, in production, with the one refusal that is deliberately
// indistinguishable from every other.
//
// So the bytes are the fixture. auth carries the same literal in
// TestOrigoWireContract; changing either side alone breaks one of them.
// Documented for an operator in docs/install.md and docs/api.md.
const anonymousReadWire = `{"subject":"","actor":"","repo":{"id":"bb0886f5-eafa-48d2-b8e3-54e96a8f3f59","owner":"changkun","slug":"hello-world"},"action":"read"}`

// TestAuthorizerWireIsTheAgreedBytes is the encoder half: what this
// client puts on the wire for the anonymous read that spec 027 admits.
func TestAuthorizerWireIsTheAgreedBytes(t *testing.T) {
	got, err := json.Marshal(Request{
		Subject: "",
		Actor:   "",
		Repo: RepoRef{
			ID:    "bb0886f5-eafa-48d2-b8e3-54e96a8f3f59",
			Owner: "changkun",
			Slug:  "hello-world",
		},
		Action: ActionRead,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != anonymousReadWire {
		t.Errorf("the authorizer request changed shape\n got %s\nwant %s", got, anonymousReadWire)
	}
}

// TestActionWordsAreTheAgreedWords pins the three action words on their
// own. They travel as bare strings and auth compares them literally.
func TestActionWordsAreTheAgreedWords(t *testing.T) {
	for _, c := range []struct {
		got  Action
		want string
	}{
		{ActionRead, "read"},
		{ActionWrite, "write"},
		{ActionAdmin, "admin"},
		{ActionList, "list"},
	} {
		if string(c.got) != c.want {
			t.Errorf("action = %q, want %q; auth compares this literally", c.got, c.want)
		}
	}
}
