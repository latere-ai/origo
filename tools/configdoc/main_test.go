// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
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
