// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/sshd"
	"github.com/latere-ai/origo/test/stubs/sshkeys"
)

// TestSSHListenerIsOptional is spec 024's start-up criterion on the
// node: without ORIGO_SSH_ADDR the node opens no SSH listener and runs
// exactly as it runs today, and with it and the other three variables
// the listener binds and speaks SSH. What it does with a connection is
// internal/sshd's own criteria; the SSH library stays confined to that
// package, so this test reads the protocol's version line and nothing
// more (spec 001, invariant 7).
func TestSSHListenerIsOptional(t *testing.T) {
	t.Run("unset opens no listener", func(t *testing.T) {
		n, stop := startNode(t, testEnv(t))
		defer func() { _ = stop() }()
		if n.ssh != nil {
			t.Error("the node built an SSH listener with ORIGO_SSH_ADDR unset")
		}
		if got := n.sshAddress(); got != "" {
			t.Errorf("the node bound %q", got)
		}
	})

	t.Run("set binds and speaks SSH", func(t *testing.T) {
		keys := sshkeys.NewHandler()
		srv := httptest.NewServer(keys.Handler())
		defer srv.Close()
		env := testEnv(t)
		env["ORIGO_SSH_ADDR"] = "127.0.0.1:0"
		env["ORIGO_SSH_HOST_KEYS"] = hostKeyFile(t)
		env["ORIGO_SSH_KEYS_URL"] = srv.URL
		env["ORIGO_SSH_KEYS_TOKEN"] = sshkeys.DefaultToken

		n, stop := startNode(t, env)
		defer func() { _ = stop() }()
		if n.ssh == nil {
			t.Fatal("the node built no SSH listener")
		}
		addr := n.sshAddress()
		if addr == "" {
			t.Fatal("the node bound no SSH address")
		}
		conn, err := net.DialTimeout("tcp", addr, 20*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(sshd.ServerVersion))
		if _, err := readFull(conn, buf); err != nil {
			t.Fatal(err)
		}
		if got := string(buf); got != sshd.ServerVersion {
			t.Errorf("the listener answered %q, want %q", got, sshd.ServerVersion)
		}
		// The start-up line names every host key's fingerprint, which is
		// what an operator publishes before a rotation.
		if got := n.ssh.Fingerprints(); !strings.HasPrefix(got, "SHA256:") {
			t.Errorf("the fingerprints line is %q", got)
		}
	})

	t.Run("a taken address fails start-up", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		keys := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer keys.Close()
		env := testEnv(t)
		env["ORIGO_SSH_ADDR"] = ln.Addr().String()
		env["ORIGO_SSH_HOST_KEYS"] = hostKeyFile(t)
		env["ORIGO_SSH_KEYS_URL"] = keys.URL
		env["ORIGO_SSH_KEYS_TOKEN"] = "t"
		var out, errOut strings.Builder
		if code := run(t.Context(), nil, getenv(env), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "ssh listener") {
			t.Fatalf("exit %d, stderr %q", code, errOut.String())
		}
	})
}

// readFull fills p from conn.
func readFull(conn net.Conn, p []byte) (int, error) {
	read := 0
	for read < len(p) {
		n, err := conn.Read(p[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// hostKeyFile writes an ed25519 host key as a PKCS#8 PEM block, one of
// the forms internal/sshd reads, and returns its path.
func hostKeyFile(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "host_ed25519")
	body := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
