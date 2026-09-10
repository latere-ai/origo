// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo-web/internal/origo"
)

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB", 4404019: "4.2 MB",
		13 << 20: "13 MB", 3 << 30: "3.0 GB", 5 << 40: "5.0 TB", 7 << 50: "7.0 PB",
		1 << 60: "1 EB", -1: "",
	} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) is %q, want %q", n, got, want)
		}
	}
}

func TestRelTimeReadsAsAPersonReadsIt(t *testing.T) {
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return base }
	t.Cleanup(func() { now = time.Now })

	for d, want := range map[time.Duration]string{
		-time.Minute:        "just now",
		30 * time.Second:    "just now",
		time.Minute:         "1 minute ago",
		14 * time.Minute:    "14 minutes ago",
		2 * time.Hour:       "2 hours ago",
		30 * time.Hour:      "yesterday",
		6 * 24 * time.Hour:  "6 days ago",
		45 * 24 * time.Hour: "1 month ago",
		90 * 24 * time.Hour: "3 months ago",
	} {
		if got := relTime(base.Add(-d)); got != want {
			t.Errorf("%v ago reads as %q, want %q", d, got, want)
		}
	}
	// Past about a year the date is more use than the count.
	if got := relTime(base.Add(-500 * 24 * time.Hour)); got != "28 Apr 2025" {
		t.Errorf("an old commit reads as %q", got)
	}
	if got := relTime(time.Time{}); got != "" {
		t.Errorf("a time that is not there reads as %q", got)
	}
	if got := absTime(time.Time{}); got != "" {
		t.Errorf("an absent exact time reads as %q", got)
	}
	if got := absTime(base); got != "10 Sep 2026, 12:00 +0000" {
		t.Errorf("the exact time reads as %q", got)
	}
}

func TestStaleSentenceScales(t *testing.T) {
	for secs, want := range map[int]string{0: "0 seconds", 45: "45 seconds", 180: "3 minutes", 7200: "2 hours"} {
		got := staleSentence(origo.Meta{StaleSet: true, Stale: time.Duration(secs) * time.Second})
		if got != want {
			t.Errorf("%ds stale reads as %q, want %q", secs, got, want)
		}
	}
	if got := staleSentence(origo.Meta{}); got != "" {
		t.Errorf("a consistent response reads as %q", got)
	}
}

func TestPathsAreCleanedAndBrokenIntoCrumbs(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		"/a/b/":              "a/b",
		"a//b":               "a/b",
		"a/./b":              "a/b",
		"a/../b":             "b",
		"../../etc/passwd":   "etc/passwd",
		"internal/diff/x.go": "internal/diff/x.go",
	} {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) is %q, want %q", in, got, want)
		}
	}
	if got := parentPath("a/b/c"); got != "a/b" {
		t.Errorf("the parent of a/b/c is %q", got)
	}
	if got := parentPath("a"); got != "" {
		t.Errorf("the parent of a root entry is %q", got)
	}

	crumbs := pathCrumbs("/r/x", "next", "a/b/c.go")
	if len(crumbs) != 3 {
		t.Fatalf("a three segment path broke into %d crumbs", len(crumbs))
	}
	if crumbs[0].URL != "/r/x/tree/a?ref=next" {
		t.Errorf("the first crumb points at %q", crumbs[0].URL)
	}
	if crumbs[2].URL != "" {
		t.Error("the page itself was rendered as a link to itself")
	}
	if pathCrumbs("/r/x", "", "") != nil {
		t.Error("the root broke into crumbs")
	}
	if got := escapePath("a b/c?d"); got != "a%20b/c%3Fd" {
		t.Errorf("a path with characters a URL reserves escaped as %q", got)
	}
}

func TestEntryTypeNamesWhatAListingMustDistinguish(t *testing.T) {
	for _, tc := range []struct {
		entry origo.Entry
		want  string
	}{
		{origo.Entry{Type: "tree"}, "directory"},
		{origo.Entry{Type: "commit"}, "submodule"},
		{origo.Entry{Type: "blob", Mode: "120000"}, "symlink"},
		{origo.Entry{Type: "blob", Mode: "100755"}, "executable"},
		{origo.Entry{Type: "blob", Mode: "100644"}, "file"},
	} {
		if got := entryType(tc.entry); got != tc.want {
			t.Errorf("%+v reads as %q, want %q", tc.entry, got, tc.want)
		}
	}
}

func TestTextualDecidesWhatIsRenderedAsText(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/plain; charset=utf-8": true,
		"text/html":                 true,
		"application/json":          true,
		"application/xhtml+xml":     true,
		"application/ld+json":       true,
		"application/yaml":          true,
		"image/png":                 false,
		"application/octet-stream":  false,
		"":                          false,
	} {
		if got := textual(ct); got != want {
			t.Errorf("textual(%q) is %v", ct, got)
		}
	}
	if got := describeType("text/plain; charset=utf-8", "file"); got != "text/plain" {
		t.Errorf("the type reads as %q", got)
	}
	if got := describeType("", "symlink"); got != "symlink" {
		t.Errorf("a type the installation did not send reads as %q", got)
	}
}

func TestSourceLinesAreBounded(t *testing.T) {
	if got := sourceLines(""); got != nil {
		t.Errorf("an empty file broke into %v", got)
	}
	got := sourceLines("a\r\nb\n")
	if len(got) != 2 || got[0].Text != "a" || got[1].N != 2 {
		t.Errorf("two lines read as %+v", got)
	}
	// A minified file is one line of megabytes, and a browser asked to lay
	// that out stops being a browser.
	long := sourceLines(strings.Repeat("x", maxLineRunes+500))
	if len([]rune(long[0].Text)) > maxLineRunes+2 {
		t.Errorf("a very long line was rendered whole: %d runes", len([]rune(long[0].Text)))
	}
	if !strings.HasSuffix(long[0].Text, "…") {
		t.Error("a cut line does not say it was cut")
	}
}

// TestReadmeIsNotAVectorForScript asserts that a readme in a repository
// nobody vetted cannot put a script, a handler, or a javascript: URL on this
// origin. The renderer runs without its unsafe option and the page carries a
// policy that refuses a script in any case.
func TestReadmeIsNotAVectorForScript(t *testing.T) {
	nasty := []byte("# Title\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n" +
		"[a link](javascript:alert(1))\n\n<a href=\"javascript:alert(1)\">two</a>\n")
	html, text := renderReadme("README.md", nasty)
	if text != "" {
		t.Fatal("a Markdown readme was not rendered as Markdown")
	}
	out := string(html)
	for _, forbidden := range []string{"<script", "onerror=", "javascript:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%q survived the renderer:\n%s", forbidden, out)
		}
	}

	// A readme that is not Markdown is text, and stays text.
	h2, plain := renderReadme("README", []byte("<b>not markup</b>"))
	if h2 != "" || plain != "<b>not markup</b>" {
		t.Errorf("a plain readme rendered as %q / %q", h2, plain)
	}
}

// TestReadmeHeadingsNestUnderThePage asserts that a readme's own h1 becomes
// an h2, so the page keeps one h1 and the document keeps an outline.
func TestReadmeHeadingsNestUnderThePage(t *testing.T) {
	out := string(mustRender(t, "# One\n\n## Two\n\n### Three\n"))
	if strings.Contains(out, "<h1") {
		t.Errorf("a readme heading stayed an h1:\n%s", out)
	}
	for _, want := range []string{"<h2", "<h3", "<h4"} {
		if !strings.Contains(out, want) {
			t.Errorf("the headings did not move down: %s missing from\n%s", want, out)
		}
	}
	// Text that is not markup comes back unchanged.
	if got := string(demoteHeadings([]byte("<p>plain</p>"))); !strings.Contains(got, "plain") {
		t.Errorf("a fragment with no heading came back as %q", got)
	}
}

func mustRender(t *testing.T, src string) []byte {
	t.Helper()
	h, _ := renderReadme("README.md", []byte(src))
	return []byte(h)
}

func TestFindReadmePrefersMarkdown(t *testing.T) {
	entries := []origo.Entry{
		{Path: "docs", Type: "tree"},
		{Path: "README", Type: "blob"},
		{Path: "README.md", Type: "blob"},
	}
	if got, ok := findReadme(entries); !ok || got.Path != "README.md" {
		t.Errorf("the readme was chosen as %+v", got)
	}
	if got, ok := findReadme(entries[:2]); !ok || got.Path != "README" {
		t.Errorf("a plain readme was chosen as %+v", got)
	}
	if _, ok := findReadme([]origo.Entry{{Path: "main.go", Type: "blob"}}); ok {
		t.Error("a repository with no readme was read as having one")
	}
}

func TestRefQueryIsEmptyOnTheDefaultBranch(t *testing.T) {
	r := repoView{ID: "r 1", DefaultBranch: "main", Ref: "main"}
	if got := r.RefQuery(); got != "" {
		t.Errorf("the default branch carried %q", got)
	}
	if got := r.URL(); got != "/r/r%201" {
		t.Errorf("an identifier with a space became %q", got)
	}
	r.Ref = "release/1.4"
	if got := r.RefQuery(); got != "?ref=release%2F1.4" {
		t.Errorf("a branch with a slash carried %q", got)
	}
	if got := r.RefQueryValue(); got != "release/1.4" {
		t.Errorf("the reference value is %q", got)
	}
}
