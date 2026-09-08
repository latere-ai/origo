// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build generate

package source_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/source"
)

// TestGenerateFixture writes testdata/fixture.bundle, the fixture
// repository of source.FixtureCommits commits on main, from one
// fast-import stream with fixed dates, so the bytes are the same on
// every run. It runs under `go generate` in the package and never in
// the suite.
func TestGenerateFixture(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	out := filepath.Join(filepath.Dir(file), "testdata", "fixture.bundle")
	dir := t.TempDir()
	gittest.Run(t, dir, nil, "init", "-q", "--bare", "-b", "main")
	var stream strings.Builder
	for i := 1; i <= source.FixtureCommits; i++ {
		content := fmt.Sprintf("commit %d\n", i)
		fmt.Fprintf(&stream, "commit refs/heads/main\ncommitter Origo Test <test@example.com> %d +0000\ndata %d\ncommit %d\n", 1757152800+i*60, len(fmt.Sprintf("commit %d\n", i)), i)
		fmt.Fprintf(&stream, "M 100644 inline counter.txt\ndata %d\n%s\n", len(content), content)
	}
	stream.WriteString("done\n")
	gittest.Run(t, dir, []byte(stream.String()), "fast-import", "--quiet", "--done")
	if got := gittest.Run(t, dir, nil, "rev-list", "--count", "refs/heads/main"); got != fmt.Sprint(source.FixtureCommits) {
		t.Fatalf("%s commits", got)
	}
	_ = os.Remove(out)
	gittest.Run(t, dir, nil, "bundle", "create", "-q", out, "refs/heads/main")
	t.Logf("wrote %s", out)
}
