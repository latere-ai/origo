// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// The fixture of spec 009 is built once per test binary; its commit
// ids are fixed by the Source's clock, so the golden files hold.
var (
	fixtureOnce sync.Once
	fixtureDir  string
	fixture     *gittest.Fixture
	fixturePack []byte
)

func TestMain(m *testing.M) {
	// The test binary doubles as the git binary a harness points the
	// cache at through a symbolic link named git.
	if filepath.Base(os.Args[0]) == "git" {
		os.Exit(fakeGit())
	}
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

func loadFixture(t *testing.T) *gittest.Fixture {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "origo-api-fixture-")
		if err != nil {
			t.Fatal(err)
		}
		fixtureDir = dir
		fixture = gittest.NewFixture(t, filepath.Join(dir, "src"))
		fixturePack = fixture.PackAll()
	})
	if fixture == nil {
		t.Fatal("the fixture was not built")
	}
	return fixture
}

// seed creates repoA and commits the fixture's whole history as one
// push entry, so the cache materializes it from the log.
func (h *harness) seed(f *gittest.Fixture) {
	h.t.Helper()
	ctx := context.Background()
	ix, err := h.log.CreateRepo(ctx, wal.Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main")
	if err != nil {
		h.t.Fatal(err)
	}
	e := wal.Entry{Kind: wal.KindPush, Subject: "alice", Pack: wal.BytesBody(fixturePack)}
	for name, sha := range f.Refs {
		if name != "HEAD" {
			e.Refs = append(e.Refs, wal.RefUpdate{Ref: name, Old: wal.ZeroSHA, New: sha})
		}
	}
	if _, err := h.log.Commit(ctx, repoA, ix, e, noCatchUp); err != nil {
		h.t.Fatal(err)
	}
}

// response is what a read request answered.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json() map[string]any {
	var out map[string]any
	_ = json.Unmarshal(r.body, &out)
	return out
}

// reason is the developer reason of an error response, "" when absent.
func (r response) reason() string {
	s, _ := details(r.json())["reason"].(string)
	return s
}

// get performs a read with the headers given as pairs.
func (h *harness) get(path string, headers ...string) response {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", h.srv.URL+path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: body}
}

// fakeGit is the test binary invoked as git: it appends its arguments
// to calls.log beside the link, holds a subcommand named in the file
// hold forever with a child of its own, and otherwise runs the real git.
func fakeGit() int {
	dir := filepath.Dir(os.Args[0])
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "__held-child" {
		time.Sleep(time.Hour)
	}
	if f, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		_ = f.Close()
	}
	if fail, err := os.ReadFile(filepath.Join(dir, "fail")); err == nil && slices.Contains(args, strings.TrimSpace(string(fail))) {
		return 7
	}
	// A subprocess that writes half of what it would and then hangs, so
	// the response headers are already sent when the deadline cuts it
	// (spec 019's export).
	if cut, err := os.ReadFile(filepath.Join(dir, "truncate")); err == nil && slices.Contains(args, strings.TrimSpace(string(cut))) {
		real, err := exec.LookPath("git")
		if err != nil {
			return 3
		}
		out, err := exec.CommandContext(context.Background(), real, args...).Output()
		if err != nil {
			return 3
		}
		n := len(out) / 2
		if raw, err := os.ReadFile(filepath.Join(dir, "truncate-bytes")); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && v < n {
				n = v
			}
		}
		_, _ = os.Stdout.Write(out[:n])
		time.Sleep(time.Hour)
	}
	if hold, err := os.ReadFile(filepath.Join(dir, "hold")); err == nil && slices.Contains(args, strings.TrimSpace(string(hold))) {
		child := exec.CommandContext(context.Background(), os.Args[0], "__held-child")
		if err := child.Start(); err != nil {
			return 3
		}
		_ = os.WriteFile(filepath.Join(dir, "held.pids"), fmt.Appendf(nil, "%d %d\n", os.Getpid(), child.Process.Pid), 0o644)
		time.Sleep(time.Hour)
	}
	real, err := exec.LookPath("git")
	if err != nil {
		return 3
	}
	if err := syscall.Exec(real, append([]string{"git"}, args...), os.Environ()); err != nil {
		return 3
	}
	return 0
}

// spyGit is a git binary that records every invocation.
type spyGit struct {
	t   *testing.T
	dir string
}

func newSpyGit(t *testing.T) *spyGit {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(self, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	return &spyGit{t: t, dir: dir}
}

func (s *spyGit) bin() string { return filepath.Join(s.dir, "git") }

// calls returns the invocations since the last reset.
func (s *spyGit) calls() []string {
	data, _ := os.ReadFile(filepath.Join(s.dir, "calls.log"))
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (s *spyGit) reset() { _ = os.Remove(filepath.Join(s.dir, "calls.log")) }

// hold makes the next invocation of the subcommand block forever.
func (s *spyGit) hold(subcommand string) {
	if err := os.WriteFile(filepath.Join(s.dir, "hold"), []byte(subcommand), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// failOn makes the next invocation of the subcommand exit 7.
func (s *spyGit) failOn(subcommand string) {
	if err := os.WriteFile(filepath.Join(s.dir, "fail"), []byte(subcommand), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// truncateOn makes the next invocation of the subcommand write half of
// its output and then hang, so the deadline cuts a body already begun.
func (s *spyGit) truncateOn(subcommand string, bytes int) {
	if err := os.WriteFile(filepath.Join(s.dir, "truncate"), []byte(subcommand), 0o644); err != nil {
		s.t.Fatal(err)
	}
	if bytes > 0 {
		if err := os.WriteFile(filepath.Join(s.dir, "truncate-bytes"), fmt.Appendf(nil, "%d", bytes), 0o644); err != nil {
			s.t.Fatal(err)
		}
		return
	}
	_ = os.Remove(filepath.Join(s.dir, "truncate-bytes"))
}

func (s *spyGit) release() {
	_ = os.Remove(filepath.Join(s.dir, "hold"))
	_ = os.Remove(filepath.Join(s.dir, "fail"))
	_ = os.Remove(filepath.Join(s.dir, "truncate"))
	_ = os.Remove(filepath.Join(s.dir, "truncate-bytes"))
}

// heldPIDs returns the held subprocess and its child.
func (s *spyGit) heldPIDs() []int {
	data, err := os.ReadFile(filepath.Join(s.dir, "held.pids"))
	if err != nil {
		return nil
	}
	var pids []int
	for f := range strings.FieldsSeq(string(data)) {
		n, _ := strconv.Atoi(f)
		pids = append(pids, n)
	}
	return pids
}

// alive reports whether a process exists.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// golden compares body to testdata/golden/<name>, rewriting it under
// -update. JSON is indented so a difference reads in a diff.
func golden(t *testing.T, name string, body []byte) {
	t.Helper()
	if strings.HasSuffix(name, ".json") {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, body)
		}
		body = append(buf.Bytes(), '\n')
	}
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run with -update to write it)", name, err)
	}
	if !bytes.Equal(want, body) {
		t.Errorf("%s differs from the golden file:\n%s", name, body)
	}
}

// TestReadEndpointsGolden compares every endpoint's body over the
// fixture to a checked-in expectation, and the 60 MiB blob to its
// digest, fetched in two ranges because it is over the whole-blob bound.
func TestReadEndpointsGolden(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.seed(f)
	parentOfRename := gittest.Run(t, f.Dir, nil, "rev-parse", f.Rename+"~1")
	readme := gittest.Run(t, f.Dir, nil, "rev-parse", f.Trailers+":README.md")
	logo := gittest.Run(t, f.Dir, nil, "rev-parse", f.Binary+":assets/logo.bin")
	base := "/v1/repos/" + repoA
	cases := []struct {
		name, path, commit, contentType string
	}{
		{"refs.json", base + "/refs", "", "application/json"},
		{"refs-tags.json", base + "/refs?prefix=refs/tags/", "", "application/json"},
		{"commits-page.json", base + "/commits?limit=5", f.Tip, "application/json"},
		{"commits-path.json", base + "/commits?ref=main&path=src/b.txt", f.Tip, "application/json"},
		{"commits-window.json", base + "/commits?ref=v1.0&since=2026-09-06T10:02:00Z&until=2026-09-06T10:04:00Z", f.Trailers, "application/json"},
		{"commits-topic.json", base + "/commits?ref=refs/heads/topic/x&limit=3", f.Merge, "application/json"},
		{"commit-rename.json", base + "/commits/" + f.Rename, f.Rename, "application/json"},
		{"commit-binary.json", base + "/commits/" + f.Binary, f.Binary, "application/json"},
		{"commit-merge.json", base + "/commits/" + f.Merge, f.Merge, "application/json"},
		{"commit-trailers.json", base + "/commits/v1.0", f.Trailers, "application/json"},
		{"compare-rename.diff", base + "/compare/" + parentOfRename + "..." + f.Rename, f.Rename, "text/x-diff"},
		{"compare-binary.diff", base + "/compare/" + f.Rename + "...v1.0", f.Trailers, "text/x-diff"},
		{"compare-path.diff", base + "/compare/-...-?base=" + f.Root + "&head=refs/tags/v1.0&path=README.md", f.Trailers, "text/x-diff"},
		{"tree-root.json", base + "/tree/v1.0", f.Trailers, "application/json"},
		{"tree-path.json", base + "/tree/" + f.Tip + "?path=src", f.Tip, "application/json"},
		{"tree-recursive.json", base + "/tree/-?ref=refs/heads/topic/x&recursive=1&path=feat", f.Merge, "application/json"},
		{"blob-text.txt", base + "/blob/" + readme, readme, "text/plain; charset=utf-8"},
		{"blob-binary.bin", base + "/blob/" + logo, logo, "application/octet-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.get(c.path)
			if resp.status != 200 {
				t.Fatalf("%s: %d %s", c.path, resp.status, resp.body)
			}
			if got := resp.header.Get("Content-Type"); !strings.HasPrefix(got, c.contentType) {
				t.Errorf("content type %q", got)
			}
			if resp.header.Get(HeaderCommit) != c.commit || resp.header.Get("ETag") != `"1"` {
				t.Errorf("headers %v", resp.header)
			}
			golden(t, c.name, resp.body)
		})
	}
	t.Run("blob-big", func(t *testing.T) {
		sum := sha256.New()
		for _, rng := range []string{"bytes=0-52428799", "bytes=52428800-"} {
			resp := h.get(base+"/blob/"+f.BigBlob, "Range", rng)
			if resp.status != 206 || resp.header.Get(HeaderCommit) != f.BigBlob {
				t.Fatalf("%s: %d %v", rng, resp.status, resp.header)
			}
			sum.Write(resp.body)
		}
		if got := hex.EncodeToString(sum.Sum(nil)); got != f.BigSHA256 {
			t.Fatalf("digest %s, want %s", got, f.BigSHA256)
		}
	})
	t.Run("archive", func(t *testing.T) {
		resp := h.get(base + "/archive/v1.0.tar.gz")
		if resp.status != 200 || resp.header.Get("Content-Type") != "application/gzip" || resp.header.Get(HeaderCommit) != f.Trailers || resp.header.Get("Content-Disposition") != `attachment; filename="app-`+f.Trailers[:7]+`.tar.gz"` {
			t.Fatalf("%d %v", resp.status, resp.header)
		}
		// Entries in git's tree order under the prefix, no .git, and
		// the commit time as mtime.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.tar.gz"), resp.body, 0o644); err != nil {
			t.Fatal(err)
		}
		list, err := exec.CommandContext(context.Background(), "tar", "-tzf", filepath.Join(dir, "a.tar.gz")).Output()
		if err != nil {
			t.Fatal(err)
		}
		prefix := "app-" + f.Trailers[:7] + "/"
		want := []string{prefix, prefix + "README.md", prefix + "assets/", prefix + "assets/logo.bin", prefix + "src/", prefix + "src/b.txt"}
		if got := strings.Fields(string(list)); !slices.Equal(got, want) {
			t.Fatalf("entries %v, want %v", got, want)
		}
	})
}

// TestArchiveIsReproducible: the archive of one commit has the same
// digest on two nodes over one log and after a rebuild from the log.
func TestArchiveIsReproducible(t *testing.T) {
	f := loadFixture(t)
	a := newHarness(t)
	a.seed(f)
	b := newHarness(t, withStore(a.store))
	digest := func(h *harness) string {
		t.Helper()
		resp := h.get("/v1/repos/" + repoA + "/archive/" + f.Tip + ".tar.gz")
		if resp.status != 200 || len(resp.body) < 1024 {
			t.Fatalf("%d %d bytes", resp.status, len(resp.body))
		}
		sum := sha256.Sum256(resp.body)
		return hex.EncodeToString(sum[:])
	}
	first := digest(a)
	if second := digest(b); second != first {
		t.Fatalf("node b: %s, node a: %s", second, first)
	}
	a.cache.Evict(repoA)
	if rebuilt := digest(a); rebuilt != first {
		t.Fatalf("after a rebuild: %s, before: %s", rebuilt, first)
	}
}

// TestCompareTruncates: a 5 MiB change answers at most 1 MiB, cut at a
// file boundary, with Origo-Truncated, in under a second.
func TestCompareTruncates(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.seed(f)
	h.get("/v1/repos/" + repoA + "/refs") // the copy is current before the clock starts
	start := time.Now()
	resp := h.get("/v1/repos/" + repoA + "/compare/" + f.Tip + "..." + f.Large)
	elapsed := time.Since(start)
	if resp.status != 200 || resp.header.Get(HeaderTruncated) != "true" || len(resp.body) > MaxCompareBytes || len(resp.body) == 0 {
		t.Fatalf("%d %v %d bytes", resp.status, resp.header, len(resp.body))
	}
	if elapsed > time.Second {
		t.Errorf("took %s", elapsed)
	}
	full, err := exec.CommandContext(context.Background(), "git", "-C", f.Dir, "diff", "-M", "--no-color", f.Tip, f.Large).Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 5<<20 {
		t.Fatalf("the fixture's change is %d bytes", len(full))
	}
	if !bytes.HasPrefix(full, resp.body) || !bytes.HasPrefix(full[len(resp.body):], []byte("diff --git ")) {
		t.Fatal("the cut is not at a file boundary of the full diff")
	}
	if n := bytes.Count(resp.body, []byte("\ndiff --git ")) + 1; n < 2 {
		t.Fatalf("%d files in the cut", n)
	}
	// A small diff is whole.
	if resp := h.get("/v1/repos/" + repoA + "/compare/" + f.Root + "..." + f.Rename); resp.status != 200 || resp.header.Get(HeaderTruncated) != "" || resp.header.Get("Content-Length") != strconv.Itoa(len(resp.body)) {
		t.Fatalf("small diff: %d %v", resp.status, resp.header)
	}
	// A diff whose first file alone is over the bound is cut to nothing.
	if got := cutAtFileBoundary([]byte("diff --git a/x b/x\n" + strings.Repeat("+x\n", 10))); got != nil {
		t.Fatalf("one file: %q", got)
	}
	if got := cutAtFileBoundary([]byte("diff --git a/x b/x\n+x\ndiff --git a/y b/y\n+y")); string(got) != "diff --git a/x b/x\n+x\n" {
		t.Fatalf("two files: %q", got)
	}
	if got := cutAtFileBoundary([]byte("no header\n")); got != nil {
		t.Fatalf("no header: %q", got)
	}
}

// TestPagingIsExact walks commits seven at a time over more than 100
// commits with an interleaving merge and a 5 001 entry tree in pages,
// and sees every item once; a cursor outside the walk is 400.
func TestPagingIsExact(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.seed(f)
	base := "/v1/repos/" + repoA
	want := strings.Split(gittest.Run(t, f.Dir, nil, "rev-list", "main"), "\n")
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		resp := h.get(base + "/commits?limit=7&cursor=" + cursor)
		if resp.status != 200 {
			t.Fatalf("page %d: %d %s", pages, resp.status, resp.body)
		}
		var page struct {
			Commits    []commitJSON `json:"commits"`
			NextCursor *string      `json:"next_cursor"`
		}
		if err := json.Unmarshal(resp.body, &page); err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Commits {
			got = append(got, c.SHA)
		}
		if page.NextCursor == nil {
			if len(page.Commits) > 7 || pages < 14 {
				t.Fatalf("%d pages, last of %d", pages+1, len(page.Commits))
			}
			break
		}
		if len(page.Commits) != 7 || *page.NextCursor != page.Commits[6].SHA {
			t.Fatalf("page %d: %d commits, cursor %v", pages, len(page.Commits), *page.NextCursor)
		}
		cursor = *page.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Fatalf("walked %d commits, want %d; equal prefix %d", len(got), len(want), commonPrefix(got, want))
	}
	if resp := h.get(base + "/commits?cursor=" + strings.Repeat("0", 40)); resp.status != 400 || code(resp.json()) != contract.CodeInvalid || details(resp.json())["reason"] != "cursor: not a commit of this walk" {
		t.Fatalf("bad cursor: %d %s", resp.status, resp.body)
	}
	if resp := h.get(base + "/commits?cursor=" + f.Root); resp.status != 200 || len(resp.json()["commits"].([]any)) != 0 || resp.json()["next_cursor"] != nil {
		t.Fatalf("cursor at the end: %d %s", resp.status, resp.body)
	}

	// The tree: 5 000 entries, then one.
	var paths []string
	first := h.get(base + "/tree/main?path=many")
	if first.status != 200 {
		t.Fatalf("tree: %d %s", first.status, first.body)
	}
	var page struct {
		Entries    []treeEntry `json:"entries"`
		NextCursor *string     `json:"next_cursor"`
	}
	_ = json.Unmarshal(first.body, &page)
	if len(page.Entries) != TreePageSize || page.NextCursor == nil || *page.NextCursor != "many/f04999" {
		t.Fatalf("first page: %d entries, cursor %v", len(page.Entries), page.NextCursor)
	}
	for _, e := range page.Entries {
		paths = append(paths, e.Path)
	}
	second := h.get(base + "/tree/main?path=many&cursor=" + *page.NextCursor)
	_ = json.Unmarshal(second.body, &page)
	if second.status != 200 || len(page.Entries) != 1 || page.NextCursor != nil || page.Entries[0].Path != "many/f05000" || page.Entries[0].Type != "blob" || page.Entries[0].Mode != "100644" || page.Entries[0].Size == nil || *page.Entries[0].Size != 5 {
		t.Fatalf("second page: %d %+v %v", second.status, page.Entries, page.NextCursor)
	}
	paths = append(paths, page.Entries[0].Path)
	if len(slices.Compact(slices.Clone(paths))) != gittest.ManyEntries || !slices.IsSorted(paths) {
		t.Fatalf("%d paths", len(paths))
	}
	if resp := h.get(base + "/tree/main?path=many&cursor=many/nope"); resp.status != 400 || details(resp.json())["reason"] != "cursor: not a path of this listing" {
		t.Fatalf("bad tree cursor: %d %s", resp.status, resp.body)
	}
	// A directory entry has no size; a recursive listing has none.
	resp := h.get(base + "/tree/main")
	_ = json.Unmarshal(resp.body, &page)
	if i := slices.IndexFunc(page.Entries, func(e treeEntry) bool { return e.Path == "many" }); i < 0 || page.Entries[i].Type != "tree" || page.Entries[i].Size != nil || page.Entries[i].Mode != "040000" {
		t.Fatalf("root: %+v", page.Entries)
	}
	resp = h.get(base + "/tree/main?recursive=1&path=trunk")
	_ = json.Unmarshal(resp.body, &page)
	if resp.status != 200 || len(page.Entries) != 51 {
		t.Fatalf("recursive: %d %d entries", resp.status, len(page.Entries))
	}
	if resp := h.get(base + "/tree/main?recursive=2"); resp.status != 400 {
		t.Fatalf("recursive=2: %d", resp.status)
	}
}

func commonPrefix(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// TestCommitsOnAnEmptyRepository: a repository with no commit answers
// an empty page for the default HEAD and for any name, while a name
// that does not exist in a repository with history is 404.
func TestCommitsOnAnEmptyRepository(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app"}`)
	for _, q := range []string{"", "?ref=main", "?ref=refs/heads/nope&limit=3"} {
		resp := h.get("/v1/repos/" + repoA + "/commits" + q)
		if resp.status != 200 || string(bytes.TrimSpace(resp.body)) != `{"commits":[],"next_cursor":null}` || resp.header.Get("ETag") != `"0"` {
			t.Fatalf("%q: %d %s %v", q, resp.status, resp.body, resp.header)
		}
	}
	g := newHarness(t)
	g.seed(f)
	resp := g.get("/v1/repos/" + repoA + "/commits?ref=nope")
	if resp.status != 404 || code(resp.json()) != contract.CodeRefNotFound || details(resp.json())["ref"] != "nope" {
		t.Fatalf("%d %s", resp.status, resp.body)
	}
	// So is every other endpoint's unresolvable name, with the name as
	// the request gave it.
	for _, path := range []string{"/commits/nope", "/commits/-?ref=refs/heads/nope", "/tree/nope", "/blob/nope", "/blob/main", "/archive/nope.tar.gz", "/compare/main...nope", "/compare/nope...main"} {
		resp := g.get("/v1/repos/" + repoA + path)
		if resp.status != 404 || code(resp.json()) != contract.CodeRefNotFound || details(resp.json())["ref"] == nil {
			t.Errorf("%s: %d %s", path, resp.status, resp.body)
		}
	}
}

// TestTrailersKeepOrderAndDuplicates: two Signed-off-by and one
// Co-authored-by answer three pairs in that order.
func TestTrailersKeepOrderAndDuplicates(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.seed(f)
	resp := h.get("/v1/repos/" + repoA + "/commits/" + f.Trailers)
	var c commitJSON
	if err := json.Unmarshal(resp.body, &c); err != nil || resp.status != 200 {
		t.Fatalf("%d %s", resp.status, resp.body)
	}
	want := []trailer{{"Signed-off-by", "Alice <alice@example.com>"}, {"Signed-off-by", "Bob <bob@example.com>"}, {"Co-authored-by", "Carol <carol@example.com>"}}
	if !slices.Equal(c.Trailers, want) {
		t.Fatalf("trailers %v", c.Trailers)
	}
	if c.Message != "Sign the work\n\nThe body of the message.\n\nSigned-off-by: Alice <alice@example.com>\nSigned-off-by: Bob <bob@example.com>\nCo-authored-by: Carol <carol@example.com>\n" || c.Author.Name != "Origo Test" || c.Committer.Email != "test@example.com" || c.Author.At.IsZero() {
		t.Fatalf("commit %+v", c)
	}
	// A commit without trailers answers an empty list, never null.
	resp = h.get("/v1/repos/" + repoA + "/commits/" + f.Root)
	if !bytes.Contains(resp.body, []byte(`"trailers":[]`)) || !bytes.Contains(resp.body, []byte(`"parents":[]`)) {
		t.Fatalf("root: %s", resp.body)
	}
}

// TestPathGrammarRefusesOptions: a value that git would read as an
// option is refused before any subprocess starts, a full reference
// name resolves through ?ref= with the placeholder, and ?ref= beside a
// real segment is refused.
func TestPathGrammarRefusesOptions(t *testing.T) {
	f := loadFixture(t)
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()))
	h.seed(f)
	base := "/v1/repos/" + repoA
	h.get(base + "/refs")
	spy.reset()
	for path, reason := range map[string]string{
		"/commits/-x":                         "option",
		"/tree/-x":                            "option",
		"/blob/-x":                            "option",
		"/archive/-x.tar.gz":                  "option",
		"/compare/-x...main":                  "option",
		"/compare/main...-x":                  "option",
		"/commits?ref=--output=/tmp/x":        "option",
		"/tree/-?ref=--output=/tmp/x":         "option",
		"/compare/-...main?base=--output=/x":  "option",
		"/compare/main...-?head=--output=/x":  "option",
		"/commits?path=-":                     "option",
		"/compare/main...main?path=-":         "option",
		"/tree/main?path=-":                   "option",
		"/tree/main?cursor=-":                 "option",
		"/commits?cursor=-":                   "option",
		"/refs?prefix=--format=x":             "option",
		"/tree/main?ref=refs/heads/x":         "ref",
		"/commits/main?ref=x":                 "ref",
		"/blob/main?ref=x":                    "ref",
		"/archive/main.tar.gz?ref=x":          "ref",
		"/compare/main...main?base=x":         "ref",
		"/compare/main...main?head=x":         "ref",
		"/tree/-":                             "ref",
		"/compare/-...main":                   "ref",
		"/compare/main":                       "ref",
		"/compare/a...b...c":                  "ref",
		"/tree/a~1":                           "ref",
		"/tree/.x":                            "ref",
		"/tree/main?path=.git/x":              "path",
		"/commits?path=a//b":                  "path",
		"/compare/main...main?path=a/../b":    "path",
		"/commits?limit=0":                    "limit",
		"/commits?limit=201":                  "limit",
		"/commits?limit=x":                    "limit",
		"/commits?since=yesterday":            "since",
		"/commits?until=2026-13-01T00:00:00Z": "until",
		"/archive/main.zip":                   "archive",
	} {
		resp := h.get(base + path)
		if resp.status != 400 || code(resp.json()) != contract.CodeInvalid || !strings.HasPrefix(resp.reason(), reason+":") {
			t.Errorf("%s: %d %s", path, resp.status, resp.body)
		}
	}
	if calls := spy.calls(); calls[0] != "" {
		t.Fatalf("subprocesses started: %v", calls)
	}
	// Every refused path of the table is refused by the handler too,
	// with no subprocess.
	for _, c := range pathCases {
		if c.ok {
			continue
		}
		resp := h.get(base + "/tree/main?path=" + url.QueryEscape(c.path))
		if resp.status != 400 || !strings.HasPrefix(resp.reason(), "path:") {
			t.Errorf("path %q: %d %s", c.path, resp.status, resp.body)
		}
	}
	if calls := spy.calls(); calls[0] != "" {
		t.Fatalf("subprocesses started for a refused path: %v", calls)
	}
	// The placeholder with a full name resolves; the segment form of
	// the same branch cannot name it.
	resp := h.get(base + "/tree/-?ref=refs/heads/topic/x")
	if resp.status != 200 || resp.header.Get(HeaderCommit) != f.Merge {
		t.Fatalf("placeholder: %d %v %s", resp.status, resp.header, resp.body)
	}
	if !slices.ContainsFunc(spy.calls(), func(c string) bool { return strings.Contains(c, "--end-of-options refs/heads/topic/x^{}") }) {
		t.Fatalf("calls %v", spy.calls())
	}
	for _, path := range []string{"/commits?ref=refs/heads/topic/x&limit=1", "/commits/-?ref=refs/heads/topic/x", "/archive/-.tar.gz?ref=refs/heads/topic/x", "/compare/-...-?base=refs/heads/topic/x&head=refs/heads/main"} {
		if resp := h.get(base + path); resp.status != 200 {
			t.Errorf("%s: %d %s", path, resp.status, resp.body)
		}
	}
	// Every value that reaches git is behind --end-of-options or --.
	for _, c := range spy.calls() {
		if strings.Contains(c, "topic/x") && !strings.Contains(c, "--end-of-options ") {
			t.Errorf("a request value before --end-of-options: %s", c)
		}
	}
}

// TestReadDeadlineIsOperationTimeout: a compare whose subprocess is
// held past the budget answers 504 operation_timeout with the budget
// in details, and neither the subprocess nor its child is left.
func TestReadDeadlineIsOperationTimeout(t *testing.T) {
	f := loadFixture(t)
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()), withReadTimeout(time.Second))
	h.seed(f)
	h.get("/v1/repos/" + repoA + "/refs")
	spy.hold("diff")
	defer spy.release()
	start := time.Now()
	resp := h.get("/v1/repos/" + repoA + "/compare/" + f.Root + "..." + f.Rename)
	if resp.status != 504 || code(resp.json()) != contract.CodeOperationTimeout || details(resp.json())["operation"] != "compare" || details(resp.json())["budget_seconds"] != float64(1) {
		t.Fatalf("%d %s after %s", resp.status, resp.body, time.Since(start))
	}
	if resp.json()["error"].(map[string]any)["message"] != contract.Sentence(contract.CodeOperationTimeout) {
		t.Fatalf("message %s", resp.body)
	}
	pids := spy.heldPIDs()
	if len(pids) != 2 {
		t.Fatalf("held pids %v", pids)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for alive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d still running", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if DefaultReadTimeout != 30*time.Second || New(Options{Cache: h.cache, Guard: h.guard, Signer: h.signer}).readTimeout != DefaultReadTimeout {
		t.Fatal("the default budget is 30 seconds")
	}
	// The other operations name themselves.
	spy.release()
	spy.hold("rev-parse")
	for path, op := range map[string]string{"/refs?x=1": "", "/commits": "commits", "/commits/main": "commits", "/tree/main": "tree", "/blob/main": "blob", "/archive/main.tar.gz": "archive"} {
		if op == "" {
			continue
		}
		resp := h.get("/v1/repos/" + repoA + path)
		if resp.status != 504 || details(resp.json())["operation"] != op {
			t.Errorf("%s: %d %s", path, resp.status, resp.body)
		}
	}
	spy.release()
	spy.hold("for-each-ref")
	if resp := h.get("/v1/repos/" + repoA + "/refs"); resp.status != 504 || details(resp.json())["operation"] != "refs" {
		t.Errorf("refs: %d %s", resp.status, resp.body)
	}
}

// TestBlobRange: the 60 MiB blob is 413 without a Range and 206 with
// one of at most 50 MiB; every Range form is honoured on a small blob.
func TestBlobRange(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.seed(f)
	base := "/v1/repos/" + repoA + "/blob/"
	resp := h.get(base + f.BigBlob)
	if resp.status != 413 || code(resp.json()) != contract.CodeBlobTooLarge || details(resp.json())["size"] != float64(gittest.BigBlobSize) || details(resp.json())["max"] != float64(MaxBlobBytes) {
		t.Fatalf("whole: %d %s", resp.status, resp.body)
	}
	if resp := h.get(base+f.BigBlob, "Range", "bytes=0-52428800"); resp.status != 413 {
		t.Fatalf("a range over the bound: %d", resp.status)
	}
	resp = h.get(base+f.BigBlob, "Range", "bytes=0-1023")
	want := gittest.Bytes(gittest.BigBlobSize, 0x9e3779b97f4a7c15)
	if resp.status != 206 || resp.header.Get("Content-Range") != fmt.Sprintf("bytes 0-1023/%d", gittest.BigBlobSize) || resp.header.Get("Content-Length") != "1024" || !bytes.Equal(resp.body, want[:1024]) || resp.header.Get("Accept-Ranges") != "bytes" || resp.header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("range: %d %v %d bytes", resp.status, resp.header, len(resp.body))
	}
	if resp := h.get(base+f.BigBlob, "Range", "bytes=-16"); resp.status != 206 || !bytes.Equal(resp.body, want[len(want)-16:]) || resp.header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", len(want)-16, len(want)-1, len(want)) {
		t.Fatalf("suffix: %d %v", resp.status, resp.header)
	}
	if resp := h.get(base+f.BigBlob, "Range", fmt.Sprintf("bytes=%d-", len(want)-8)); resp.status != 206 || !bytes.Equal(resp.body, want[len(want)-8:]) {
		t.Fatalf("open end: %d", resp.status)
	}
	readme := gittest.Run(t, f.Dir, nil, "rev-parse", f.Trailers+":README.md")
	small := h.get(base + readme)
	if small.status != 200 || string(small.body) != "# Fixture\n\nSigned.\n" || small.header.Get("Content-Length") != "19" {
		t.Fatalf("small: %d %v %q", small.status, small.header, small.body)
	}
	if resp := h.get(base+readme, "Range", "bytes=2-8"); resp.status != 206 || string(resp.body) != "Fixture" {
		t.Fatalf("small range: %d %q", resp.status, resp.body)
	}
	if resp := h.get(base+readme, "Range", "bytes=11-1000"); resp.status != 206 || string(resp.body) != "Signed.\n" || resp.header.Get("Content-Range") != "bytes 11-18/19" {
		t.Fatalf("clamped range: %d %q", resp.status, resp.body)
	}
	if resp := h.get(base+readme, "Range", "bytes=100-"); resp.status != 416 || resp.header.Get("Content-Range") != "bytes */19" || code(resp.json()) != contract.CodeInvalid {
		t.Fatalf("past the end: %d %v %s", resp.status, resp.header, resp.body)
	}
	for _, rng := range []string{"bytes=0-1,3-4", "items=0-1", "bytes=5-2", "bytes=-", "bytes=x-1", "bytes=1-x", "bytes=-0"} {
		if resp := h.get(base+readme, "Range", rng); resp.status != 400 || !strings.HasPrefix(resp.reason(), "range:") {
			t.Errorf("%s: %d %s", rng, resp.status, resp.body)
		}
	}
	// A name that resolves to something other than a blob is not found.
	if resp := h.get(base + "v1.0"); resp.status != 404 || code(resp.json()) != contract.CodeRefNotFound {
		t.Fatalf("a tag as a blob: %d %s", resp.status, resp.body)
	}
}

// TestETagRevalidates: If-None-Match with the ETag is 304 and runs no
// git; a push changes the tag.
func TestETagRevalidates(t *testing.T) {
	f := loadFixture(t)
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()))
	h.seed(f)
	base := "/v1/repos/" + repoA
	first := h.get(base + "/commits?limit=1")
	if first.status != 200 || first.header.Get("ETag") != `"1"` {
		t.Fatalf("%d %v", first.status, first.header)
	}
	spy.reset()
	for _, inm := range []string{`"1"`, `W/"1"`, `"0", "1"`, "*"} {
		resp := h.get(base+"/commits?limit=1", "If-None-Match", inm)
		if resp.status != 304 || len(resp.body) != 0 || resp.header.Get("ETag") != `"1"` {
			t.Fatalf("%s: %d %q", inm, resp.status, resp.body)
		}
	}
	if resp := h.get(base+"/archive/main.tar.gz", "If-None-Match", `"1"`); resp.status != 304 {
		t.Fatalf("archive: %d", resp.status)
	}
	if calls := spy.calls(); calls[0] != "" {
		t.Fatalf("git ran on a revalidation: %v", calls)
	}
	if resp := h.get(base+"/commits?limit=1", "If-None-Match", `"0"`); resp.status != 200 {
		t.Fatalf("stale tag: %d", resp.status)
	}
	// Another push moves the sequence; the old tag no longer matches.
	ix, _, _ := h.log.Newest(context.Background(), repoA, 0, false)
	if _, err := h.log.Commit(context.Background(), repoA, ix, wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{Ref: "refs/heads/again", Old: wal.ZeroSHA, New: f.Root}}}, noCatchUp); err != nil {
		t.Fatal(err)
	}
	resp := h.get(base+"/commits?limit=1", "If-None-Match", `"1"`)
	if resp.status != 200 || resp.header.Get("ETag") != `"2"` {
		t.Fatalf("after a push: %d %v", resp.status, resp.header)
	}
	// GET /v1/repos/{id} serves pushed_at from the same index object.
	status, out := h.do("GET", base, "")
	if status != 200 || out["pushed_at"] == nil {
		t.Fatalf("pushed_at: %d %v", status, out)
	}
	if at, err := time.Parse(time.RFC3339, out["pushed_at"].(string)); err != nil || ix.PushedAt == nil || at.Before(*ix.PushedAt) {
		t.Fatalf("pushed_at %v, index %v", out["pushed_at"], ix.PushedAt)
	}
}

// TestReadRefusalsAndFailures covers the paths around the happy ones:
// a denied caller, an unknown and a deleted repository, the refs cap,
// a storage failure, and a subprocess that fails.
func TestReadRefusalsAndFailures(t *testing.T) {
	f := loadFixture(t)
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()))
	h.seed(f)
	base := "/v1/repos/" + repoA
	h.authz.Deny(authorizer.Rule{Subject: "eve"}, "eve is denied")
	h.as(auth.Principal{Subject: "eve"})
	for _, path := range []string{"/refs", "/commits", "/commits/main", "/compare/main...main", "/tree/main", "/blob/main", "/archive/main.tar.gz"} {
		if resp := h.get(base + path); resp.status != 403 || code(resp.json()) != contract.CodeForbidden {
			t.Errorf("eve %s: %d %s", path, resp.status, resp.body)
		}
	}
	h.as(auth.Principal{Subject: "alice"})
	for _, path := range []string{"/refs", "/commits", "/archive/main.tar.gz"} {
		if resp := h.get("/v1/repos/" + unknown + path); resp.status != 404 || code(resp.json()) != contract.CodeRepoNotFound {
			t.Errorf("unknown %s: %d %s", path, resp.status, resp.body)
		}
	}
	// Many references: the cap and the header.
	ix, _, _ := h.log.Newest(context.Background(), repoA, 0, false)
	e := wal.Entry{Kind: wal.KindPush}
	for i := range MaxRefs + 5 {
		e.Refs = append(e.Refs, wal.RefUpdate{Ref: fmt.Sprintf("refs/tags/t%05d", i), Old: wal.ZeroSHA, New: f.Root})
	}
	if _, err := h.log.Commit(context.Background(), repoA, ix, e, noCatchUp); err != nil {
		t.Fatal(err)
	}
	resp := h.get(base + "/refs?prefix=refs/tags/t")
	var refs []map[string]any
	_ = json.Unmarshal(resp.body, &refs)
	if resp.status != 200 || resp.header.Get(HeaderTruncated) != "true" || len(refs) != MaxRefs || refs[0]["name"] != "refs/tags/t00000" || refs[0]["peeled"] != nil || refs[MaxRefs-1]["name"] != "refs/tags/t09999" {
		t.Fatalf("cap: %d %v %d refs", resp.status, resp.header, len(refs))
	}
	// A prefix is a prefix, not a directory: the tags under t1 are the
	// five past the cap, and refs/heads/ is under it.
	resp = h.get(base + "/refs?prefix=refs/tags/t1")
	_ = json.Unmarshal(resp.body, &refs)
	if resp.status != 200 || resp.header.Get(HeaderTruncated) != "" || len(refs) != 5 || refs[0]["name"] != "refs/tags/t10000" {
		t.Fatalf("prefix t1: %d %v %d refs", resp.status, resp.header, len(refs))
	}
	resp = h.get(base + "/refs?prefix=refs/heads/")
	_ = json.Unmarshal(resp.body, &refs)
	if resp.status != 200 || resp.header.Get(HeaderTruncated) != "" || len(refs) != 6 {
		t.Fatalf("under the cap: %d %v %d refs", resp.status, resp.header, len(refs))
	}
	if resp := h.get(base + "/refs?prefix=nope"); resp.status != 200 || string(bytes.TrimSpace(resp.body)) != "[]" {
		t.Fatalf("no match: %d %s", resp.status, resp.body)
	}
	// A subprocess that fails for a reason other than the deadline is
	// 503, logged for the developer.
	readme := gittest.Run(t, f.Dir, nil, "rev-parse", f.Trailers+":README.md")
	for path, sub := range map[string]string{"/refs": "for-each-ref", "/commits": "rev-list", "/commits/main": "show", "/compare/main...main": "diff", "/tree/main": "ls-tree", "/blob/" + readme: "cat-file", "/archive/main.tar.gz": "archive"} {
		spy.failOn(sub)
		if resp := h.get(base + path); resp.status != 503 || code(resp.json()) != contract.CodeStorageUnavailable {
			t.Errorf("failing %s: %d %s", sub, resp.status, resp.body)
		}
		spy.release()
	}
	// The cache's own git failing makes the open fail the same way.
	failing := t.TempDir()
	if err := os.WriteFile(filepath.Join(failing, "git"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := newHarness(t, withGit(filepath.Join(failing, "git")), withStore(h.store))
	if resp := broken.get(base + "/refs"); resp.status != 503 || code(resp.json()) != contract.CodeStorageUnavailable {
		t.Errorf("broken git: %d %s", resp.status, resp.body)
	}
	// Storage failing under the currency check.
	h.store.SetFault(func(op, key string) error { return fmt.Errorf("storage down") })
	if resp := h.get(base + "/refs"); resp.status != 503 || code(resp.json()) != contract.CodeStorageUnavailable {
		t.Fatalf("storage: %d %s", resp.status, resp.body)
	}
	h.store.SetFault(nil)
	// A deleted repository is 404 on every read.
	h.do("DELETE", base, "")
	if resp := h.get(base + "/refs"); resp.status != 404 || code(resp.json()) != contract.CodeRepoNotFound {
		t.Fatalf("deleted: %d %s", resp.status, resp.body)
	}
}

// TestCommitScannerAndTreeParsing covers the record parsers on inputs
// git does not produce.
func TestCommitScannerAndTreeParsing(t *testing.T) {
	sc := newCommitScanner(strings.NewReader("a\x00b\x00"))
	if _, ok, err := sc.next(); ok || err == nil {
		t.Fatalf("short record: %v %v", ok, err)
	}
	bad := strings.Repeat("x\x00", 4) + "not a date\x00" + strings.Repeat("y\x00", 5)
	if _, ok, err := newCommitScanner(strings.NewReader(bad)).next(); ok || err == nil {
		t.Fatalf("bad date: %v %v", ok, err)
	}
	if _, ok, err := newCommitScanner(strings.NewReader("")).next(); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	if e, ok := parseTreeEntry("100644 blob abc -\tx"); !ok || e.Size != nil || e.Path != "x" {
		t.Fatalf("no size: %+v %v", e, ok)
	}
	if _, ok := parseTreeEntry("garbage"); ok {
		t.Fatal("garbage parsed")
	}
	if _, ok := parseTreeEntry("a b\tc"); ok {
		t.Fatal("short record parsed")
	}
	if !etagMatches(`"1"`, `"1"`) || etagMatches("", `"1"`) || etagMatches(`"2"`, `"1"`) {
		t.Fatal("etagMatches")
	}
	rec := &readError{status: 400, code: contract.CodeInvalid, details: map[string]any{"reason": "x"}}
	if rec.Error() == "" {
		t.Fatal("Error")
	}
}

// TestReadWithoutASubprocessSlotIsRateLimited is spec 012's semaphore
// on the read API: a request that finds no slot inside the wait is 429
// rate_limited with details.limit "subprocesses", not the 504 a spent
// budget answers, and it starts no subprocess. One slot covers the
// whole read, so the request that got one runs both its subprocesses.
func TestReadWithoutASubprocessSlotIsRateLimited(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t, withLimits(limits.Options{MaxGitProcs: 1, SlotWait: 20 * time.Millisecond}))
	h.seed(f)
	held, ok := h.limits.Slots().Acquire(context.Background(), time.Second)
	if !ok {
		t.Fatal("the test could not take the one slot")
	}
	resp := h.get("/v1/repos/" + repoA + "/refs")
	if resp.status != http.StatusTooManyRequests || code(resp.json()) != contract.CodeRateLimited {
		t.Fatalf("%d %s", resp.status, resp.body)
	}
	if d := details(resp.json()); d["limit"] != "subprocesses" || d["retry_after"] != float64(1) {
		t.Errorf("details: %+v", d)
	}
	if resp.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	held()
	if resp := h.get("/v1/repos/" + repoA + "/refs"); resp.status != http.StatusOK {
		t.Fatalf("after the slot was freed: %d %s", resp.status, resp.body)
	}
	// The slot is given back when the read ends, whatever it answered.
	if held := h.limits.Slots().Held(); held != 0 {
		t.Fatalf("%d slots held after the read", held)
	}
}
