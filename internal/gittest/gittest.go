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
	"sort"
	"strings"
	"testing"
	"time"
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

// Source is a working repository the tests commit into. Every commit
// gets a date one minute after the previous one, from a fixed start, so
// a history is the same on every run and its commits sort by date.
type Source struct {
	Dir  string
	t    testing.TB
	tick int
}

// NewSource creates a working repository with branch main and no commit.
func NewSource(t testing.TB) *Source {
	t.Helper()
	return NewSourceAt(t, filepath.Join(t.TempDir(), "src"))
}

// NewSourceAt creates a working repository in dir, which the caller
// owns, so a fixture built once can outlive the test that built it.
func NewSourceAt(t testing.TB, dir string) *Source {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	Run(t, dir, nil, "init", "-q", "-b", "main", ".")
	return &Source{Dir: dir, t: t}
}

// epoch is the date of the first commit of a Source.
var epoch = time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)

// commitEnv is the environment of one commit: the hermetic one with the
// next date. A later entry wins over an earlier one for the same key.
func (s *Source) commitEnv() []string {
	s.tick++
	at := epoch.Add(time.Duration(s.tick) * time.Minute).Format(time.RFC3339)
	return append(Env(s.Dir), "GIT_AUTHOR_DATE="+at, "GIT_COMMITTER_DATE="+at)
}

// run runs git in the source with the commit environment.
func (s *Source) run(stdin []byte, args ...string) string {
	s.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = s.Dir
	cmd.Env = s.commitEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// Commit writes a file with content and commits it on the current branch,
// returning the commit id.
func (s *Source) Commit(name, content, message string) string {
	s.t.Helper()
	s.Write(name, []byte(content))
	Run(s.t, s.Dir, nil, "add", "--", name)
	return s.CommitAll(message)
}

// Write writes a file under the working tree, creating its directories.
func (s *Source) Write(name string, content []byte) {
	s.t.Helper()
	path := filepath.Join(s.Dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// CommitAll stages every change and commits it with the message, which
// may span lines and carry trailers, returning the commit id.
func (s *Source) CommitAll(message string) string {
	s.t.Helper()
	Run(s.t, s.Dir, nil, "add", "-A", "--", ".")
	s.run([]byte(message), "commit", "-q", "--allow-empty", "-F", "-")
	return s.Rev("HEAD")
}

// Rename moves a file with git mv, so the next commit records a rename.
func (s *Source) Rename(from, to string) {
	s.t.Helper()
	Run(s.t, s.Dir, nil, "mv", "--", from, to)
}

// Branch creates a branch at HEAD and checks it out.
func (s *Source) Branch(name string) {
	s.t.Helper()
	Run(s.t, s.Dir, nil, "checkout", "-q", "-b", name)
}

// Checkout switches to an existing branch.
func (s *Source) Checkout(name string) {
	s.t.Helper()
	Run(s.t, s.Dir, nil, "checkout", "-q", name)
}

// Merge merges branch into the current one with a merge commit, never a
// fast-forward, and returns the merge commit id.
func (s *Source) Merge(branch, message string) string {
	s.t.Helper()
	s.run(nil, "merge", "-q", "--no-ff", "--no-edit", "-m", message, branch)
	return s.Rev("HEAD")
}

// Tag creates an annotated tag at HEAD and returns the tag object id.
func (s *Source) Tag(name, message string) string {
	s.t.Helper()
	s.run(nil, "tag", "-a", "-m", message, name)
	return s.Rev("refs/tags/" + name)
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

// PackAll builds one packfile holding every object reachable from every
// reference, the pack of a push that carries the whole history.
func (s *Source) PackAll() []byte {
	s.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "pack-objects", "--stdout", "--revs", "--all", "-q")
	cmd.Dir = s.Dir
	cmd.Env = Env(s.Dir)
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatalf("pack-objects: %v: %s", err, stderr.String())
	}
	return out
}

// Refs lists every reference under refs/ with its object id, and HEAD
// with its symbolic target, in the form the log's reference map uses.
func (s *Source) Refs() map[string]string {
	s.t.Helper()
	refs := map[string]string{"HEAD": "ref: " + Run(s.t, s.Dir, nil, "symbolic-ref", "HEAD")}
	for line := range strings.SplitSeq(Run(s.t, s.Dir, nil, "for-each-ref", "--format=%(refname) %(objectname)"), "\n") {
		if name, sha, ok := strings.Cut(line, " "); ok {
			refs[name] = sha
		}
	}
	return refs
}

// Bytes returns n bytes from a fixed generator, the same on every call
// with the same seed, so a fixture's large blob has a known digest
// without a copy in the tree. The generator is xorshift64, written here
// so the sequence never changes with a library.
func Bytes(n int, seed uint64) []byte {
	out := make([]byte, n)
	x := seed | 1
	for i := 0; i < n; i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		for j := range min(8, n-i) {
			out[i+j] = byte(x >> (8 * j))
		}
	}
	return out
}

// RevList returns the commits reachable from any reference, one id per
// line in lexical order, so two repositories with the same history
// compare equal whatever their reference names.
func RevList(t testing.TB, dir string) string {
	t.Helper()
	ids := strings.Split(Run(t, dir, nil, "rev-list", "--all"), "\n")
	sort.Strings(ids)
	return strings.Join(ids, "\n")
}
