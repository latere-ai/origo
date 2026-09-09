// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deck writes a two-spec directory with a README carrying the markers,
// so every mode of the command runs against a tree of its own.
func deck(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"001-a.md": "---\ntitle: t\n---\n\n| Metric | Type |\n|---|---|\n| `origo_pushes_total` | counter |\n",
		"README.md": "# Specs\n\n<!-- specindex:begin -->\n" +
			"| Kind | Name | Owner | Also named in |\n|---|---|---|---|\n" +
			"| metric | `origo_pushes_total` | [001](001-a.md) | - |\n" +
			"<!-- specindex:end -->\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runIn(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCheckWriteAndDefaultModes(t *testing.T) {
	dir := deck(t)
	if code, out, errOut := runIn(t, "-specs", dir, "-check"); code != 0 || !strings.Contains(out, "README.md current") {
		t.Fatalf("check: %d %q %q", code, out, errOut)
	}
	if code, out, _ := runIn(t, "-specs", dir); code != 0 || !strings.Contains(out, "origo_pushes_total") {
		t.Fatalf("default: %d %q", code, out)
	}
	if code, out, errOut := runIn(t, "-specs", dir, "-write"); code != 0 || !strings.Contains(out, "1 names written") {
		t.Fatalf("write: %d %q %q", code, out, errOut)
	}
}

func TestDriftAndMissingDirectoryFail(t *testing.T) {
	dir := deck(t)
	readme := filepath.Join(dir, "README.md")
	body, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readme, bytes.ReplaceAll(body, []byte("origo_pushes_total"), []byte("origo_other_total")), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runIn(t, "-specs", dir, "-check"); code != 1 || !strings.Contains(errOut, "differs from the specs") {
		t.Fatalf("drift: %d %q", code, errOut)
	}
	if code, _, _ := runIn(t, "-specs", filepath.Join(dir, "nowhere")); code != 1 {
		t.Fatalf("missing directory: %d", code)
	}
	if code, _, _ := runIn(t, "-nosuchflag"); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
}

func TestRulesModePrintsTheDocumentAndFailsOnAnUnknownMetric(t *testing.T) {
	dir := deck(t)
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("spec:\n  groups:\n    - name: g\n      rules:\n        - alert: A\n          expr: rate(origo_pushes_total[5m]) > 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runIn(t, "-specs", dir, "-rules", good)
	if code != 0 || !strings.Contains(out, "- alert: A") {
		t.Fatalf("rules: %d %q %q", code, out, errOut)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("spec:\n  groups:\n    - name: g\n      rules:\n        - alert: B\n          expr: origo_nowhere_total > 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runIn(t, "-specs", dir, "-rules", bad); code != 1 || !strings.Contains(errOut, "which no spec defines") {
		t.Fatalf("unknown metric: %d %q", code, errOut)
	}
	if code, _, _ := runIn(t, "-specs", dir, "-rules", filepath.Join(dir, "nowhere.yaml")); code != 1 {
		t.Fatalf("missing rules file: %d", code)
	}
}

func TestMissingMarkersAndFindingsExitOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Specs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A spec that names a metric no spec defines: a finding, and a
	// README with no markers, which neither mode can read or rewrite.
	if err := os.WriteFile(filepath.Join(dir, "001-a.md"), []byte("---\ntitle: t\n---\n\nNames `origo_nowhere_total`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"-check", "-write"} {
		code, _, errOut := runIn(t, "-specs", dir, mode)
		if code != 1 || !strings.Contains(errOut, "no spec defines it") || !strings.Contains(errOut, "markers") {
			t.Fatalf("%s: %d %q", mode, code, errOut)
		}
	}
}
