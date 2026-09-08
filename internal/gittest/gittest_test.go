// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package gittest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
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
	want := []string{c1, c2}
	sort.Strings(want)
	if got := RevList(t, src.Dir); got != strings.Join(want, "\n") {
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

func TestFixtureHasEveryShape(t *testing.T) {
	f := NewFixture(t, "")
	if f.Refs["HEAD"] != "ref: refs/heads/main" || f.Refs["refs/heads/main"] != f.Tip || f.Refs["refs/tags/v1.0"] != f.TagV1 || f.Refs["refs/tags/latest"] != f.Tip || f.Refs["refs/heads/big"] != f.Big || f.Refs["refs/heads/large"] != f.Large || f.Refs["refs/heads/topic/x"] != f.Merge {
		t.Fatalf("refs: %v", f.Refs)
	}
	// Merges have two parents, the rename is detected, the binary file
	// is binary, and the trailers are three pairs in order.
	for _, m := range []string{f.Merge, f.SideMerge} {
		if parents := strings.Fields(Run(t, f.Dir, nil, "show", "-s", "--format=%P", m)); len(parents) != 2 {
			t.Fatalf("merge %s has parents %v", m, parents)
		}
	}
	if out := Run(t, f.Dir, nil, "show", "--name-status", "--format=", "-M", f.Rename); !strings.HasPrefix(out, "R100\tsrc/a.txt\tsrc/b.txt") {
		t.Fatalf("rename: %q", out)
	}
	if out := Run(t, f.Dir, nil, "show", "--numstat", "--format=", f.Binary); out != "-\t-\tassets/logo.bin" {
		t.Fatalf("binary: %q", out)
	}
	if out := Run(t, f.Dir, nil, "show", "-s", "--format=%(trailers:only,unfold)", f.Trailers); out != "Signed-off-by: Alice <alice@example.com>\nSigned-off-by: Bob <bob@example.com>\nCo-authored-by: Carol <carol@example.com>" {
		t.Fatalf("trailers: %q", out)
	}
	if Run(t, f.Dir, nil, "cat-file", "-t", f.TagV1) != "tag" || Run(t, f.Dir, nil, "rev-parse", f.TagV1+"^{commit}") != f.Trailers {
		t.Fatal("tag")
	}
	// More than 100 commits on main, the side merge's parents
	// interleaving in rev-list order.
	commits := strings.Split(Run(t, f.Dir, nil, "rev-list", "main"), "\n")
	if len(commits) <= 100 {
		t.Fatalf("%d commits on main", len(commits))
	}
	order := Run(t, f.Dir, nil, "rev-list", "--format=%s", "--no-commit-header", f.SideMerge, "^"+f.Merge)
	if !strings.HasPrefix(order, "Merge side\nMain 49\nSide 49\nMain 48\nSide 48\n") {
		t.Fatalf("parents do not interleave:\n%s", order[:min(200, len(order))])
	}
	// The many directory, the large change, and the big blob.
	if n := strings.Count(Run(t, f.Dir, nil, "ls-tree", f.Tip, "many/"), "\n") + 1; n != ManyEntries {
		t.Fatalf("many has %d entries", n)
	}
	if out := Run(t, f.Dir, nil, "diff", "--shortstat", f.Tip, f.Large); !strings.HasPrefix(out, fmt.Sprintf("%d files changed", LargeFiles)) {
		t.Fatalf("large: %q", out)
	}
	if size := Run(t, f.Dir, nil, "cat-file", "-s", f.BigBlob); size != fmt.Sprint(BigBlobSize) {
		t.Fatalf("big blob size %s", size)
	}
	sum := sha256.Sum256(Bytes(BigBlobSize, bigBlobSeed))
	if hex.EncodeToString(sum[:]) != f.BigSHA256 || Bytes(10, 1)[0] == Bytes(10, 2)[0] && Bytes(10, 1)[1] == Bytes(10, 2)[1] {
		t.Fatal("Bytes is not deterministic per seed")
	}
	// The whole history in one pack, and the dates advance per commit.
	pack := f.PackAll()
	if !bytes.HasPrefix(pack, []byte("PACK")) || len(pack) < BigBlobSize/2 {
		t.Fatalf("pack of %d bytes", len(pack))
	}
	if a, b := Run(t, f.Dir, nil, "show", "-s", "--format=%cI", f.Root), Run(t, f.Dir, nil, "show", "-s", "--format=%cI", f.Tip); a != "2026-09-06T10:01:00Z" || b <= a {
		t.Fatalf("dates %s %s", a, b)
	}
}
