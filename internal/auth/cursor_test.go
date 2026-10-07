// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"latere.ai/x/origo/test/stubs/authorizer"
)

const stubURL = "http://authorizer.invalid/"

func newCursors(t *testing.T, key *ecdsa.PrivateKey, url string) *Cursors {
	t.Helper()
	c, err := NewCursors(key, url)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func seal(t *testing.T, c *Cursors, subject, cursor string) string {
	t.Helper()
	out, err := c.Seal(subject, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTheCursorKeyReadsTheFixedWidthScalar: a scalar whose first byte is
// zero is 31 bytes as a big integer and 32 in its fixed-width form, and
// the key derives from the 32 on every path, so two nodes that read the
// same ORIGO_TOKEN_KEY open each other's cursors.
func TestTheCursorKeyReadsTheFixedWidthScalar(t *testing.T) {
	scalar := make([]byte, 32)
	for i := 1; i < len(scalar); i++ {
		scalar[i] = byte(i)
	}
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := new(big.Int).SetBytes(scalar).Bytes()
	if len(trimmed) != 31 {
		t.Fatalf("the test key's big-integer form is %d bytes, not the 31 the case needs", len(trimmed))
	}

	got, err := cursorKey(key)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hkdf.Key(sha256.New, scalar, nil, "origo directory cursor v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	short, err := hkdf.Key(sha256.New, trimmed, nil, "origo directory cursor v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || bytes.Equal(got, short) {
		t.Fatalf("the cursor key does not derive from the fixed-width scalar")
	}

	// The key as a node reads it, from the PEM of ORIGO_TOKEN_KEY,
	// derives the same cursor key, so it opens the other's cursors.
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKey(string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := cursorKey(parsed); err != nil || !bytes.Equal(again, want) {
		t.Fatalf("the parsed key derives %x, %v", again, err)
	}
	opened, err := newCursors(t, parsed, stubURL).Open("alice", seal(t, newCursors(t, key, stubURL), "alice", repoA))
	if err != nil || opened != repoA {
		t.Fatalf("a cursor sealed under the key opened as %q, %v under the parsed key", opened, err)
	}

	if _, err := NewCursors(nil, stubURL); err == nil {
		t.Fatal("a cursor key derived from no signing key")
	}
}

// TestACursorOpensUnderItsBindingAlone: the cursor is the authorizer's
// bytes under authenticated encryption bound to the action, the
// authorizer and the subject, and everything that is not exactly what
// this installation wrote for that binding is ErrCursor.
func TestACursorOpensUnderItsBindingAlone(t *testing.T) {
	key := newKey(t)
	c := newCursors(t, key, stubURL)

	sealed := seal(t, c, "alice", repoA)
	if !strings.HasPrefix(sealed, "v1.") || strings.Contains(sealed, repoA) {
		t.Fatalf("the sealed cursor is %q", sealed)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(sealed[3:]); err != nil || bytes.Contains(raw, []byte(repoA)) {
		t.Fatalf("the sealed cursor carries the authorizer's bytes: %v", err)
	}
	if again := seal(t, c, "alice", repoA); again == sealed {
		t.Fatal("two seals of one cursor are equal: the nonce is not fresh")
	}
	if got, err := c.Open("alice", sealed); err != nil || got != repoA {
		t.Fatalf("the cursor opened as %q, %v", got, err)
	}

	// The longest authorizer cursor seals to the longest cursor read.
	longest := seal(t, c, "alice", strings.Repeat("x", maxAuthorizerCursor))
	if len(longest) != maxCursor || maxCursor != 723 {
		t.Fatalf("a %d-byte cursor seals to %d characters, the bound is %d", maxAuthorizerCursor, len(longest), maxCursor)
	}
	if got, err := c.Open("alice", longest); err != nil || len(got) != maxAuthorizerCursor {
		t.Fatalf("the longest cursor opened as %d bytes, %v", len(got), err)
	}
	if _, err := c.Seal("alice", strings.Repeat("x", maxAuthorizerCursor+1)); err == nil || !strings.Contains(err.Error(), "513 bytes") {
		t.Fatalf("a 513-byte cursor sealed: %v", err)
	}

	raw, err := base64.RawURLEncoding.DecodeString(sealed[3:])
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(raw)
	flipped[len(flipped)-1] ^= 0x80
	for name, row := range map[string]struct {
		c       *Cursors
		subject string
		cursor  string
	}{
		"another subject":      {c, "bob", sealed},
		"another authorizer":   {newCursors(t, key, "http://another.invalid/"), "alice", sealed},
		"no authorizer":        {newCursors(t, key, ""), "alice", sealed},
		"another key":          {newCursors(t, newKey(t), stubURL), "alice", sealed},
		"no key":               {nil, "alice", sealed},
		"no prefix":            {c, "alice", sealed[3:]},
		"a bare id":            {c, "alice", repoA},
		"padded":               {c, "alice", sealed + "="},
		"not base64url":        {c, "alice", "v1.a+b/"},
		"a changed spelling":   {c, "alice", sealed[:len(sealed)-1] + string(sealed[len(sealed)-1]^0x01)},
		"shorter than the tag": {c, "alice", "v1." + base64.RawURLEncoding.EncodeToString(raw[:27])},
		"a byte flipped":       {c, "alice", "v1." + base64.RawURLEncoding.EncodeToString(flipped)},
		"too long":             {c, "alice", "v1." + strings.Repeat("A", maxCursor-2)},
	} {
		if got, err := row.c.Open(row.subject, row.cursor); !errors.Is(err, ErrCursor) {
			t.Errorf("%s: opened as %q, %v", name, got, err)
		}
	}
}

// TestTheGuardOpensASealedCursor is the guard's half of the first of
// spec 031's two releases: a v1. cursor is opened before the lister sees
// it, one that does not open is ErrCursor with no call, and any other
// cursor passes through as the authorizer's.
func TestTheGuardOpensASealedCursor(t *testing.T) {
	stub := authorizer.New(t)
	stub.SetDirectory(true,
		authorizer.DirectoryEntry{ID: repoA, Owner: "acme", Slug: "app"},
		authorizer.DirectoryEntry{ID: repoB, Owner: "acme", Slug: "lib"},
	)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, newClock(), pkgmetrics.NewRegistry())
	g := NewGuard(c, nil)
	cursors := newCursors(t, newKey(t), stub.URL())
	g.SetCursors(cursors)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	alice := Principal{Subject: "alice"}

	first, err := g.Directory(ctx, alice, "", 1)
	if err != nil || first.NextCursor != repoA {
		t.Fatalf("the first page is %+v, %v", first, err)
	}
	for _, cursor := range []string{seal(t, cursors, "alice", repoA), repoA} {
		stub.ClearRequests()
		got, err := g.Directory(ctx, alice, cursor, 1)
		if err != nil || len(got.Repos) != 1 || got.Repos[0].ID != repoB {
			t.Fatalf("%s: the second page is %+v, %v", cursor, got, err)
		}
		if seen := stub.Requests(); len(seen) != 1 || seen[0].Resource.String("cursor") != repoA {
			t.Fatalf("%s: the authorizer saw %+v", cursor, seen)
		}
	}

	stub.ClearRequests()
	if _, err := g.Directory(ctx, Principal{Subject: "bob"}, seal(t, cursors, "alice", repoA), 1); !errors.Is(err, ErrCursor) {
		t.Fatalf("another subject's cursor gave %v", err)
	}
	// A guard with no cursor key opens nothing.
	bare := NewGuard(c, nil)
	if _, err := bare.Directory(ctx, alice, seal(t, cursors, "alice", repoA), 1); !errors.Is(err, ErrCursor) {
		t.Fatalf("a guard with no cursor key gave %v", err)
	}
	if n := len(stub.Requests()); n != 0 {
		t.Fatalf("a refused cursor reached the authorizer %d times", n)
	}
}
