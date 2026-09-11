// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// root is the checkout, resolved from this file rather than from the
// working directory, so the gates that run the suite from an empty
// directory still see the specs.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// Nothing in the tree connected a green run to the specs waiting behind
// it. The v0.1.3 release produced the live run that four specs named as
// the one thing they waited on, and all four sat at `testing` for a day
// because no one went back to read them. The `Waits on:` marker and
// waiting.sh are that connection: a spec names what holds it in a form a
// script can match, and the job that proves the thing names the specs.
//
// The marker's grammar is held by TestWaitsOnMarkersAreWellFormed below;
// the sweep is held here.

// markerTokens is the closed set a `Waits on:` line may name. A spec
// waiting on something not in this set says so in prose instead, and no
// job will name it, which is the honest outcome for a thing no run
// proves.
var markerTokens = []string{
	"the live job of release.yml",
	"the weekly fuzz job of verify.yml",
	"a maintainer",
	"a test not yet in the tree",
}

func specsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(root(t), "specs")
}

func runWaiting(t *testing.T, dir, token string) []string {
	t.Helper()
	script := filepath.Join(root(t), "tools", "release", "waiting.sh")
	cmd := exec.CommandContext(context.Background(), "/bin/sh", script, dir, token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("waiting.sh: %v\n%s", err, out)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestWaitingNamesOnlyTestingSpecsThatSaySo is the sweep's own proof: a
// spec at `testing` naming the token is reported, one at `complete`
// naming it is not, and one at `testing` naming another token is not.
func TestWaitingNamesOnlyTestingSpecsThatSaySo(t *testing.T) {
	dir := t.TempDir()
	write := func(name, status, marker string) {
		body := "---\ntitle: t\nstatus: " + status + "\n---\n\n## Outcome\n\n" + marker + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const live = "the live job of release.yml"
	write("101-waiting.md", "testing", "Waits on: "+live+".")
	write("102-done.md", "complete", "Waits on: "+live+".")
	write("103-other.md", "testing", "Waits on: a maintainer.")
	write("104-silent.md", "testing", "Nothing here names a token.")

	got := runWaiting(t, dir, live)
	want := []string{"101-waiting.md"}
	if len(got) != len(want) || (len(got) == 1 && got[0] != want[0]) {
		t.Fatalf("waiting.sh = %q, want %q", got, want)
	}
}

// TestWaitsOnMarkersAreWellFormed holds the grammar across the real
// deck. The marker is optional, because a spec may be moving under
// another builder, but where it is present it names a token a job can
// prove, and a spec that has reached `complete` carries none: a closed
// spec that still says what it waits on is the defect this whole
// mechanism exists to catch.
func TestWaitsOnMarkersAreWellFormed(t *testing.T) {
	dir := specsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "README.md" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		var status string
		for l := range strings.SplitSeq(text, "\n") {
			if after, ok := strings.CutPrefix(l, "status: "); ok {
				status = strings.TrimSpace(after)
				break
			}
		}
		var markers []string
		for l := range strings.SplitSeq(text, "\n") {
			if strings.HasPrefix(l, "Waits on:") {
				markers = append(markers, l)
			}
		}
		if len(markers) == 0 {
			continue
		}
		if status != "testing" {
			t.Errorf("%s is at %q and carries %d Waits on marker(s); only a spec at testing may say what it waits on", name, status, len(markers))
			continue
		}
		for _, m := range markers {
			tok := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(m, "Waits on:")), ".")
			if !contains(markerTokens, tok) {
				t.Errorf("%s: %q names %q, which is not one of %q", name, m, tok, markerTokens)
			}
		}
	}
}

func contains(all []string, s string) bool {
	return slices.Contains(all, s)
}
