// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/contract"
)

// The rows of spec 019: transfer, freeze and unfreeze, stats, gc, the
// bundle export, the import with its three refusals and its event, and
// the events of the lifecycle operations. gone is proved by the code
// table alone.

func cases019() []testCase {
	return []testCase{
		{name: "transfer", run: case019Transfer},
		{name: "freeze", run: case019Freeze},
		{name: "stats", run: case019Stats},
		{name: "gc", run: case019GC},
		{name: "export", run: case019Export},
		{name: "import_not_found", run: case019ImportNotFound},
		{name: "lifecycle-events", run: case019LifecycleEvents},
		{name: "repo_not_empty", group: GroupSource, run: case019RepoNotEmpty},
		{name: "import", group: GroupSource, run: case019Import},
	}
}

func case019Transfer(t *testing.T, s *session) {
	id := s.create(t, "transfer")
	slug := SlugPrefix + "transfer-" + id[:8]
	r := s.call(t, "POST", "/v1/repos/"+id+"/transfer", `{"owner":"`+Owner+`-b"}`)
	expectStatus(t, r, http.StatusOK)
	failIf(t, r.json["owner"] != Owner+"-b" || r.json["slug"] != slug || r.json["id"] != id, "transfer: %s", r.body)
	expectError(t, s.call(t, "GET", "/"+Owner+"/"+slug+".git/info/refs?service=git-upload-pack", ""), http.StatusNotFound, contract.CodeRepoNotFound)
	expectStatus(t, s.call(t, "GET", "/"+Owner+"-b/"+slug+".git/info/refs?service=git-upload-pack", ""), http.StatusOK)
	expectError(t, s.call(t, "POST", "/v1/repos/"+id+"/transfer", `{"owner":"r"}`), http.StatusBadRequest, contract.CodeInvalid)
	if d, ok := s.expectEvent(t, id, "transferred", 1); ok {
		e := event(t, d)
		from, _ := e["from"].(map[string]any)
		to, _ := e["to"].(map[string]any)
		failIf(t, from["owner"] != Owner || to["owner"] != Owner+"-b" || e["pusher"] == nil, "transferred: %s", d.Body)
	}
}

func case019Freeze(t *testing.T, s *session) {
	id := s.create(t, "freeze")
	work := clone(t, s.repoURL(id))
	commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	r := s.call(t, "POST", "/v1/repos/"+id+"/freeze", "")
	expectStatus(t, r, http.StatusOK)
	failIf(t, r.json["frozen_at"] == nil, "freeze: %s", r.body)
	// Reads go on, a push is refused before any pack with the line. The
	// push path reads a pusher's meta once per advertisement window
	// (spec 019), so each push after a state change goes through a
	// fresh repository-bound token, which is how a pusher who has not
	// pushed in the last minute sees the change.
	if got := clone(t, s.repoURL(id)); mustGit(t, got, "rev-parse", "HEAD") != mustGit(t, work, "rev-parse", "HEAD") {
		t.Fatal("the frozen repository does not clone")
	}
	commitFile(t, work, "b.txt", []byte("b"), "second")
	mustGit(t, work, "remote", "set-url", "origin", s.repoURLAs(s.writeToken(t, id), id))
	out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
	failIf(t, err == nil || !strings.Contains(out, "remote error: "+contract.Line(contract.CodeRepoFrozen)), "push to a frozen repository: %v\n%s", err, redact(out))
	d := expectError(t, s.call(t, "POST", "/v1/repos/"+id+"/freeze", ""), http.StatusConflict, contract.CodeRepoFrozen)
	failIf(t, d["frozen_at"] == nil, "second freeze: %v", d)
	s.expectEvent(t, id, "frozen", 1)
	r = s.call(t, "POST", "/v1/repos/"+id+"/unfreeze", "")
	expectStatus(t, r, http.StatusOK)
	failIf(t, r.json["frozen_at"] != nil, "unfreeze: %s", r.body)
	mustGit(t, work, "remote", "set-url", "origin", s.repoURLAs(s.writeToken(t, id), id))
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	s.expectEvent(t, id, "unfrozen", 1)
	expectStatus(t, s.call(t, "POST", "/v1/repos/"+id+"/unfreeze", ""), http.StatusOK)
	s.expectNoEvent(t, id, "unfrozen", 1)
}

func case019Stats(t *testing.T, s *session) {
	r := s.call(t, "GET", "/v1/repos/"+s.fixture.id+"/stats", "")
	expectStatus(t, r, http.StatusOK)
	for _, k := range []string{"size_bytes", "lfs_bytes", "packs", "entries_since_compaction", "refs", "pushed_at", "compacted_at"} {
		if _, ok := r.json[k]; !ok {
			t.Fatalf("stats lack %s: %s", k, r.body)
		}
	}
	repo := s.call(t, "GET", "/v1/repos/"+s.fixture.id, "")
	failIf(t, r.json["size_bytes"] != repo.json["size_bytes"] || r.json["pushed_at"] != repo.json["pushed_at"] || num(r.json["refs"]) < 2 || r.json["entries_since_compaction"] != float64(1), "stats %s against %s", r.body, repo.body)
}

// case019GC asks for a compaction: 200 with before and after when it
// ran inside the wait, 202 when it is running or scheduled on the
// primary; a second within the hour is refused with the repository
// limit once a compaction ran.
func case019GC(t *testing.T, s *session) {
	id := s.create(t, "gc")
	work := clone(t, s.repoURL(id))
	for i := range 3 {
		commitFile(t, work, fmt.Sprintf("f%d.txt", i), []byte(fmt.Sprint(i)), fmt.Sprint(i))
		mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	}
	r := s.call(t, "POST", "/v1/repos/"+id+"/gc", "")
	switch r.status {
	case http.StatusOK:
		before, _ := r.json["before"].(map[string]any)
		after, _ := r.json["after"].(map[string]any)
		failIf(t, before["entries"] != float64(3) || after["packs"] != float64(1) || after["size_bytes"] == nil, "gc: %s", r.body)
		d := expectError(t, s.call(t, "POST", "/v1/repos/"+id+"/gc", ""), http.StatusTooManyRequests, contract.CodeRateLimited)
		failIf(t, d["limit"] != "repository" || d["retry_after"] == nil, "second gc: %v", d)
		s.expectEvent(t, id, "compacted", 1)
	case http.StatusAccepted:
		failIf(t, r.json["status"] != "running" && r.json["status"] != "scheduled", "gc: %s", r.body)
	default:
		t.Fatalf("gc: %d %s", r.status, r.body)
	}
}

func case019Export(t *testing.T, s *session) {
	f := s.fixture
	r := s.call(t, "GET", "/v1/repos/"+f.id+"/export.bundle", "")
	expectStatus(t, r, http.StatusOK)
	failIf(t, r.header.Get("Content-Type") != "application/x-git-bundle", "Content-Type %q", r.header.Get("Content-Type"))
	bundle := filepath.Join(t.TempDir(), "export.bundle")
	if err := os.WriteFile(bundle, r.body, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.work, "bundle", "verify", bundle)
	restored := clone(t, bundle)
	failIf(t, revList(t, restored) != revList(t, f.work), "%v", "the bundle's history differs")
	expectError(t, s.call(t, "GET", "/v1/repos/"+s.create(t, "export-empty")+"/export.bundle", ""), http.StatusNotFound, contract.CodeRefNotFound)
}

func case019ImportNotFound(t *testing.T, s *session) {
	id := s.create(t, "no-import")
	d := expectError(t, s.call(t, "GET", "/v1/repos/"+id+"/import", ""), http.StatusNotFound, contract.CodeImportNotFound)
	failIf(t, d["id"] != id, "details: %v", d)
}

// case019LifecycleEvents asserts the events of the operations spec 003
// owns: renamed on a PATCH, deleted with purge_after, undeleted once,
// and no push event for the undelete.
func case019LifecycleEvents(t *testing.T, s *session) {
	id := s.create(t, "lifecycle-events")
	slug := SlugPrefix + "lifecycle-events-" + id[:8]
	expectStatus(t, s.call(t, "PATCH", "/v1/repos/"+id, `{"slug":"`+slug+`-2"}`), http.StatusOK)
	if d, ok := s.expectEvent(t, id, "renamed", 1); ok {
		e := event(t, d)
		to, _ := e["to"].(map[string]any)
		failIf(t, to["slug"] != slug+"-2" || to["owner"] != Owner, "renamed: %s", d.Body)
	}
	r := s.call(t, "DELETE", "/v1/repos/"+id, "")
	expectStatus(t, r, http.StatusAccepted)
	if d, ok := s.expectEvent(t, id, "deleted", 1); ok {
		if e := event(t, d); e["purge_after"] != r.json["purge_after"] {
			t.Fatalf("deleted: %s", d.Body)
		}
	}
	expectStatus(t, s.call(t, "POST", "/v1/repos/"+id+"/undelete", ""), http.StatusOK)
	s.expectEvent(t, id, "undeleted", 1)
	s.expectNoEvent(t, id, "push", 0)
}

func case019RepoNotEmpty(t *testing.T, s *session) {
	body := fmt.Sprintf(`{"source":%q,"token":%q}`, s.target.Source, s.target.SourceToken)
	d := expectError(t, s.call(t, "POST", "/v1/repos/"+s.fixture.id+"/import", body), http.StatusConflict, contract.CodeRepoNotEmpty)
	failIf(t, d["seq"] == nil, "details: %v", d)
}

// case019Import imports the source: 202 at once, running in the state
// endpoint, pushes and a second import refused with repo_importing
// meanwhile, then done with the reference count and the bytes, the
// imported event, and a clone of the history.
func case019Import(t *testing.T, s *session) {
	id := s.create(t, "import")
	body := fmt.Sprintf(`{"source":%q,"token":%q}`, s.target.Source, s.target.SourceToken)
	r := s.call(t, "POST", "/v1/repos/"+id+"/import", body)
	expectStatus(t, r, http.StatusAccepted)
	failIf(t, r.json["state"] != api.ImportRunning, "import: %s", r.body)
	if st := s.call(t, "GET", "/v1/repos/"+id+"/import", ""); st.status != http.StatusOK || (st.json["state"] != api.ImportRunning && st.json["state"] != api.ImportDone) {
		t.Fatalf("state: %d %s", st.status, st.body)
	}
	// While it runs, a push and a second import are refused; an import
	// of a small source may finish first, in which case the refusal is
	// repo_not_empty instead.
	if push := s.call(t, "GET", "/r/"+id+".git/info/refs?service=git-receive-pack", ""); push.status == http.StatusConflict {
		expectError(t, push, http.StatusConflict, contract.CodeRepoImporting)
		expectError(t, s.call(t, "POST", "/v1/repos/"+id+"/import", body), http.StatusConflict, contract.CodeRepoImporting)
	}
	var st response
	waitFor(t, 15*time.Minute, "the import", func() bool {
		st = s.call(t, "GET", "/v1/repos/"+id+"/import", "")
		return st.status == http.StatusOK && st.json["state"] != api.ImportRunning
	})
	failIf(t, st.json["state"] != api.ImportDone || num(st.json["refs"]) < 1 || num(st.json["bytes"]) < 1 || st.json["finished_at"] == nil, "import state: %s", st.body)
	if d, ok := s.expectEvent(t, id, "imported", 1); ok {
		e := event(t, d)
		failIf(t, e["refs"] != st.json["refs"] || e["bytes"] != st.json["bytes"] || e["source"] == nil || strings.Contains(fmt.Sprint(e["source"]), s.target.SourceToken), "imported: %s", d.Body)
	}
	got := clone(t, s.repoURL(id), "--mirror")
	failIf(t, mustGit(t, got, "rev-list", "--count", "--all") == "0", "%v", "the imported repository is empty")
}

// writeToken mints a repository-bound write token for the repository.
func (s *session) writeToken(t *testing.T, id string) string {
	t.Helper()
	r := s.call(t, "POST", "/v1/repos/"+id+"/tokens", `{"scope":"write","ttl":600}`)
	expectStatus(t, r, http.StatusCreated)
	token, _ := r.json["token"].(string)
	return token
}
