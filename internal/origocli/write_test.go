// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli_test

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/origocli"
)

// head is a whole object id the fixture's branch is at.
const head = "1111111111111111111111111111111111111111"

// writable is a stand-in with one commit and a working directory to commit
// from, which is what a write command needs.
func writable(t *testing.T) (*fake, map[string]string) {
	t.Helper()
	f := newFake()
	f.commits = []map[string]any{commitJSON(head, "Ada", "2026-09-10T12:00:00Z", "a change")}
	env := envFor(f.start(t))
	dir := t.TempDir()
	t.Chdir(dir)
	return f, env
}

// TestAuthorIsNotFlagControlledAndDryRunSaysSo: the body carries the author
// from the environment and the file's bytes as base64, and a dry run says it
// did not land.
func TestAuthorIsNotFlagControlledAndDryRunSaysSo(t *testing.T) {
	f, env := writable(t)
	if err := os.WriteFile("a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := run(t, env, "commit", "-m", "add a.txt", "-expect", head, "a.txt")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "cccccccccccccccccccccccccccccccccccccccc refs/heads/main entry 42 committed") {
		t.Fatalf("receipt: %q", got.stdout)
	}
	body := f.bodies[len(f.bodies)-1]
	author, _ := body["author"].(map[string]any)
	if author["name"] != "Ada Lovelace" || author["email"] != "ada@example.com" {
		t.Fatalf("author: %v", body["author"])
	}
	if body["expected_head"] != head || body["branch"] != "main" {
		t.Fatalf("common fields: %v", body)
	}
	changes, _ := body["changes"].([]any)
	change, _ := changes[0].(map[string]any)
	if change["path"] != "a.txt" || change["content"] != base64.StdEncoding.EncodeToString([]byte("hello")) {
		t.Fatalf("change: %v", change)
	}
	if change["mode"] != "100644" {
		t.Fatalf("mode: %v", change["mode"])
	}

	// There is no -author flag: a model writing a command line cannot choose
	// who a commit is attributed to.
	got = run(t, env, "commit", "-m", "x", "-expect", head, "-author", "Someone <e@x.com>", "a.txt")
	if got.code != origocli.CodeMisusage {
		t.Fatalf("-author was accepted, exit %d", got.code)
	}

	// A dry run says dry-run and carries no entry sequence.
	got = run(t, env, "commit", "-m", "x", "-expect", head, "-dry-run", "a.txt")
	if !strings.Contains(got.stdout, "entry - dry-run") {
		t.Fatalf("a dry run read as %q", got.stdout)
	}
	if dry, _ := f.bodies[len(f.bodies)-1]["dry_run"].(bool); !dry {
		t.Fatal("dry_run did not reach the wire")
	}
}

func TestAnExecutableAndADeleteAndAMappedFile(t *testing.T) {
	f, env := writable(t)
	if err := os.WriteFile("run.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("local.txt", []byte("mapped"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := run(t, env, "commit", "-m", "several", "-expect", head,
		"-file", "docs/remote.txt=local.txt", "-delete", "old.txt", "run.sh")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	changes, _ := f.bodies[len(f.bodies)-1]["changes"].([]any)
	if len(changes) != 3 {
		t.Fatalf("%d changes, want 3", len(changes))
	}
	first, _ := changes[0].(map[string]any)
	if first["path"] != "run.sh" || first["mode"] != "100755" {
		t.Fatalf("an executable lost its mode: %v", first)
	}
	second, _ := changes[1].(map[string]any)
	if second["path"] != "docs/remote.txt" || second["content"] != base64.StdEncoding.EncodeToString([]byte("mapped")) {
		t.Fatalf("the mapping did not hold: %v", second)
	}
	third, _ := changes[2].(map[string]any)
	if third["path"] != "old.txt" || third["delete"] != true {
		t.Fatalf("the delete did not hold: %v", third)
	}
}

// TestLocalReadsStayUnderTheWorkingDirectory is the bound this command owns:
// the repository path is checked by the node, the local read by nobody else.
func TestLocalReadsStayUnderTheWorkingDirectory(t *testing.T) {
	f, env := writable(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"commit", "-m", "x", "-expect", head, "../../etc/passwd"},
		{"commit", "-m", "x", "-expect", head, "-file", "stolen=" + outside},
		{"commit", "-m", "x", "-expect", head, outside},
	}
	before := len(f.bodies)
	for _, args := range cases {
		got := run(t, env, args...)
		if got.code != origocli.CodeMisusage {
			t.Fatalf("%v exited %d, want a refusal before any request", args, got.code)
		}
		if !strings.Contains(got.stderr, "outside the working directory") {
			t.Fatalf("%v: %q", args, got.stderr)
		}
	}
	if len(f.bodies) != before {
		t.Fatal("a write was attempted for a path outside the working directory")
	}
	// A directory is named rather than walked, and a missing file is named.
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := run(t, env, "commit", "-m", "x", "-expect", head, "sub"); !strings.Contains(got.stderr, "is a directory") {
		t.Fatalf("a directory: %q", got.stderr)
	}
	if got := run(t, env, "commit", "-m", "x", "-expect", head, "absent.txt"); !strings.Contains(got.stderr, "cannot be read") {
		t.Fatalf("a missing file: %q", got.stderr)
	}
}

// TestShortExpectIsExpandedBeforeTheWrite: a short id becomes the whole one
// before a write is sent, one that does not resolve is ref_not_found with
// nothing written, and a branch that moved is non_fast_forward.
func TestShortExpectIsExpandedBeforeTheWrite(t *testing.T) {
	f, env := writable(t)
	if err := os.WriteFile("a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := run(t, env, "commit", "-m", "x", "-expect", head[:7], "a.txt")
	if got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if f.bodies[len(f.bodies)-1]["expected_head"] != head {
		t.Fatalf("the short id was not expanded: %v", f.bodies[len(f.bodies)-1]["expected_head"])
	}
	var expanded bool
	for _, c := range f.seen() {
		if strings.Contains(c, "/commits/-?ref="+head[:7]) {
			expanded = true
		}
	}
	if !expanded {
		t.Fatalf("no expansion call was made: %v", f.seen())
	}

	// A short id that no longer resolves: nothing is written.
	before := len(f.bodies)
	got = run(t, env, "commit", "-m", "x", "-expect", "0000000", "a.txt")
	if got.code != origocli.CodeRefused || !strings.Contains(got.stderr, contract.CodeRefNotFound) {
		t.Fatalf("exit %d: %q", got.code, got.stderr)
	}
	if len(f.bodies) != before {
		t.Fatal("a write was sent after the expansion failed")
	}

	// A branch that moved: non_fast_forward carrying the actual head.
	f.answer = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "POST" {
			return false
		}
		f.refuse(w, 409, contract.CodeNonFastForward, map[string]any{
			"ref": "refs/heads/main", "expected": head, "actual": "2222222222222222222222222222222222222222",
		})
		return true
	}
	got = run(t, env, "commit", "-m", "x", "-expect", head, "a.txt")
	if got.code != origocli.CodeRefused {
		t.Fatalf("exit %d", got.code)
	}
	if !strings.Contains(got.stderr, "expected="+head) || !strings.Contains(got.stderr, "actual=2222222222222222222222222222222222222222") {
		t.Fatalf("the line did not name both heads: %q", got.stderr)
	}
}

func TestAWriteWithoutExpectIsRefusedBeforeAnyRequest(t *testing.T) {
	f, env := writable(t)
	if err := os.WriteFile("a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"commit", "-m", "x", "a.txt"},
		{"merge", "topic"},
		{"cherry-pick", head},
		{"revert", head},
	} {
		before := len(f.bodies)
		got := run(t, env, args...)
		if got.code != origocli.CodeMisusage {
			t.Fatalf("%v exited %d, want a refusal", args, got.code)
		}
		if !strings.Contains(got.stderr, "-expect") {
			t.Fatalf("%v: %q", args, got.stderr)
		}
		if len(f.bodies) != before {
			t.Fatalf("%v sent a write", args)
		}
	}
	// -create is the one case with no head, because the branch does not exist.
	got := run(t, env, "commit", "-m", "x", "-create", "-branch", "topic", "-from", "main", "a.txt")
	if got.code != origocli.CodeOK {
		t.Fatalf("a created branch was refused: %d %s", got.code, got.stderr)
	}
	body := f.bodies[len(f.bodies)-1]
	if body["create_branch"] != true || body["from"] != "main" || body["expected_head"] != nil {
		t.Fatalf("body: %v", body)
	}
}

func TestMergeAndReplayReachTheirRoutes(t *testing.T) {
	f, env := writable(t)
	run(t, env, "merge", "-expect", head, "-strategy", "merge_commit", "-m", "Merge topic", "topic")
	run(t, env, "cherry-pick", "-expect", head, "-mainline", "1", head, head)
	run(t, env, "revert", "-expect", head, head)
	seen := strings.Join(f.seen(), "\n")
	for _, want := range []string{"POST /v1/repos/id-1/merge", "POST /v1/repos/id-1/cherry-pick", "POST /v1/repos/id-1/revert"} {
		if !strings.Contains(seen, want) {
			t.Fatalf("%q missing from:\n%s", want, seen)
		}
	}
	merge := f.bodies[0]
	if merge["source"] != "topic" || merge["strategy"] != "merge_commit" || merge["message"] != "Merge topic" {
		t.Fatalf("merge body: %v", merge)
	}
	pick := f.bodies[1]
	commits, _ := pick["commits"].([]any)
	if len(commits) != 2 || pick["mainline"] != float64(1) {
		t.Fatalf("cherry-pick body: %v", pick)
	}
}

func TestWriteMisuseIsRefusedBeforeAnyRequest(t *testing.T) {
	f, env := writable(t)
	if err := os.WriteFile("a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"commit", "-expect", head, "a.txt"}, "-m is the commit message"},
		{[]string{"commit", "-m", "x", "-expect", head}, "name at least one path"},
		{[]string{"commit", "-m", "x", "-expect", head, "-file", "nopair"}, "-file is"},
		{[]string{"merge", "-expect", head}, "merge takes one source"},
		{[]string{"merge", "-expect", head, "-strategy", "rebase", "topic"}, "-strategy is"},
		{[]string{"cherry-pick", "-expect", head, "-mainline", "-1", head}, "-mainline"},
		{[]string{"revert", "-expect", head}, "revert takes at least one commit"},
	}
	before := len(f.bodies)
	for _, tc := range cases {
		got := run(t, env, tc.args...)
		if got.code != origocli.CodeMisusage {
			t.Fatalf("%v exited %d", tc.args, got.code)
		}
		if !strings.Contains(got.stderr, tc.want) {
			t.Fatalf("%v: %q does not carry %q", tc.args, got.stderr, tc.want)
		}
	}
	if len(f.bodies) != before {
		t.Fatal("a misuse reached the wire")
	}
}

// TestTheBranchDefaultsToTheRepositorys: a write with no -branch reads the
// repository's default rather than guessing main.
func TestTheBranchDefaultsToTheRepositorys(t *testing.T) {
	f, env := writable(t)
	f.repos[0]["default_branch"] = "trunk"
	if err := os.WriteFile("a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(t, env, "commit", "-m", "x", "-expect", head, "a.txt"); got.code != origocli.CodeOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if f.bodies[len(f.bodies)-1]["branch"] != "trunk" {
		t.Fatalf("branch: %v", f.bodies[len(f.bodies)-1]["branch"])
	}
}
