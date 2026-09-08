// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The hand-off between the node and the hook must never sleep in a
// FIFO open. The first channel relied on the open rendezvous, a blocking
// open for reading meeting a blocking open for writing, and on macOS
// that rendezvous loses its wakeup about once in a thousand: the hook's
// open, write, and close all complete between the kernel counting the
// node's reader and putting it to sleep, and the reader then sleeps
// until a writer that never comes. The whole push waited on it, up to
// the request timeout, after which the client hung on a response
// without git's closing flush (the CI hang of
// TestStalePushIsRefusedAndConcurrentBranchesLand). Four thousand
// exchanges with the installed script through /bin/sh, eight at a time,
// met the lost wakeup with near certainty under the first channel;
// every exchange is bounded, so a regression fails instead of hanging.
func TestHookHandOffNeverSleepsInAnOpen(t *testing.T) {
	repoDir := t.TempDir()
	if err := installHook(repoDir); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(repoDir, "hooks", "pre-receive")
	spool := t.TempDir()
	const workers, perWorker = 8, 500
	zero, one := strings.Repeat("0", 40), strings.Repeat("1", 40)
	var wg sync.WaitGroup
	failures := make(chan string, workers*perWorker)
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				if msg := hookExchange(hook, spool, fmt.Sprintf("q-%d-%d", w, i), zero, one); msg != "" {
					failures <- msg
					return
				}
			}
		})
	}
	wg.Wait()
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}
}

// hookExchange runs one push's hook exchange: the script through /bin/sh
// with the transaction on its stdin, the node reading the updates and
// answering ok. It returns a message on any failure, a hang included.
func hookExchange(hook, spool, quarantine, old, updated string) string {
	ch, err := newHookChannel(spool)
	if err != nil {
		return err.Error()
	}
	defer ch.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", hook)
	cmd.Env = []string{"ORIGO_HOOK_DIR=" + ch.dir, "GIT_QUARANTINE_PATH=" + quarantine}
	cmd.Stdin = strings.NewReader(old + " " + updated + " refs/heads/main\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	type updates struct {
		refs       int
		quarantine string
		ok         bool
		err        error
	}
	fromHook := make(chan updates, 1)
	go func() {
		refs, q, ok, err := ch.readUpdates()
		fromHook <- updates{len(refs), q, ok, err}
	}()
	select {
	case u := <-fromHook:
		if u.err != nil || !u.ok || u.refs != 1 || u.quarantine != quarantine {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Sprintf("updates: %+v", u)
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "the node's read of the updates did not return: the hook or the node slept in a FIFO open"
	}
	if err := ch.writeVerdict("ok"); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err.Error()
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return "the hook did not exit after the verdict: it slept in the verdict FIFO's open"
		}
		return fmt.Sprintf("hook: %v: %s", err, stderr.String())
	}
	return ""
}

// The hook refuses to run outside the node, and relays a rejection to
// git through its exit status and stderr.
func TestHookRefusesOutsideTheNodeAndRelaysARejection(t *testing.T) {
	repoDir := t.TempDir()
	if err := installHook(repoDir); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(repoDir, "hooks", "pre-receive")
	cmd := exec.CommandContext(context.Background(), "/bin/sh", hook)
	cmd.Env = []string{}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "outside origod") {
		t.Fatalf("outside the node: %v, %q", err, stderr.String())
	}
	ch, err := newHookChannel(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.close()
	cmd = exec.CommandContext(context.Background(), "/bin/sh", hook)
	cmd.Env = []string{"ORIGO_HOOK_DIR=" + ch.dir, "GIT_QUARANTINE_PATH=/q"}
	cmd.Stdin = strings.NewReader(strings.Repeat("0", 40) + " " + strings.Repeat("2", 40) + " refs/heads/main\n")
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, q, ok, err := ch.readUpdates(); err != nil || !ok || q != "/q" {
		t.Fatalf("updates: %q %v %v", q, ok, err)
	}
	if err := ch.writeVerdict("reject non_fast_forward: main moved; fetch first"); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 || strings.TrimSpace(stderr.String()) != "non_fast_forward: main moved; fetch first" {
		t.Fatalf("rejection: %v, %q", err, stderr.String())
	}
	// The node going away mid-exchange ends the hook with "no verdict"
	// rather than leaving it, and git with it, waiting: the verdict FIFO
	// loses its writer when the node closes its end.
	ch2, err := newHookChannel(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(context.Background(), "/bin/sh", hook)
	cmd.Env = []string{"ORIGO_HOOK_DIR=" + ch2.dir, "GIT_QUARANTINE_PATH=/q"}
	cmd.Stdin = strings.NewReader(strings.Repeat("0", 40) + " " + strings.Repeat("3", 40) + " refs/heads/main\n")
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := ch2.readUpdates(); err != nil || !ok {
		t.Fatalf("updates: %v %v", ok, err)
	}
	ch2.close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err == nil || !strings.Contains(stderr.String(), "no verdict") {
			t.Fatalf("after the node went away: %v, %q", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the hook waited for a verdict after the node closed the channel")
	}
	_ = os.RemoveAll(ch2.dir)
}
