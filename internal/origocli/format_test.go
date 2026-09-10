// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli

import (
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/origoclient"
)

// These are the pure pieces of the byte rules. They are tested in the package
// rather than through a command because each one has edges a command would
// only reach by accident, and every one of them is a line a reader sees.

func TestShortAndSubjectHoldTheirColumns(t *testing.T) {
	if got := short("abcdef0123456789"); got != "abcdef0" {
		t.Fatalf("short = %q", got)
	}
	// A short id is already short; nothing is added and nothing is cut.
	if got := short("abc"); got != "abc" {
		t.Fatalf("short of a short id = %q", got)
	}
	long := strings.Repeat("a", 100)
	got := subject(long + "\n\nthe body")
	if len(got) != subjectMax || !strings.HasSuffix(got, "...") {
		t.Fatalf("subject = %q (%d)", got, len(got))
	}
	if got := subject("  a short one  \nbody"); got != "a short one" {
		t.Fatalf("subject = %q", got)
	}
	if got := subject(""); got != "" {
		t.Fatalf("subject of an empty message = %q", got)
	}
}

func TestDayAndStampAreUTC(t *testing.T) {
	at := time.Date(2026, 9, 10, 23, 30, 0, 0, time.FixedZone("east", 2*60*60))
	if got := day(at); got != "2026-09-10" {
		t.Fatalf("day = %q; a local date would read 2026-09-11", got)
	}
	if got := stamp(at); got != "2026-09-10T21:30:00Z" {
		t.Fatalf("stamp = %q", got)
	}
}

func TestEntryLineMarksWhatEachEntryIs(t *testing.T) {
	size := int64(4096)
	cases := []struct{ path, mode, kind, want string }{
		{"internal", "040000", "tree", "       -  internal/"},
		{"run.sh", "100755", "blob", "    4096  run.sh*"},
		{"link", "120000", "blob", "    4096  link@"},
		{"go.mod", "100644", "blob", "    4096  go.mod"},
	}
	for _, tc := range cases {
		var s *int64
		if tc.kind != "tree" {
			s = &size
		}
		if got := entryLine(tc.path, tc.mode, tc.kind, s); got != tc.want {
			t.Fatalf("entryLine(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRenderPrintsEachDetailInTheRegisterALineTakes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"absent", nil, ""},
		{"a string", "main", "main"},
		{"a whole number without its point", float64(300), "300"},
		{"a fraction keeps its point", 1.5, "1.5"},
		{"a flag", true, "true"},
		{"a list, in a stable order", []any{"b.go", "a.go"}, "a.go,b.go"},
		{"a list of numbers", []any{float64(2), float64(1)}, "1,2"},
		{"anything else as the JSON it arrived as", map[string]any{"k": "v"}, `{"k":"v"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(tc.in); got != tc.want {
				t.Fatalf("render(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWindowIsOnlyAboutTheNodesBlobLimit(t *testing.T) {
	small := int64(1024)
	if got := window(&small); got != nil {
		t.Fatalf("a small blob asked for a Range: %+v", got)
	}
	if got := window(nil); got != nil {
		t.Fatalf("a tree asked for a Range: %+v", got)
	}
	// The cap is the node's, measured against the requested length, so the
	// window is exactly what it will serve at once.
	big := int64(origoclient.MaxBlobBytes + 1)
	got := window(&big)
	if got == nil || got.First != 0 || got.Last != origoclient.MaxBlobBytes-1 {
		t.Fatalf("window = %+v", got)
	}
}

func TestSizeAndOrDashNameWhatIsAbsent(t *testing.T) {
	n := int64(12)
	if size(&n) != "12" || size(nil) != "-" {
		t.Fatalf("size: %q %q", size(&n), size(nil))
	}
	if orDash("") != "-" || orDash("abc") != "abc" {
		t.Fatal("orDash")
	}
}

func TestPatchCapZeroMeansEveryByte(t *testing.T) {
	if got := patchCap(0, 500); got != 500 {
		t.Fatalf("patchCap(0, 500) = %d", got)
	}
	if got := patchCap(100, 500); got != 100 {
		t.Fatalf("patchCap(100, 500) = %d", got)
	}
}

func TestParseDiffReadsTheMarkersRatherThanTheHeader(t *testing.T) {
	// A rename: the header names both sides, and the +++ marker is what the
	// count belongs to.
	diff := "diff --git a/old.go b/new.go\n" +
		"--- a/old.go\n+++ b/new.go\n@@\n+one\n+two\n-gone\n" +
		"diff --git a/bin b/bin\nBinary files a/bin and b/bin differ\n" +
		"diff --git a/added.go b/added.go\n--- /dev/null\n+++ b/added.go\n@@\n+new\n"
	files := parseDiff([]byte(diff))
	if len(files) != 3 {
		t.Fatalf("%d files: %+v", len(files), files)
	}
	if files[0].Path != "new.go" || files[0].Additions != 2 || files[0].Deletions != 1 {
		t.Fatalf("the rename: %+v", files[0])
	}
	if !files[1].Binary {
		t.Fatalf("the binary file: %+v", files[1])
	}
	if files[2].Path != "added.go" || files[2].Additions != 1 {
		t.Fatalf("the added file: %+v", files[2])
	}
	lines := statLines(files)
	if lines[1] != "binary  bin" {
		t.Fatalf("a binary file's line: %q", lines[1])
	}
	// A binary file counts as a file and adds no lines to the totals.
	if lines[len(lines)-1] != "3 files  +3 -1" {
		t.Fatalf("totals: %q", lines[len(lines)-1])
	}
	if got := statLines(nil); got[0] != "0 files  +0 -0" {
		t.Fatalf("an empty comparison: %q", got[0])
	}
	if got := statLines([]fileStat{{Path: "a", Additions: 1}}); got[1] != "1 file  +1 -0" {
		t.Fatalf("one file: %q", got[1])
	}
	// Text before the first header belongs to no file and is not counted.
	if got := parseDiff([]byte("preamble\n+not a change\n")); len(got) != 0 {
		t.Fatalf("preamble produced %+v", got)
	}
	// A header with no b/ side falls back to the a/ side.
	if got := parseDiff([]byte("diff --git a/only.go\n@@\n+one\n")); got[0].Path != "only.go" {
		t.Fatalf("fallback path: %+v", got[0])
	}
}

func TestCutPatchCutsAtAFileBoundary(t *testing.T) {
	first := "diff --git a/a.go b/a.go\n" + strings.Repeat("+line\n", 20)
	second := "diff --git a/b.go b/b.go\n" + strings.Repeat("+line\n", 20)
	whole := []byte(first + second)
	if got, cut := cutPatch(whole, len(whole)); cut || len(got) != len(whole) {
		t.Fatalf("a patch inside the bound was cut")
	}
	// The bound must reach past the whole marker: cutPatch looks for
	// "\ndiff --git ", so a bound landing inside that marker has no
	// boundary to cut at and keeps what it has.
	const marker = "diff --git "
	got, cut := cutPatch(whole, len(first)+len(marker))
	if !cut || string(got) != first {
		t.Fatalf("the cut was not at the boundary: %q", got)
	}
	// A bound landing inside the marker finds no boundary and cuts at the
	// bound, which is reported rather than passed off as complete.
	got, cut = cutPatch(whole, len(first)+len(marker)-2)
	if !cut || len(got) != len(first)+len(marker)-2 {
		t.Fatalf("a bound inside the marker: %d %v", len(got), cut)
	}
	// A single file past the bound has no boundary to cut at, so it is cut
	// at the bound and reported.
	one := []byte(first)
	got, cut = cutPatch(one, 10)
	if !cut || len(got) != 10 {
		t.Fatalf("a single oversized file: %d %v", len(got), cut)
	}
}

func TestLineWindowCountsTheFileAndNotTheWindow(t *testing.T) {
	text := "a\nb\nc\nd\n"
	win, shown, taken, total := lineWindow(text, 1, 2, 1000)
	if win != "b\nc\n" || shown != 2 || taken != 4 || total != 4 {
		t.Fatalf("%q %d %d %d", win, shown, taken, total)
	}
	// A trailing newline is a terminator, not an empty last line.
	if _, _, _, total := lineWindow("a\n", 0, 10, 1000); total != 1 {
		t.Fatalf("total = %d", total)
	}
	if _, _, _, total := lineWindow("a", 0, 10, 1000); total != 1 {
		t.Fatalf("total of an unterminated file = %d", total)
	}
	// An offset past the end is an empty window, not an error and not a wrap.
	win, shown, _, total = lineWindow(text, 9, 2, 1000)
	if win != "" || shown != 0 || total != 4 {
		t.Fatalf("%q %d %d", win, shown, total)
	}
	// The byte budget stops the window at a line boundary.
	win, shown, taken, _ = lineWindow(text, 0, 10, 5)
	if win != "a\nb\n" || shown != 2 || taken != 4 {
		t.Fatalf("%q %d %d", win, shown, taken)
	}
	// The first line is always emitted, even past the budget, because a
	// fragment is not a line.
	win, shown, taken, _ = lineWindow("averylongline\nb\n", 0, 10, 3)
	if win != "averylongline\n" || shown != 1 || taken <= 3 {
		t.Fatalf("%q %d %d", win, shown, taken)
	}
}

func TestIndentAndTruncationVocabulary(t *testing.T) {
	got := indent("one\ntwo\n")
	if len(got) != 2 || got[0] != "    one" || got[1] != "    two" {
		t.Fatalf("indent = %q", got)
	}
	if got := truncation("20 of 300", "raise -n"); got != "[truncated: 20 of 300; raise -n]" {
		t.Fatalf("truncation = %q", got)
	}
	if got := wall("the cap"); got != "[truncated: the cap]" {
		t.Fatalf("wall = %q", got)
	}
	if got := staleLine("37"); !strings.HasPrefix(got, "[stale:") || !strings.Contains(got, "37 seconds") {
		t.Fatalf("staleLine = %q", got)
	}
	if !binaryHead([]byte("a\x00b")) || binaryHead([]byte("plain text")) {
		t.Fatal("binaryHead")
	}
}

func TestParseAuthorSplitsTheVariable(t *testing.T) {
	got, err := parseAuthor("Ada Lovelace <ada@example.com>")
	if err != nil || got.Name != "Ada Lovelace" || got.Email != "ada@example.com" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"", "Ada Lovelace", "Ada <ada.example.com>", "<ada@example.com>", "Ada >ada@example.com<"} {
		if _, err := parseAuthor(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

func TestOperationOfAndSubcommandParsing(t *testing.T) {
	name, rest := subcommand([]string{"-json", "log", "-n", "5"})
	if name != "log" || strings.Join(rest, " ") != "-json -n 5" {
		t.Fatalf("%q %v", name, rest)
	}
	if name, rest := subcommand(nil); name != "" || len(rest) != 0 {
		t.Fatalf("%q %v", name, rest)
	}
	if name, _ := subcommand([]string{"-h"}); name != "-h" {
		t.Fatalf("a lone flag = %q", name)
	}
}
