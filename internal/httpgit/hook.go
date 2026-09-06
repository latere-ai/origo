// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/latere-ai/origo/internal/wal"
)

// preReceiveHook is installed in every materialized repository. It runs
// inside git receive-pack after the objects are quarantined and checked
// and before any reference moves: it hands the transaction git resolved
// to the node over a FIFO, then blocks on a second FIFO for the verdict.
// The node commits the entry to the log in between, so the push is
// durable before git's own update and refused before it when the log
// refuses. Only shell builtins are used: the hook runs wherever /bin/sh
// runs, with nothing on PATH.
const preReceiveHook = `#!/bin/sh
# Installed by origod (internal/httpgit/hook.go). Do not edit.
d="$ORIGO_HOOK_DIR"
[ -n "$d" ] || { echo "origo: pre-receive ran outside origod" >&2; exit 1; }
{ while IFS= read -r line; do printf '%s\n' "$line"; done; } > "$d/updates"
if read -r verdict < "$d/verdict"; then :; else verdict="reject origo: no verdict"; fi
case "$verdict" in
  ok) exit 0 ;;
  *) printf '%s\n' "${verdict#reject }" >&2; exit 1 ;;
esac
`

// installHook writes the pre-receive hook into the repository if it is
// missing or differs.
func installHook(repoDir string) error {
	path := filepath.Join(repoDir, "hooks", "pre-receive")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == preReceiveHook {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(preReceiveHook), 0o755); err != nil { //nolint:gosec // a hook must be executable
		return err
	}
	return os.Rename(tmp, path)
}

// hookChannel is one request's pair of FIFOs.
type hookChannel struct {
	dir     string
	updates string
	verdict string
}

func newHookChannel(spoolDir string) (*hookChannel, error) {
	dir, err := os.MkdirTemp(spoolDir, "hook-")
	if err != nil {
		return nil, err
	}
	h := &hookChannel{dir: dir, updates: filepath.Join(dir, "updates"), verdict: filepath.Join(dir, "verdict")}
	for _, p := range []string{h.updates, h.verdict} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	return h, nil
}

func (h *hookChannel) close() { _ = os.RemoveAll(h.dir) }

// readUpdates blocks until the hook has written the transaction and
// returns it. It returns ok false when the FIFO was released without a
// writer, which is how the node unblocks it once git exited without
// running the hook.
func (h *hookChannel) readUpdates() ([]wal.RefUpdate, bool, error) {
	f, err := os.OpenFile(h.updates, os.O_RDONLY, 0)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	var refs []wal.RefUpdate
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		u, err := parseCommand([]byte(sc.Text()))
		if err != nil {
			return nil, true, err
		}
		refs = append(refs, u)
	}
	if err := sc.Err(); err != nil {
		return nil, true, err
	}
	return refs, len(refs) > 0, nil
}

// release opens the updates FIFO for writing and closes it, so a
// reader blocked in readUpdates sees end of file.
func (h *hookChannel) release() {
	if f, err := os.OpenFile(h.updates, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		_ = f.Close()
	}
}

// writeVerdict delivers "ok" or "reject <message>" to the hook. The
// FIFO has no reader until the hook reaches its read, and none ever if
// git died, so the open is non-blocking and retried until ctx ends.
func (h *hookChannel) writeVerdict(ctx context.Context, verdict string) error {
	for {
		f, err := os.OpenFile(h.verdict, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, werr := fmt.Fprintln(f, strings.ReplaceAll(verdict, "\n", " "))
			return errors.Join(werr, f.Close())
		}
		if !errors.Is(err, syscall.ENXIO) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
