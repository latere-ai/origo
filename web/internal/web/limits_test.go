// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// TestFileViewLimits holds the three cases that decide whether the file
// screen is usable on a real repository: a binary file, a file larger than
// the render limit, and a file the request itself cuts short.
func TestFileViewLimits(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	t.Run("binary shows type, size and a download and no bytes", func(t *testing.T) {
		h.fake.Reset()
		rec := h.get(h.repoPath("/blob/logo.png"), c)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		for _, want := range []string{"not a text file", "48 KB", "image/png", "/raw/logo.png"} {
			if !strings.Contains(body, want) {
				t.Errorf("the page does not say %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "PNG") {
			t.Error("the bytes of a binary file reached the page")
		}
		if n := len(elements(doc(t, body), "table")); n != 0 {
			t.Errorf("a binary file rendered %d source tables", n)
		}
	})

	t.Run("a file over the render limit is never fetched", func(t *testing.T) {
		h.fake.Reset()
		rec := h.get(h.repoPath("/blob/huge.json"), c)
		body := rec.Body.String()
		if !strings.Contains(body, "too large to show here") {
			t.Errorf("the page does not say the file is too large:\n%s", body)
		}
		if !strings.Contains(body, "60 MB") {
			t.Error("the page does not give the size")
		}
		if !strings.Contains(body, "git clone https://git.example/infra/origo.git") {
			t.Error("the page does not offer the clone")
		}
		for _, call := range h.fake.Calls() {
			if strings.Contains(call, "/blob/") {
				t.Errorf("the bytes were fetched anyway: %s", call)
			}
		}
	})

	t.Run("a ten megabyte text file is cut by the request itself", func(t *testing.T) {
		h.fake.Reset()
		rec := h.get(h.repoPath("/blob/big.txt"), c)
		if got := h.fake.blobRanges(); len(got) != 1 || got[0] != "bytes=0-1048575" {
			t.Errorf("the blob call carried %v, want one Range of the render limit", got)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Only the first part of this file is shown") {
			t.Errorf("the page does not say it was cut:\n%s", body[:min(600, len(body))])
		}
	})

	t.Run("a file cut without a partial status still says it was cut", func(t *testing.T) {
		// A proxy that strips the Range header leaves the installation
		// answering 200 with the whole file, and one that answers 200
		// with part of it has still cut it. The notice follows the
		// bytes on the page, not the status of the response.
		h.fake.ignoreRange = true
		defer func() { h.fake.ignoreRange = false }()
		body := h.get(h.repoPath("/blob/big.txt"), c).Body.String()
		if !strings.Contains(body, "Only the first part of this file is shown") {
			t.Error("a file cut by the reader's own limit did not say so")
		}
	})
}

// TestLargeDiffRenderBudget asserts the two budgets a commit screen applies
// before a byte of HTML is written, and that a five thousand line diff is
// rendered well inside the budget the spec sets.
func TestLargeDiffRenderBudget(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	h.fake.diff = syntheticDiff(5000)

	start := time.Now()
	rec := h.get(h.repoPath("/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"), c)
	took := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if took > 500*time.Millisecond {
		t.Errorf("a 5 000 line diff took %v to render, the budget is 500ms", took)
	}

	// Past the per-file collapse the file is rendered shut, and its body is
	// still in the document, so opening it costs no request.
	page := doc(t, rec.Body.String())
	details := elements(page, "details")
	if len(details) == 0 {
		t.Fatal("no file was rendered")
	}
	for _, d := range details {
		if attr(d, "open") != "" {
			t.Error("a file over the collapse budget rendered open")
		}
	}
	if n := len(elements(page, "tr")); n < 5000 {
		t.Errorf("the collapsed file holds %d rows, so its body is not there", n)
	}
}

// TestTruncatedDiffRenders asserts what a commit whose diff Origo cut shows:
// every file it received, the notice, and a link that re-requests one path.
func TestTruncatedDiffRenders(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	h.fake.diff = syntheticDiff(20) + syntheticFile("second.go", 30)
	h.fake.headers["/v1/repos/1f2e3d/compare/4c02f7e10b4d3a1e8f6b2c9d05a7e3f1b8c4d6e2...9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"] =
		http.Header{"Origo-Truncated": []string{"true"}}

	rec := h.get(h.repoPath("/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"), c)
	body := rec.Body.String()
	if !strings.Contains(body, "cut this diff short") {
		t.Errorf("no truncation notice:\n%s", body)
	}
	for _, want := range []string{"generated.go", "second.go"} {
		if !strings.Contains(body, want) {
			t.Errorf("a file the installation did send is missing: %s", want)
		}
	}

	// The per-file link re-requests exactly one path.
	var only string
	find(doc(t, body), func(e *html.Node) {
		if e.Data == "a" && strings.Contains(attr(e, "href"), "path=") {
			only = attr(e, "href")
		}
	})
	if only == "" {
		t.Fatal("no link re-requests one path")
	}
	h.fake.Reset()
	if rec := h.get(only, c); rec.Code != http.StatusOK {
		t.Fatalf("the one-path link answered %d", rec.Code)
	}
	var asked bool
	for _, call := range h.fake.Calls() {
		if strings.Contains(call, "compare") && strings.Contains(call, "path") {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the link did not re-request one path: %v", h.fake.Calls())
	}
}

// TestPagingIsExact asserts that the tree and the log page with Origo's
// cursors, and that every entry appears once over the pages.
func TestPagingIsExact(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")

	h.fake.trees["internal"] = manyEntries(120)
	h.fake.nextCursor = "internal/f119.go"
	body := h.get(h.repoPath("/tree/internal"), c).Body.String()
	if !strings.Contains(body, "cursor=internal%2Ff119.go") {
		t.Errorf("the next page link does not carry the cursor:\n%s", body[:min(len(body), 400)])
	}

	seen := map[string]int{}
	for _, a := range elements(doc(t, body), "a") {
		if strings.Contains(attr(a, "href"), "/blob/") {
			seen[attr(a, "href")]++
		}
	}
	if len(seen) != 120 {
		t.Errorf("the page shows %d entries, the installation returned 120", len(seen))
	}
	for href, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times", href, n)
		}
	}

	h.fake.commits = manyCommits(50)
	body = h.get(h.repoPath("/log"), c).Body.String()
	if !strings.Contains(body, "cursor=") {
		t.Error("the log offers no next page while the installation returned a cursor")
	}
}

// TestAccessibleStructure asserts the structural half of the accessibility
// criterion, which is the half a document carries: one h1 per page, a header
// row with scope on every table, a label on every control, and a caption that
// names what a table holds.
func TestAccessibleStructure(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	for name, path := range h.screens() {
		page := doc(t, h.get(path, c).Body.String())

		if n := len(elements(page, "h1")); n != 1 {
			t.Errorf("%s has %d h1 elements, want one", name, n)
		}
		if len(elements(page, "html")) > 0 && attr(elements(page, "html")[0], "lang") == "" {
			t.Errorf("%s does not name its language", name)
		}
		for _, table := range elements(page, "table") {
			if len(elements(table, "caption")) == 0 {
				t.Errorf("%s: a table with no caption", name)
			}
			for _, th := range elements(table, "th") {
				if attr(th, "scope") == "" {
					t.Errorf("%s: a header cell with no scope: %q", name, text(th))
				}
			}
		}
		labels := map[string]bool{}
		for _, l := range elements(page, "label") {
			labels[attr(l, "for")] = true
		}
		for _, tag := range []string{"input", "select", "textarea"} {
			for _, in := range elements(page, tag) {
				if attr(in, "type") == "hidden" {
					continue
				}
				if id := attr(in, "id"); id == "" || !labels[id] {
					t.Errorf("%s: a %s control with no label", name, tag)
				}
			}
		}
	}

	// The stylesheet defines a visible focus treatment that is an outline
	// and not a colour alone, and a dark theme for every token.
	css := string(mustAsset(t, "app.css"))
	for _, want := range []string{":focus-visible", "outline:", "prefers-color-scheme: dark"} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet has no %q", want)
		}
	}
}

func mustAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := assetFS.ReadFile("assets/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// syntheticDiff makes a diff of one file with n changed lines.
func syntheticDiff(n int) string { return syntheticFile("generated.go", n) }

func syntheticFile(path string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nindex 1..2 100644\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", n, n)
	for i := range n {
		fmt.Fprintf(&b, "+line %d of the change\n", i)
	}
	return b.String()
}
