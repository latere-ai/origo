// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/origocli"
)

// TestListWalksAndPagesTheWholeTree is the pipeline the whole shape is argued
// on: `origo ls -r -n 0 | grep -i handler` reaches a file in a tree that pages
// several times, and nothing but data reaches stdout.
func TestListWalksAndPagesTheWholeTree(t *testing.T) {
	f := newFake()
	f.treePage = 2 // three pages, so the cursor is exercised
	for i := range 5 {
		f.entries = append(f.entries, entryJSON(fmt.Sprintf("internal/api/file%d.go", i), "100644", "blob", int64(100+i)))
	}
	f.entries = append(f.entries, entryJSON("internal/api/read_handler.go", "100644", "blob", 4096))
	env := envFor(f.start(t))

	got := run(t, env, "ls", "-r", "-n", "0")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if n := len(lines(got.stdout)); n != 6 {
		t.Fatalf("%d entries, want 6:\n%s", n, got.stdout)
	}
	if !strings.Contains(got.stdout, "internal/api/read_handler.go") {
		t.Fatalf("the walk did not reach the last page:\n%s", got.stdout)
	}
	if got.stderr != "" {
		t.Fatalf("a complete answer wrote to stderr: %q", got.stderr)
	}
	// The grep the skill documents finds it, and finds nothing else.
	var hits []string
	for _, l := range lines(got.stdout) {
		if strings.Contains(strings.ToLower(l), "handler") {
			hits = append(hits, l)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("grep -i handler matched %v", hits)
	}

	// The default is 200, and a cut answer names the flag on stderr.
	f.treePage = 5000
	f.entries = nil
	for i := range 250 {
		f.entries = append(f.entries, entryJSON(fmt.Sprintf("d/f%03d.go", i), "100644", "blob", 1))
	}
	got = run(t, env, "ls", "-r")
	if n := len(lines(got.stdout)); n != 200 {
		t.Fatalf("%d entries, want the default 200", n)
	}
	if !strings.Contains(got.stderr, "[truncated:") || !strings.Contains(got.stderr, "-n 0") {
		t.Fatalf("the cut did not name its flag: %q", got.stderr)
	}
	if strings.Contains(got.stdout, "[truncated") {
		t.Fatal("a truncation line reached stdout, where a grep would read it")
	}
}

func TestListMarksTreesExecutablesAndLinks(t *testing.T) {
	f := newFake()
	f.entries = []map[string]any{
		entryJSON("internal", "040000", "tree", 0),
		entryJSON("run.sh", "100755", "blob", 12),
		entryJSON("link", "120000", "blob", 3),
		entryJSON("go.mod", "100644", "blob", 4096),
	}
	got := run(t, envFor(f.start(t)), "ls")
	want := []string{"       -  internal/", "      12  run.sh*", "       3  link@", "    4096  go.mod"}
	if strings.Join(lines(got.stdout), "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", got.stdout, strings.Join(want, "\n"))
	}
}

// TestLogPagesAndHoldsTheColumns: 200 commits page past the route's cap, the
// columns hold, and the default is 20 with the flag named.
func TestLogPagesAndHoldsTheColumns(t *testing.T) {
	f := newFake()
	f.commitPage = 50
	long := strings.Repeat("a long subject that runs well past the column ", 4)
	for i := range 250 {
		name := "Ada Lovelace"
		if i == 137 {
			name = "Grace Hopper"
		}
		f.commits = append(f.commits, commitJSON(fmt.Sprintf("%040d", i), name, "2026-09-10T12:00:00Z", long+"\n\nbody"))
	}
	env := envFor(f.start(t))

	got := run(t, env, "log", "-n", "200")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	out := lines(got.stdout)
	if len(out) != 200 {
		t.Fatalf("%d commits, want 200", len(out))
	}
	for _, l := range out {
		fields := strings.SplitN(l, " ", 3)
		if len(fields) < 3 || len(fields[0]) != 7 || len(fields[1]) != 10 {
			t.Fatalf("the columns did not hold: %q", l)
		}
		if len(l) > 7+1+10+1+len("Grace Hopper")+1+72 {
			t.Fatalf("a subject ran past its column: %q", l)
		}
	}
	// The author filter the tool shape could not offer is one grep.
	var hits []string
	for _, l := range out {
		if strings.Contains(l, "Grace Hopper") {
			hits = append(hits, l)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("grep by author matched %d lines", len(hits))
	}

	got = run(t, env, "log")
	if n := len(lines(got.stdout)); n != 20 {
		t.Fatalf("%d commits, want the default 20", n)
	}
	if !strings.Contains(got.stderr, "[truncated:") || !strings.Contains(got.stderr, "-n") {
		t.Fatalf("the cut did not name its flag: %q", got.stderr)
	}
}

// TestCatWindowsAndRefusesBinary: two windows are the file's lines exactly,
// with no overlap and no gap, the header is on stderr, and a binary file is
// named rather than printed.
func TestCatWindowsAndRefusesBinary(t *testing.T) {
	f := newFake()
	var b strings.Builder
	for i := range 3120 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	text := b.String()
	f.entries = []map[string]any{entryJSON("internal/api/read.go", "100644", "blob", int64(len(text)))}
	f.blobs["sha-internal/api/read.go"] = []byte(text)
	env := envFor(f.start(t))

	first := run(t, env, "cat", "-max-bytes", "0", "internal/api/read.go")
	if first.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", first.code, first.stderr)
	}
	if n := len(lines(first.stdout)); n != 800 {
		t.Fatalf("%d lines, want the default 800", n)
	}
	if !strings.Contains(first.stderr, "3120 lines") {
		t.Fatalf("the header is not on stderr: %q", first.stderr)
	}
	if !strings.Contains(first.stderr, "-offset 800") {
		t.Fatalf("the cut did not name -offset 800: %q", first.stderr)
	}
	second := run(t, env, "cat", "-max-bytes", "0", "-offset", "800", "internal/api/read.go")
	both := append(lines(first.stdout), lines(second.stdout)...)
	if len(both) != 1600 {
		t.Fatalf("%d lines across the two windows, want 1600", len(both))
	}
	for i, l := range both {
		if l != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d is %q; the windows overlap or leave a gap", i, l)
		}
	}
	// The header never reaches stdout, so `origo cat x > x` writes the file.
	if strings.Contains(first.stdout, "@") && strings.Contains(first.stdout, "bytes") {
		t.Fatal("the header reached stdout")
	}

	// A file with a NUL in its first 8 KiB is named, not printed.
	binary := append([]byte("ELF\x00\x01\x02"), []byte(strings.Repeat("\x7f", 64))...)
	f.entries = append(f.entries, entryJSON("a.out", "100755", "blob", int64(len(binary))))
	f.blobs["sha-a.out"] = binary
	got := run(t, env, "cat", "a.out")
	if got.stdout != "" {
		t.Fatalf("binary bytes reached stdout: %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "binary") || !strings.Contains(got.stderr, "sha-a.o") {
		t.Fatalf("the refusal did not name the object: %q", got.stderr)
	}
}

// TestCatHoldsTheByteCapExceptAcrossOneLine: the cap bounds a window of
// lines, and a line is atomic, so a single line longer than the cap is
// printed whole and the answer says why.
func TestCatHoldsTheByteCapExceptAcrossOneLine(t *testing.T) {
	f := newFake()
	// Many ordinary lines: the cap bounds the window.
	many := strings.Repeat("a line of text\n", 4000)
	f.entries = []map[string]any{entryJSON("many.txt", "100644", "blob", int64(len(many)))}
	f.blobs["sha-many.txt"] = []byte(many)
	// One enormous line: the cap yields to it and says so.
	one := strings.Repeat("x", 100000) + "\n"
	f.entries = append(f.entries, entryJSON("one.txt", "100644", "blob", int64(len(one))))
	f.blobs["sha-one.txt"] = []byte(one)
	env := envFor(f.start(t))

	got := run(t, env, "cat", "-max-bytes", "1024", "many.txt")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(got.stdout) > 1024 {
		t.Fatalf("%d bytes printed, want at most the cap", len(got.stdout))
	}
	if !strings.Contains(got.stderr, "-offset") {
		t.Fatalf("the cut did not name -offset: %q", got.stderr)
	}

	got = run(t, env, "cat", "-max-bytes", "1024", "one.txt")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(got.stdout) != len(one) {
		t.Fatalf("%d bytes printed, want the whole line", len(got.stdout))
	}
	if !strings.Contains(got.stderr, "longer than -max-bytes") {
		t.Fatalf("the answer did not say why the cap was exceeded: %q", got.stderr)
	}
}

// TestCatWindowsAtTheDefaultCap is the criterion under the flags a caller
// actually uses. The byte cap bounds what is printed and not what is
// fetched, so -offset reaches past it and the line count is the file's.
func TestCatWindowsAtTheDefaultCap(t *testing.T) {
	f := newFake()
	var b strings.Builder
	for i := range 3120 {
		fmt.Fprintf(&b, "line %d is padded so the file is far larger than the byte cap\n", i)
	}
	text := b.String()
	if len(text) < 64*1024 {
		t.Fatalf("the fixture is %d bytes; it must exceed the 32 KiB default", len(text))
	}
	f.entries = []map[string]any{entryJSON("big.go", "100644", "blob", int64(len(text)))}
	f.blobs["sha-big.go"] = []byte(text)
	env := envFor(f.start(t))

	// Walk the whole file at the default cap, following each -offset the
	// answer names, and rebuild it exactly.
	var got []string
	offset := 0
	for range 20 {
		out := run(t, env, "cat", "-offset", strconv.Itoa(offset), "big.go")
		if out.code != origocli.CodeOK {
			t.Fatalf("offset %d exited %d: %s", offset, out.code, out.stderr)
		}
		if !strings.Contains(out.stderr, "3120 lines") {
			t.Fatalf("the line count is of a fragment, not of the file: %q", out.stderr)
		}
		got = append(got, lines(out.stdout)...)
		if !strings.Contains(out.stderr, "add -offset ") {
			break
		}
		next := strings.TrimSpace(strings.Split(strings.SplitN(out.stderr, "add -offset ", 2)[1], "]")[0])
		n, err := strconv.Atoi(next)
		if err != nil || n <= offset {
			t.Fatalf("the answer named -offset %q after %d", next, offset)
		}
		offset = n
	}
	if len(got) != 3120 {
		t.Fatalf("the windows rebuilt %d lines of 3120", len(got))
	}
	for i, l := range got {
		if l != fmt.Sprintf("line %d is padded so the file is far larger than the byte cap", i) {
			t.Fatalf("line %d is %q; the windows overlap or leave a gap", i, l)
		}
	}
}

// TestDiffIsStatFirstAndHonestAboutTheCut is the largest byte saving in the
// set, and the honesty rule beside it.
func TestDiffIsStatFirstAndHonestAboutTheCut(t *testing.T) {
	f := newFake()
	var d strings.Builder
	var names []string
	for i := range 12 {
		name := fmt.Sprintf("pkg/file%02d.go", i)
		names = append(names, name)
		fmt.Fprintf(&d, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@\n+added\n+added\n-gone\n", name, name, name, name)
	}
	f.diff = d.String()
	env := envFor(f.start(t))

	got := run(t, env, "diff", "main", "topic")
	out := lines(got.stdout)
	if len(out) != 13 {
		t.Fatalf("%d lines, want 12 stat lines and a totals line:\n%s", len(out), got.stdout)
	}
	if !strings.HasPrefix(out[0], "+2 -1  pkg/file00.go") {
		t.Fatalf("stat line: %q", out[0])
	}
	if out[12] != "12 files  +24 -12" {
		t.Fatalf("totals: %q", out[12])
	}
	if strings.Contains(got.stdout, "@@") {
		t.Fatal("the patch was printed without -p")
	}
	if !strings.Contains(got.stderr, "add -p") {
		t.Fatalf("stat-first did not name the flag: %q", got.stderr)
	}

	// -path narrows the fetch itself, one call per path.
	before := len(f.seen())
	got = run(t, env, "diff", "-p", "-path", names[0]+","+names[1], "main", "topic")
	after := f.seen()[before:]
	compares := 0
	for _, c := range after {
		if strings.Contains(c, "/compare/") {
			compares++
		}
	}
	if compares != 2 {
		t.Fatalf("%d compare calls, want one per path", compares)
	}
	if !strings.Contains(got.stdout, names[0]) || !strings.Contains(got.stdout, names[1]) || strings.Contains(got.stdout, names[2]) {
		t.Fatalf("the patch is not the two named files:\n%s", got.stdout)
	}

	// Where Origo cut the comparison, the answer says Origo cut it and names
	// the last file it saw, and is not presented as complete.
	f.compareTruncated = true
	got = run(t, env, "diff", "main", "topic")
	if !strings.Contains(got.stderr, "cut the comparison") || !strings.Contains(got.stderr, names[11]) {
		t.Fatalf("the cut was not reported honestly: %q", got.stderr)
	}
}

func TestShowIsStatFirstAndHandlesARootCommitAndAMerge(t *testing.T) {
	f := newFake()
	f.diff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@\n+one\n"
	root := commitJSON("1111111111111111111111111111111111111111", "Ada", "2026-09-10T12:00:00Z", "the first commit")
	root["parents"] = []string{}
	root["stats"] = map[string]any{"files": 1, "additions": 1, "deletions": 0}
	root["trailers"] = []any{map[string]any{"key": "Origo-Subject", "value": "ada"}}
	merge := commitJSON("2222222222222222222222222222222222222222", "Ada", "2026-09-10T12:00:00Z", "merge topic")
	merge["parents"] = []string{"3333333333333333333333333333333333333333", "4444444444444444444444444444444444444444"}
	merge["stats"] = map[string]any{"files": 1, "additions": 1, "deletions": 0}
	f.commits = []map[string]any{root, merge}
	env := envFor(f.start(t))

	got := run(t, env, "show", root["sha"].(string))
	if !strings.Contains(got.stdout, "commit    "+root["sha"].(string)) {
		t.Fatalf("the whole id is not printed:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "    the first commit") {
		t.Fatalf("the message is not indented:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "Origo-Subject: ada") {
		t.Fatalf("the trailers are missing:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "1 file  +1 -0") {
		t.Fatalf("the totals are missing:\n%s", got.stdout)
	}
	if !strings.Contains(got.stderr, "root commit has no comparison") {
		t.Fatalf("a root commit did not say why: %q", got.stderr)
	}
	if strings.Contains(got.stdout, "a.go") {
		t.Fatal("a root commit was compared against something")
	}

	got = run(t, env, "show", merge["sha"].(string))
	if !strings.Contains(got.stderr, "first parent") || !strings.Contains(got.stderr, "3333333") {
		t.Fatalf("a merge did not name its base: %q", got.stderr)
	}
	if !strings.Contains(got.stdout, "+1 -0  a.go") {
		t.Fatalf("the per-file line is missing:\n%s", got.stdout)
	}
}

func TestReposPagesAndSaysWhenThereIsNoDirectory(t *testing.T) {
	f := newFake()
	env := envFor(f.start(t))
	got := run(t, env, "repos")
	want := "id-1  acme/web\nid-2  acme/api\n"
	if got.stdout != want {
		t.Fatalf("got %q, want %q", got.stdout, want)
	}
	if got.stderr != "" {
		t.Fatalf("stderr: %q", got.stderr)
	}

	got = run(t, env, "repos", "-n", "1")
	if got.stdout != "id-1  acme/web\n" {
		t.Fatalf("-n did not bound the answer: %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "-n 0") {
		t.Fatalf("the cut did not name its flag: %q", got.stderr)
	}

	f.directoryCode = contract.CodeDirectoryUnsupported
	got = run(t, env, "repos")
	if got.code != origocli.CodeRefused {
		t.Fatalf("exit %d", got.code)
	}
	if !strings.Contains(got.stderr, "directory_unsupported") || !strings.Contains(got.stderr, "-repo") {
		t.Fatalf("the refusal did not say what to do instead: %q", got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("a refusal wrote to stdout: %q", got.stdout)
	}
}

func TestInfoIsOneCall(t *testing.T) {
	f := newFake()
	env := envFor(f.start(t))
	got := run(t, env, "info")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("%d calls, want 1: %v", n, f.seen())
	}
	for _, want := range []string{"repo     id-1", "name     acme/web", "branch   main", "head     aaaaaaaa", "size     4096", "pushed   2026-09-10T11:00:00Z"} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("%q missing from:\n%s", want, got.stdout)
		}
	}
	// The whole head, because it is what -expect takes.
	if !strings.Contains(got.stdout, "head     aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("the head was shortened:\n%s", got.stdout)
	}
}

func TestRefsSelectsAndReportsTheWall(t *testing.T) {
	f := newFake()
	f.refs = []map[string]any{
		{"name": "refs/heads/main", "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "peeled": nil},
		{"name": "refs/heads/feature/x", "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "peeled": nil},
	}
	env := envFor(f.start(t))
	got := run(t, env, "refs")
	if got.stdout != "refs/heads/main aaaaaaa\nrefs/heads/feature/x bbbbbbb\n" {
		t.Fatalf("got %q", got.stdout)
	}
	for _, args := range [][]string{{"refs", "-tags"}, {"refs", "-all"}, {"refs", "-prefix", "refs/tags/v1"}} {
		run(t, env, args...)
	}
	seen := strings.Join(f.seen(), "\n")
	for _, want := range []string{"prefix=refs%2Fheads%2F", "prefix=refs%2Ftags%2F", "prefix=refs%2F", "prefix=refs%2Ftags%2Fv1"} {
		if !strings.Contains(seen, want) {
			t.Fatalf("%q missing from:\n%s", want, seen)
		}
	}

	f.refsTruncated = true
	got = run(t, env, "refs")
	if !strings.Contains(got.stderr, "cannot be asked for") {
		t.Fatalf("the wall was not reported as a wall: %q", got.stderr)
	}
	if strings.Contains(got.stderr, "call refs again") {
		t.Fatal("the wall promised a continuation that does not exist")
	}
}

// TestEveryTruncationNamesItsFlagOnStderr is the structural half of rule 2: a
// bracketed line never reaches stdout, on any command.
func TestEveryTruncationNamesItsFlagOnStderr(t *testing.T) {
	f := newFake()
	f.treePage = 5000
	f.commitPage = 200
	for i := range 300 {
		f.entries = append(f.entries, entryJSON(fmt.Sprintf("d/f%03d.go", i), "100644", "blob", 1))
		f.commits = append(f.commits, commitJSON(fmt.Sprintf("%040d", i), "Ada", "2026-09-10T12:00:00Z", "a change"))
		f.refs = append(f.refs, map[string]any{"name": fmt.Sprintf("refs/heads/b%03d", i), "sha": fmt.Sprintf("%040d", i), "peeled": nil})
	}
	text := strings.Repeat("a line of text\n", 4000)
	f.entries = append(f.entries, entryJSON("big.txt", "100644", "blob", int64(len(text))))
	f.blobs["sha-big.txt"] = []byte(text)
	f.diff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@\n" + strings.Repeat("+a line\n", 20000)
	env := envFor(f.start(t))

	cut := [][]string{
		{"repos", "-n", "1"},
		{"refs"},
		{"ls", "-r"},
		{"cat", "big.txt"},
		{"log"},
		{"diff", "-p", "main", "topic"},
	}
	for _, args := range cut {
		got := run(t, env, args...)
		if !strings.Contains(got.stderr, "[truncated:") {
			t.Fatalf("%v: no truncation line on stderr: %q", args, got.stderr)
		}
		if strings.Contains(got.stdout, "[truncated") || strings.Contains(got.stdout, "[stale") {
			t.Fatalf("%v: a bracketed line reached stdout", args)
		}
	}
	whole := [][]string{
		{"repos"},
		{"info"},
		{"refs", "-n", "0"},
		{"ls", "-r", "-n", "0"},
		{"cat", "-n", "0", "-max-bytes", "0", "big.txt"},
		{"log", "-n", "0"},
	}
	for _, args := range whole {
		got := run(t, env, args...)
		if strings.Contains(got.stderr, "[truncated:") {
			t.Fatalf("%v: a complete answer claimed a truncation: %q", args, got.stderr)
		}
	}
}

func TestAStaleAnswerSaysSoOnceOnStderr(t *testing.T) {
	f := newFake()
	f.stale = "37"
	f.treePage = 2
	for i := range 6 {
		f.entries = append(f.entries, entryJSON(fmt.Sprintf("d/f%d.go", i), "100644", "blob", 1))
	}
	got := run(t, envFor(f.start(t)), "ls", "-r", "-n", "0")
	if strings.Count(got.stderr, "[stale:") != 1 {
		t.Fatalf("the stale line was written %d times: %q", strings.Count(got.stderr, "[stale:"), got.stderr)
	}
	if !strings.Contains(got.stderr, "37 seconds") {
		t.Fatalf("stale line: %q", got.stderr)
	}
	if strings.Contains(got.stdout, "stale") {
		t.Fatal("the stale line reached stdout")
	}
}

// TestJSONIsTheContractsOwnShape: --json prints the route's own envelope, and
// a paged answer is one merged object in that same envelope.
func TestJSONIsTheContractsOwnShape(t *testing.T) {
	f := newFake()
	f.treePage = 2
	f.commitPage = 2
	for i := range 5 {
		f.entries = append(f.entries, entryJSON(fmt.Sprintf("d/f%d.go", i), "100644", "blob", 1))
		f.commits = append(f.commits, commitJSON(fmt.Sprintf("%040d", i), "Ada", "2026-09-10T12:00:00Z", "a change"))
	}
	f.refs = []map[string]any{{"name": "refs/heads/main", "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "peeled": nil}}
	env := envFor(f.start(t))

	// The pipeline the skill documents.
	got := run(t, env, "log", "-n", "1", "--json")
	var page struct {
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &page); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, got.stdout)
	}
	if len(page.Commits) != 1 || page.Commits[0].SHA != f.commits[0]["sha"] {
		t.Fatalf("jq -r '.commits[0].sha' would not read the head: %s", got.stdout)
	}

	// A paged answer is one object in the same envelope, not a stream.
	got = run(t, env, "log", "-n", "0", "--json")
	if n := len(lines(got.stdout)); n != 1 {
		t.Fatalf("a paged --json wrote %d lines, want one merged object", n)
	}
	var merged struct {
		Commits []json.RawMessage `json:"commits"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &merged); err != nil || len(merged.Commits) != 5 {
		t.Fatalf("the pages did not merge: %v\n%s", err, got.stdout)
	}

	got = run(t, env, "ls", "-r", "-n", "0", "--json")
	var tree struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &tree); err != nil || len(tree.Entries) != 5 {
		t.Fatalf("ls --json: %v\n%s", err, got.stdout)
	}

	// refs is a bare array on the wire, so it is a bare array here.
	got = run(t, env, "refs", "--json")
	var refs []json.RawMessage
	if err := json.Unmarshal([]byte(got.stdout), &refs); err != nil || len(refs) != 1 {
		t.Fatalf("refs --json: %v\n%s", err, got.stdout)
	}

	// The repository representation is the contract's own object.
	got = run(t, env, "info", "--json")
	var repo map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &repo); err != nil {
		t.Fatalf("info --json: %v", err)
	}
	for _, key := range []string{"id", "owner", "slug", "default_branch", "head", "size_bytes", "updated_at", "pushed_at"} {
		if _, ok := repo[key]; !ok {
			t.Fatalf("info --json dropped %q: %s", key, got.stdout)
		}
	}

	got = run(t, env, "repos", "--json")
	var dir struct {
		Repos []json.RawMessage `json:"repos"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &dir); err != nil || len(dir.Repos) != 2 {
		t.Fatalf("repos --json: %v\n%s", err, got.stdout)
	}
}

// TestNameResolvesOnceAndTheIdIsUsedAsItStands
func TestNameResolvesOnceAndTheIdIsUsedAsItStands(t *testing.T) {
	f := newFake()
	env := envFor(f.start(t))
	env["ORIGO_REPO"] = "acme/web"
	f.commits = []map[string]any{commitJSON("1111111111111111111111111111111111111111", "Ada", "2026-09-10T12:00:00Z", "a change")}
	got := run(t, env, "log")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	resolves := 0
	for _, c := range f.seen() {
		if strings.Contains(c, "owner=acme") {
			resolves++
		}
	}
	if resolves != 1 {
		t.Fatalf("%d name resolutions, want one: %v", resolves, f.seen())
	}
	if !strings.Contains(strings.Join(f.seen(), "\n"), "/v1/repos/id-1/commits") {
		t.Fatalf("the resolved id was not used: %v", f.seen())
	}
	if got := run(t, env, "log", "-repo", "acme/nothing"); got.code != origocli.CodeRefused {
		t.Fatalf("an unknown name exited %d", got.code)
	}
	if got := run(t, env, "log", "-repo", "a/b/c"); got.code != origocli.CodeMisusage {
		t.Fatalf("a malformed name exited %d", got.code)
	}
}

// TestTokenIsNeverWritten mirrors spec 014's TestSourceTokenIsNeverLogged.
func TestTokenIsNeverWritten(t *testing.T) {
	f := newFake()
	f.diff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@\n+one\n"
	f.commits = []map[string]any{commitJSON("1111111111111111111111111111111111111111", "Ada", "2026-09-10T12:00:00Z", "a change")}
	f.entries = []map[string]any{entryJSON("a.go", "100644", "blob", 4)}
	f.blobs["sha-a.go"] = []byte("one\n")
	f.refs = []map[string]any{{"name": "refs/heads/main", "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "peeled": nil}}
	env := envFor(f.start(t))
	const token = "a-very-distinctive-bearer"

	dir := t.TempDir()
	t.Chdir(dir)
	if err := writeFile(dir+"/a.go", "two\n"); err != nil {
		t.Fatal(err)
	}
	runs := [][]string{
		{"repos"}, {"info"}, {"refs"}, {"ls"}, {"cat", "a.go"}, {"log"},
		{"show", "1111111111111111111111111111111111111111"}, {"diff", "main", "topic"},
		{"commit", "-m", "one", "-expect", "1111111111111111111111111111111111111111", "a.go"},
		{"merge", "-expect", "1111111111111111111111111111111111111111", "topic"},
		{"cherry-pick", "-expect", "1111111111111111111111111111111111111111", "1111111111111111111111111111111111111111"},
		{"revert", "-expect", "1111111111111111111111111111111111111111", "1111111111111111111111111111111111111111"},
		{"version"}, {"help"},
	}
	for _, args := range runs {
		got := run(t, env, args...)
		if strings.Contains(got.stdout, token) {
			t.Fatalf("%v wrote the bearer to stdout", args)
		}
		if strings.Contains(got.stderr, token) {
			t.Fatalf("%v wrote the bearer to stderr", args)
		}
	}
	for _, c := range f.seen() {
		if strings.Contains(c, token) {
			t.Fatalf("the bearer reached a URL: %s", c)
		}
	}
}

// writeFile is a test helper: the command reads local files on a commit, so a
// test that exercises one needs a file to name.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
