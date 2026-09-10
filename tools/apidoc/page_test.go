// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/latere-ai/origo/tools/specindex/specs"
)

// root is the repository root, resolved from this file rather than from
// the working directory, so the test reads the specs and the page
// wherever it is run from.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func index(t *testing.T) *specs.Index {
	t.Helper()
	idx, err := specs.Build(filepath.Join(root(t), "specs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Findings) > 0 {
		t.Fatalf("the deck has findings: %v", idx.Findings)
	}
	return idx
}

// TestAPIDocIsCurrent is spec 018's criterion: docs/api.md is what
// `make docs` renders from the specs, and it carries every endpoint,
// header, and code the cross-reference lists and no other name.
func TestAPIDocIsCurrent(t *testing.T) {
	idx := index(t)
	want, err := Page(idx)
	if err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root(t), "docs", "api.md")
	got, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("docs/api.md differs from the specs; run make docs")
	}

	name := regexp.MustCompile("`([^`]+)`")
	for _, kind := range []specs.Kind{specs.KindEndpoint, specs.KindHeader, specs.KindCode} {
		var want, first []string
		for _, n := range idx.Names {
			if n.Kind == kind {
				want = append(want, n.Name)
			}
		}
		// The first cell of every row of every table of this kind is
		// what the page carries; a name it holds that the deck does not
		// define, or one the deck defines that it drops, fails here.
		for _, g := range groups(idx, kind) {
			for _, r := range g.rows {
				cell := r.cells[0]
				if kind == specs.KindEndpoint {
					for _, m := range name.FindAllStringSubmatch(r.cells[1], -1) {
						first = append(first, strings.Trim(cell, "`")+" "+m[1])
					}
					continue
				}
				for _, m := range name.FindAllStringSubmatch(cell, -1) {
					first = append(first, m[1])
				}
			}
		}
		missing, extra := diff(want, first), diff(first, want)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s: the page is missing %v and carries %v the deck does not define", kind, missing, extra)
		}
	}
}

// diff reports the entries of a that b does not hold.
func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// TestPageStatesTheContractAndNothingElse holds the two rules of the
// page's own text: the contract number comes from spec 003's header row,
// and a deck that states none is an error rather than a guess.
func TestPageStatesTheContractAndNothingElse(t *testing.T) {
	idx := index(t)
	page, err := Page(idx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "This is contract version 1.") {
		t.Error("the page does not state the contract version spec 003 defines")
	}
	for _, n := range idx.Names {
		if n.Kind == specs.KindHeader && n.Name == "Origo-Contract" {
			n.Row[1] = "the contract version, on every response"
		}
	}
	if _, err := Page(idx); err == nil {
		t.Error("a deck that states no contract version renders a page anyway")
	}
}

// TestAuthorizationSectionComesFromTheSpec holds the rule the section
// exists for: the contract an operator's endpoint is held to is written
// once, in the spec, and the page carries that passage rather than a
// restatement of it. A deck whose spec does not state it renders no page
// at all, which is what keeps the two from drifting apart silently.
func TestAuthorizationSectionComesFromTheSpec(t *testing.T) {
	idx := index(t)
	page, err := Page(idx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := idx.Section(authzSpec, authzHeading)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "\n## The authorization endpoint\n") {
		t.Fatal("the page carries no authorization endpoint section")
	}
	if !strings.Contains(page, want) {
		t.Error("the section on the page is not the passage the spec writes")
	}
	// The five rules, the probe id, and the single-tenant sentence are
	// what spec 072 of the auth deck asked this page to carry; each is a
	// line an operator's endpoint is held to.
	for _, s := range []string{
		"00000000-0000-0000-0000-000000000001",
		"A single-tenant installation needs no service",
		"Answer 200 for both verdicts",
		"Treat the endpoint's availability as Origo's",
	} {
		if !strings.Contains(page, s) {
			t.Errorf("the page does not state %q", s)
		}
	}
	// A deck that defines every kind but states no such contract renders
	// no page: the section is required, not optional.
	dir := t.TempDir()
	body := "---\ntitle: t\n---\n\n| Code | Status | Message |\n|---|---|---|\n| `repo_not_found` | 404 | Repository not found. |\n\n" +
		"| Method | Path | Body |\n|---|---|---|\n| GET | `/version` | the version |\n\n" +
		"| Header | Meaning |\n|---|---|\n| `Origo-Contract` | the contract version, `1` |\n"
	if err := os.WriteFile(filepath.Join(dir, "003-a.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	bare, err := specs.Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Page(bare); err == nil {
		t.Error("a deck that states no authorization contract renders a page anyway")
	}
}

func TestRunWritesAndReportsFindings(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "api.md")
	specsDir := filepath.Join(root(t), "specs")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-specs", specsDir, "-out", out, "-write"}, &stdout, &stderr); code != 0 {
		t.Fatalf("write: %d %q", code, stderr.String())
	}
	if body, err := os.ReadFile(out); err != nil || !bytes.Contains(body, []byte("# The Origo API")) {
		t.Fatalf("written page: %v", err)
	}
	stdout.Reset()
	if code := run([]string{"-specs", specsDir}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "# The Origo API") {
		t.Fatalf("print: %d", code)
	}
	if code := run([]string{"-specs", filepath.Join(dir, "nowhere")}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing specs: %d", code)
	}
	if code := run([]string{"-nosuchflag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
	// A deck with a finding renders nothing, and an unwritable path is
	// reported rather than ignored.
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "001-a.md"), []byte("---\ntitle: t\n---\n\nNames `origo_nowhere_total`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"-specs", bad}, &stdout, &stderr); code != 1 {
		t.Fatalf("finding: %d", code)
	}
	if code := run([]string{"-specs", specsDir, "-out", filepath.Join(dir, "nowhere", "api.md"), "-write"}, &stdout, &stderr); code != 1 {
		t.Fatalf("unwritable: %d", code)
	}
}
