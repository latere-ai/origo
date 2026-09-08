// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/gittest"
)

// pathCases is the table of spec 009's path rules: every row's path
// and whether it is accepted.
var pathCases = []struct {
	path string
	ok   bool
}{
	{"", true},
	{"a/b", true},
	{"a", true},
	{"README.md", true},
	{".gitignore", true},
	{".gitmodules", true},
	{"a/.gitx/b", true},
	{"git~x/a", true},
	{"git~/a", true},
	{"a/gitx~1", true},
	{"-leading-dash", true},
	{"a b/c d", true},
	{"ünïcödé/файл", true},
	{strings.Repeat("a", MaxPathBytes), true},
	{".git/x", false},
	{"a/.GIT/x", false},
	{"x/.Git", false},
	{"git~1/x", false},
	{"a/GIT~1", false},
	{"git~12/x", false},
	{".git./x", false},
	{".git /x", false},
	{".git. . /x", false},
	{"git~1./x", false},
	{".git:stream/x", false},
	{"a//b", false},
	{"/a", false},
	{"a/", false},
	{"a/../b", false},
	{"../a", false},
	{"./a", false},
	{"a/./b", false},
	{".", false},
	{"..", false},
	{strings.Repeat("a", MaxPathBytes+1), false},
	{"a\x00b", false},
	{"a/.git\u200c/b", false},
	{"\u200c.git", false},
	{".g\ufeffit/x", false},
	{".GI\u202eT", false},
}

// TestPathRules classifies every path of the table as the rules say.
// That no refused path reaches a subprocess is asserted by
// TestPathGrammarRefusesOptions over the handler.
func TestPathRules(t *testing.T) {
	for _, c := range pathCases {
		if got := ValidPath(c.path); got != c.ok {
			t.Errorf("ValidPath(%q) = %v, want %v", c.path, got, c.ok)
		}
	}
}

// pathIndex is a repository whose index the fuzz function writes into,
// with the protections on, so git's own verify_path is the oracle.
type pathIndex struct {
	dir, blob string
}

func newPathIndex(t testing.TB) *pathIndex {
	t.Helper()
	dir := t.TempDir()
	gittest.Run(t, dir, nil, "init", "-q", ".")
	gittest.Run(t, dir, nil, "config", "core.protectNTFS", "true")
	gittest.Run(t, dir, nil, "config", "core.protectHFS", "true")
	blob := gittest.Run(t, dir, []byte("x\n"), "hash-object", "-w", "--stdin")
	return &pathIndex{dir: dir, blob: blob}
}

// accepts reports whether git records path as a regular file in a
// fresh index.
func (p *pathIndex) accepts(t testing.TB, path string) bool {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "update-index", "-z", "--index-info")
	cmd.Dir = p.dir
	cmd.Env = append(gittest.Env(p.dir), "GIT_INDEX_FILE="+filepath.Join(p.dir, "fuzz.index"))
	cmd.Stdin = bytes.NewReader(append([]byte("100644 "+p.blob+"\t"+path), 0))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil && !strings.Contains(stderr.String(), "invalid path") && !strings.Contains(stderr.String(), "Invalid path") && !strings.Contains(stderr.String(), "malformed") {
		t.Fatalf("update-index %q: %v: %s", path, err, stderr.String())
	}
	return err == nil
}

// FuzzValidPath holds ValidPath to git: a path the validator accepts is
// one git update-index accepts too, whatever the input. The validator
// may be stricter (the length, and the empty path spec 009 gives a
// meaning of its own), never looser.
func FuzzValidPath(f *testing.F) {
	for _, c := range pathCases {
		f.Add(c.path)
	}
	f.Add("a/.git\x00")
	f.Add("b/\u200d.GiT/c")
	f.Add(".git\u200c\u200d:x")
	ix := newPathIndex(f)
	f.Fuzz(func(t *testing.T, path string) {
		if !ValidPath(path) || path == "" {
			return
		}
		if !ix.accepts(t, path) {
			t.Fatalf("ValidPath accepts %q, git refuses it", path)
		}
	})
}
