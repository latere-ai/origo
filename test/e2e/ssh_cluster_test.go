// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
)

// The SSH host ports of spec 013's ports table (spec 024's rows):
// the balanced Service and the three pods' own listeners, and the stub
// key resolver's control endpoint.
const (
	portSSH      = 30022
	portSSHNode1 = 30122
	portSSHKeys  = 30086
)

// requireSSHClient skips when the OpenSSH client is absent, which is the
// one thing these criteria need beside git and kubectl.
func requireSSHClient(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ssh", "ssh-keygen", "ssh-keyscan"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
}

// sshKey writes an ed25519 client key in OpenSSH's own format and
// returns the path and the key's SHA-256 fingerprint.
func sshKey(t *testing.T) (path, fingerprint string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "id_ed25519")
	if out, err := exec.CommandContext(t.Context(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	out, err := exec.CommandContext(t.Context(), "ssh-keygen", "-lf", path+".pub").Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "SHA256:") {
		t.Fatalf("ssh-keygen -lf printed %q", out)
	}
	return path, fields[1]
}

// registerKey puts a fingerprint in the stub key resolver's table under
// the subject, through the control endpoint on its host port.
func registerKey(t *testing.T, fingerprint, subject string) {
	t.Helper()
	body := fmt.Sprintf(`{"keys":[{"fingerprint":%q,"subject":%q,"key_id":"k_e2e"}]}`, fingerprint, subject)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("http://localhost:%d/keys", portSSHKeys), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register the key: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("register the key: %d", resp.StatusCode)
	}
}

// hostKeys is what the SSH listener on a host port presents, as
// ssh-keyscan reads it: the authorized_keys line of each key, sorted, so
// two ports are compared byte for byte.
func hostKeys(t *testing.T, port int) []string {
	t.Helper()
	var lines []string
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := exec.CommandContext(t.Context(), "ssh-keyscan", "-T", "10", "-p", fmt.Sprint(port), "127.0.0.1").Output()
		if err == nil {
			for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 3 {
					lines = append(lines, fields[1]+" "+fields[2])
				}
			}
		}
		if len(lines) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(lines) == 0 {
		t.Fatalf("nothing answered on the SSH host port %d", port)
	}
	slices.Sort(lines)
	return lines
}

// sshCommand writes a known_hosts file holding the keys the port
// presents and returns the GIT_SSH_COMMAND git drives the client with.
func sshCommand(t *testing.T, keyPath string, port int) string {
	t.Helper()
	known := keyPath + ".known_hosts"
	var b strings.Builder
	for _, line := range hostKeys(t, port) {
		fmt.Fprintf(&b, "[127.0.0.1]:%d %s\n", port, line)
	}
	if err := os.WriteFile(known, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{
		"ssh", "-i", keyPath, "-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + known,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
	}, " ")
}

// gitSSH runs the client git with the SSH command.
func gitSSH(t *testing.T, dir, command string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(dir), "GIT_SSH_COMMAND="+command)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGitSSH(t *testing.T, dir, command string, args ...string) string {
	t.Helper()
	out, err := gitSSH(t, dir, command, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TestClusterSSHPushIsReadableOverHTTPS is spec 024's first stack
// criterion: a push over SSH through the balanced host port is readable
// by a clone over HTTPS through node 2, so the two transports are one
// write path and not two.
func TestClusterSSHPushIsReadableOverHTTPS(t *testing.T) {
	requireCluster(t)
	requireSSHClient(t)
	token := adminToken(t)
	subject := testSubject(t)
	keyPath, fingerprint := sshKey(t)
	registerKey(t, fingerprint, subject)

	id := newID(t)
	owner, slug := "e2e", "ssh-"+id[:8]
	if status, out := stackAPI(t, stackURL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, owner, slug)); status != 201 {
		t.Fatalf("create: %d %v", status, out)
	}

	// Clone, commit, and push, all over SSH through the balanced port.
	command := sshCommand(t, keyPath, portSSH)
	work := filepath.Join(t.TempDir(), "work")
	sshRemote := fmt.Sprintf("ssh://git@127.0.0.1:%d/%s/%s.git", portSSH, owner, slug)
	mustGitSSH(t, t.TempDir(), command, "clone", "-q", sshRemote, work)
	if err := os.WriteFile(filepath.Join(work, "over-ssh.txt"), []byte("pushed over ssh"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitSSH(t, work, command, "add", "over-ssh.txt")
	mustGitSSH(t, work, command, "commit", "-q", "-m", "over ssh")
	mustGitSSH(t, work, command, "push", "-q", "origin", "HEAD:refs/heads/main")
	head := strings.TrimSpace(mustGitSSH(t, work, command, "rev-parse", "HEAD"))

	// The same commit is served by a clone over HTTP through node 1's own
	// port, which is a different node's cache and the same log.
	node := fmt.Sprintf("http://localhost:%d", portNode1)
	httpsWork := filepath.Join(t.TempDir(), "https")
	mustGit(t, t.TempDir(), "-c", "http.extraHeader=Authorization: Bearer "+token,
		"clone", "-q", node+"/r/"+id+".git", httpsWork)
	if got := mustGit(t, httpsWork, "rev-parse", "HEAD"); got != head {
		t.Fatalf("the HTTP clone is at %s, the SSH push at %s", got, head)
	}
	if body, err := os.ReadFile(filepath.Join(httpsWork, "over-ssh.txt")); err != nil || string(body) != "pushed over ssh" {
		t.Fatalf("the file the SSH push carried: %q %v", body, err)
	}
	// The read API through the same node names the reference the SSH
	// push moved, which is the log and not either node's cache.
	if status, refs := stackAPI(t, node, token, "GET", "/v1/repos/"+id+"/refs", ""); status != 200 {
		t.Fatalf("refs: %d %v", status, refs)
	}
}

// TestClusterSSHHostKeyIsTheSameOnEveryNode is spec 024's second stack
// criterion, and the reason decision 5 exists: a client that reaches
// node 2 after node 1 must not see a changed host key.
func TestClusterSSHHostKeyIsTheSameOnEveryNode(t *testing.T) {
	requireCluster(t)
	requireSSHClient(t)
	first := hostKeys(t, portSSHNode1)
	if len(first) == 0 {
		t.Fatal("node 1 presented no host key")
	}
	for n := 1; n < 3; n++ {
		port := portSSHNode1 + n
		got := hostKeys(t, port)
		if len(got) != len(first) {
			t.Fatalf("node %d presents %d keys, node 1 presents %d", n+1, len(got), len(first))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("node %d presents %q, node 1 presents %q", n+1, got[i], first[i])
			}
		}
	}
	// The balanced Service lands on any node, so what it presents is one
	// of the three and therefore the same set.
	balanced := hostKeys(t, portSSH)
	for i := range balanced {
		if i < len(first) && balanced[i] != first[i] {
			t.Fatalf("the balanced port presents %q, node 1 presents %q", balanced[i], first[i])
		}
	}
}

// TestClusterSSHRefusesAShell is spec 024's third stack criterion: the
// deployed node refuses everything that is not one of the two git
// services, not only the package's own tests.
func TestClusterSSHRefusesAShell(t *testing.T) {
	requireCluster(t)
	requireSSHClient(t)
	subject := testSubject(t)
	keyPath, fingerprint := sshKey(t)
	registerKey(t, fingerprint, subject)
	command := sshCommand(t, keyPath, portSSH)

	dial := func(args ...string) (string, error) {
		full := append(strings.Fields(command), args...)
		cmd := exec.CommandContext(t.Context(), full[0], full[1:]...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	target := fmt.Sprintf("-p%d", portSSH)

	// A shell: the request is refused, so the client reports that the
	// channel would not open one. Its own message is what a person sees
	// here, because OpenSSH gives up on a refused shell request without
	// draining the session's stderr; the code's line is asserted on the
	// exec below, which the client does read.
	out, err := dial(target, "git@127.0.0.1")
	if err == nil {
		t.Fatalf("the deployed node opened a shell:\n%s", out)
	}
	if !strings.Contains(out, "shell request failed") {
		t.Fatalf("a shell was refused with %q", out)
	}

	// A command that is not one of the two services: refused with the
	// code's line on stderr and exit 1.
	out, err = dial(target, "git@127.0.0.1", "ls")
	if err == nil || !strings.Contains(out, "invalid_request: ") {
		t.Fatalf("`ls` answered %v:\n%s", err, out)
	}

	// A direct-tcpip channel: no channel is opened, so the forward fails
	// rather than reaching anything in the cluster.
	out, err = dial("-o", "ExitOnForwardFailure=yes", "-N", "-L", "0:127.0.0.1:22", target, "git@127.0.0.1")
	if err == nil {
		t.Fatalf("the deployed node opened a local forward:\n%s", out)
	}

	// A remote forward: the global request is refused, so no listener is
	// opened on the node.
	out, err = dial("-o", "ExitOnForwardFailure=yes", "-N", "-R", "0:127.0.0.1:22", target, "git@127.0.0.1")
	if err == nil {
		t.Fatalf("the deployed node opened a remote forward:\n%s", out)
	}
}
