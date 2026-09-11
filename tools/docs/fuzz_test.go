// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package docs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The `fuzz` target is the only place the 40 second search of every fuzz
// function runs; the seed corpora run in the `test` gate and prove far
// less. A recipe that builds the wrong -fuzz pattern still exits 0,
// because `go test` reports "no fuzz tests to fuzz" as a warning and
// passes, so the job stays green while fuzzing nothing. Specs 009 and
// 016 both cite that job as what closes their remaining criterion, which
// is why the pattern is asserted here rather than trusted.
//
// The check runs the real recipe with $(GO) pointed at a stub that
// records the arguments instead of compiling anything, so it is
// hermetic and takes no fuzzing time.

const goStub = `#!/bin/sh
case "$1 $2" in
"list ./...")
	echo example.com/m/pkga
	exit 0
	;;
esac
case "$1" in
test)
	for a in "$@"; do
		case "$a" in
		-list) echo FuzzAlpha; echo FuzzBeta; echo ok; exit 0 ;;
		esac
	done
	for a in "$@"; do
		case "$a" in
		-fuzz=*) echo "$a" >> "$FUZZ_RECORD" ;;
		-fuzztime=*) echo "$a" >> "$FUZZ_RECORD" ;;
		esac
	done
	exit 0
	;;
esac
exit 0
`

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// TestFuzzTargetNamesEachFunctionExactly runs `make fuzz` against a stub
// `go` and asserts every -fuzz pattern is anchored at both ends on the
// function's own name. The defect this pins wrote `-fuzz="^$$fn$$$$"`,
// where make turns `$$$$` into `$$` and the shell turns that into its own
// pid, so the pattern read `^FuzzAlpha24748`, matched no function, and
// every package reported "no fuzz tests to fuzz" in five milliseconds.
func TestFuzzTargetNamesEachFunctionExactly(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "go")
	if err := os.WriteFile(stub, []byte(goStub), 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "args")

	cmd := exec.CommandContext(context.Background(), "make", "-C", repoRoot(t), "fuzz", "GO="+stub)
	cmd.Env = append(os.Environ(), "FUZZ_RECORD="+record)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make fuzz: %v\n%s", err, out)
	}

	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the recipe ran no `go test`: %v", err)
	}
	var patterns, times []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		switch {
		case strings.HasPrefix(l, "-fuzz="):
			patterns = append(patterns, strings.TrimPrefix(l, "-fuzz="))
		case strings.HasPrefix(l, "-fuzztime="):
			times = append(times, strings.TrimPrefix(l, "-fuzztime="))
		}
	}

	want := []string{"^FuzzAlpha$", "^FuzzBeta$"}
	if len(patterns) != len(want) {
		t.Fatalf("-fuzz patterns = %q, want %q", patterns, want)
	}
	for i, got := range patterns {
		if got != want[i] {
			t.Errorf("-fuzz pattern = %q, want %q; an unanchored or pid-suffixed pattern matches no function and `go test` still exits 0", got, want[i])
		}
	}
	for _, got := range times {
		if got != "40s" {
			t.Errorf("-fuzztime = %q, want 40s, the search specs 009 and 016 name", got)
		}
	}
}
