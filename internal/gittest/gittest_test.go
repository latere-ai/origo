// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package gittest

import (
	"bytes"
	"strings"
	"testing"
)

func TestSourceBuildsHistoryAndPacks(t *testing.T) {
	src := NewSource(t)
	c1 := src.Commit("a.txt", "one", "first")
	c2 := src.Commit("b.txt", "two", "second")
	if src.Rev("HEAD") != c2 || src.Rev("HEAD~1") != c1 {
		t.Fatal("history")
	}
	full := src.Pack(c2)
	thin := src.Pack(c2, c1)
	if !bytes.HasPrefix(full, []byte("PACK")) || !bytes.HasPrefix(thin, []byte("PACK")) || len(thin) >= len(full) {
		t.Fatalf("packs: full %d thin %d", len(full), len(thin))
	}
	if got := RevList(t, src.Dir); got != c2+"\n"+c1 {
		t.Fatalf("rev-list = %q", got)
	}
	if _, err := Try(src.Dir, nil, "rev-parse", "--verify", "nope"); err == nil || !strings.Contains(err.Error(), "fatal") {
		t.Fatalf("Try: %v", err)
	}
	if out, err := Try(src.Dir, []byte("x\n"), "hash-object", "--stdin"); err != nil || len(out) != 40 {
		t.Fatalf("Try with stdin: %q, %v", out, err)
	}
	if !strings.Contains(strings.Join(Env(src.Dir), " "), "GIT_CONFIG_NOSYSTEM=1") {
		t.Fatal("Env")
	}
}

func TestRunFailsTheTestOnAGitError(t *testing.T) {
	src := NewSource(t)
	ft := &fakeT{TB: t}
	Run(ft, src.Dir, nil, "rev-parse", "--verify", "nope")
	if !ft.failed {
		t.Fatal("Run did not fail the test")
	}
	ft = &fakeT{TB: t}
	src2 := &Source{Dir: t.TempDir(), t: ft}
	src2.Commit("a", "b", "c")
	if !ft.failed {
		t.Fatal("Commit outside a repository did not fail the test")
	}
	ft = &fakeT{TB: t}
	src2 = &Source{Dir: t.TempDir(), t: ft}
	src2.Pack("HEAD")
	if !ft.failed {
		t.Fatal("Pack outside a repository did not fail the test")
	}
	ft = &fakeT{TB: t}
	src3 := &Source{Dir: "/nonexistent/dir", t: ft}
	src3.Commit("a", "b", "c")
	if !ft.failed {
		t.Fatal("Commit into a missing directory did not fail the test")
	}
}

// fakeT records a fatal instead of stopping the test.
type fakeT struct {
	testing.TB
	failed bool
}

func (f *fakeT) Fatalf(string, ...any) { f.failed = true }
func (f *fakeT) Fatal(...any)          { f.failed = true }
func (f *fakeT) Helper()               {}
