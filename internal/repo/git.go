// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package repo is the local repository cache of spec 004: a bare
// repository per repository id under the data directory, materialized
// from the log, caught up by the currency check before every use, and
// rebuilt from the log when corrupt. The local copy is disposable.
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Git runs the git binary against one bare repository with a hermetic
// environment and a per-invocation deadline.
type Git struct {
	// Bin is the git binary; resolved on PATH when it has no separator.
	Bin string
	// Home is an empty directory git reads no user configuration from.
	Home string
	// Timeout bounds one invocation.
	Timeout time.Duration
}

// Env is the environment of every subprocess. GIT_DIR names the
// repository so no argument can point git elsewhere.
func (g *Git) Env(dir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.Home,
		"GIT_DIR=" + dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
}

// Command builds a command in dir. The caller wires the streams and runs
// it; the context bounds it.
func (g *Git) Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Dir = dir
	cmd.Env = g.Env(dir)
	cmd.WaitDelay = time.Second
	return cmd
}

// Run runs git to completion with stdin and returns stdout. A non-zero
// exit becomes an error carrying stderr.
func (g *Git) Run(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, error) {
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}
	cmd := g.Command(ctx, dir, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

// Error is a failed git invocation.
type Error struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// corruptionMarkers are the phrases git prints when the object store is
// damaged. An error carrying one is a reason to rebuild from the log.
var corruptionMarkers = []string{
	"corrupt", "bad object", "missing object", "does not point to a valid object",
	"broken link", "invalid sha1 pointer", "bad pack", "pack has bad object",
	"packfile", "index file corrupt", "unable to read", "is not a valid",
}

// IsCorruption reports whether err names a corrupt local repository.
func IsCorruption(err error) bool {
	var ge *Error
	if !errors.As(err, &ge) {
		return false
	}
	msg := strings.ToLower(ge.Stderr)
	for _, m := range corruptionMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// missingBaseMarkers are the phrases index-pack prints when a thin pack
// names a base object the repository does not hold: the base is in no
// entry and no pack (spec 015).
var missingBaseMarkers = []string{"did not receive expected object", "unresolved delta"}

// IsMissingBase reports whether err is index-pack refusing a thin pack
// whose base is in no entry.
func IsMissingBase(err error) bool {
	var ge *Error
	if !errors.As(err, &ge) {
		return false
	}
	msg := strings.ToLower(ge.Stderr)
	for _, m := range missingBaseMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
