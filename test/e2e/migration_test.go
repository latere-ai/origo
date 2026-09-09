// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/gittest"
)

// The cut-over of spec 014 seen from a developer's machine: the prior
// host answers 308 from the old clone URL to Origo's, and git follows
// it, rewrites the base for the rest of the session, and clones and
// pushes against Origo with nothing changed on the machine.

// priorHost is a stub of the prior host after its cut-over: every path
// under the old repository answers 308 to the same path under target.
func priorHost(t *testing.T, oldPath, target string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix, ok := strings.CutPrefix(r.URL.Path, oldPath)
		if !ok {
			http.NotFound(w, r)
			return
		}
		to := target + suffix
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusPermanentRedirect)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestE2EOldCloneURLRedirectsToOrigo is spec 014's cut-over criterion:
// a 308 from the prior host makes git clone and git push against the
// old URL succeed against Origo, with no change on the client beyond
// the token every request to Origo carries.
func TestE2EOldCloneURLRedirectsToOrigo(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "migration", "cutover-"+id[:8])

	// The repository as it stands after a migration: history in Origo,
	// reachable at Origo's own URL.
	seeded := clone(t, n.url(id))
	commitFile(t, seeded, "a.txt", "one", "first")
	mustGit(t, seeded, "push", "-q", "origin", "HEAD:refs/heads/main")

	const oldPath = "/acme/api.git"
	old := priorHost(t, oldPath, "http://"+n.public+"/r/"+id+".git") + oldPath
	bearer := "http.extraHeader=Authorization: Bearer " + s.token

	// A clone of the old URL is a clone of Origo.
	work := filepath.Join(t.TempDir(), "old")
	mustGit(t, t.TempDir(), "-c", bearer, "clone", "-q", old, work)
	if got, want := gittest.RevList(t, work), gittest.RevList(t, seeded); got != want {
		t.Fatalf("the clone of the old URL differs:\n%s\nwant\n%s", got, want)
	}
	if remote := mustGit(t, work, "remote", "get-url", "origin"); remote != old {
		t.Fatalf("the clone's remote is %q, not the old URL", remote)
	}

	// A push to the old URL lands in Origo, which the API reports.
	commitFile(t, work, "b.txt", "two", "second")
	mustGit(t, work, "-c", bearer, "push", "-q", "origin", "HEAD:refs/heads/main")
	head := mustGit(t, work, "rev-parse", "HEAD")
	if status, out := n.api("GET", "/v1/repos/"+id, ""); status != 200 || out["head"] != head {
		t.Fatalf("after the push through the old URL: %d %v", status, out)
	}
	// And a fetch of the old URL sees it too.
	mustGit(t, seeded, "fetch", "-q", "origin")
	if got := mustGit(t, seeded, "rev-parse", "origin/main"); got != head {
		t.Fatalf("origin/main is %s, the push was %s", got, head)
	}
}
