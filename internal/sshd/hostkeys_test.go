// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
)

// TestHostKeysAreRefusedByAlgorithmAndSize holds the start-up rule: the
// three algorithms of spec 024 are host keys, RSA below the floor and
// every other algorithm are problems, and the message names every one
// of them at once.
func TestHostKeysAreRefusedByAlgorithmAndSize(t *testing.T) {
	dir := t.TempDir()
	strong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The host key algorithms are the client key algorithms: ed25519,
	// the three ECDSA curves, and RSA at 2048 bits or more (spec 024,
	// decision 10, widened on 2026-09-12).
	good := []crypto.PrivateKey{generateEd25519(t), generateKey(t), strong, p384}
	var paths []string
	for i, k := range good {
		paths = append(paths, writeKey(t, dir, "good"+string(rune('a'+i)), k))
	}
	keys, err := ParseHostKeys(paths)
	if err != nil {
		t.Fatalf("four host key algorithms were refused: %v", err)
	}
	if got := algosOf(keys.Presented()); len(got) != 4 {
		t.Errorf("presented = %v, want one per algorithm", got)
	}
	// An algorithm outside the set is refused by name. ssh-dss is the
	// one x/crypto still parses and MarshalPrivateKey cannot write, so
	// testdata/ssh-dss.pem is a checked-in OpenSSL-format key of that
	// type (generated once with crypto/dsa; it guards nothing).
	missing := filepath.Join(dir, "absent")
	bad := ParseHostKeysError(t, []string{
		writeKey(t, dir, "weak", weak),
		filepath.Join("testdata", "ssh-dss.pem"),
		missing,
	})
	for _, want := range []string{"at least 2048 bits", "ssh-dss is not a host key algorithm", "absent"} {
		if !strings.Contains(bad, want) {
			t.Errorf("the message %q does not name %q", bad, want)
		}
	}
	if _, err := ParseHostKeys(nil); err == nil {
		t.Error("an empty list was accepted")
	}
	// A key that is not a key at all.
	notAKey := filepath.Join(dir, "notakey")
	if err := os.WriteFile(notAKey, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseHostKeys([]string{notAKey}); err == nil {
		t.Error("a file that is not a key was accepted")
	}
	// rsaBits reads 0 for a key that exposes no modulus, which is every
	// algorithm that is not RSA.
	if got := rsaBits(keys.All()[0].PublicKey()); got != 0 {
		t.Errorf("rsaBits of an ed25519 key = %d", got)
	}
	if got := rsaBits(keys.All()[2].PublicKey()); got != 2048 {
		t.Errorf("rsaBits of the RSA key = %d", got)
	}
}

// ParseHostKeysError is ParseHostKeys' message, for a case that asserts
// on what an operator reads.
func ParseHostKeysError(t *testing.T, paths []string) string {
	t.Helper()
	if _, err := ParseHostKeys(paths); err != nil {
		return err.Error()
	}
	t.Fatal("the keys were accepted")
	return ""
}

// TestResolverAnswersAreCachedByFingerprint holds the node's cache and
// the shapes the endpoint may answer with.
func TestResolverAnswersAreCachedByFingerprint(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	calls := 0
	var body string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("the call carried %q", got)
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	r, err := NewResolver(ResolverOptions{URL: srv.URL, Token: "t", HTTP: srv.Client(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(generateKey(t))
	if err != nil {
		t.Fatal(err)
	}
	key := signer.PublicKey()

	// The request carries the fingerprint, the algorithm, and the
	// authorized_keys form.
	req := RequestFor(key)
	if req.Fingerprint != ssh.FingerprintSHA256(key) || req.Type != key.Type() ||
		!strings.HasPrefix(req.PublicKey, key.Type()+" ") || strings.Contains(req.PublicKey, "\n") {
		t.Fatalf("the request body is %+v", req)
	}

	// A ttl above the cap is held to the cap.
	body = `{"found":true,"subject":"u_1","key_id":"k_9","ttl":100000}`
	answer, err := r.Resolve(context.Background(), key)
	if err != nil || !answer.Found || answer.Subject != "u_1" || answer.KeyID != "k_9" || answer.TTL != MaxTTL {
		t.Fatalf("answer = %+v, %v", answer, err)
	}
	if r.CacheLen() != 1 {
		t.Errorf("the answer was not cached: %d entries", r.CacheLen())
	}
	if _, err := r.Resolve(context.Background(), key); err != nil || calls != 1 {
		t.Errorf("the second resolve made %d calls", calls)
	}

	// An answer with no ttl gets the default; a found answer with no
	// subject names nobody and is a refusal, not an identity.
	clock.Advance(MaxTTL + time.Second)
	body = `{"found":true,"subject":"u_1"}`
	if answer, err = r.Resolve(context.Background(), key); err != nil || answer.TTL != DefaultTTL {
		t.Fatalf("the default ttl: %+v %v", answer, err)
	}
	clock.Advance(MaxTTL + time.Second)
	body = `{"found":true}`
	if _, err := r.Resolve(context.Background(), key); err == nil {
		t.Error("a found answer with no subject was an identity")
	}
	clock.Advance(MaxTTL + time.Second)
	body = `{}`
	if _, err := r.Resolve(context.Background(), key); err == nil {
		t.Error("an answer with no found field was accepted")
	}
	clock.Advance(MaxTTL + time.Second)
	body = `not json`
	if _, err := r.Resolve(context.Background(), key); err == nil {
		t.Error("a body that does not parse was accepted")
	}
	clock.Advance(MaxTTL + time.Second)
	body, status = "", http.StatusInternalServerError
	if _, err := r.Resolve(context.Background(), key); err == nil {
		t.Error("a 500 was accepted")
	}

	// A URL nothing listens on is a refusal, after the one retry.
	dead, err := NewResolver(ResolverOptions{URL: "http://127.0.0.1:1", Token: "t", HTTP: srv.Client(), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.Resolve(context.Background(), key); err == nil {
		t.Error("a resolver nothing listens on answered")
	}
	if _, err := NewResolver(ResolverOptions{URL: "://", HTTP: srv.Client()}); err != nil {
		// A URL the request builder refuses fails on the call, not here.
		t.Fatal(err)
	}
}

// TestStorageCodeNamesTheRepositoryWhenTheLogIsBroken holds the one
// refusal that is not temporary: a log that names objects which are
// missing or fail their digest is unavailable until an operator
// restores it (spec 015).
func TestStorageCodeNamesTheRepositoryWhenTheLogIsBroken(t *testing.T) {
	integrity := &wal.IntegrityError{Key: "origo/repos/x/entry", Err: errors.New("missing")}
	if got := storageCode(integrity); got.Code != contract.CodeRepositoryUnavailable {
		t.Errorf("an integrity error is %q", got.Code)
	}
	if got := storageCode(errors.New("unreachable")); got.Code != contract.CodeStorageUnavailable {
		t.Errorf("a transient failure is %q", got.Code)
	}
}

// TestExecPayloadIsBounded holds the exec request's own bound: one SSH
// string, no longer than a path.
func TestExecPayloadIsBounded(t *testing.T) {
	if _, err := execCommand(appendString(nil, []byte("git-upload-pack '/a/b.git'"))); err != nil {
		t.Errorf("a well-formed exec was refused: %v", err)
	}
	for _, payload := range [][]byte{
		nil,
		{0, 0},
		append(appendString(nil, []byte("a")), appendString(nil, []byte("b"))...),
		appendString(nil, make([]byte, MaxPathBytes+1)),
	} {
		if _, err := execCommand(payload); err == nil {
			t.Errorf("%x was accepted", payload)
		}
	}
}

// TestFingerprintsNameEveryConfiguredKey is the start-up line an
// operator reads before publishing a fingerprint for a rotation.
func TestFingerprintsNameEveryConfiguredKey(t *testing.T) {
	f := newFixture(t, withHostKeys(generateEd25519(t), generateKey(t)))
	got := f.server.Fingerprints()
	for _, want := range f.hosts.Fingerprints() {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not name %s", got, want)
		}
	}
}
