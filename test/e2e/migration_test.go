// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/origo/internal/gittest"
)

// The cut-over of spec 014 seen from a developer's machine: the prior
// host answers 308 from the old clone URL to Origo's, and git follows
// it, rewrites the base for the rest of the session, and clones and
// pushes against Origo with the remote unchanged, authenticating with
// the Origo credential the machine already holds.

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

// credentialHelper writes a git credential helper that answers every
// get with token and appends the host git asks about to a file. It
// returns the helper's path and the file's.
func credentialHelper(t *testing.T, token string) (helper, asked string) {
	t.Helper()
	dir := t.TempDir()
	helper, asked = filepath.Join(dir, "helper"), filepath.Join(dir, "asked")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != get ]; then cat >/dev/null; exit 0; fi\n" +
		"while IFS= read -r line && [ -n \"$line\" ]; do\n" +
		"\tcase $line in host=*) printf '%s\\n' \"${line#host=}\" >>'" + asked + "' ;; esac\n" +
		"done\n" +
		"printf 'username=x\\npassword=%s\\n' '" + token + "'\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return helper, asked
}

// TestE2EOldCloneURLRedirectsToOrigo is spec 014's cut-over criterion:
// a 308 from the prior host makes git clone and git push against the
// old URL succeed against Origo for a client that holds its own Origo
// credential, with nothing else changed on the client.
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

	// The prior host after its cut-over. Its 308 names Origo's URL and
	// carries no credential. Git rewrites its base for the rest of the
	// session from the redirect target as libcurl reports it, and a
	// libcurl with the fix for CVE-2026-94266 reports that URL without
	// its user info, so a token there would reach at most the first
	// request after the redirect and never the service request.
	const oldPath = "/acme/api.git"
	old := priorHost(t, oldPath, "http://"+n.public+"/r/"+id+".git") + oldPath

	// The client's own credential for Origo, from a credential helper,
	// which is how a developer's git already reaches Origo. The helper
	// records every host git asks it about.
	helper, asked := credentialHelper(t, s.token)

	// A clone of the old URL is a clone of Origo. The helper is set in
	// the new repository's configuration, so the push below uses it too.
	work := filepath.Join(t.TempDir(), "old")
	mustGit(t, t.TempDir(), "clone", "-q", "-c", "credential.helper="+helper, old, work)
	if got, want := gittest.RevList(t, work), gittest.RevList(t, seeded); got != want {
		t.Fatalf("the clone of the old URL differs:\n%s\nwant\n%s", got, want)
	}
	if remote := mustGit(t, work, "remote", "get-url", "origin"); remote != old {
		t.Fatalf("the clone's remote is %q, not the old URL", remote)
	}

	// A push to the old URL lands in Origo, which the API reports.
	commitFile(t, work, "b.txt", "two", "second")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	head := mustGit(t, work, "rev-parse", "HEAD")
	if status, out := n.api("GET", "/v1/repos/"+id, ""); status != 200 || out["head"] != head {
		t.Fatalf("after the push through the old URL: %d %v", status, out)
	}
	// And a fetch of Origo's own URL sees it too.
	mustGit(t, seeded, "fetch", "-q", "origin")
	if got := mustGit(t, seeded, "rev-parse", "origin/main"); got != head {
		t.Fatalf("origin/main is %s, the push was %s", got, head)
	}

	// Git asked for a credential for Origo after following the redirect,
	// and for no other host: the prior host needs none, and the token
	// reached Origo through the helper rather than through the redirect.
	raw, err := os.ReadFile(asked)
	if err != nil {
		t.Fatalf("git never asked the credential helper: %v", err)
	}
	hosts := strings.Fields(string(raw))
	if len(hosts) == 0 {
		t.Fatal("git never asked the credential helper for a host")
	}
	for _, h := range hosts {
		if h != n.public {
			t.Fatalf("git asked the helper about %q, want only Origo's %q (all: %v)", h, n.public, hosts)
		}
	}
}
