// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package repo

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSubprocessEnvironment is spec 016's criterion for the subprocess
// environment: a git subprocess observes exactly the seven variables
// the spec documents and nothing of the parent's environment, asserted
// by running env through Git.Command in place of git.
func TestSubprocessEnvironment(t *testing.T) {
	t.Setenv("ORIGO_S3_SECRET", "must-not-leak")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	home := t.TempDir()
	dir := t.TempDir()
	g := &Git{Bin: "env", Home: home}
	var out strings.Builder
	cmd := g.Command(context.Background(), dir)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	slices.Sort(got)
	want := []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_DIR=" + dir,
		"GIT_TERMINAL_PROMPT=0",
		"HOME=" + home,
		"LC_ALL=C",
		"PATH=" + os.Getenv("PATH"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("subprocess environment:\n got %q\nwant %q", got, want)
	}
}

// TestBareRepositoryConfiguration: a repository the cache initializes
// carries the object checks of spec 016, so a pushed or fetched pack
// with a malformed object or a tree entry naming the git directory is
// refused by git itself.
func TestBareRepositoryConfiguration(t *testing.T) {
	c := newHarness(t).cache
	r := &Repo{ID: repoA, Dir: filepath.Join(t.TempDir(), "bare")}
	if err := c.initBare(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"core.protectNTFS", "core.protectHFS", "receive.fsckObjects", "transfer.fsckObjects"} {
		out, err := c.git.Run(context.Background(), r.Dir, nil, "config", "--get", key)
		if err != nil || strings.TrimSpace(string(out)) != "true" {
			t.Errorf("%s = %q, %v", key, out, err)
		}
	}
}
