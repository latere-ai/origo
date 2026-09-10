// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two documents of spec 025, relative to this package.
const (
	docPath   = "../../docs/cli.md"
	skillPath = "../../skills/origo/SKILL.md"
)

// binaryCommands is the surface, read from the help page the binary itself
// prints, so this test compares two documents against the binary rather than
// against a third list that could drift from both.
func binaryCommands(t *testing.T) []string {
	t.Helper()
	got := run(t, map[string]string{}, "help")
	var out []string
	inList := false
	for _, l := range lines(got.stdout) {
		if l == "commands:" {
			inList = true
			continue
		}
		if !inList {
			continue
		}
		if !strings.HasPrefix(l, "  ") || l == "" {
			break // the list ends at the first line that is not an entry
		}
		name := strings.Fields(l)[0]
		if name == "version" || name == "help" {
			continue
		}
		out = append(out, name)
	}
	if len(out) != 12 {
		t.Fatalf("the help page lists %d commands, want 12: %v", len(out), out)
	}
	return out
}

// TestDocumentedCommandsMatchTheBinary: neither document may name a command
// the binary does not have, and docs/cli.md names every one it does.
func TestDocumentedCommandsMatchTheBinary(t *testing.T) {
	doc := read(t, docPath)
	skill := read(t, skillPath)
	for _, name := range binaryCommands(t) {
		if !strings.Contains(doc, "`origo "+name) {
			t.Fatalf("docs/cli.md does not document %q", name)
		}
	}
	// A command named in a document that the binary does not serve is the
	// drift that matters: a reader would run it and get exit 2.
	named := regexp.MustCompile("`origo ([a-z-]+)")
	have := map[string]bool{"help": true, "version": true}
	for _, name := range binaryCommands(t) {
		have[name] = true
	}
	for _, source := range []struct{ name, body string }{{"docs/cli.md", doc}, {"skills/origo/SKILL.md", skill}} {
		for _, m := range named.FindAllStringSubmatch(source.body, -1) {
			if !have[m[1]] {
				t.Fatalf("%s names `origo %s`, which the binary does not serve", source.name, m[1])
			}
		}
	}
}

// TestDocumentedFlagsMatchTheBinary: a flag in either document is a flag the
// command accepts, checked by running it and refusing an exit 2 that names the
// flag as unknown.
func TestDocumentedFlagsMatchTheBinary(t *testing.T) {
	f := newFake()
	env := envFor(f.start(t))
	// Every flag each command is documented with, gathered from the tables.
	flags := map[string][]string{
		"repos":       {"-n", "--json"},
		"info":        {"-repo", "--json"},
		"refs":        {"-tags", "-all", "-prefix", "-n", "--json"},
		"ls":          {"-r", "-n", "-ref", "--json"},
		"cat":         {"-n", "-offset", "-max-bytes", "-ref"},
		"log":         {"-n", "-ref", "-path", "-since", "-until", "--json"},
		"show":        {"-p", "-path", "-max-bytes"},
		"diff":        {"-p", "-path", "-max-bytes"},
		"commit":      {"-m", "-expect", "-branch", "-delete", "-file", "-create", "-from", "-dry-run"},
		"merge":       {"-expect", "-branch", "-strategy", "-m", "-dry-run"},
		"cherry-pick": {"-expect", "-branch", "-mainline", "-dry-run"},
		"revert":      {"-expect", "-branch", "-mainline", "-dry-run"},
	}
	for name, want := range flags {
		for _, flag := range want {
			got := run(t, env, name, flag+"=x", "-h")
			if strings.Contains(got.stderr, "flag provided but not defined: "+strings.TrimLeft(flag, "-")) {
				t.Fatalf("origo %s does not accept %s, which the documents name", name, flag)
			}
		}
	}
	doc := read(t, docPath) + read(t, skillPath)
	for name, want := range flags {
		for _, flag := range want {
			if flag == "--json" || flag == "-repo" {
				continue // documented once, in the variables and defaults tables
			}
			if !strings.Contains(doc, flag) {
				t.Fatalf("%s of origo %s is in the binary but in neither document", flag, name)
			}
		}
	}
}

// TestTheSkillsResidentCostIsItsFrontmatter is the whole argument for the
// shape, measured rather than asserted: what an agent holds between turns is a
// name and one sentence, and the body loads only when it is used.
func TestTheSkillsResidentCostIsItsFrontmatter(t *testing.T) {
	body := read(t, skillPath)
	parts := strings.SplitN(body, "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatal("the skill has no frontmatter")
	}
	var name, description string
	for _, l := range lines(parts[1]) {
		if v, ok := strings.CutPrefix(l, "name: "); ok {
			name = v
		}
		if v, ok := strings.CutPrefix(l, "description: "); ok {
			description = v
		}
	}
	if name != "origo" {
		t.Fatalf("name = %q", name)
	}
	if description == "" || !strings.Contains(description, "origo command") {
		t.Fatalf("the description does not say what loading it gets you: %q", description)
	}
	resident := len(name) + len(description)
	if resident > 256 {
		t.Fatalf("the resident cost is %d bytes; the whole advantage is that it is small", resident)
	}
	if len(parts[2]) < 2000 {
		t.Fatal("the body is too short to have earned its place")
	}
	// The five things it must teach, each by a mark a reader can find.
	for _, want := range []string{
		"ORIGO_URL", "ORIGO_TOKEN", "ORIGO_REPO", "ORIGO_AUTHOR",
		"/tokens", `"scope":"read"`,
		"origo ls -r -n 0 | grep",
		"origo log -n 200 | grep",
		"origo diff -p -path",
		"-expect",
		"[truncated:",
		"[stale:",
	} {
		if !strings.Contains(parts[2], want) {
			t.Fatalf("the skill does not teach %q", want)
		}
	}
}

// TestTheDocumentTeachesTheTwoStreams: the rule that makes every pipeline in
// both documents correct is stated in both.
func TestTheDocumentTeachesTheTwoStreams(t *testing.T) {
	for _, path := range []string{docPath, skillPath} {
		body := read(t, path)
		lower := strings.ToLower(body)
		if !strings.Contains(lower, "standard error") || !strings.Contains(lower, "standard output") {
			t.Fatalf("%s does not state the two streams", path)
		}
		if !strings.Contains(body, "[truncated:") {
			t.Fatalf("%s does not show the truncation line", path)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
