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

func TestRunPrintsWritesAndReportsAFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "# Configuration") {
		t.Fatalf("print: %d %q", code, stderr.String())
	}
	out := filepath.Join(t.TempDir(), "configuration.md")
	stdout.Reset()
	if code := run([]string{"-write", "-out", out}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "wrote "+out) {
		t.Fatalf("write: %d %q", code, stderr.String())
	}
	if code := run([]string{"-write", "-out", filepath.Join(out, "nowhere.md")}, &stdout, &stderr); code != 1 {
		t.Fatalf("unwritable: %d", code)
	}
	if code := run([]string{"-nosuchflag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
}

// TestWriteReproducesTheCommittedPage is the check `make docs` followed
// by `git diff --exit-code docs/` used to be in CI: the generator writes
// into a directory of its own and the page must equal the committed
// docs/configuration.md byte for byte, so a variable changed without
// regenerating the page fails the suite on the push that changed it.
func TestWriteReproducesTheCommittedPage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "configuration.md")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-write", "-out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("write: %d %q", code, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The package's directory is two levels below the repository root.
	want, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("docs/configuration.md differs from what the generator writes; run make docs")
	}
}
