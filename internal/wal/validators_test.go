// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/latere-ai/origo/internal/gittest"
)

// TestValidLabel: an owner or slug is the grammar of spec 003, and a
// label carrying a shell metacharacter, a control character, or a path
// component git refuses is refused before any subprocess sees it (spec
// 016).
func TestValidLabel(t *testing.T) {
	for _, ok := range []string{"a", "acme", "Acme-1", "a.b_c", "0", strings.Repeat("x", 128)} {
		if !ValidLabel(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".git", "-a", "_a", "a b", "a;b", "a$b", "a`b", "a|b", "a/b", "a\\b", "a\x01", "a\x7f", "a~1", "a:b", "ä", strings.Repeat("x", 129)} {
		if ValidLabel(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// gitOracle runs git once per accepted input against a scratch
// repository with the protections of spec 016 on, the way the node's
// repositories are configured.
type gitOracle struct {
	once sync.Once
	dir  string
	env  []string
}

var oracle gitOracle

func (o *gitOracle) init(t testing.TB) {
	o.once.Do(func() {
		dir, err := os.MkdirTemp("", "origo-oracle-")
		if err != nil {
			t.Fatal(err)
		}
		o.dir = dir
		o.env = gittest.Env(filepath.Join(dir, "home"))
		for _, args := range [][]string{
			{"init", "-q", "."},
			{"config", "core.protectNTFS", "true"},
			{"config", "core.protectHFS", "true"},
		} {
			if _, err := o.run(args...); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func (o *gitOracle) run(args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = o.dir
	cmd.Env = o.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// refFormatOK reports whether git accepts name as a reference.
func (o *gitOracle) refFormatOK(name string) bool {
	_, err := o.run("check-ref-format", "--allow-onelevel", name)
	return err == nil
}

// pathOK reports whether git accepts name as one path component of the
// index, which is where the NTFS and HFS+ rules of core.protectNTFS
// and core.protectHFS apply.
func (o *gitOracle) pathOK(name string) bool {
	const emptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	_, err := o.run("update-index", "--add", "--cacheinfo", "100644,"+emptyBlob+","+name)
	if err == nil {
		_, _ = o.run("update-index", "--force-remove", "--", name)
	}
	return err == nil
}

// FuzzValidRefName is spec 016's criterion for the reference validator:
// no name it accepts is one git check-ref-format refuses. It runs its
// seed corpus in the suite and for 40 seconds under make fuzz.
func FuzzValidRefName(f *testing.F) {
	for _, s := range []string{"HEAD", "refs/heads/main", "refs/tags/v1.0", "refs/heads/feature/x-y_z", "refs/heads/a.b", "refs/heads/a..b", "refs/heads/.hidden", "refs/heads/x.lock", "refs/heads/a b", "refs/heads/a~1", "refs/heads/a//b", "refs/heads/@", "refs/heads/a@{b}", "refs/heads/a\x01", "refs/heads/a\\b", "refs/heads/a:b", "refs/heads/a?b", "refs/heads/a*", "refs/heads/a[b", "refs/heads/a/", "refs/heads/-", "refs/heads/a.", "refs/heads/ä", "refs/heads/a/.b"} {
		f.Add(s)
	}
	oracle.init(f)
	f.Fuzz(func(t *testing.T, name string) {
		if !ValidRefName(name) {
			return
		}
		if !oracle.refFormatOK(name) {
			t.Fatalf("%q accepted by ValidRefName and refused by git check-ref-format", name)
		}
	})
}

// FuzzValidLabel is the same criterion for owner and slug: a label is a
// path segment of a clone URL and of the names/ key, so no label the
// validator accepts is a path component git refuses under the NTFS and
// HFS+ rules the node's repositories run with.
func FuzzValidLabel(f *testing.F) {
	for _, s := range []string{"acme", "app", ".git", ".GIT", "git~1", "GIT~1", ".g‌it", ".", "..", "a..b", "a b", "a;b", "a/b", "a\x00b", "ä", "-", "_", strings.Repeat("x", 128)} {
		f.Add(s)
	}
	oracle.init(f)
	f.Fuzz(func(t *testing.T, label string) {
		if !ValidLabel(label) {
			return
		}
		if label == "" || strings.ContainsAny(label, "/\\ \t\r\n\x00;&|$`'\"<>()~^:?*[]") {
			t.Fatalf("%q accepted with a separator or a shell metacharacter", label)
		}
		if !oracle.pathOK(label) {
			t.Fatalf("%q accepted by ValidLabel and refused by git as a path component", label)
		}
	})
}
