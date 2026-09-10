// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

// hostAlias is the name the ssh_config below maps to the fixture's
// listener, so the scp form git@host:owner/slug.git, which has no place
// to put a port, reaches an ephemeral one.
const hostAlias = "origo-test"

// sshClient writes what the real OpenSSH client needs to reach the
// fixture: the client key, a known_hosts file holding the key the
// listener presents, and a configuration naming the port. It returns the
// GIT_SSH_COMMAND git drives it with.
func (f *fixture) sshClient(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	keyPath := writeKey(t, dir, "id", f.clientKey)
	host, port, err := splitAddr(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	// known_hosts carries the key the listener presents under both names
	// the two URL forms reach it by.
	var known bytes.Buffer
	for _, signer := range f.hosts.Presented() {
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
		fmt.Fprintf(&known, "[%s]:%s %s\n", host, port, line)
		fmt.Fprintf(&known, "%s %s\n", hostAlias, line)
	}
	knownPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownPath, known.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`Host %s
  HostName %s
  Port %s
Host *
  User git
  IdentityFile %s
  IdentitiesOnly yes
  UserKnownHostsFile %s
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  BatchMode yes
  PubkeyAcceptedAlgorithms +ecdsa-sha2-nistp256
`, hostAlias, host, port, keyPath, knownPath)
	configPath := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return "ssh -F " + configPath
}

func splitAddr(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("sshd: %q is not host:port", addr)
	}
	return addr[:i], addr[i+1:], nil
}

// git runs the client git against a working directory with the fixture's
// SSH command.
func (f *fixture) runGit(t *testing.T, dir, sshCommand string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(dir), "GIT_SSH_COMMAND="+sshCommand, "GIT_SSH_VARIANT=ssh")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	return out.String() + stderr.String(), err
}

func (f *fixture) mustGit(t *testing.T, dir, sshCommand string, args ...string) string {
	t.Helper()
	out, err := f.runGit(t, dir, sshCommand, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TestSSHCloneFetchPushWithRealGit is spec 024's first criterion: the
// real git client clones, fetches, and pushes over SSH in both URL
// forms, and the entry the push commits carries the subject the key
// resolved to and an empty actor, because delegation does not exist over
// SSH.
func TestSSHCloneFetchPushWithRealGit(t *testing.T) {
	requireSSH(t)
	f := newFixture(t)
	f.create(repoA, "acme", "app")
	sshCommand := f.sshClient(t)
	host, port, err := splitAddr(f.addr)
	if err != nil {
		t.Fatal(err)
	}

	// The id form, over the ssh:// URL a port can be written in.
	work := filepath.Join(t.TempDir(), "work")
	idURL := fmt.Sprintf("ssh://git@%s:%s/r/%s.git", host, port, repoA)
	f.mustGit(t, t.TempDir(), sshCommand, "clone", "-q", idURL, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustGit(t, work, sshCommand, "add", "a.txt")
	f.mustGit(t, work, sshCommand, "commit", "-q", "-m", "first")
	f.mustGit(t, work, sshCommand, "push", "-q", "origin", "HEAD:refs/heads/main")
	first := strings.TrimSpace(f.mustGit(t, work, sshCommand, "rev-parse", "HEAD"))

	ix, _, err := f.log.Newest(context.Background(), repoA, 0, false)
	if err != nil || ix.Seq != 1 || ix.Refs["refs/heads/main"] != first {
		t.Fatalf("after the push: %+v, %v", ix, err)
	}
	// The entry carries the subject the key resolved to and no actor.
	rc, _, err := f.store.Get(context.Background(), f.log.RepoPrefix(repoA)+ix.Entry, "")
	if err != nil {
		t.Fatal(err)
	}
	hdr, refs, _, err := wal.ReadEntryHead(rc)
	_ = rc.Close()
	if err != nil || hdr.Subject != "u_7f3c" || hdr.Actor != "" || len(refs) != 1 {
		t.Fatalf("entry header %+v refs %+v, %v", hdr, refs, err)
	}

	// The name form, over the scp syntax that carries no port, through
	// the host alias of the configuration.
	second := filepath.Join(t.TempDir(), "second")
	f.mustGit(t, t.TempDir(), sshCommand, "clone", "-q", "git@"+hostAlias+":acme/app.git", second)
	if got := gittest.RevList(t, second); got != gittest.RevList(t, work) {
		t.Fatal("the scp form cloned a different history")
	}

	// A push through the name form, then a fetch through the id form.
	if err := os.WriteFile(filepath.Join(second, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustGit(t, second, sshCommand, "add", "b.txt")
	f.mustGit(t, second, sshCommand, "commit", "-q", "-m", "second")
	f.mustGit(t, second, sshCommand, "tag", "v1")
	f.mustGit(t, second, sshCommand, "push", "-q", "--atomic", "origin", "HEAD:refs/heads/dev", "refs/tags/v1")
	head := strings.TrimSpace(f.mustGit(t, second, sshCommand, "rev-parse", "HEAD"))
	f.mustGit(t, work, sshCommand, "fetch", "-q", "origin")
	if got := strings.TrimSpace(f.mustGit(t, work, sshCommand, "rev-parse", "origin/dev")); got != head {
		t.Fatalf("fetched dev = %s, want %s", got, head)
	}
	ix, _, _ = f.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 2 || ix.Refs["refs/tags/v1"] != head || ix.Refs["refs/heads/dev"] != head {
		t.Fatalf("after the second push: %+v", ix)
	}
	if n := f.sessions("receive-pack", "ok"); n != 2 {
		t.Errorf("receive-pack sessions = %d, want 2", n)
	}
	if n := f.sessions("upload-pack", "ok"); n < 3 {
		t.Errorf("upload-pack sessions = %d, want one per clone and fetch", n)
	}
	// One resolve call per fingerprint per ttl, not one per connection.
	if n := len(f.keys.Requests()); n != 1 {
		t.Errorf("resolve calls = %d, want one for the run's ttl", n)
	}
}

// requireSSH skips when the OpenSSH client is not installed, which is
// the one thing this criterion needs beside git.
func requireSSH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("the OpenSSH client is not on PATH")
	}
}
