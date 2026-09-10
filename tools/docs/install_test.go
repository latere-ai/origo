// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package docs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// root is the checkout, resolved from this file rather than from the
// working directory, so the test reads the tree wherever it is run from.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// blocks returns the fenced sh blocks of a document, in order, the way
// run-blocks.sh reads them.
func blocks(body string) []string {
	var out []string
	var cur []string
	in := false
	for l := range strings.SplitSeq(body, "\n") {
		switch {
		case !in && strings.TrimSpace(l) == "```sh":
			in, cur = true, nil
		case in && strings.TrimSpace(l) == "```":
			in = false
			out = append(out, strings.Join(cur, "\n"))
		case in:
			cur = append(cur, l)
		}
	}
	return out
}

// link matches a Markdown link target and a backticked path, the two
// forms docs/install.md names a file in the tree by.
var (
	link = regexp.MustCompile(`\]\(([^)]+)\)`)
	path = regexp.MustCompile("`(deploy/[A-Za-z0-9._/-]+)`")
)

// TestInstallDocumentIsWellFormed is the push-time half of spec 018's
// document criterion. The install job walks the blocks against a real
// cluster on every push; this runs in the test gate and catches the two
// failures that need no cluster: a block that is not valid shell, and a
// path or a link the document names that the tree does not carry.
func TestInstallDocumentIsWellFormed(t *testing.T) {
	dir := root(t)
	doc := filepath.Join(dir, "docs", "install.md")
	body, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}

	got := blocks(string(body))
	if len(got) < 5 {
		t.Fatalf("docs/install.md has %d sh blocks, too few to be the install", len(got))
	}
	for i, b := range got {
		cmd := exec.CommandContext(context.Background(), "/bin/sh", "-n")
		cmd.Stdin = strings.NewReader(b)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("block %d is not valid shell: %v\n%s\n%s", i+1, err, out, b)
		}
	}

	for _, m := range link.FindAllStringSubmatch(string(body), -1) {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "#") {
			continue
		}
		target, _, _ = strings.Cut(target, "#")
		if target == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "docs", target)); err != nil {
			t.Errorf("docs/install.md links to %q, which is not there: %v", m[1], err)
		}
	}
	for _, m := range path.FindAllStringSubmatch(string(body), -1) {
		if _, err := os.Stat(filepath.Join(dir, m[1])); err != nil {
			t.Errorf("docs/install.md names %q, which is not there: %v", m[1], err)
		}
	}
}

// TestInstallWaitsForTheAddress holds the rule the install job's failure
// on run 34452566717 fixed: a rollout reports ready a moment before the
// datapath routes to the new pods, so the first request the document
// makes through $ORIGO_URL is reset. The block that names the address
// first retries until it answers, and the retry is bounded, so a wrong
// hostname fails the walk instead of hanging until the job's timeout.
func TestInstallWaitsForTheAddress(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(root(t), "docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range blocks(string(body)) {
		if !strings.Contains(b, "$ORIGO_URL") {
			continue
		}
		if !strings.Contains(b, `until curl -sf "$ORIGO_URL/version"`) {
			t.Errorf("block %d reaches $ORIGO_URL without waiting for it to answer:\n%s", i+1, b)
		}
		if !strings.Contains(b, "exit 1") {
			t.Errorf("block %d waits for $ORIGO_URL without a bound:\n%s", i+1, b)
		}
		return
	}
	t.Fatal("no block of docs/install.md names $ORIGO_URL")
}
