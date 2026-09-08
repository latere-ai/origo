// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestE2EDevStackClones is spec 013's criterion for `make dev`: started
// in the background with a distinct DEV_PROJECT, it prints within 2
// minutes a clone line whose token the node accepts, and `make dev-down`
// for that project tears the stack down whatever happened, never `make
// clean`, which would remove the out/ of the checkout under test. Skips
// without a container engine, because the stack's MinIO is compose.
func TestE2EDevStackClones(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not on PATH")
	}
	engine := ""
	for _, candidate := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(candidate); err == nil {
			engine = candidate
			break
		}
	}
	if engine == "" {
		t.Skip("no container engine on PATH")
	}
	root := repoRoot(t)
	project := fmt.Sprintf("e2e-dev-%d", os.Getpid())
	makeEnv := append(os.Environ(), "DEV_PROJECT="+project, "DEV_ENGINE="+engine)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		down := exec.CommandContext(ctx, "make", "dev-down")
		down.Dir, down.Env = root, makeEnv
		if out, err := down.CombinedOutput(); err != nil {
			t.Errorf("make dev-down: %v\n%s", err, out)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dev := exec.CommandContext(ctx, "make", "dev")
	dev.Dir, dev.Env = root, makeEnv
	dev.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := dev.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	dev.Stderr = &stderr
	if err := dev.Start(); err != nil {
		t.Fatal(err)
	}
	// The whole process group ends with the test: the node runs in the
	// foreground of make's shell.
	t.Cleanup(func() {
		_ = syscall.Kill(-dev.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = dev.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = syscall.Kill(-dev.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	var mu sync.Mutex
	var lines []string
	cloneLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
			if strings.HasPrefix(line, "git clone ") {
				select {
				case cloneLine <- line:
				default:
				}
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	var line string
	select {
	case line = <-cloneLine:
	case <-time.After(2 * time.Minute):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no clone line within 2 minutes\nstdout:\n%s\nstderr:\n%s", strings.Join(lines, "\n"), stderr.String())
	}
	args := strings.Fields(line)[1:]
	dir := filepath.Join(t.TempDir(), "hello")
	if out, err := git(t, t.TempDir(), append(args, dir)...); err != nil {
		t.Fatalf("%s: %v\n%s", line, err, out)
	}
	// The clone is of the node's dev/hello: a push lands and a second
	// clone sees it.
	commitFile(t, dir, "README.md", "hello", "first")
	mustGit(t, dir, "push", "-q", "origin", "HEAD:refs/heads/main")
	if out, err := git(t, t.TempDir(), append(args, filepath.Join(t.TempDir(), "again"))...); err != nil {
		t.Fatalf("second clone: %v\n%s", err, out)
	}
}
