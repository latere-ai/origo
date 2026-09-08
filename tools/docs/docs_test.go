// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package docs

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRunBlocks is spec 013's criterion for run-blocks.sh, run through
// run_blocks_test.sh beside it; the script's directory is resolved from
// this file, never from the working directory.
func TestRunBlocks(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", filepath.Join(filepath.Dir(file), "run_blocks_test.sh"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
