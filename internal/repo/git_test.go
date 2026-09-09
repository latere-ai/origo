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

// TestDropCapabilityTurnsItsKeyOff is spec 021's mutation seam: with
// DropCapability set, the repository the cache initializes carries that
// capability's key off and every other capability on, and the two
// sha1-in-want capabilities share one key.
func TestDropCapabilityTurnsItsKeyOff(t *testing.T) {
	for name, key := range capabilityKeys {
		c := newHarness(t).cache
		c.dropCapability = name
		r := &Repo{ID: repoA, Dir: filepath.Join(t.TempDir(), "bare")}
		if err := c.initBare(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"uploadpack.allowFilter", "uploadpack.allowAnySHA1InWant", "receive.advertiseAtomic", "receive.advertisePushOptions"} {
			out, err := c.git.Run(context.Background(), r.Dir, nil, "config", "--get", k)
			want := "true"
			if k == key {
				want = "false"
			}
			if err != nil || strings.TrimSpace(string(out)) != want {
				t.Errorf("%s dropped: %s = %q, want %s (%v)", name, k, out, want, err)
			}
		}
	}
	if c := newHarness(t).cache; c.dropCapability != "" {
		t.Fatal("a cache built without the option drops a capability")
	}
}
