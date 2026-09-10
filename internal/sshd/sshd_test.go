// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/latere-ai/origo/internal/contract"
)

// exec runs one command on a session of its own and reports the exit
// status, the service's output, and its stderr. Every criterion below
// drives the listener with it, because one connection carries one
// operation.
func (f *fixture) exec(c *ssh.Client, command string, stdin io.Reader) (status int, stdout, stderr string) {
	f.t.Helper()
	session, err := c.NewSession()
	if err != nil {
		f.t.Fatalf("session: %v", err)
	}
	defer func() { _ = session.Close() }()
	var out, errOut strings.Builder
	session.Stdout, session.Stderr, session.Stdin = &out, &errOut, stdin
	err = session.Run(command)
	if err != nil {
		var exit *ssh.ExitError
		if !errors.As(err, &exit) {
			f.t.Fatalf("run %q: %v", command, err)
		}
		status = exit.ExitStatus()
	}
	return status, out.String(), errOut.String()
}

// raw is a client connection whose global requests the test reads
// itself, which is what the host key announcement needs: ssh.Dial hands
// the request channel to the client library, which answers and discards
// every request the server sends.
type raw struct {
	conn  ssh.Conn
	reqs  <-chan *ssh.Request
	chans <-chan ssh.NewChannel
}

func (f *fixture) dialRaw(signer ssh.Signer, algorithms ...string) (*raw, ssh.PublicKey) {
	f.t.Helper()
	nc, err := net.DialTimeout("tcp", f.addr, 20*time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	var offered ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: "git",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			offered = key
			return nil
		},
		HostKeyAlgorithms: algorithms,
		Timeout:           20 * time.Second,
	}
	conn, chans, reqs, err := ssh.NewClientConn(nc, f.addr, cfg)
	if err != nil {
		_ = nc.Close()
		f.t.Fatalf("dial: %v", err)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	return &raw{conn: conn, reqs: reqs, chans: chans}, offered
}

// waitFor reads the server's global requests until one of the type
// arrives, refusing the rest the way a client that does not know them
// would.
func (r *raw) waitFor(t *testing.T, kind string) []byte {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case req, ok := <-r.reqs:
			if !ok {
				t.Fatalf("the connection closed before %s arrived", kind)
			}
			if req.Type == kind {
				return req.Payload
			}
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		case <-deadline:
			t.Fatalf("%s did not arrive", kind)
		}
	}
}

// sessionRequest opens a session channel, sends one request on it, and
// reads what the server answered: whether the request was accepted, the
// exit status, and the session's stderr. It is the low-level form the
// requests a git client never sends are driven with.
func (f *fixture) sessionRequest(c *ssh.Client, kind string, payload []byte) (accepted bool, status int, stderr string) {
	f.t.Helper()
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		f.t.Fatalf("session channel: %v", err)
	}
	defer func() { _ = ch.Close() }()
	errs := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(ch.Stderr())
		errs <- string(out)
	}()
	accepted, err = ch.SendRequest(kind, true, payload)
	if err != nil {
		f.t.Fatalf("%s: %v", kind, err)
	}
	status = -1
	for req := range reqs {
		if req.Type == "exit-status" {
			var e struct{ Status uint32 }
			if err := ssh.Unmarshal(req.Payload, &e); err == nil {
				status = int(e.Status)
			}
		}
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
	}
	return accepted, status, <-errs
}

// TestSSHRefusesEverythingButTheTwoServices walks the maintained list of
// decision 7, one entry at a time. Nothing on it opens a subprocess,
// reads the repository, or asks the authorizer, which the stub's request
// list and the fetch counter prove together.
func TestSSHRefusesEverythingButTheTwoServices(t *testing.T) {
	f := newFixture(t)
	f.create(repoA, "acme", "app")

	t.Run("a channel type that is not a session", func(t *testing.T) {
		c := f.mustDial()
		for _, kind := range []string{"direct-tcpip", "direct-streamlocal@openssh.com", "x11", "forwarded-tcpip"} {
			_, _, err := c.OpenChannel(kind, nil)
			var open *ssh.OpenChannelError
			if !errors.As(err, &open) || open.Reason != ssh.UnknownChannelType {
				t.Errorf("%s opened or was refused with %v", kind, err)
			}
		}
	})

	t.Run("a second session channel", func(t *testing.T) {
		c := f.mustDial()
		first, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = first.Close() }()
		if _, err := c.NewSession(); err == nil {
			t.Error("a second session was opened on one connection")
		}
	})

	t.Run("a shell", func(t *testing.T) {
		ok, status, errOut := f.sessionRequest(f.mustDial(), "shell", nil)
		if ok {
			t.Fatal("a shell was opened")
		}
		if status != 1 {
			t.Errorf("exit status = %d, want 1", status)
		}
		if got := strings.TrimSpace(errOut); got != contract.Line(contract.CodeInvalid) {
			t.Errorf("stderr = %q, want the invalid_request line", got)
		}
	})

	t.Run("a command that is not one of the two services", func(t *testing.T) {
		for _, command := range []string{
			"ls",
			"ls -la /",
			"git-upload-archive '/acme/app.git'",
			"git-lfs-authenticate acme/app.git download",
			"git-receive-pack",
			"scp -t /tmp",
			"git-upload-pack '/acme/app.git'; sh",
			"git-upload-pack '/acme/app.git' extra",
		} {
			status, _, errOut := f.exec(f.mustDial(), command, nil)
			if status != 1 {
				t.Errorf("%q exited %d, want 1", command, status)
			}
			if got := strings.TrimSpace(errOut); got != contract.Line(contract.CodeInvalid) {
				t.Errorf("%q: stderr = %q, want the invalid_request line", command, got)
			}
		}
	})

	t.Run("a subsystem", func(t *testing.T) {
		for _, name := range []string{"sftp", "netconf"} {
			ok, status, errOut := f.sessionRequest(f.mustDial(), "subsystem", ssh.Marshal(struct{ Name string }{name}))
			if ok {
				t.Errorf("the %s subsystem was opened", name)
			}
			if status != 1 || strings.TrimSpace(errOut) != contract.Line(contract.CodeInvalid) {
				t.Errorf("%s: exit %d, stderr %q", name, status, errOut)
			}
		}
	})

	t.Run("a pty, an environment variable, X11, and an agent socket", func(t *testing.T) {
		c := f.mustDial()
		session, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = session.Close() }()
		if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
			t.Error("a pty was allocated")
		}
		if err := session.Setenv("GIT_PROTOCOL", "version=2"); err == nil {
			t.Error("an environment variable reached the session")
		}
		for _, kind := range []string{"x11-req", "auth-agent-req@openssh.com", "signal", "window-change"} {
			ok, err := session.SendRequest(kind, true, nil)
			if ok || err != nil {
				t.Errorf("%s: accepted %v, %v", kind, ok, err)
			}
		}
	})

	t.Run("every forwarding request", func(t *testing.T) {
		c := f.mustDial()
		for _, kind := range []string{"tcpip-forward", "cancel-tcpip-forward", "streamlocal-forward@openssh.com"} {
			ok, _, err := c.SendRequest(kind, true, nil)
			if ok || err != nil {
				t.Errorf("%s: accepted %v, %v", kind, ok, err)
			}
		}
		if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
			t.Error("a remote forward opened a listener")
		}
	})

	// Nothing above reached the authorizer or started a subprocess: every
	// entry of the list is refused before either. The one operation that
	// does reach them is the legitimate first exec of the case below.
	if got := f.authz.Requests(); len(got) != 0 {
		t.Errorf("a refusal asked the authorizer: %+v", got)
	}
	if got := f.set.Fetches.Value(nil); got != 0 {
		t.Errorf("a refusal ran upload-pack: %d", got)
	}

	t.Run("a second exec on one session", func(t *testing.T) {
		c := f.mustDial()
		session, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = session.Close() }()
		var errOut strings.Builder
		session.Stderr = &errOut
		stdin, err := session.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Start("git-upload-pack '/acme/app.git'"); err != nil {
			t.Fatal(err)
		}
		// The first service is running; a second exec on the same session
		// is refused without disturbing it.
		ok, err := session.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{"git-upload-pack '/acme/app.git'"}))
		if ok || err != nil {
			t.Errorf("a second exec was accepted: %v %v", ok, err)
		}
		_ = stdin.Close()
		_ = session.Wait()
		if !strings.Contains(errOut.String(), contract.Line(contract.CodeInvalid)) {
			t.Errorf("stderr = %q, want the invalid_request line", errOut.String())
		}
	})

	// The one authorizer call of the whole test is the second-exec case's
	// first exec, which is a real read, and it never became a write.
	if got := f.authz.Requests(); len(got) != 1 || got[0].Action != "read" {
		t.Errorf("the authorizer saw %+v", got)
	}
	if n := f.sessions("receive-pack", "ok"); n != 0 {
		t.Errorf("a refusal committed a push: %d", n)
	}
}

// TestSSHHostKeysArePresentedAndAnnounced is decision 5: with three keys
// configured, two of one algorithm, the first of each algorithm is
// presented, every key reaches the client through
// hostkeys-00@openssh.com, and each one signs the challenge of
// hostkeys-prove-00@openssh.com.
func TestSSHHostKeysArePresentedAndAnnounced(t *testing.T) {
	first, second, third := generateEd25519(t), generateEd25519(t), generateKey(t)
	f := newFixture(t, withHostKeys(first, second, third))

	if got := len(f.hosts.All()); got != 3 {
		t.Fatalf("configured keys = %d, want 3", got)
	}
	presented := f.hosts.Presented()
	if len(presented) != 2 || presented[0].PublicKey().Type() != AlgoED25519 || presented[1].PublicKey().Type() != AlgoECDSA {
		t.Fatalf("presented = %v", algosOf(presented))
	}
	if got := len(f.hosts.Fingerprints()); got != 3 {
		t.Errorf("fingerprints = %d, want one per key", got)
	}

	c, offered := f.dialRaw(f.client, ssh.KeyAlgoED25519)
	if !equalKeys(offered, presented[0].PublicKey()) {
		t.Errorf("the connection was offered %s, want the first ed25519 key", ssh.FingerprintSHA256(offered))
	}

	blobs, err := readStrings(c.waitFor(t, hostKeysRequest))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 3 {
		t.Fatalf("the announcement carries %d keys, want 3", len(blobs))
	}
	for i, blob := range blobs {
		if !equalKeys(mustParseKey(t, blob), f.hosts.All()[i].PublicKey()) {
			t.Errorf("announced key %d is not the configured one", i)
		}
	}

	var ask []byte
	for _, blob := range blobs {
		ask = appendString(ask, blob)
	}
	ok, reply, err := c.conn.SendRequest(hostKeysProve, true, ask)
	if err != nil || !ok {
		t.Fatalf("hostkeys-prove: %v %v", ok, err)
	}
	sigs, err := readStrings(reply)
	if err != nil || len(sigs) != 3 {
		t.Fatalf("proof carries %d signatures: %v", len(sigs), err)
	}
	for i, rawSig := range sigs {
		var sig ssh.Signature
		if err := ssh.Unmarshal(rawSig, &sig); err != nil {
			t.Fatalf("signature %d: %v", i, err)
		}
		var data []byte
		data = appendString(data, []byte(hostKeysProve))
		data = appendString(data, c.conn.SessionID())
		data = appendString(data, blobs[i])
		if err := mustParseKey(t, blobs[i]).Verify(data, &sig); err != nil {
			t.Errorf("signature %d does not verify: %v", i, err)
		}
	}

	// A key this server does not hold is refused rather than answered
	// with a short reply a client could read as a proof, and so is a
	// payload that does not parse.
	stranger, err := ssh.NewSignerFromKey(generateEd25519(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{appendString(nil, stranger.PublicKey().Marshal()), {0, 0}, nil, {0, 0, 0, 9, 1}} {
		if ok, _, err := c.conn.SendRequest(hostKeysProve, true, payload); ok || err != nil {
			t.Errorf("a proof was given for %x: %v %v", payload, ok, err)
		}
	}
}

func algosOf(signers []ssh.Signer) []string {
	out := make([]string, 0, len(signers))
	for _, s := range signers {
		out = append(out, s.PublicKey().Type())
	}
	return out
}

func equalKeys(a, b ssh.PublicKey) bool {
	return a != nil && b != nil && ssh.FingerprintSHA256(a) == ssh.FingerprintSHA256(b)
}

func mustParseKey(t *testing.T, blob []byte) ssh.PublicKey {
	t.Helper()
	key, err := ssh.ParsePublicKey(blob)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
