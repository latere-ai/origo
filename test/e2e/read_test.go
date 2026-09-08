// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
)

// read performs a GET of the read API on the node with the headers
// given as pairs and returns the status, the headers, and the body.
func (n *node) read(id, path string, headers ...string) (int, http.Header, []byte) {
	n.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://"+n.public+"/v1/repos/"+id+path, nil)
	req.Header.Set("Authorization", "Bearer "+n.s.token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		n.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, body
}

// TestE2EReadAPI drives every endpoint of spec 009 against a running
// node over the fixture pushed with the real git: refs, commits,
// commits/{sha}, compare, tree, blob, and the archive, plus pushed_at
// on the repository and a revalidation.
func TestE2EReadAPI(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "acme", "app-"+id[:8])
	f := gittest.NewFixture(t, "")
	mustGit(t, f.Dir, "push", "-q", n.url(id), "--all")
	mustGit(t, f.Dir, "push", "-q", n.url(id), "--tags")

	status, hdr, body := n.read(id, "/refs")
	var refs []struct {
		Name   string  `json:"name"`
		SHA    string  `json:"sha"`
		Peeled *string `json:"peeled"`
	}
	_ = json.Unmarshal(body, &refs)
	byName := map[string]int{}
	for i, r := range refs {
		byName[r.Name] = i
	}
	if status != 200 || hdr.Get("ETag") != `"2"` || refs[byName["refs/heads/main"]].SHA != f.Tip || refs[byName["refs/tags/v1.0"]].Peeled == nil || *refs[byName["refs/tags/v1.0"]].Peeled != f.Trailers {
		t.Fatalf("refs: %d %v %s", status, hdr, body)
	}

	status, hdr, body = n.read(id, "/commits?limit=3")
	var page struct {
		Commits []struct {
			SHA      string              `json:"sha"`
			Trailers []map[string]string `json:"trailers"`
			Stats    *map[string]int     `json:"stats"`
		} `json:"commits"`
		NextCursor *string `json:"next_cursor"`
	}
	_ = json.Unmarshal(body, &page)
	if status != 200 || hdr.Get("Origo-Commit") != f.Tip || len(page.Commits) != 3 || page.Commits[0].SHA != f.Tip || page.NextCursor == nil || *page.NextCursor != page.Commits[2].SHA {
		t.Fatalf("commits: %d %v %s", status, hdr, body)
	}
	status, _, body = n.read(id, "/commits/"+f.Trailers)
	var commit struct {
		SHA      string              `json:"sha"`
		Trailers []map[string]string `json:"trailers"`
		Stats    map[string]int      `json:"stats"`
	}
	_ = json.Unmarshal(body, &commit)
	if status != 200 || commit.SHA != f.Trailers || len(commit.Trailers) != 3 || commit.Trailers[2]["key"] != "Co-authored-by" || commit.Stats["files"] != 1 {
		t.Fatalf("commit: %d %s", status, body)
	}

	status, hdr, body = n.read(id, "/compare/"+gittest.Run(t, f.Dir, nil, "rev-parse", f.Rename+"~1")+"..."+f.Rename)
	if status != 200 || hdr.Get("Content-Type") != "text/x-diff" || hdr.Get("Origo-Commit") != f.Rename || !bytes.Contains(body, []byte("rename from src/a.txt")) {
		t.Fatalf("compare: %d %v %s", status, hdr, body)
	}
	status, hdr, body = n.read(id, "/compare/"+f.Tip+"..."+f.Large)
	if status != 200 || hdr.Get("Origo-Truncated") != "true" || len(body) > 1<<20 {
		t.Fatalf("compare truncated: %d %v %d bytes", status, hdr, len(body))
	}

	status, _, body = n.read(id, "/tree/main?path=src")
	var tree struct {
		Entries []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(body, &tree)
	if status != 200 || len(tree.Entries) != 1 || tree.Entries[0].Path != "src/b.txt" || tree.Entries[0].Type != "blob" {
		t.Fatalf("tree: %d %s", status, body)
	}
	readme := gittest.Run(t, f.Dir, nil, "rev-parse", f.Trailers+":README.md")
	status, hdr, body = n.read(id, "/blob/"+readme)
	if status != 200 || string(body) != "# Fixture\n\nSigned.\n" || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") || hdr.Get("Content-Length") != "19" {
		t.Fatalf("blob: %d %v %q", status, hdr, body)
	}
	if status, _, body := n.read(id, "/blob/"+f.BigBlob); status != 413 || !bytes.Contains(body, []byte("blob_too_large")) {
		t.Fatalf("big blob: %d %s", status, body)
	}
	status, hdr, body = n.read(id, "/blob/"+f.BigBlob, "Range", "bytes=0-1023")
	if status != 206 || len(body) != 1024 || hdr.Get("Content-Range") != fmt.Sprintf("bytes 0-1023/%d", gittest.BigBlobSize) {
		t.Fatalf("blob range: %d %v %d bytes", status, hdr, len(body))
	}

	status, hdr, body = n.read(id, "/archive/v1.0.tar.gz")
	if status != 200 || hdr.Get("Content-Type") != "application/gzip" || hdr.Get("Origo-Commit") != f.Trailers {
		t.Fatalf("archive: %d %v", status, hdr)
	}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(archive, body, 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := exec.CommandContext(context.Background(), "tar", "-tzf", archive).Output()
	if err != nil {
		t.Fatal(err)
	}
	prefix := "app-" + id[:8] + "-" + f.Trailers[:7] + "/"
	if got := strings.Fields(string(list)); len(got) != 6 || got[0] != prefix || got[1] != prefix+"README.md" {
		t.Fatalf("archive entries %v", got)
	}
	// The same bytes from a second node with an empty disk.
	sum := sha256.Sum256(body)
	other := startNode(t, s, "", nil)
	if _, _, again := other.read(id, "/archive/v1.0.tar.gz"); sha256.Sum256(again) != sum {
		t.Fatal("the archive differs between nodes")
	}

	// A revalidation is 304; the repository carries pushed_at.
	if status, _, _ := n.read(id, "/tree/main", "If-None-Match", `"2"`); status != 304 {
		t.Fatalf("revalidation: %d", status)
	}
	if status, out := n.api("GET", "/v1/repos/"+id, ""); status != 200 || out["pushed_at"] == nil {
		t.Fatalf("pushed_at: %d %v", status, out)
	}
	if status, _, body := n.read(id, "/commits/nope"); status != 404 || !bytes.Contains(body, []byte("ref_not_found")) {
		t.Fatalf("unknown ref: %d %s", status, body)
	}
}

// pushRandomTree pushes files of size bytes each of random content to
// the repository's main branch and returns the commit id.
func pushRandomTree(t *testing.T, n *node, id string, files int, size int) string {
	t.Helper()
	work := clone(t, n.url(id))
	buf := make([]byte, size)
	for i := range files {
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("blob-%03d.bin", i)), buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, work, "add", "-A", ".")
	mustGit(t, work, "commit", "-q", "-m", "tree")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return mustGit(t, work, "rev-parse", "HEAD")
}

// firstByte requests the archive of the commit and returns the time to
// its first body byte and the number of bytes streamed.
func firstByte(t *testing.T, n *node, id, sha string) (time.Duration, int64) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://"+n.public+"/v1/repos/"+id+"/archive/"+sha+".tar.gz", nil)
	req.Header.Set("Authorization", "Bearer "+n.s.token)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var one [1]byte
	if _, err := io.ReadFull(resp.Body, one[:]); err != nil || resp.StatusCode != 200 {
		t.Fatalf("archive: %d %v", resp.StatusCode, err)
	}
	elapsed := time.Since(start)
	rest, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return elapsed, rest + 1
}

// TestE2EArchiveStreams: the archive of a 200 MiB tree sends its first
// byte within 200 ms of the request.
func TestE2EArchiveStreams(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "acme", "tree-"+id[:8])
	sha := pushRandomTree(t, n, id, 20, 10<<20)
	n.read(id, "/refs") // the copy is current before the clock starts
	// A read of the tree is the baseline of the node's prologue, logged
	// beside the figure so a slow runner is told from a slow archive.
	start := time.Now()
	n.read(id, "/tree/main")
	baseline := time.Since(start)
	elapsed, size := firstByte(t, n, id, sha)
	t.Logf("first byte after %s (a tree read took %s), %d bytes", elapsed, baseline, size)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("first byte after %s", elapsed)
	}
	if size < 190<<20 {
		t.Fatalf("%d bytes streamed", size)
	}
	// A second request with the tag is served from the same copy.
	if status, _, _ := n.read(id, "/refs", "If-None-Match", `"1"`); status != 304 {
		t.Fatalf("revalidation: %d", status)
	}
}

// measureArchive prints the time to the first byte of the archive of a
// 1 GiB tree, the figure TestE2EArchiveStreams asserts for 200 MiB.
func measureArchive(t *testing.T, s *stack) {
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "bench", "archive-"+id[:8])
	sha := pushRandomTree(t, n, id, 64, 16<<20)
	n.read(id, "/refs")
	elapsed, size := firstByte(t, n, id, sha)
	t.Logf("MEASURE archive: first byte of a 1 GiB tree after %s; %d bytes streamed", elapsed, size)
}
