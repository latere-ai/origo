// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// TestExportServesTheWholeRepository is spec 019's export: the complete
// history in one portable file the client verifies and clones from,
// with the read action and the empty repository named.
func TestExportServesTheWholeRepository(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.seed(f)
	res := h.get("/v1/repos/" + repoA + "/export.bundle")
	if res.status != 200 || res.header.Get("Content-Type") != exportContentType || len(res.body) == 0 {
		t.Fatalf("export: %d %q %d bytes", res.status, res.header.Get("Content-Type"), len(res.body))
	}
	dir := bundleDir(t)
	path := filepath.Join(dir, "out.bundle")
	if err := os.WriteFile(path, res.body, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitRun(t, dir, "bundle", "verify", path); err != nil {
		t.Fatalf("bundle verify: %v\n%s", err, out)
	}
	// The bundle carries the whole history the fixture has.
	clone := filepath.Join(dir, "clone")
	if out, err := gitRun(t, dir, "clone", "-q", path, clone); err != nil {
		t.Fatalf("clone from the bundle: %v\n%s", err, out)
	}
	if gittest.RevList(t, clone) != gittest.RevList(t, f.Dir) {
		t.Fatal("the bundle is not the fixture's history")
	}

	// A repository with no reference has nothing to bundle.
	empty := newHarness(t)
	empty.create(repoB, "acme", "empty")
	if res := empty.get("/v1/repos/" + repoB + "/export.bundle"); res.status != 404 || code(res.json()) != contract.CodeRefNotFound {
		t.Fatalf("export of an empty repository: %d %v", res.status, res.json())
	}

	// A caller without read is refused, and an unknown repository 404.
	h.authz.SetRules(authorizer.Rule{Allow: true}, authorizer.Rule{Subject: "eve", Allow: false, Reason: "no"})
	h.as(auth.Principal{Subject: "eve"})
	if res := h.get("/v1/repos/" + repoA + "/export.bundle"); res.status != 403 {
		t.Fatalf("export without read: %d", res.status)
	}
	h.as(auth.Principal{Subject: "alice"})
	if res := h.get("/v1/repos/" + unknown + "/export.bundle"); res.status != 404 {
		t.Fatalf("export of an unknown repository: %d", res.status)
	}
}

// TestExportDeadline is spec 019's deadline criterion: a git bundle
// held past the export budget leaves a truncated body, never a status,
// because the headers went with the first byte, and the client refuses
// the result. A cut inside the header is what git bundle verify
// refuses; a cut inside the pack it accepts, because it reads the
// header and the prerequisites and never the pack, so the client's
// refusal of that one is git clone's.
func TestExportDeadline(t *testing.T) {
	f := loadFixture(t)
	spy := newSpyGit(t)
	// The budget bounds the subprocess the export starts, so it covers
	// that subprocess's own start, which -race on a loaded runner slows
	// down. The criterion is what a cut body looks like; the figure only
	// bounds how long this test waits for the cut.
	h := newHarness(t, withGit(spy.bin()), withExportTimeout(5*time.Second))
	h.seed(f)
	defer spy.release()

	// The bundle the cut subprocess writes from is made once, before any
	// request: making it inside the export's budget would race it.
	full := filepath.Join(t.TempDir(), "full.bundle")
	if out, err := gitRun(t, f.Dir, "bundle", "create", full, "--all"); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}

	cut := func(bytes int) string {
		t.Helper()
		spy.truncateOn("bundle", full, bytes)
		res := h.get("/v1/repos/" + repoA + "/export.bundle")
		if res.status != 200 || res.header.Get("Content-Type") != exportContentType {
			t.Fatalf("cut export: %d %q", res.status, res.header.Get("Content-Type"))
		}
		if len(res.body) == 0 {
			t.Fatal("the cut export sent no bytes at all")
		}
		dir := bundleDir(t)
		path := filepath.Join(dir, "cut.bundle")
		if err := os.WriteFile(path, res.body, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A cut inside the bundle signature: git bundle verify refuses it.
	path := cut(8)
	if out, err := gitRun(t, filepath.Dir(path), "bundle", "verify", path); err == nil {
		t.Fatalf("git bundle verify accepted a bundle cut in its header:\n%s", out)
	}
	// A cut inside the pack: the clone that reads the pack refuses it.
	path = cut(int(info.Size()) / 2)
	if out, err := gitRun(t, filepath.Dir(path), "clone", "-q", path, filepath.Join(filepath.Dir(path), "clone")); err == nil {
		t.Fatalf("a clone from a bundle cut in its pack succeeded:\n%s", out)
	}
}

// bundleDir is a directory git bundle verify runs in: the command needs
// a repository around it whatever the bundle names.
func bundleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := gitRun(t, dir, "init", "-q", "--initial-branch=main", "."); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return dir
}

// gitRun runs the real git client in dir.
func gitRun(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = gittest.Env(dir)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
