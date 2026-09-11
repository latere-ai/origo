// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// The positive half of spec 027: a caller with no credential clones a
// repository the authorizer opens, and is refused everything else with the
// one 401.
//
// The middleware's admission rule is internal/auth's; the node here is
// given the empty principal that rule produces, so this is the guard, the
// handlers and the refusal shape end to end.

// TestAnonymousClone is the feature working.
func TestAnonymousClone(t *testing.T) {
	store := wal.NewMemStore()

	// A repository with one commit, pushed by somebody who signed in.
	owner := newNode(t, store)
	owner.create(repoA, "acme", "app")
	work := clone(t, owner.url("/r/"+repoA+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")

	// The same node seen by a caller with no credential. The authorizer
	// allows a read of this repository, which is what auth answers for a
	// repository whose owner marked it public.
	n := newNode(t, store, anonymously())
	n.authz.Allow(authorizer.Rule{Repo: repoA, Action: string(auth.ActionRead)})
	n.authz.Deny(authorizer.Rule{Repo: repoA, Action: string(auth.ActionWrite)}, "anonymous_subject")

	// Clone by id and by name, with no credential of any kind, and get
	// the same history the owner pushed.
	for _, addr := range []string{n.url("/r/" + repoA + ".git"), n.url("/acme/app.git")} {
		got := clone(t, addr)
		if gittest.RevList(t, got) != gittest.RevList(t, work) {
			t.Fatalf("%s: the anonymous clone differs from what was pushed", addr)
		}
	}
}

// TestAnonymousIsRefusedWithTheOne401 is the negative half, on the same
// node: everything the authorizer does not allow an empty subject answers
// the 401 a request with no credential already gets, with the Basic
// challenge that makes git ask for one.
func TestAnonymousIsRefusedWithTheOne401(t *testing.T) {
	store := wal.NewMemStore()
	owner := newNode(t, store)
	owner.create(repoA, "acme", "app")
	owner.create("1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "acme", "private")

	n := newNode(t, store, anonymously())
	n.authz.Deny(authorizer.Rule{}, "anonymous_subject")
	client := n.srv.Client()

	// A repository that exists but is not open, a name that resolves to
	// nothing, an id registered nowhere, and a push. Every one is the
	// same answer.
	paths := []string{
		"/r/" + repoA + ".git/info/refs?service=git-upload-pack",
		"/acme/private.git/info/refs?service=git-upload-pack",
		"/acme/nothing.git/info/refs?service=git-upload-pack",
		"/r/00000000-0000-4000-8000-000000000000.git/info/refs?service=git-upload-pack",
		"/r/" + repoA + ".git/info/refs?service=git-receive-pack",
	}
	var first string
	for _, path := range paths {
		resp, err := client.Get(n.url(path))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d %s, want 401", path, resp.StatusCode, body)
			continue
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != `Basic realm="origo"` {
			t.Errorf("%s: challenge %q, want the Basic challenge git prompts on", path, got)
		}
		answer := string(body)
		if !strings.Contains(answer, contract.CodeUnauthenticated) || !strings.Contains(answer, `"missing"`) {
			t.Errorf("%s: body %s, want the unauthenticated envelope with reason missing", path, answer)
		}
		if first == "" {
			first = answer
			continue
		}
		if answer != first {
			t.Errorf("%s answered %s; every anonymous refusal must be %s", path, answer, first)
		}
	}
}
