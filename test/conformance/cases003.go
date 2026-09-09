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

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
)

// The rows of spec 003: the unauthenticated paths, identity, the
// lifecycle table, smart HTTP with every capability, the refusals of
// the contract's own table.

// capabilities are the rows of spec 003's capability table with the
// service that advertises each.
var capabilities = []struct{ name, service string }{
	{"allow-tip-sha1-in-want", "git-upload-pack"},
	{"allow-reachable-sha1-in-want", "git-upload-pack"},
	{"filter", "git-upload-pack"},
	{"shallow", "git-upload-pack"},
	{"deepen-since", "git-upload-pack"},
	{"deepen-not", "git-upload-pack"},
	{"atomic", "git-receive-pack"},
	{"push-options", "git-receive-pack"},
	{"report-status-v2", "git-receive-pack"},
}

func cases003() []testCase {
	cases := []testCase{
		{name: "version", run: case003Version},
		{name: "unauthenticated", run: case003Unauthenticated},
		{name: "invalid_request", run: case003InvalidRequest},
		{name: "lifecycle", run: case003Lifecycle},
		{name: "repo_not_found", run: case003RepoNotFound},
		{name: "repo_exists", run: case003RepoExists},
		{name: "smart-http", run: case003SmartHTTP},
	}
	for _, c := range capabilities {
		cases = append(cases, testCase{name: "capability/" + c.name, run: func(t *testing.T, s *session) {
			check(t, !(!strings.Contains(s.advertisement(t, s.fixture.id, c.service), " "+c.name+" ")), "%s does not advertise %s", c.service, c.name)
		}})
	}
	return append(cases,
		testCase{name: "fetch-by-hash", run: case003FetchByHash},
		testCase{name: "partial-clone", run: case003PartialClone},
		testCase{name: "shallow", run: case003Shallow},
		testCase{name: "atomic-push", run: case003AtomicPush},
		testCase{name: "push-options", run: case003PushOptions},
		testCase{name: "non_fast_forward", run: case003NonFastForward},
		testCase{name: "storage_unavailable", group: GroupStorage, run: case003StorageUnavailable},
	)
}

// advertisement is the protocol v0 capability line of a service's
// advertisement, padded with spaces so a name matches whole.
func (s *session) advertisement(t *testing.T, id, service string) string {
	t.Helper()
	r := s.call(t, "GET", "/r/"+id+".git/info/refs?service="+service, "")
	expectStatus(t, r, http.StatusOK)
	if ct := r.header.Get("Content-Type"); ct != "application/x-"+service+"-advertisement" {
		t.Fatalf("Content-Type %q", ct)
	}
	// The first reference line carries the capabilities after a NUL.
	_, caps, ok := strings.Cut(string(r.body), "\x00")
	check(t, !(!ok), "no capability line in the advertisement:\n%s", r.body)
	line, _, _ := strings.Cut(caps, "\n")
	return " " + strings.TrimSpace(line) + " "
}

func case003Version(t *testing.T, s *session) {
	r := s.as(t, "", "GET", "/version", "")
	expectStatus(t, r, http.StatusOK)
	check(t, !(r.json["version"] == nil), "/version: %s", r.body)
	expectStatus(t, s.as(t, "", "GET", "/readyz", ""), http.StatusOK)
}

func case003Unauthenticated(t *testing.T, s *session) {
	for _, path := range []string{"/v1/repos/" + s.fixture.id, "/r/" + s.fixture.id + ".git/info/refs?service=git-upload-pack"} {
		r := s.as(t, "", "GET", path, "")
		d := expectError(t, r, http.StatusUnauthorized, contract.CodeUnauthenticated)
		check(t, !(r.header.Get("WWW-Authenticate") != `Basic realm="origo"` || d["reason"] != "missing"), "%s: %v %v", path, r.header.Get("WWW-Authenticate"), d)
	}
	d := expectError(t, s.as(t, "not-a-token", "GET", "/v1/repos/"+s.fixture.id, ""), http.StatusUnauthorized, contract.CodeUnauthenticated)
	check(t, !(d["reason"] != "malformed"), "malformed token: %v", d)
}

func case003InvalidRequest(t *testing.T, s *session) {
	d := expectError(t, s.call(t, "POST", "/v1/repos", `{"id":"nope","owner":"a","slug":"b"}`), http.StatusBadRequest, contract.CodeInvalid)
	check(t, !(d["field"] != "id" || d["reason"] == nil), "bad id: %v", d)
	d = expectError(t, s.call(t, "GET", "/r/"+s.fixture.id+".git/info/refs?service=git-nope", ""), http.StatusBadRequest, contract.CodeInvalid)
	check(t, !(d["field"] != "service"), "bad service: %v", d)
	expectError(t, s.call(t, "POST", "/v1/repos", `{"id":"`+newID(t)+`","owner":"r","slug":"x"}`), http.StatusBadRequest, contract.CodeInvalid)
	expectError(t, s.call(t, "POST", "/v1/repos", `{"unknown":1}`), http.StatusBadRequest, contract.CodeInvalid)
}

// representation is the fields of spec 003's representation, with the
// ones later specs own.
var representation = []string{"default_branch", "frozen_at", "head", "id", "owner", "pushed_at", "size_bytes", "slug", "updated_at"}

func case003Lifecycle(t *testing.T, s *session) {
	id := newID(t)
	slug := SlugPrefix + "life-" + id[:8]
	r := s.call(t, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, Owner, slug))
	s.record(id)
	expectStatus(t, r, http.StatusCreated)
	check(t, !(r.json["id"] != id || r.json["owner"] != Owner || r.json["slug"] != slug || r.json["default_branch"] != "main" || r.json["head"] != "" || r.json["pushed_at"] != nil || r.json["frozen_at"] != nil), "representation: %s", r.body)
	for _, k := range representation {
		if _, ok := r.json[k]; !ok {
			t.Fatalf("representation lacks %s: %s", k, r.body)
		}
	}
	r = s.call(t, "GET", "/v1/repos/"+id, "")
	expectStatus(t, r, http.StatusOK)
	check(t, !(r.json["id"] != id || r.json["size_bytes"] != float64(0)), "get: %s", r.body)
	// A rename takes effect at once and the old URL answers 404.
	expectStatus(t, s.call(t, "GET", "/"+Owner+"/"+slug+".git/info/refs?service=git-upload-pack", ""), http.StatusOK)
	r = s.call(t, "PATCH", "/v1/repos/"+id, `{"slug":"`+slug+`-renamed"}`)
	expectStatus(t, r, http.StatusOK)
	check(t, !(r.json["slug"] != slug+"-renamed"), "rename: %s", r.body)
	expectError(t, s.call(t, "GET", "/"+Owner+"/"+slug+".git/info/refs?service=git-upload-pack", ""), http.StatusNotFound, contract.CodeRepoNotFound)
	expectStatus(t, s.call(t, "GET", "/"+Owner+"/"+slug+"-renamed.git/info/refs?service=git-upload-pack", ""), http.StatusOK)
	// default_branch moves HEAD through the log.
	r = s.call(t, "PATCH", "/v1/repos/"+id, `{"default_branch":"trunk"}`)
	expectStatus(t, r, http.StatusOK)
	check(t, !(r.json["default_branch"] != "trunk"), "default_branch: %s", r.body)
	// Delete, the hold, a repeated delete, undelete.
	r = s.call(t, "DELETE", "/v1/repos/"+id, "")
	expectStatus(t, r, http.StatusAccepted)
	deletedAt, purgeAfter := r.json["deleted_at"], r.json["purge_after"]
	check(t, !(r.json["id"] != id || deletedAt == nil || purgeAfter == nil), "delete: %s", r.body)
	da, err := time.Parse(time.RFC3339Nano, str(deletedAt))
	pa, err2 := time.Parse(time.RFC3339Nano, str(purgeAfter))
	check(t, !(err != nil || err2 != nil || pa.Sub(da) != 7*24*time.Hour), "hold: %v %v %v %v", deletedAt, purgeAfter, err, err2)
	expectError(t, s.call(t, "GET", "/v1/repos/"+id, ""), http.StatusNotFound, contract.CodeRepoNotFound)
	expectError(t, s.call(t, "GET", "/r/"+id+".git/info/refs?service=git-upload-pack", ""), http.StatusNotFound, contract.CodeRepoNotFound)
	r = s.call(t, "DELETE", "/v1/repos/"+id, "")
	expectStatus(t, r, http.StatusAccepted)
	check(t, !(r.json["deleted_at"] != deletedAt || r.json["purge_after"] != purgeAfter), "repeated delete: %s", r.body)
	r = s.call(t, "POST", "/v1/repos/"+id+"/undelete", "")
	expectStatus(t, r, http.StatusOK)
	check(t, !(r.json["id"] != id || r.json["default_branch"] != "trunk"), "undelete: %s", r.body)
	expectStatus(t, s.call(t, "GET", "/v1/repos/"+id, ""), http.StatusOK)
}

func case003RepoNotFound(t *testing.T, s *session) {
	id := newID(t)
	d := expectError(t, s.call(t, "GET", "/v1/repos/"+id, ""), http.StatusNotFound, contract.CodeRepoNotFound)
	check(t, !(d["id"] != id), "details: %v", d)
	expectError(t, s.call(t, "GET", "/v1/repos/not-a-uuid", ""), http.StatusNotFound, contract.CodeRepoNotFound)
	d = expectError(t, s.call(t, "GET", "/"+Owner+"/"+SlugPrefix+"nowhere.git/info/refs?service=git-upload-pack", ""), http.StatusNotFound, contract.CodeRepoNotFound)
	check(t, !(d["owner"] != Owner || d["slug"] != SlugPrefix+"nowhere"), "details by name: %v", d)
}

func case003RepoExists(t *testing.T, s *session) {
	f := s.fixture
	d := expectError(t, s.call(t, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":"other"}`, f.id, Owner)), http.StatusConflict, contract.CodeRepoExists)
	check(t, !(d["field"] != "id" || d["id"] != f.id), "duplicate id: %v", d)
	id := newID(t)
	r := s.call(t, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, Owner, f.slug))
	d = expectError(t, r, http.StatusConflict, contract.CodeRepoExists)
	check(t, !(d["field"] != "name" || d["owner"] != Owner || d["slug"] != f.slug), "taken name: %v", d)
	// The refused create left nothing behind.
	expectError(t, s.call(t, "GET", "/v1/repos/"+id, ""), http.StatusNotFound, contract.CodeRepoNotFound)
}

func case003SmartHTTP(t *testing.T, s *session) {
	id := s.create(t, "smart")
	slug := SlugPrefix + "smart-" + id[:8]
	work := clone(t, s.repoURL(id))
	c1 := commitFile(t, work, "a.txt", []byte("one"), "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	r := s.call(t, "GET", "/v1/repos/"+id, "")
	expectStatus(t, r, http.StatusOK)
	check(t, !(r.json["head"] != c1 || r.json["pushed_at"] == nil || num(r.json["size_bytes"]) <= 0), "after the push: %s", r.body)
	// The label form clones the same history, and a fetch through it
	// sees the next push made through the id form, a push whose body
	// exceeds git's post buffer and arrives chunked.
	byName := clone(t, s.nameURL(Owner, slug))
	check(t, !(revList(t, byName) != revList(t, work)), "%v", "the clone by name differs")
	c2 := commitFile(t, work, "b.txt", gittest.Bytes(256<<10, 3), "second")
	mustGit(t, work, "-c", "http.postBuffer=65536", "push", "-q", "origin", "HEAD:refs/heads/main")
	mustGit(t, byName, "fetch", "-q", "origin")
	check(t, !(mustGit(t, byName, "rev-parse", "origin/main") != c2), "%v", "the fetch by name did not see the push by id")
	// Protocol v2 on the same routes.
	v2 := clone(t, s.repoURL(id), "-c", "protocol.version=2")
	check(t, !(mustGit(t, v2, "rev-parse", "HEAD") != c2), "%v", "the protocol v2 clone")
	// A branch deletion is a push without a pack.
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/gone")
	mustGit(t, work, "push", "-q", "origin", ":refs/heads/gone")
	r = s.call(t, "GET", "/v1/repos/"+id+"/refs", "")
	expectStatus(t, r, http.StatusOK)
	check(t, !(strings.Contains(string(r.body), "refs/heads/gone")), "the deleted branch is still there: %s", r.body)
}

// case003FetchByHash fetches a reachable commit by its hash, under
// protocol version 0, where the two sha1-in-want capabilities decide;
// protocol v2 lets a client want any object whatever the server's
// configuration, so v0 is what proves the rows.
func case003FetchByHash(t *testing.T, s *session) {
	f := s.fixture
	dir := initRepo(t, s.repoURL(f.id))
	mustGit(t, dir, "-c", "protocol.version=0", "fetch", "-q", "--depth", "1", "origin", f.c1)
	check(t, !(mustGit(t, dir, "rev-parse", "FETCH_HEAD") != f.c1), "%v", "fetched the wrong commit")
}

// case003PartialClone clones with blob:none and proves the clone is
// partial: the blobs are missing until read.
func case003PartialClone(t *testing.T, s *session) {
	f := s.fixture
	dir := clone(t, s.repoURL(f.id), "--filter=blob:none", "--no-checkout")
	missing := mustGit(t, dir, "rev-list", "--objects", "--all", "--missing=print")
	check(t, !(!strings.Contains(missing, "?"+f.blobA)), "the clone holds every blob; not partial:\n%s", missing)
	// A read fetches the blob from the server.
	if got := mustGit(t, dir, "cat-file", "-p", f.blobA); got != "one\ntwo" {
		t.Fatalf("blob through the promisor: %q", got)
	}
}

func case003Shallow(t *testing.T, s *session) {
	f := s.fixture
	dir := clone(t, s.repoURL(f.id), "--depth", "1")
	check(t, !(mustGit(t, dir, "rev-list", "--count", "HEAD") != "1"), "%v", "depth 1")
	mustGit(t, dir, "fetch", "-q", "--deepen", "1", "origin")
	check(t, !(mustGit(t, dir, "rev-list", "--count", "HEAD") != "2"), "%v", "deepen")
	mustGit(t, dir, "fetch", "-q", "--unshallow", "origin")
	check(t, !(mustGit(t, dir, "rev-list", "--count", "HEAD") != "3"), "%v", "unshallow")
	since := clone(t, s.repoURL(f.id), "--shallow-since="+f.commitDate)
	check(t, !(mustGit(t, since, "rev-list", "--count", "HEAD") != "3"), "%v", "deepen-since")
	not := clone(t, s.repoURL(f.id), "--shallow-exclude=v1")
	check(t, !(mustGit(t, not, "rev-list", "--count", "HEAD") != "1"), "%v", "deepen-not")
}

func case003AtomicPush(t *testing.T, s *session) {
	id := s.create(t, "atomic")
	work := clone(t, s.repoURL(id))
	c := commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "tag", "v1")
	mustGit(t, work, "push", "-q", "--atomic", "origin", "HEAD:refs/heads/main", "HEAD:refs/heads/dev", "refs/tags/v1")
	r := s.call(t, "GET", "/v1/repos/"+id+"/refs", "")
	expectStatus(t, r, http.StatusOK)
	for _, ref := range []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1"} {
		check(t, !(!strings.Contains(string(r.body), `"name":"`+ref+`","sha":"`+c+`"`)), "%s missing after the atomic push: %s", ref, r.body)
	}
	// One push, one push event.
	if d, ok := s.expectEvent(t, id, "push", 1); ok {
		if updates, _ := event(t, d)["updates"].([]any); len(updates) != 3 {
			t.Fatalf("the atomic push's event carries %d updates", len(updates))
		}
	}
}

func case003PushOptions(t *testing.T, s *session) {
	id := s.create(t, "options")
	work := clone(t, s.repoURL(id))
	commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "push", "-q", "-o", "origo.event=off", "origin", "HEAD:refs/heads/main")
	expectStatus(t, s.call(t, "GET", "/v1/repos/"+id, ""), http.StatusOK)
}

// case003NonFastForward pushes over a reference another clone moved
// between the advertisement and the pack: a pre-push hook in the second
// clone runs the first clone's push after git read the advertisement.
// The server's refusal is the table's line, and no name or hash.
func case003NonFastForward(t *testing.T, s *session) {
	id := s.create(t, "nff")
	a := clone(t, s.repoURL(id))
	commitFile(t, a, "a.txt", []byte("a"), "base")
	mustGit(t, a, "push", "-q", "origin", "HEAD:refs/heads/main")
	b := clone(t, s.repoURL(id))
	commitFile(t, a, "a.txt", []byte("a2"), "a2")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ngit -C '" + a + "' push -q origin HEAD:refs/heads/main\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(hook), 0o755); err != nil { //nolint:gosec // a hook must be executable
		t.Fatal(err)
	}
	mustGit(t, b, "config", "core.hooksPath", hooks)
	commitFile(t, b, "b.txt", []byte("b"), "b")
	out, err := git(t, b, "push", "origin", "HEAD:refs/heads/main")
	check(t, !(err == nil), "the stale push landed:\n%s", redact(out))
	if want := "remote: " + contract.Line(contract.CodeNonFastForward); !strings.Contains(out, want) {
		t.Fatalf("git's output lacks %q:\n%s", want, redact(out))
	}
	// After a fetch the push lands.
	mustGit(t, b, "config", "--unset", "core.hooksPath")
	mustGit(t, b, "pull", "-q", "--rebase", "origin", "main")
	mustGit(t, b, "push", "-q", "origin", "HEAD:refs/heads/main")
}

// case003StorageUnavailable cuts the bucket and asserts the row: 503
// with the sentence, on the JSON API and on the git routes.
func case003StorageUnavailable(t *testing.T, s *session) {
	f := s.fixture
	s.target.Fault.CutStorage(t)
	var r response
	waitFor(t, 3*time.Minute, "503 storage_unavailable under the cut", func() bool {
		r = s.call(t, "GET", "/v1/repos/"+f.id, "")
		return r.status == http.StatusServiceUnavailable
	})
	expectError(t, r, http.StatusServiceUnavailable, contract.CodeStorageUnavailable)
	id := newID(t)
	expectError(t, s.call(t, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, Owner, SlugPrefix+"cut-"+id[:8])), http.StatusServiceUnavailable, contract.CodeStorageUnavailable)
}
