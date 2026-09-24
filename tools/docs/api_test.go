// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"latere.ai/x/origo/internal/contract"
)

// openAPIPath is one key of the paths object of api/openapi.yaml: two
// spaces of indent, the path, and a colon closing the line. The document
// is generated with a fixed layout, so a line reader is exact and keeps
// a YAML package off this module's build list.
var openAPIPath = regexp.MustCompile(`(?m)^  (/[^\s:]*):$`)

// TestAPIPageCoversTheSurface holds docs/api.md, which is written by hand
// for a client author, to the surface the node serves. The generated
// reference under docs/internals follows the specs by construction; this
// page does not, so a route or an error code added to the contract fails
// here until the page names it.
func TestAPIPageCoversTheSurface(t *testing.T) {
	dir := root(t)
	page, err := os.ReadFile(filepath.Join(dir, "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := os.ReadFile(filepath.Join(dir, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	paths := openAPIPath.FindAllSubmatch(document, -1)
	if len(paths) < 30 {
		t.Fatalf("api/openapi.yaml yields %d paths, too few to be the surface", len(paths))
	}
	for _, m := range paths {
		if !strings.Contains(string(page), "`"+string(m[1])+"`") {
			t.Errorf("docs/api.md does not name the path `%s` the OpenAPI document serves", m[1])
		}
	}
	for _, code := range contract.Codes() {
		if !strings.Contains(string(page), "`"+code+"`") {
			t.Errorf("docs/api.md does not name the error code `%s` the node can send", code)
		}
	}
}

// userDocuments are the pages written for people who run Origo or build
// against it, as opposed to docs/internals, CONTRIBUTING.md and specs/,
// which are written for people who change it.
func userDocuments(t *testing.T, dir string) []string {
	t.Helper()
	out := []string{"README.md", "SECURITY.md", filepath.Join("skills", "origo", "SKILL.md")}
	for _, pattern := range []string{"docs/*.md", "docs/upgrades/*.md"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(dir, m)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, rel)
		}
	}
	return out
}

var (
	// specCitation is a design spec cited by number in prose, and a
	// wikilink, which renders as literal brackets outside the spec tree.
	specCitation = regexp.MustCompile(`(?i)\bspecs? \d{3}\b|\[\[`)
	// specLink is a link into one numbered spec.
	specLink = regexp.MustCompile(`specs/(\d{3}-[a-z0-9-]+)\.md`)
)

// citableSpecs are the specs a user page may link by name: the threat
// model is written for a reviewer deciding whether to trust an
// installation, which is a user's question.
var citableSpecs = map[string]bool{"016-security-and-threat-model": true}

// TestUserDocumentsCiteNoSpecs keeps the user pages in the reader's
// terms. A spec number tells a user nothing they can act on, and a
// wikilink is a spec-tree convention GitHub renders as text.
func TestUserDocumentsCiteNoSpecs(t *testing.T) {
	dir := root(t)
	for _, name := range userDocuments(t, dir) {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if m := specCitation.FindString(line); m != "" {
				t.Errorf("%s:%d cites %q: %s", name, i+1, m, strings.TrimSpace(line))
			}
			for _, m := range specLink.FindAllStringSubmatch(line, -1) {
				if !citableSpecs[m[1]] {
					t.Errorf("%s:%d links the spec %s: %s", name, i+1, m[1], strings.TrimSpace(line))
				}
			}
		}
	}
}
