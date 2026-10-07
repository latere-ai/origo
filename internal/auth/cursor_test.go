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
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
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

// TestTheDirectoryCursorIsSealed: every next_cursor a caller sees is one
// the node wrote, holding nothing of the authorizer's cursor even once
// decoded, and the next request carrying it reaches the authorizer with
// that cursor byte for byte. The stub pages by the last id served, which
// makes the authorizer's cursor recognizable. A cursor the node did not
// write never reaches the lister, and a guard with no cursor key answers
// no page rather than one with the authorizer's cursor in it.
func TestTheDirectoryCursorIsSealed(t *testing.T) {
	stub := authorizer.New(t)
	stub.SetDirectory(true,
		authorizer.DirectoryEntry{ID: repoA, Owner: "acme", Slug: "app"},
		authorizer.DirectoryEntry{ID: repoB, Owner: "acme", Slug: "lib"},
	)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, newClock(), pkgmetrics.NewRegistry())
	g := NewGuard(c, nil)
	g.SetCursors(newCursors(t, newKey(t), stub.URL()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	alice := Principal{Subject: "alice"}

	first, err := g.Directory(ctx, alice, "", 1)
	next := first.NextCursor
	if err != nil || len(first.Repos) != 1 || !strings.HasPrefix(next, "v1.") || strings.Contains(next, repoA) {
		t.Fatalf("the first page is %+v, %v", first, err)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(next, "v1.")); err != nil || bytes.Contains(raw, []byte(repoA)) {
		t.Fatalf("the cursor decodes to the authorizer's: %v", err)
	}

	stub.ClearRequests()
	second, err := g.Directory(ctx, alice, next, 1)
	if err != nil || len(second.Repos) != 1 || second.Repos[0].ID != repoB || second.NextCursor != "" {
		t.Fatalf("the second page is %+v, %v", second, err)
	}
	if seen := stub.Requests(); len(seen) != 1 || seen[0].Resource.String("cursor") != repoA {
		t.Fatalf("the authorizer saw %+v", seen)
	}

	// The authorizer's own cursor, and a sealed one another subject sends,
	// are not this installation's for this caller.
	stub.ClearRequests()
	for name, row := range map[string]struct {
		p      Principal
		cursor string
	}{
		"the authorizer's own": {alice, repoA},
		"another subject's":    {Principal{Subject: "bob"}, next},
	} {
		if _, err := g.Directory(ctx, row.p, row.cursor, 1); !errors.Is(err, ErrCursor) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if dir, err := NewGuard(c, nil).Directory(ctx, alice, "", 1); err == nil {
		t.Fatalf("a guard with no cursor key answered %+v", dir)
	}
	if n := len(stub.Requests()); n != 0 {
		t.Fatalf("a refused directory reached the authorizer %d times", n)
	}
}

// TestTheAuthorizerCursorIsBounded: an authorizer's next_cursor of 512
// bytes is sealed and served, and one of 513 is no answer, the 503
// authorizer_unavailable a caller sees for an outage, logged with its
// length.
func TestTheAuthorizerCursorIsBounded(t *testing.T) {
	ctx := context.Background()
	alice := Principal{Subject: "alice"}
	page := func(cursor string) string {
		return `{"repos":[{"id":"` + repoA + `","owner":"acme","slug":"app"}],"next_cursor":"` + cursor + `"}`
	}

	at := strings.Repeat("c", 512)
	c, _ := newAnswering(t, http.StatusOK, page(at))
	cursors := newCursors(t, newKey(t), "http://authorizer.invalid/")
	g := NewGuard(c, nil)
	g.SetCursors(cursors)
	dir, err := g.Directory(ctx, alice, "", 1)
	if err != nil || len(dir.NextCursor) != 723 {
		t.Fatalf("a 512-byte cursor gave %d characters, %v", len(dir.NextCursor), err)
	}
	if opened, err := cursors.Open("alice", dir.NextCursor); err != nil || opened != at {
		t.Fatalf("the 512-byte cursor opened as %d bytes, %v", len(opened), err)
	}

	over, _ := newAnswering(t, http.StatusOK, page(strings.Repeat("c", 513)))
	g = NewGuard(over, nil)
	g.SetCursors(cursors)
	_, err = g.Directory(ctx, alice, "", 1)
	if !isUnavailable(err) {
		t.Fatalf("a 513-byte cursor gave %v, want an *Unavailable", err)
	}
	var logs bytes.Buffer
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(WithPrincipal(ctx, alice), http.MethodGet, "/v1/repos", nil)
	WriteRefusal(rec, req, err, slog.New(slog.NewTextHandler(&logs, nil)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"authorizer_unavailable"`) {
		t.Fatalf("a 513-byte cursor rendered %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "513 bytes") {
		t.Fatalf("the log does not carry the length: %s", logs.String())
	}
}
