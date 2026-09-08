// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/latere-ai/origo/internal/wal"
)

// preReceiveHook is installed in every materialized repository. It runs
// inside git receive-pack after the objects are quarantined and checked
// and before any reference moves: it opens the verdict FIFO, reports
// the quarantine directory and hands the transaction git resolved to
// the node over the updates FIFO, ends that with the terminator line,
// then blocks reading the verdict. The node commits the entry to the
// log in between and reads the pushed objects from the quarantine for
// the forced flag (spec 008), so the push is durable before git's own
// update and refused before it when the log refuses. Only shell
// builtins are used: the hook runs wherever /bin/sh runs, with nothing
// on PATH.
//
// The node holds both ends of each FIFO open for the whole request, so
// neither of the hook's opens waits for the other side (see
// hookChannel). The verdict FIFO is opened first, before the node can
// have answered, so a node that goes away after that ends the hook with
// end of file on its read, "no verdict", rather than leaving it, and
// git with it, waiting in an open.
const preReceiveHook = `#!/bin/sh
# Installed by origod (internal/httpgit/hook.go). Do not edit.
d="$ORIGO_HOOK_DIR"
[ -n "$d" ] || { echo "origo: pre-receive ran outside origod" >&2; exit 1; }
exec 3< "$d/verdict"
{ printf 'quarantine %s\n' "$GIT_QUARANTINE_PATH"; while IFS= read -r line; do printf '%s\n' "$line"; done; printf 'end\n'; } > "$d/updates"
if read -r verdict <&3; then :; else verdict="reject origo: no verdict"; fi
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

// hookChannel is one request's pair of FIFOs, each held open by the
// node for reading and writing from before git starts until the
// request ends.
//
// A FIFO opened for one direction blocks until the other direction
// opens, and that rendezvous is what the first version of this channel
// relied on: the node's blocking open for reading met the hook's
// blocking open for writing. On macOS that rendezvous loses a wakeup
// about once in a thousand: the hook's open, write, and close all
// complete between the moment the kernel counts the node's reader and
// the moment it puts the reader to sleep, and the reader sleeps until a
// later writer arrives, which for a request is never. git waits for
// the hook, the hook waits for the verdict, the request runs into its
// timeout, and the client's push, whose response ends without git's
// closing flush, waits forever. Holding both ends from the start makes
// every later open immediate on every platform: an open for writing
// finds a reader, an open for reading finds a writer, and nothing
// sleeps. It also means the reader never sees end of file, so the
// updates end with a terminator line instead, and the node can end a
// read itself by writing that line when git exits without running the
// hook.
type hookChannel struct {
	dir     string
	updates *os.File
	verdict *os.File
}

// updatesEnd is the line that ends the hook's updates. A command is
// three fields and the quarantine line has its prefix, so neither can
// be it.
const updatesEnd = "end"

func newHookChannel(spoolDir string) (*hookChannel, error) {
	dir, err := os.MkdirTemp(spoolDir, "hook-")
	if err != nil {
		return nil, err
	}
	h := &hookChannel{dir: dir}
	for _, fifo := range []struct {
		name string
		file **os.File
	}{{"updates", &h.updates}, {"verdict", &h.verdict}} {
		path := filepath.Join(dir, fifo.name)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			h.close()
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			h.close()
			return nil, err
		}
		*fifo.file = f
	}
	return h, nil
}

// close releases both FIFOs and removes the directory. A hook still
// holding one sees end of file or a broken pipe on its next operation,
// which ends it with "no verdict".
func (h *hookChannel) close() {
	for _, f := range []*os.File{h.updates, h.verdict} {
		if f != nil {
			_ = f.Close()
		}
	}
	_ = os.RemoveAll(h.dir)
}

// quarantineLine is the first line the hook writes: the directory git
// holds the pushed objects in until the verdict.
const quarantineLine = "quarantine "

// readUpdates blocks until the updates end with the terminator and
// returns the quarantine path and the transaction. It returns ok false
// when no command arrived before the terminator, which is how the node
// unblocks it once git exited without running the hook (release).
func (h *hookChannel) readUpdates() (refs []wal.RefUpdate, quarantine string, ok bool, err error) {
	sc := bufio.NewScanner(h.updates)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == updatesEnd {
			return refs, quarantine, len(refs) > 0, nil
		}
		if refs == nil && quarantine == "" && strings.HasPrefix(line, quarantineLine) {
			quarantine = strings.TrimPrefix(line, quarantineLine)
			continue
		}
		u, err := parseCommand([]byte(line))
		if err != nil {
			return nil, quarantine, true, err
		}
		refs = append(refs, u)
	}
	if err := sc.Err(); err != nil {
		return nil, quarantine, true, err
	}
	// End of file arrives only when the node closed its own end.
	return nil, quarantine, true, errors.New("hook: updates ended before the terminator")
}

// release ends a readUpdates that will never see the hook, because git
// exited before running it: the node writes the terminator on the end
// it holds. A terminator after the hook's own is never read and is
// discarded with the FIFO.
func (h *hookChannel) release() error {
	_, err := fmt.Fprintln(h.updates, updatesEnd)
	return err
}

// writeVerdict delivers "ok" or "reject <message>" to the hook through
// the end the node holds; the hook's own open for reading finds that
// writer and returns at once.
func (h *hookChannel) writeVerdict(verdict string) error {
	_, err := fmt.Fprintln(h.verdict, strings.ReplaceAll(verdict, "\n", " "))
	return err
}
