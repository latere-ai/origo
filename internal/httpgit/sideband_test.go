// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
)

// TestRejectLinesAreTheTableSentences is spec 021's sideband rule: a
// push the log refuses reaches the client as "remote: <code>:
// <sentence>" with the table's sentence and nothing else. Two pushes
// through the handler over the in-memory store: one over a reference
// another clone moved between the advertisement and the pack, made
// deterministic by a pre-push hook that runs the other clone's push
// after git read the advertisement, and one whose entry Put fails after
// the advertisement. The moved reference's name and the two hashes are
// on the handler's info line and not in git's output; the storage error
// is on the error line and not in git's output.
func TestRejectLinesAreTheTableSentences(t *testing.T) {
	records := &logRecords{}
	store := wal.NewMemStore()
	n := newNode(t, store, withLog(records))
	n.create(repoA, "acme", "app")
	a := clone(t, n.url("/r/"+repoA+".git"))
	_ = os.WriteFile(filepath.Join(a, "a.txt"), []byte("a"), 0o644)
	mustGit(t, a, "add", "a.txt")
	mustGit(t, a, "commit", "-q", "-m", "base")
	mustGit(t, a, "push", "-q", "origin", "HEAD:refs/heads/main")
	base := strings.TrimSpace(mustGit(t, a, "rev-parse", "HEAD"))
	b := clone(t, n.url("/r/"+repoA+".git"))

	// a's next commit lands from b's pre-push hook, once b has read the
	// advertisement and before it sends its pack.
	_ = os.WriteFile(filepath.Join(a, "a.txt"), []byte("a2"), 0o644)
	mustGit(t, a, "commit", "-q", "-am", "a2")
	aHead := strings.TrimSpace(mustGit(t, a, "rev-parse", "HEAD"))
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ngit -C '" + a + "' push -q origin HEAD:refs/heads/main\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(hook), 0o755); err != nil { //nolint:gosec // a hook must be executable
		t.Fatal(err)
	}
	mustGit(t, b, "config", "core.hooksPath", hooks)
	_ = os.WriteFile(filepath.Join(b, "b.txt"), []byte("b"), 0o644)
	mustGit(t, b, "add", "b.txt")
	mustGit(t, b, "commit", "-q", "-m", "b")
	bHead := strings.TrimSpace(mustGit(t, b, "rev-parse", "HEAD"))
	out, err := git(t, b, "push", "origin", "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("the stale push landed:\n%s", out)
	}
	want := "remote: " + contract.Line(contract.CodeNonFastForward)
	if !strings.Contains(out, want) {
		t.Fatalf("git's output lacks %q:\n%s", want, out)
	}
	for _, secret := range []string{"refs/heads/main", base, aHead, bHead} {
		if strings.Contains(out, secret) {
			t.Errorf("git's output carries %q, which belongs on the log line:\n%s", secret, out)
		}
	}
	line := records.find("push refused")
	if line == nil || line["code"] != contract.CodeNonFastForward || line["ref"] != "refs/heads/main" || line["expected"] != base || line["actual"] != aHead || line["repo"] != repoA {
		t.Fatalf("the info line of the refused push: %v", line)
	}

	// The entry Put fails after the advertisement: the sentence and
	// nothing of the error in git's output, the error on the error line.
	mustGit(t, b, "config", "--unset", "core.hooksPath")
	mustGit(t, b, "fetch", "-q", "origin")
	mustGit(t, b, "reset", "-q", "--hard", "origin/main")
	_ = os.WriteFile(filepath.Join(b, "c.txt"), []byte("c"), 0o644)
	mustGit(t, b, "add", "c.txt")
	mustGit(t, b, "commit", "-q", "-m", "c")
	store.SetFault(func(op, key string) error {
		if op == "Put" && strings.Contains(key, "/wal/") {
			return errors.New("bucket down")
		}
		return nil
	})
	out, err = git(t, b, "push", "origin", "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("the push landed with the bucket down:\n%s", out)
	}
	want = "remote: " + contract.Line(contract.CodeStorageUnavailable)
	if !strings.Contains(out, want) || strings.Contains(out, "bucket down") || strings.Contains(out, "not recorded") {
		t.Fatalf("git's output for a failed entry write:\n%s", out)
	}
	if line := records.find("commit failed"); line == nil || line["repo"] != repoA || !strings.Contains(line["error"].(error).Error(), "bucket down") {
		t.Fatalf("the error line of the failed commit: %v", line)
	}
}
