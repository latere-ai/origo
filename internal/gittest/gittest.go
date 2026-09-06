// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package gittest builds fixture histories and packfiles with the real
// git binary for the tests of the repository cache and the smart HTTP
// surface. It is test support: nothing outside a _test.go file imports it.
package gittest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Env is the hermetic environment every git invocation runs with: no
// user or system configuration, no prompts, a fixed identity.
func Env(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Origo Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Origo Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-09-06T10:00:00Z",
		"GIT_COMMITTER_DATE=2026-09-06T10:00:00Z",
		"LC_ALL=C",
	}
}

// Run runs git in dir with stdin and returns trimmed stdout, failing the
// test on a non-zero exit.
func Run(t testing.TB, dir string, stdin []byte, args ...string) string {
	t.Helper()
	out, err := Try(dir, stdin, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// Try runs git in dir and returns trimmed stdout, or an error carrying
// stderr.
func Try(dir string, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = Env(dir)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Source is a working repository the tests commit into.
type Source struct {
	Dir string
	t   testing.TB
}

// NewSource creates a working repository with branch main and no commit.
func NewSource(t testing.TB) *Source {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	Run(t, dir, nil, "init", "-q", "-b", "main", ".")
	return &Source{Dir: dir, t: t}
}

// Commit writes a file with content and commits it on the current branch,
// returning the commit id.
func (s *Source) Commit(name, content, message string) string {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
	Run(s.t, s.Dir, nil, "add", "--", name)
	Run(s.t, s.Dir, nil, "commit", "-q", "-m", message)
	return Run(s.t, s.Dir, nil, "rev-parse", "HEAD")
}

// Rev resolves a revision.
func (s *Source) Rev(rev string) string { return Run(s.t, s.Dir, nil, "rev-parse", rev) }

// Pack builds a packfile holding everything reachable from want that is
// not reachable from any of have. With a non-empty have the pack is thin,
// as a push's pack is.
func (s *Source) Pack(want string, have ...string) []byte {
	s.t.Helper()
	var revs strings.Builder
	revs.WriteString(want + "\n")
	for _, h := range have {
		revs.WriteString("^" + h + "\n")
	}
	args := []string{"pack-objects", "--stdout", "--revs", "-q"}
	if len(have) > 0 {
		args = append(args, "--thin")
	}
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = s.Dir
	cmd.Env = Env(s.Dir)
	cmd.Stdin = strings.NewReader(revs.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatalf("pack-objects: %v: %s", err, stderr.String())
	}
	return out
}

// RevList returns `rev-list --all` of a repository, sorted by git.
func RevList(t testing.TB, dir string) string {
	t.Helper()
	return Run(t, dir, nil, "rev-list", "--all")
}
