// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	versionpkg "github.com/latere-ai/origo/internal/version"
)

// TestVersionHasOneSource is spec 017's criterion for the binary's
// identity: internal/version is the one source, set by -ldflags the
// way the Makefile and release.yml set it. A binary built with
// -X internal/version.Version=v1.2.3 prints it for -version, a node
// whose Version is v1.2.3 serves it on GET /version, and no file under
// cmd/ names main.version.
func TestVersionHasOneSource(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "origod")
	build := exec.CommandContext(context.Background(), "go", "build", "-trimpath",
		"-ldflags", "-X github.com/latere-ai/origo/internal/version.Version=v1.2.3", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.CommandContext(context.Background(), binary, "-version").Output()
	if err != nil || !strings.HasPrefix(string(out), "origod v1.2.3 (") {
		t.Fatalf("-version printed %q, %v", out, err)
	}

	t.Cleanup(func() { versionpkg.Version = "dev" })
	versionpkg.Version = "v1.2.3"
	env, _ := newEnv(t)
	env["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
	env["ORIGO_S3_PATH_STYLE"] = "1"
	n, stop := startNode(t, env)
	public, _, _ := n.addrs()
	if code, body := get(t, "http://"+public+"/version"); code != 200 || body["version"] != "v1.2.3" {
		t.Fatalf("GET /version: %d %v", code, body)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// grep -r 'main.version' cmd/ finds nothing, this file excepted,
	// which names it to say so.
	err = filepath.WalkDir(filepath.Join(moduleRoot(t), "cmd"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) == "version_test.go" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "main."+"version") {
			t.Errorf("%s names main.version", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
