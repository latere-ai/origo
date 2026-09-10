// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package diff

import (
	"fmt"
	"strings"
	"testing"
)

const sample = `diff --git a/internal/diff/render.go b/internal/diff/render.go
index 1111111..2222222 100644
--- a/internal/diff/render.go
+++ b/internal/diff/render.go
@@ -38,6 +38,8 @@ func Render(w io.Writer, p *Patch) error {
 	for _, f := range p.Files {
 		if f.Binary {
-			writeStub(w, f, "binary")
+			writeStub(w, f, reasonBinary)
 			continue
+		}
+		if f.Lines() > p.Budget {
 		}
diff --git a/web/static/logo.png b/web/static/logo.png
index 3333333..4444444 100644
Binary files a/web/static/logo.png and b/web/static/logo.png differ
diff --git a/old/name.go b/new/name.go
similarity index 92%
rename from old/name.go
rename to new/name.go
--- a/old/name.go
+++ b/new/name.go
@@ -1,2 +1,2 @@
-package old
+package new
diff --git a/added.txt b/added.txt
new file mode 100644
index 0000000..5555555
--- /dev/null
+++ b/added.txt
@@ -0,0 +1,1 @@
+the first line
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 6666666..0000000
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-the last line
`

func TestParseReadsWhatGitWrote(t *testing.T) {
	p := Parse([]byte(sample), DefaultBudget)
	if len(p.Files) != 5 {
		t.Fatalf("read %d files, the text holds 5", len(p.Files))
	}

	first := p.Files[0]
	if first.Path != "internal/diff/render.go" {
		t.Errorf("the first file is %q", first.Path)
	}
	if first.Status != "modified" {
		t.Errorf("the first file is %q, want modified", first.Status)
	}
	if first.Additions != 3 || first.Deletions != 1 {
		t.Errorf("the first file counts +%d -%d, want +3 -1", first.Additions, first.Deletions)
	}

	// The line numbers follow both sides of the hunk header.
	var firstAdd, firstDel Row
	for _, r := range first.Rows {
		if r.IsAdd() && firstAdd.Text == "" {
			firstAdd = r
		}
		if r.IsDel() && firstDel.Text == "" {
			firstDel = r
		}
	}
	if firstDel.Old != 40 || firstDel.New != 0 {
		t.Errorf("a removed line is numbered %d/%d, want 40 on the base side alone", firstDel.Old, firstDel.New)
	}
	if firstAdd.New != 40 || firstAdd.Old != 0 {
		t.Errorf("an added line is numbered %d/%d, want 40 on the head side alone", firstAdd.Old, firstAdd.New)
	}
	if firstAdd.Sign != "+" || firstDel.Sign != "-" {
		t.Error("a changed line lost the literal sign git wrote, which is what makes the diff readable in greyscale")
	}

	if !p.Files[1].Binary || len(p.Files[1].Rows) != 0 {
		t.Error("a binary file was read as text")
	}
	if p.Files[2].Status != "renamed" || p.Files[2].OldPath != "old/name.go" || p.Files[2].Path != "new/name.go" {
		t.Errorf("a rename read as %+v", p.Files[2])
	}
	if p.Files[3].Status != "added" || p.Files[4].Status != "deleted" {
		t.Errorf("an addition read as %q and a deletion as %q", p.Files[3].Status, p.Files[4].Status)
	}
	if p.Additions != 5 || p.Deletions != 3 {
		t.Errorf("the totals are +%d -%d", p.Additions, p.Deletions)
	}
}

func TestParseHandlesTextGitDoesNotWrite(t *testing.T) {
	if got := Parse(nil, DefaultBudget); len(got.Files) != 0 {
		t.Error("an empty diff read as files")
	}
	if got := Parse([]byte("not a diff at all\n"), DefaultBudget); len(got.Files) != 0 {
		t.Error("text with no header read as a file")
	}
	// A zero budget is the default budget, so a caller cannot switch the
	// limits off by leaving the struct empty.
	big := Parse([]byte(synthetic("f.go", 6000)), Budget{})
	if !big.Files[0].OverBudget {
		t.Error("a zero budget rendered a six thousand line file")
	}
	// A missing final newline is git's own remark and is kept as one.
	p := Parse([]byte("diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n"), DefaultBudget)
	var notes int
	for _, r := range p.Files[0].Rows {
		if r.IsNote() {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("git's remark about the final newline was read as %d notes", notes)
	}
}

// TestTruncatedDiffRenders is the parser's half of the criterion: every file
// the installation sent is read, a file past the per-file budget keeps its
// counts and loses only its body, and one named path is rendered whatever its
// size.
func TestTruncatedDiffRenders(t *testing.T) {
	text := synthetic("small.go", 10) + synthetic("generated.json", 6000)
	p := Parse([]byte(text), DefaultBudget)
	if len(p.Files) != 2 {
		t.Fatalf("read %d files, want both the installation sent", len(p.Files))
	}
	if p.Files[0].OverBudget || len(p.Files[0].Rows) == 0 {
		t.Error("a small file was dropped")
	}
	over := p.Files[1]
	if !over.OverBudget {
		t.Fatal("a six thousand line file rendered its body")
	}
	if over.Additions != 6000 {
		t.Errorf("the counts were lost with the body: +%d", over.Additions)
	}
	if len(over.Rows) != 0 {
		t.Error("the body was kept anyway")
	}
	if p.OverBudget != 1 {
		t.Errorf("%d files were counted as over the budget", p.OverBudget)
	}

	// The link that re-requests one path renders it whatever its size.
	forced := Parse([]byte(text), Budget{Collapse: 500, PerFile: 5000, PerPatch: 20000, Force: "generated.json"})
	if forced.Files[1].OverBudget || len(forced.Files[1].Rows) == 0 {
		t.Error("the named path was not rendered")
	}
	if forced.Files[1].Collapsed {
		t.Error("the named path was rendered shut")
	}
}

func TestBudgetCollapses(t *testing.T) {
	// Past the collapse budget a file is rendered shut, and its body is
	// still there: opening it costs no request.
	p := Parse([]byte(synthetic("f.go", 900)), DefaultBudget)
	if !p.Files[0].Collapsed || len(p.Files[0].Rows) == 0 {
		t.Error("a 900 line file did not render shut with its body in place")
	}
	if got := Parse([]byte(synthetic("f.go", 10)), DefaultBudget); got.Files[0].Collapsed {
		t.Error("a ten line file rendered shut")
	}

	// Past the whole-commit budget every file renders shut, so a four file
	// commit and a four hundred file commit are the same document.
	var b strings.Builder
	for i := range 30 {
		b.WriteString(synthetic(fmt.Sprintf("f%d.go", i), 800))
	}
	whole := Parse([]byte(b.String()), DefaultBudget)
	for i, f := range whole.Files {
		if !f.Collapsed {
			t.Errorf("file %d of a 24 000 line commit rendered open", i)
		}
	}
}

func TestRowPredicatesAndShapes(t *testing.T) {
	p := Parse([]byte(sample), DefaultBudget)
	var hunks, context int
	for _, r := range p.Files[0].Rows {
		switch {
		case r.IsHunk():
			hunks++
		case r.IsContext():
			context++
		}
	}
	if hunks != 1 {
		t.Errorf("%d hunk headers, want one", hunks)
	}
	if context == 0 {
		t.Error("no context line survived")
	}
	if got := p.Files[0].Changed(); got != 4 {
		t.Errorf("Changed is %d, want the sum of the two counts", got)
	}
}

func synthetic(path string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nindex 1..2 100644\n--- a/%s\n+++ b/%s\n@@ -1,%d +1,%d @@\n", path, path, path, path, n, n)
	for i := range n {
		fmt.Fprintf(&b, "+line %d\n", i)
	}
	return b.String()
}
