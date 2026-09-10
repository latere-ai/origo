// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// root is the repository root, resolved from this file rather than from
// the working directory.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// started is the set of statuses at which a spec's design is in the
// tree, the started list of .lateregate.yaml. The reference below is
// generated from this package, so a variable of a spec before that is a
// name the deck has written and no code reads yet; requiring a row for
// it would leave a drafted spec unable to name its own configuration.
// Spec 021's code-table test scopes its call-site rule the same way.
var started = map[string]bool{
	"dispatched":  true,
	"in-progress": true,
	"testing":     true,
	"complete":    true,
}

// crossReferenceRow matches one row of the generated cross-reference of
// specs/README.md: the name, and the number of the spec that defines it.
var crossReferenceRow = regexp.MustCompile("(?m)^\\| variable \\| `([^`]+)` \\| \\[([0-9]+)\\]")

// specStatus reads the status: line of the frontmatter of
// specs/<nnn>-*.md under dir, or of specs/.archive/<nnn>-*.md where a
// terminal spec sits. A number no file matches answers the empty
// string, which no started status equals: a row for a spec that is not
// in the tree obliges the reference no more than a drafted one does,
// and the deck's own lint is what reports the missing file.
func specStatus(t *testing.T, dir, number string) string {
	t.Helper()
	var matches []string
	for _, d := range []string{"specs", filepath.Join("specs", ".archive")} {
		found, _ := filepath.Glob(filepath.Join(dir, d, number+"-*.md"))
		matches = append(matches, found...)
	}
	if len(matches) == 0 {
		return ""
	}
	if len(matches) > 1 {
		t.Fatalf("spec %s: %d files match under specs/ and specs/.archive/", number, len(matches))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^status:\s*(\S+)\s*$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("spec %s has no status: line", number)
	}
	return m[1]
}

// definedVariables is every variable the deck defines whose owning spec
// has started, read from the generated table in specs/README.md under
// dir. It takes the directory rather than resolving it, so the scoping
// rule above is testable on a fixture.
func definedVariables(t *testing.T, dir string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "specs", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range crossReferenceRow.FindAllStringSubmatch(string(body), -1) {
		if !started[specStatus(t, dir, m[2])] {
			continue
		}
		out = append(out, m[1])
	}
	return out
}

// crossReference is every variable the deck defines that a started spec
// owns.
func crossReference(t *testing.T) []string {
	t.Helper()
	out := definedVariables(t, root(t))
	if len(out) == 0 {
		t.Fatal("specs/README.md carries no variable of a started spec in its cross-reference")
	}
	return out
}

// TestUnstartedSpecsDoNotNeedAReferenceRow holds the scoping rule on a
// fixture of its own: a variable of a spec at complete is required of
// the reference, one of a spec at drafted is not, and one whose spec
// file is not in the tree is not either.
func TestUnstartedSpecsDoNotNeedAReferenceRow(t *testing.T) {
	dir := t.TempDir()
	specs := filepath.Join(dir, "specs")
	if err := os.MkdirAll(specs, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(specs, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("900-built.md", "---\ntitle: \"built\"\nstatus: complete\n---\n")
	write("901-designed.md", "---\ntitle: \"designed\"\nstatus: drafted\n---\n")
	write("README.md", "| Kind | Name | Owner | Also named in |\n"+
		"|---|---|---|---|\n"+
		"| variable | `ORIGO_BUILT` | [900](900-built.md) | - |\n"+
		"| variable | `ORIGO_DESIGNED` | [901](901-designed.md) | - |\n"+
		"| variable | `ORIGO_ABSENT` | [902](902-absent.md) | - |\n")

	if got := definedVariables(t, dir); !slices.Equal(got, []string{"ORIGO_BUILT"}) {
		t.Errorf("definedVariables = %v, want the started spec's variable alone", got)
	}
}

// TestConfigurationDocIsCurrent is spec 018's criterion: the page in the
// tree is what make docs renders, and it documents every variable the
// deck defines and no other.
func TestConfigurationDocIsCurrent(t *testing.T) {
	page := filepath.Join(root(t), "docs", "configuration.md")
	got, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != Document() {
		t.Error("docs/configuration.md differs from internal/config; run make docs")
	}

	documented, defined := Names(), crossReference(t)
	for _, name := range defined {
		if !slices.Contains(documented, name) {
			t.Errorf("the deck defines %s and the reference has no row for it", name)
		}
	}
	for _, name := range documented {
		if !slices.Contains(defined, name) {
			t.Errorf("the reference has a row for %s, which no spec defines", name)
		}
	}
	for _, name := range documented {
		if strings.Count(string(got), "\n| `"+name+"` | ") != 1 {
			t.Errorf("%s is not on the page exactly once", name)
		}
	}
}

// TestReferenceRowsAreComplete holds the shape of every row: no cell is
// empty, a required variable has no default, and the page carries no
// spec number, because its reader has no spec deck.
func TestReferenceRowsAreComplete(t *testing.T) {
	spec := regexp.MustCompile(`(?i)spec \d`)
	for _, g := range Groups {
		if g.Title == "" || g.Intro == "" {
			t.Errorf("group %q has no title or no introduction", g.Title)
		}
		for _, v := range g.Variables {
			switch {
			case v.Name == "" || v.Required == "" || v.Default == "" || v.Purpose == "":
				t.Errorf("%s: an empty cell in %+v", v.Name, v)
			case v.Required == "yes" && v.Default != "none":
				t.Errorf("%s is required and carries the default %q", v.Name, v.Default)
			case !strings.HasSuffix(v.Purpose, "."):
				t.Errorf("%s: the purpose is not a sentence: %q", v.Name, v.Purpose)
			}
		}
	}
	if m := spec.FindString(Document()); m != "" {
		t.Errorf("the page names %q; its reader has no spec deck", m)
	}
}

// TestDefaultsOnThePageAreTheDefaultsInTheCode proves the rendering of
// the durations, which is the one place the page restates a value.
func TestDefaultsOnThePageAreTheDefaultsInTheCode(t *testing.T) {
	page := Document()
	for _, c := range []struct {
		name string
		want time.Duration
	}{
		{"ORIGO_STORAGE_TIMEOUT", DefaultStorageTimeout},
		{"ORIGO_STALE_MAX", DefaultStaleMax},
		{"ORIGO_SWEEP_INTERVAL", DefaultSweepInterval},
		{"ORIGO_SWEEP_MIN_AGE", DefaultSweepMinAge},
		{"ORIGO_REPAIR_INTERVAL", DefaultRepairInterval},
		{"ORIGO_REPAIR_UNHEARD", DefaultRepairUnheard},
	} {
		row := rowFor(t, page, c.name)
		if want := "`" + short(c.want) + "`"; !strings.Contains(row, "| "+want+" |") {
			t.Errorf("%s: the page says %q, the code %s", c.name, row, want)
		}
	}
	if got := short(90 * time.Minute); got != "1h30m" {
		t.Errorf("short(1h30m) = %q", got)
	}
}

func rowFor(t *testing.T, page, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(page, "\n") {
		if strings.HasPrefix(line, "| `"+name+"` |") {
			return line
		}
	}
	t.Fatalf("%s is not on the page", name)
	return ""
}
