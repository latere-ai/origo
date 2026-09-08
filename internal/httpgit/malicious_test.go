// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

// TestMaliciousPackWritesNothing is spec 016's criterion for hostile
// objects: a pushed pack holding a tree entry named .git, one that
// names the git directory under NTFS or HFS+ rules, or a broken object
// is rejected by git's own check with git's message, and the log holds
// no entry for it: the store's keys and the index are what they were
// before the push, and the local copy's reference did not move.
func TestMaliciousPackWritesNothing(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	work := clone(t, n.url("/r/"+repoA+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "base")
	blob := strings.TrimSpace(mustGit(t, work, "hash-object", "-w", "a.txt"))
	head := strings.TrimSpace(mustGit(t, work, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(mustGit(t, work, "rev-parse", "HEAD^{tree}"))
	// A commit whose tree carries the named entry beside a.txt.
	withEntry := func(name string) string {
		t.Helper()
		listing := fmt.Sprintf("100644 blob %s\t%s\n100644 blob %s\ta.txt\n", blob, name, blob)
		tree := gitStdin(t, work, listing, "mktree")
		return gitStdin(t, work, "entry "+name, "commit-tree", tree, "-p", head)
	}
	broken := strings.TrimSpace(gitStdin(t, work,
		fmt.Sprintf("tree %s\nparent %s\nauthor a <a@b> 1 +0000\ncommitter a 1 +0000\n\nbroken\n", tree, head),
		"hash-object", "-t", "commit", "-w", "--stdin", "--literally"))
	cases := []struct{ name, commit, message string }{
		{"dotgit", withEntry(".git"), "hasDotgit"},
		{"ntfs", withEntry("git~1"), "hasDotgit"},
		{"hfs", withEntry(".g\u200cit"), "hasDotgit"},
		{"broken", broken, "missingEmail"},
	}
	before := store.Keys()
	for _, c := range cases {
		mustGit(t, work, "update-ref", "refs/heads/"+c.name, c.commit)
		out, err := git(t, work, "push", "origin", "refs/heads/"+c.name)
		if err == nil || !strings.Contains(out, c.message) || !strings.Contains(out, "[remote rejected]") {
			t.Fatalf("%s: push %v\n%s", c.name, err, out)
		}
	}
	if after := store.Keys(); !slices.Equal(before, after) {
		t.Fatalf("the log changed:\nbefore %v\nafter  %v", before, after)
	}
	ix, _, err := n.log.Newest(context.Background(), repoA, 0, false)
	if err != nil || ix.Seq != 0 || len(ix.Refs) != 1 {
		t.Fatalf("index after the pushes: %+v, %v", ix, err)
	}
	// The good history still lands, so the rejections were git's verdict
	// on the objects and not a broken node.
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	ix, _, _ = n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 1 || ix.Refs["refs/heads/main"] != head {
		t.Fatalf("good push: %+v", ix)
	}
}

// gitStdin runs git with stdin and returns trimmed stdout.
func gitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = gittest.Env(dir)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
