// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDeployArchive runs deploy_archive_test.sh beside this file: the
// archive carries deploy/base and deploy/examples with every image at
// the version and no placeholder, and a tree without the placeholder
// is refused. The script's directory is resolved from this file, and
// TMPDIR is the test's own.
func TestDeployArchive(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", filepath.Join(filepath.Dir(file), "deploy_archive_test.sh"))
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
