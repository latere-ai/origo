// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// TestE2EPushOverQuota is spec 012's repository size rule against a
// built origod: with the authorizer naming a quota of one mebibyte and
// one mebibyte already under lfs/, a push is refused in the sideband
// with over_quota and its sentence and nothing reaches the log, while
// the same push to a repository with nothing under lfs/ is taken.
func TestE2EPushOverQuota(t *testing.T) {
	s := requireStack(t)
	const quota = 1 << 20
	full, lean := newID(t), newID(t)
	s.authz.SetRules(
		authorizer.Rule{Repo: full, Allow: true, QuotaBytes: quota},
		authorizer.Rule{Repo: lean, Allow: true, QuotaBytes: quota},
	)
	n := startNode(t, s, "", nil)
	n.createRepo(full, "acme", "full-"+full[:8])
	n.createRepo(lean, "acme", "lean-"+lean[:8])

	// A mebibyte of LFS objects on the first repository is the whole
	// quota, so the pack alone puts it over.
	key := s.log.RepoPrefix(full) + "lfs/" + strings.Repeat("a", 64)
	if _, err := s.store.Put(context.Background(), key, wal.BytesBody(make([]byte, quota))); err != nil {
		t.Fatal(err)
	}

	work := clone(t, n.url(lean))
	commitFile(t, work, "a.txt", "one", "first")
	mustGit(t, work, "remote", "set-url", "origin", n.url(full))
	out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("the push over the quota was taken:\n%s", out)
	}
	want := contract.CodeOverQuota + ": " + contract.Sentence(contract.CodeOverQuota)
	if !strings.Contains(out, want) {
		t.Fatalf("the sideband does not carry %q:\n%s", want, out)
	}
	if entries := s.keys(t, full, "wal/"); len(entries) != 0 {
		t.Fatalf("the refused push wrote %v", entries)
	}
	// The same push without the lfs/ object fits.
	mustGit(t, work, "remote", "set-url", "origin", n.url(lean))
	if out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("the same push under the quota was refused:\n%s", out)
	}
	if entries := s.keys(t, lean, "wal/"); len(entries) != 1 {
		t.Fatalf("the accepted push wrote %v", entries)
	}
}
