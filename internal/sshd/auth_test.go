// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sshkeys"
)

// TestSSHKeyResolverFailsClosedAndRecovers is spec 024's fourth
// criterion: an unknown fingerprint is refused, a resolver that answers
// 500 or not at all refuses within the timeout and recovers without a
// restart, and a not-found answer is not held past its five seconds.
func TestSSHKeyResolverFailsClosedAndRecovers(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	f := newFixture(t, withNow(clock.Now))
	f.create(repoA, "acme", "app")

	t.Run("an unknown fingerprint", func(t *testing.T) {
		stranger, err := ssh.NewSignerFromKey(generateKey(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.dial(stranger); err == nil {
			t.Fatal("an unregistered key authenticated")
		}
		if n := f.auths("unknown_key"); n == 0 {
			t.Error("the refusal was not counted as an unknown key")
		}
		if n := f.keyCalls("not_found"); n == 0 {
			t.Error("the resolve call was not recorded as not found")
		}
	})

	t.Run("a not-found answer is not held past its window", func(t *testing.T) {
		stranger, err := ssh.NewSignerFromKey(generateKey(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.dial(stranger); err == nil {
			t.Fatal("an unregistered key authenticated")
		}
		before := len(f.keys.Requests())
		// Inside the window the node answers from its own cache.
		if _, err := f.dial(stranger); err == nil {
			t.Fatal("an unregistered key authenticated")
		}
		if got := len(f.keys.Requests()); got != before {
			t.Errorf("the negative answer was not cached: %d calls, want %d", got, before)
		}
		// Past it the store is asked again, so a key registered in the
		// meantime authenticates.
		clock.Advance(NotFoundTTL + time.Second)
		f.keys.Register(sshkeys.Key{Fingerprint: ssh.FingerprintSHA256(stranger.PublicKey()), Subject: "u_late"})
		if _, err := f.dial(stranger); err != nil {
			t.Fatalf("a key registered after the window was refused: %v", err)
		}
	})

	t.Run("a resolver that answers 500", func(t *testing.T) {
		fresh, err := ssh.NewSignerFromKey(generateKey(t))
		if err != nil {
			t.Fatal(err)
		}
		f.keys.Register(sshkeys.Key{Fingerprint: ssh.FingerprintSHA256(fresh.PublicKey()), Subject: "u_500"})
		f.keys.Fail(500)
		if _, err := f.dial(fresh); err == nil {
			t.Fatal("a 500 from the key store authenticated a key")
		}
		if n := f.auths("resolver_error"); n == 0 {
			t.Error("the refusal was not counted as a resolver error")
		}
		if n := f.keyCalls("error"); n == 0 {
			t.Error("the failed call was not recorded")
		}
		// The node recovers without a restart.
		f.keys.Resume()
		if _, err := f.dial(fresh); err != nil {
			t.Fatalf("the node did not recover: %v", err)
		}
	})

	t.Run("a resolver that never answers", func(t *testing.T) {
		fresh, err := ssh.NewSignerFromKey(generateKey(t))
		if err != nil {
			t.Fatal(err)
		}
		f.keys.Register(sshkeys.Key{Fingerprint: ssh.FingerprintSHA256(fresh.PublicKey()), Subject: "u_hang"})
		f.keys.Hang()
		start := time.Now()
		_, err = f.dial(fresh)
		waited := time.Since(start)
		if err == nil {
			t.Fatal("a hung key store authenticated a key")
		}
		if waited > KeysTimeout+5*time.Second {
			t.Errorf("the refusal took %s, want at most the timeout and a margin", waited)
		}
		f.keys.Resume()
		if _, err := f.dial(fresh); err != nil {
			t.Fatalf("the node did not recover: %v", err)
		}
	})
}

// TestSSHRevokedKeyStopsWithinTTL is the other half of the fourth
// criterion: a key revoked at the store stops authenticating within the
// ttl the store named, measured on a clock the test moves.
func TestSSHRevokedKeyStopsWithinTTL(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	f := newFixture(t, withNow(clock.Now))
	fingerprint := ssh.FingerprintSHA256(f.client.PublicKey())
	f.keys.SetKeys(sshkeys.Key{Fingerprint: fingerprint, Subject: "u_7f3c", TTL: 120})

	if _, err := f.dial(); err != nil {
		t.Fatalf("a registered key was refused: %v", err)
	}
	f.keys.Revoke(fingerprint)
	// Inside the ttl the node still holds the answer, which is the cost
	// the ttl buys and what an operator sets it against.
	clock.Advance(60 * time.Second)
	if _, err := f.dial(); err != nil {
		t.Fatalf("a key inside its ttl was refused: %v", err)
	}
	// Past it the store is asked again and the key is gone.
	clock.Advance(61 * time.Second)
	if _, err := f.dial(); err == nil {
		t.Fatal("a revoked key still authenticates past its ttl")
	}
}

// TestSSHAuthorizerDecidesTheOperation is spec 024's fifth criterion:
// the key names the subject and the authorizer decides the operation. A
// subject allowed read clones and is refused git-receive-pack with the
// forbidden line and exit 1, and every call carries the empty actor,
// because delegation does not exist over SSH.
func TestSSHAuthorizerDecidesTheOperation(t *testing.T) {
	f := newFixture(t)
	f.create(repoA, "acme", "app")
	f.authz.SetRules(
		authorizer.Rule{Subject: "u_7f3c", Action: "read", Allow: true},
		authorizer.Rule{Subject: "u_7f3c", Action: "write", Allow: false, Reason: "read only"},
	)

	if status, _, errOut := f.exec(f.mustDial(), "git-upload-pack '/acme/app.git'", strings.NewReader("0000")); status != 0 {
		t.Errorf("upload-pack exited %d: %s", status, errOut)
	}
	status, _, errOut := f.exec(f.mustDial(), "git-receive-pack '/acme/app.git'", strings.NewReader("0000"))
	if status != 1 {
		t.Errorf("receive-pack exited %d, want 1", status)
	}
	if got := strings.TrimSpace(errOut); got != contract.Line(contract.CodeForbidden) {
		t.Errorf("stderr = %q, want the forbidden line", got)
	}

	requests := f.authz.Requests()
	if len(requests) != 2 {
		t.Fatalf("the authorizer saw %d requests, want two", len(requests))
	}
	for _, r := range requests {
		if r.Subject != "u_7f3c" || r.Actor != "" {
			t.Errorf("call carried subject %q actor %q; SSH has no delegation", r.Subject, r.Actor)
		}
		if r.Repo.ID != repoA {
			t.Errorf("call named repository %+v, want the resolved id", r.Repo)
		}
	}
	if requests[0].Action != "read" || requests[1].Action != "write" {
		t.Errorf("actions = %q %q, want read then write", requests[0].Action, requests[1].Action)
	}

	// An authorizer that answers nothing is not an allow. The repository
	// is one no decision was cached for, so the outage is what decides.
	f.create(repoB, "acme", "other")
	f.authz.Fail(500)
	status, _, errOut = f.exec(f.mustDial(), "git-upload-pack '/r/"+repoB+".git'", strings.NewReader("0000"))
	if status != 1 || strings.TrimSpace(errOut) != contract.Line(contract.CodeAuthorizerUnavailable) {
		t.Errorf("an authorizer outage: exit %d, stderr %q", status, errOut)
	}
}

// TestSSHDenyBeforeLookup is the other half of the fifth criterion: on
// an unknown name and on an unknown id the authorizer is asked before
// any read of the repository, so a denied caller cannot tell a
// repository apart from one that does not exist.
func TestSSHDenyBeforeLookup(t *testing.T) {
	f := newFixture(t)
	f.create(repoA, "acme", "app")
	f.authz.SetRules(authorizer.Rule{Subject: "u_7f3c", Allow: false, Reason: "no"})

	const unknownID = "11111111-2222-4333-8444-555555555555"
	for _, path := range []string{"/acme/app.git", "/acme/missing.git", "/r/" + unknownID + ".git"} {
		status, _, errOut := f.exec(f.mustDial(), "git-upload-pack '"+path+"'", strings.NewReader("0000"))
		if status != 1 {
			t.Errorf("%s exited %d, want 1", path, status)
		}
		if got := strings.TrimSpace(errOut); got != contract.Line(contract.CodeForbidden) {
			t.Errorf("%s: stderr = %q, want the forbidden line, the same for one that exists and one that does not", path, got)
		}
	}
	if got := len(f.authz.Requests()); got != 3 {
		t.Errorf("the authorizer saw %d requests, want one per path", got)
	}

	// An allowed caller reaches the lookup and reads repo_not_found,
	// which is the one answer that says a repository is absent. The
	// paths are other ones, because a deny is cached by id for its own
	// window (spec 007) and this criterion is about the order of the two
	// calls, not about the cache.
	f.authz.SetRules(authorizer.Rule{Subject: "u_7f3c", Allow: true})
	for _, path := range []string{"/acme/gone.git", "/r/22222222-3333-4444-8555-666666666666.git"} {
		status, _, errOut := f.exec(f.mustDial(), "git-upload-pack '"+path+"'", strings.NewReader("0000"))
		if status != 1 || strings.TrimSpace(errOut) != contract.Line(contract.CodeRepoNotFound) {
			t.Errorf("%s: exit %d, stderr %q, want the repo_not_found line", path, status, errOut)
		}
	}
}

// TestSSHRefusalsAreTheTableSentences is spec 024's sixth criterion:
// every refusal reaches the client as contract.Line of its code on
// stderr, byte for byte, with the wait on a line of its own where there
// is one, and a stale read carries Origo-Stale.
func TestSSHRefusalsAreTheTableSentences(t *testing.T) {
	t.Run("rate_limited names its wait", func(t *testing.T) {
		f := newFixture(t, withLimits(limits.Options{PerMinute: 1, Burst: 1}))
		f.create(repoA, "acme", "app")
		if status, _, errOut := f.exec(f.mustDial(), "git-upload-pack '/acme/app.git'", strings.NewReader("0000")); status != 0 {
			t.Fatalf("the first session was refused: %d %s", status, errOut)
		}
		status, _, errOut := f.exec(f.mustDial(), "git-upload-pack '/acme/app.git'", strings.NewReader("0000"))
		lines := strings.Split(strings.TrimSpace(errOut), "\n")
		if status != 1 || lines[0] != contract.Line(contract.CodeRateLimited) {
			t.Fatalf("exit %d, stderr %q", status, errOut)
		}
		if len(lines) != 2 || !strings.HasPrefix(lines[1], "retry after ") {
			t.Errorf("stderr = %q, want the wait on a line of its own", errOut)
		}
	})

	t.Run("over_quota on the single-push bound", func(t *testing.T) {
		requireSSH(t)
		f := newFixture(t, withLimits(limits.Options{MaxPushBytes: 512}))
		f.create(repoA, "acme", "app")
		sshCommand := f.sshClient(t)
		host, port, err := splitAddr(f.addr)
		if err != nil {
			t.Fatal(err)
		}
		work := f.workingCopy(t, sshCommand, host, port)
		writeBlob(t, work, 64<<10)
		f.mustGit(t, work, sshCommand, "add", "big.bin")
		f.mustGit(t, work, sshCommand, "commit", "-q", "-m", "big")
		out, err := f.runGit(t, work, sshCommand, "push", "origin", "HEAD:refs/heads/main")
		if err == nil {
			t.Fatal("a push past the single-push bound was accepted")
		}
		if !strings.Contains(out, contract.Line(contract.CodeOverQuota)) {
			t.Errorf("the push output does not carry the over_quota line:\n%s", out)
		}
	})

	t.Run("storage_unavailable when the write breaker is open", func(t *testing.T) {
		f := newFixture(t, withBreakers(5*time.Minute))
		f.create(repoA, "acme", "app")
		f.openWrites()
		status, _, errOut := f.exec(f.mustDial(), "git-receive-pack '/r/"+repoA+".git'", strings.NewReader("0000"))
		if status != 1 || strings.TrimSpace(errOut) != contract.Line(contract.CodeStorageUnavailable) {
			t.Errorf("exit %d, stderr %q, want the storage_unavailable line", status, errOut)
		}
	})

	t.Run("a stale read carries Origo-Stale and a stale write is refused", func(t *testing.T) {
		f := newFixture(t, withBreakers(5*time.Minute))
		f.create(repoA, "acme", "app")
		// Warm the local copy, then cut every read: the lease is served
		// from the copy and says how old it is.
		if status, _, errOut := f.exec(f.mustDial(), "git-upload-pack '/acme/app.git'", strings.NewReader("0000")); status != 0 {
			t.Fatalf("the warming clone was refused: %d %s", status, errOut)
		}
		// The bucket goes away. The check that runs into it fails on its
		// own error and opens the read breaker, so the first read after
		// it is refused and the next is served from the copy. The id form
		// is what a stale read is served in: the name form resolves
		// origo/names/<owner>/<slug> against the bucket first, which an
		// open read breaker refuses before any copy is reached.
		f.cutReads()
		f.clock.Advance(60 * time.Second)
		if status, _, _ := f.exec(f.mustDial(), "git-upload-pack '/r/"+repoA+".git'", strings.NewReader("0000")); status != 1 {
			t.Fatalf("the failing check was not a refusal: %d", status)
		}
		status, _, errOut := f.exec(f.mustDial(), "git-upload-pack '/r/"+repoA+".git'", strings.NewReader("0000"))
		if status != 0 {
			t.Fatalf("a stale read was refused: %d %s", status, errOut)
		}
		if !strings.Contains(errOut, contract.HeaderStale+": 60") {
			t.Errorf("stderr = %q, want the age of the copy", errOut)
		}
		status, _, errOut = f.exec(f.mustDial(), "git-receive-pack '/r/"+repoA+".git'", strings.NewReader("0000"))
		if status != 1 || strings.TrimSpace(errOut) != contract.Line(contract.CodeStorageUnavailable) {
			t.Errorf("a push on a stale copy: exit %d, stderr %q", status, errOut)
		}
	})
}

// TestSSHUnauthenticatedConnectionIsBounded is spec 024's eighth
// criterion: before authentication the connection is cheap and bounded.
func TestSSHUnauthenticatedConnectionIsBounded(t *testing.T) {
	f := newFixture(t, withHandshake(500*time.Millisecond))

	t.Run("a client that sends nothing is closed", func(t *testing.T) {
		conn, err := net.Dial("tcp", f.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		// The server sends its version and then waits; the read ends when
		// the deadline closes the connection.
		start := time.Now()
		for {
			if _, err := conn.Read(buf); err != nil {
				break
			}
		}
		if waited := time.Since(start); waited > 10*time.Second {
			t.Errorf("the connection was held %s", waited)
		}
	})

	t.Run("a fourth public key attempt is refused", func(t *testing.T) {
		var offers []ssh.Signer
		for range 6 {
			signer, err := ssh.NewSignerFromKey(generateKey(t))
			if err != nil {
				t.Fatal(err)
			}
			offers = append(offers, signer)
		}
		// The last key is the registered one; the server never reaches it
		// because MaxAuthTries stops the connection first.
		offers = append(offers, f.client)
		if _, err := f.dial(offers...); err == nil {
			t.Fatal("a connection offering seven keys authenticated")
		}
	})

	t.Run("no password and no keyboard-interactive method", func(t *testing.T) {
		_, err := ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
			User:            "git",
			Auth:            []ssh.AuthMethod{ssh.Password("x")},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // the host key is another criterion
			Timeout:         20 * time.Second,
		})
		if err == nil {
			t.Fatal("a password authenticated")
		}
		if !strings.Contains(err.Error(), "publickey") && !strings.Contains(err.Error(), "no supported methods") {
			t.Errorf("the refusal names %v, want publickey as the one method", err)
		}
	})
}

// TestSSHServerNeedsItsParts holds the constructor's requirements, so a
// node that is missing one fails at start-up rather than on a
// connection.
func TestSSHServerNeedsItsParts(t *testing.T) {
	f := newFixture(t)
	base := Options{HostKeys: f.hosts, Keys: f.server.keys, Guard: f.server.guard, Cache: f.cache, Git: f.git}
	for name, mutate := range map[string]func(*Options){
		"no host key":  func(o *Options) { o.HostKeys = nil },
		"no resolver":  func(o *Options) { o.Keys = nil },
		"no guard":     func(o *Options) { o.Guard = nil },
		"no cache":     func(o *Options) { o.Cache = nil },
		"no git":       func(o *Options) { o.Git = nil },
		"empty keyset": func(o *Options) { o.HostKeys = &HostKeys{} },
	} {
		o := base
		mutate(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: the server was built", name)
		}
	}
	if _, err := New(base); err != nil {
		t.Errorf("a complete set of parts: %v", err)
	}
	if _, err := NewResolver(ResolverOptions{URL: "https://keys.example"}); err == nil {
		t.Error("a resolver was built without an HTTP client")
	}
	if _, err := NewResolver(ResolverOptions{HTTP: f.server.keys.http}); err == nil {
		t.Error("a resolver was built without a URL")
	}
	if _, err := ParseHostKeys(nil); err == nil {
		t.Error("an empty host key list was accepted")
	}
}
