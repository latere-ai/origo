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

// crossReference is every variable the spec deck defines, read from the
// generated table in specs/README.md.
func crossReference(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root(t), "specs", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| variable \\| `([^`]+)` \\|")
	var out []string
	for _, m := range row.FindAllStringSubmatch(string(body), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatal("specs/README.md carries no variable in its cross-reference")
	}
	return out
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
