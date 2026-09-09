// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/contract"
)

// The rows of spec 009: the read endpoints against the fixture the
// session pushed, the archive's reproducibility, and the two refusals.

func cases009() []testCase {
	return []testCase{
		{name: "refs", run: case009Refs},
		{name: "commits", run: case009Commits},
		{name: "commit", run: case009Commit},
		{name: "compare", run: case009Compare},
		{name: "tree", run: case009Tree},
		{name: "blob", run: case009Blob},
		{name: "archive", run: case009Archive},
		{name: "ref_not_found", run: case009RefNotFound},
		{name: "blob_too_large", run: case009BlobTooLarge},
	}
}

// read calls a read endpoint of the fixture and asserts 200 with
// Origo-Commit.
func (s *session) read(t *testing.T, path string, header map[string]string) response {
	t.Helper()
	r := s.do(t, request{method: "GET", path: "/v1/repos/" + s.fixture.id + path, token: s.target.Token, header: header})
	expectStatus(t, r, http.StatusOK)
	return r
}

func case009Refs(t *testing.T, s *session) {
	f := s.fixture
	r := s.read(t, "/refs", nil)
	var refs []map[string]any
	decode(t, r.body, &refs)
	byName := map[string]map[string]any{}
	for _, ref := range refs {
		byName[str(ref["name"])] = ref
	}
	failIf(t, byName["refs/heads/main"]["sha"] != f.c2 || byName["refs/tags/v1"]["sha"] != f.tag || byName["refs/tags/v1"]["peeled"] != f.tagged, "refs: %s", r.body)
	r = s.read(t, "/refs?prefix=refs/tags/", nil)
	decode(t, r.body, &refs)
	failIf(t, len(refs) != 1 || refs[0]["name"] != "refs/tags/v1" || r.header.Get(api.HeaderTruncated) != "", "tags: %s", r.body)
}

func case009Commits(t *testing.T, s *session) {
	f := s.fixture
	r := s.read(t, "/commits?limit=2", nil)
	failIf(t, r.header.Get(api.HeaderCommit) != f.c2, "%s %q", api.HeaderCommit, r.header.Get(api.HeaderCommit))
	commits, _ := r.json["commits"].([]any)
	failIf(t, len(commits) != 2 || r.json["next_cursor"] == nil, "first page: %s", r.body)
	first, _ := commits[0].(map[string]any)
	failIf(t, first["sha"] != f.c2 || first["message"] == nil || first["author"] == nil || first["committer"] == nil || first["parents"] == nil, "commit shape: %v", first)
	if trailers, _ := first["trailers"].([]any); len(trailers) != 1 || obj(trailers[0])["key"] != "Signed-off-by" {
		t.Fatalf("trailers: %v", first["trailers"])
	}
	r = s.read(t, "/commits?limit=2&cursor="+str(r.json["next_cursor"]), nil)
	commits, _ = r.json["commits"].([]any)
	failIf(t, len(commits) != 1 || obj(commits[0])["sha"] != f.c1 || r.json["next_cursor"] != nil, "second page: %s", r.body)
	r = s.read(t, "/commits?ref=v1", nil)
	commits, _ = r.json["commits"].([]any)
	failIf(t, len(commits) != 2, "by tag: %s", r.body)
	d := expectError(t, s.call(t, "GET", "/v1/repos/"+f.id+"/commits?cursor="+strings.Repeat("0", 40), ""), http.StatusBadRequest, contract.CodeInvalid)
	if reason, _ := d["reason"].(string); !strings.HasPrefix(reason, "cursor") {
		t.Fatalf("bad cursor: %v", d)
	}
	// An empty repository has no commits and no error.
	empty := s.create(t, "empty")
	r = s.call(t, "GET", "/v1/repos/"+empty+"/commits", "")
	expectStatus(t, r, http.StatusOK)
	commits, _ = r.json["commits"].([]any)
	failIf(t, len(commits) != 0 || r.json["next_cursor"] != nil, "empty repository: %s", r.body)
}

func case009Commit(t *testing.T, s *session) {
	f := s.fixture
	r := s.read(t, "/commits/"+f.c2, nil)
	stats, _ := r.json["stats"].(map[string]any)
	failIf(t, r.json["sha"] != f.c2 || stats["files"] != float64(1) || stats["additions"] != float64(1) || stats["deletions"] != float64(0), "commit: %s", r.body)
	// ETag revalidation.
	r2 := s.do(t, request{method: "GET", path: "/v1/repos/" + f.id + "/commits/" + f.c2, token: s.target.Token, header: map[string]string{"If-None-Match": r.header.Get("ETag")}})
	failIf(t, r.header.Get("ETag") == "" || r2.status != http.StatusNotModified, "ETag %q: %d", r.header.Get("ETag"), r2.status)
}

func case009Compare(t *testing.T, s *session) {
	f := s.fixture
	r := s.read(t, "/compare/"+f.c1+"..."+f.c2, nil)
	failIf(t, !strings.HasPrefix(r.header.Get("Content-Type"), "text/x-diff") || !strings.Contains(string(r.body), "+++ b/sub/b.txt") || !strings.Contains(string(r.body), "Binary files"), "compare: %q\n%s", r.header.Get("Content-Type"), r.body)
	r = s.read(t, "/compare/"+f.c1+"..."+f.c2+"?path=sub", nil)
	failIf(t, strings.Contains(string(r.body), "bin.dat"), "path filter: %s", r.body)
}

func case009Tree(t *testing.T, s *session) {
	f := s.fixture
	r := s.read(t, "/tree/"+f.c2, nil)
	entries, _ := r.json["entries"].([]any)
	failIf(t, len(entries) != 3, "tree: %s", r.body)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		for _, k := range []string{"path", "mode", "type", "sha", "size"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("entry lacks %s: %v", k, m)
			}
		}
	}
	r = s.read(t, "/tree/"+f.c2+"?recursive=1", nil)
	failIf(t, !strings.Contains(string(r.body), `"path":"sub/b.txt"`), "recursive: %s", r.body)
	r = s.read(t, "/tree/"+f.c2+"?path=sub", nil)
	entries, _ = r.json["entries"].([]any)
	failIf(t, len(entries) != 1, "path: %s", r.body)
}

func case009Blob(t *testing.T, s *session) {
	f := s.fixture
	r := s.read(t, "/blob/"+f.blobA, nil)
	failIf(t, string(r.body) != "one\ntwo\n" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") || r.header.Get("Content-Length") != "8" || r.header.Get(api.HeaderCommit) != f.blobA, "blob: %v %q", r.header, r.body)
	binary := mustGit(t, f.work, "rev-parse", "HEAD:bin.dat")
	r = s.read(t, "/blob/"+binary, nil)
	failIf(t, !bytes.Equal(r.body, f.binary) || r.header.Get("Content-Type") != "application/octet-stream", "binary blob: %q %d bytes", r.header.Get("Content-Type"), len(r.body))
	r = s.do(t, request{method: "GET", path: "/v1/repos/" + f.id + "/blob/" + f.blobA, token: s.target.Token, header: map[string]string{"Range": "bytes=4-6"}})
	failIf(t, r.status != http.StatusPartialContent || string(r.body) != "two" || r.header.Get("Content-Range") != "bytes 4-6/8", "range: %d %q %q", r.status, r.body, r.header.Get("Content-Range"))
}

// case009Archive downloads the archive twice and asserts the bytes are
// the same, the prefix is <slug>-<7 hex>/, and the tree is the
// commit's.
func case009Archive(t *testing.T, s *session) {
	f := s.fixture
	first := s.read(t, "/archive/"+f.c2+".tar.gz", nil)
	second := s.read(t, "/archive/"+f.c2+".tar.gz", nil)
	failIf(t, first.header.Get("Content-Type") != "application/gzip" || !bytes.Equal(first.body, second.body), "archive: %q, %d and %d bytes", first.header.Get("Content-Type"), len(first.body), len(second.body))
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(path, first.body, 0o644); err != nil {
		t.Fatal(err)
	}
	prefix := f.slug + "-" + f.c2[:7] + "/"
	entries := strings.Split(mustGit(t, f.work, "ls-tree", "-r", "--name-only", f.c2), "\n")
	listed := listTar(t, path)
	for _, e := range entries {
		failIf(t, !strings.Contains(listed, prefix+e), "archive lacks %s%s:\n%s", prefix, e, listed)
	}
	failIf(t, strings.Contains(listed, ".git/"), "archive carries .git:\n%s", listed)
}

// listTar lists a tar.gz through the tar binary git itself needs.
func listTar(t *testing.T, path string) string {
	t.Helper()
	dir := filepath.Dir(path)
	out, err := run(dir, "tar", "-tzf", path)
	failIf(t, err != nil, "tar: %v\n%s", err, out)
	return out
}

func case009RefNotFound(t *testing.T, s *session) {
	f := s.fixture
	d := expectError(t, s.call(t, "GET", "/v1/repos/"+f.id+"/commits?ref=refs/heads/nope", ""), http.StatusNotFound, contract.CodeRefNotFound)
	failIf(t, d["ref"] != "refs/heads/nope", "details: %v", d)
	expectError(t, s.call(t, "GET", "/v1/repos/"+f.id+"/tree/"+strings.Repeat("0", 40), ""), http.StatusNotFound, contract.CodeRefNotFound)
}

// case009BlobTooLarge pushes a blob one byte over 50 MiB, which git
// compresses to a few kilobytes, and asserts 413 whole and 206 in a
// range.
func case009BlobTooLarge(t *testing.T, s *session) {
	id := s.create(t, "big")
	work := clone(t, s.repoURL(id))
	c := commitFile(t, work, "big.bin", make([]byte, api.MaxBlobBytes+1), "big")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	blob := mustGit(t, work, "rev-parse", c+":big.bin")
	d := expectError(t, s.call(t, "GET", "/v1/repos/"+id+"/blob/"+blob, ""), http.StatusRequestEntityTooLarge, contract.CodeBlobTooLarge)
	failIf(t, d["size"] != float64(api.MaxBlobBytes+1) || d["max"] != float64(api.MaxBlobBytes), "details: %v", d)
	r := s.do(t, request{method: "GET", path: "/v1/repos/" + id + "/blob/" + blob, token: s.target.Token, header: map[string]string{"Range": "bytes=0-1023"}})
	failIf(t, r.status != http.StatusPartialContent || len(r.body) != 1024, "range of the large blob: %d %d bytes", r.status, len(r.body))
}
