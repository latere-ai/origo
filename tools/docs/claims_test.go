// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A document can state a fact about the project that was true when it was
// written and is false now. Nothing in the build reads prose, so the claim
// survives every gate and is read by whoever arrives from a release note or a
// link. Two such claims have a single source of truth in the tree and are
// checked here against it.

var releaseHeading = regexp.MustCompile(`(?m)^## (v\d+\.\d+\.\d+) - \d{4}-\d{2}-\d{2}$`)

// newestRelease is the first version section of the changelog, which is the
// most recent release: `lateregate release` prepends each one.
func newestRelease(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := releaseHeading.FindSubmatch(body)
	if m == nil {
		t.Fatal("CHANGELOG.md has no version section")
	}
	return string(m[1])
}

// TestSecurityNamesTheCurrentRelease pins SECURITY.md to the changelog. The
// file says which release fixes go to, and a reader deciding whether they are
// covered acts on it, so a version left behind by a tag misinforms them.
func TestSecurityNamesTheCurrentRelease(t *testing.T) {
	dir := root(t)
	want := newestRelease(t, dir)

	body, err := os.ReadFile(filepath.Join(dir, "SECURITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`v\d+\.\d+\.\d+ is the\s+current release`).FindString(strings.Join(strings.Fields(string(body)), " "))
	if named == "" {
		t.Fatal("SECURITY.md names no current release")
	}
	if got := strings.Fields(named)[0]; got != want {
		t.Errorf("SECURITY.md says %s is the current release, CHANGELOG.md's newest is %s", got, want)
	}
}

// TestNoDocumentClaimsAPrivateRepository catches the class of claim that
// outlives the condition it describes. The repository is public, and a
// document saying otherwise is read by someone standing in the public
// repository, so it is visibly wrong to the only audience it has. The
// release workflow's own comments and conditions are exempt: they branch on
// visibility at run time and must keep naming both cases.
func TestNoDocumentClaimsAPrivateRepository(t *testing.T) {
	dir := root(t)
	docs := []string{"README.md", "SECURITY.md", "CONTRIBUTING.md", "docs/upgrades/README.md"}
	claim := regexp.MustCompile(`(?i)(while|because) the repository is private|repository is private`)

	for _, name := range docs {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if claim.MatchString(line) {
				t.Errorf("%s:%d claims the repository is private: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
