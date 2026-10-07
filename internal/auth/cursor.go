// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// The directory cursor (Origo spec 031). The authorizer's next_cursor is
// a third party's bytes, so the node seals them before a caller sees
// them and opens what the caller sends back before the authorizer does:
// a caller reads nothing of the authorizer's cursor and cannot make one
// up, and the authorizer only ever receives a cursor it wrote, on a
// request from the subject it wrote it for.

// ErrCursor is a directory cursor this installation did not write, or
// wrote for another subject or another authorizer. The guard returns it
// before the authorizer is called, and the collection route answers it
// as 400 invalid_request with reason and field "cursor": it is the
// caller's request at fault, never the authorizer's outage.
var ErrCursor = errors.New("auth: cursor")

const (
	// cursorPrefix names the construction, so a later one can be told
	// apart from this one.
	cursorPrefix = "v1."
	// cursorInfo is the HKDF info string. It keeps the cursor key apart
	// from every other use of ORIGO_TOKEN_KEY: the derived key signs
	// nothing and the signing key encrypts nothing.
	cursorInfo = "origo directory cursor v1"
	// cursorKeyBytes is the AES-256 key size.
	cursorKeyBytes = 32
	// cursorOverhead is the random 12-byte nonce and the 16-byte tag
	// around the authorizer's bytes.
	cursorOverhead = 12 + 16
	// maxAuthorizerCursor bounds the authorizer's next_cursor. The
	// cursor travels in a query string, through ingresses and proxies
	// that cap a request line at a few KiB, and every endpoint that
	// pages by id is far inside it.
	maxAuthorizerCursor = 512
	// maxCursor is the longest cursor this installation writes: the
	// prefix and the unpadded base64url of the overhead and the longest
	// authorizer cursor, 723 characters.
	maxCursor = len(cursorPrefix) + ((cursorOverhead+maxAuthorizerCursor)*8+5)/6
)

// cursorEncoding is unpadded base64url, strict so that one sealed cursor
// has one spelling and a changed character is never read as the same
// bytes.
var cursorEncoding = base64.RawURLEncoding.Strict()

// Cursors seals and opens directory cursors for one installation and one
// authorizer. Every node of an installation holds the same
// ORIGO_TOKEN_KEY, so a cursor sealed on one node opens on whichever
// node the next request reaches.
type Cursors struct {
	aead       cipher.AEAD
	authorizer string
}

// NewCursors derives the cursor key from the signing key and binds every
// cursor to the authorizer URL, ORIGO_AUTHORIZER_URL as configured, or
// empty for the owner policy.
func NewCursors(key *ecdsa.PrivateKey, authorizerURL string) (*Cursors, error) {
	secret, err := cursorKey(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, fmt.Errorf("auth: cursor key: %w", err)
	}
	// A fresh random 12-byte nonce per seal, prepended to the
	// ciphertext, which is the wire form's nonce, ciphertext, and tag.
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("auth: cursor key: %w", err)
	}
	return &Cursors{aead: aead, authorizer: authorizerURL}, nil
}

// cursorKey is HKDF-SHA256 over the private scalar in its fixed-width
// encoding. The scalar's big-integer bytes drop leading zeros, so one
// key in about 256 would derive from 31 bytes on one path and 32 on
// another; Bytes is always the curve's width.
func cursorKey(key *ecdsa.PrivateKey) ([]byte, error) {
	if key == nil {
		return nil, errors.New("auth: cursor key: no signing key")
	}
	scalar, err := key.Bytes()
	if err != nil {
		return nil, fmt.Errorf("auth: cursor key: %w", err)
	}
	return hkdf.Key(sha256.New, scalar, nil, cursorInfo, cursorKeyBytes)
}

// data is the associated data: the action, the authorizer URL, and the
// subject, each apart by a zero byte. A cursor therefore opens only on
// a request from the subject it was issued to, and only while the node
// asks the authorizer that wrote it.
func (c *Cursors) data(subject string) []byte {
	return []byte(string(ActionList) + "\x00" + c.authorizer + "\x00" + subject)
}

// Seal encrypts the authorizer's cursor for the subject. A cursor over
// the bound is no answer, and the error says how long it was.
func (c *Cursors) Seal(subject, cursor string) (string, error) {
	if len(cursor) > maxAuthorizerCursor {
		return "", fmt.Errorf("next_cursor: %d bytes, over the bound of %d", len(cursor), maxAuthorizerCursor)
	}
	return cursorPrefix + cursorEncoding.EncodeToString(c.aead.Seal(nil, nil, []byte(cursor), c.data(subject))), nil
}

// Open reads a cursor a caller sent back and returns the authorizer's
// bytes. Every failure is ErrCursor, with what failed beside it for the
// log; a guard with no Cursors opens nothing.
func (c *Cursors) Open(subject, sealed string) (string, error) {
	switch {
	case c == nil:
		return "", fmt.Errorf("%w: no cursor key", ErrCursor)
	case len(sealed) > maxCursor:
		return "", fmt.Errorf("%w: %d characters, over the bound of %d", ErrCursor, len(sealed), maxCursor)
	case !strings.HasPrefix(sealed, cursorPrefix):
		return "", fmt.Errorf("%w: no %s prefix", ErrCursor, cursorPrefix)
	}
	raw, err := cursorEncoding.DecodeString(sealed[len(cursorPrefix):])
	if err != nil {
		return "", fmt.Errorf("%w: not base64url", ErrCursor)
	}
	if len(raw) < cursorOverhead {
		return "", fmt.Errorf("%w: shorter than a nonce and a tag", ErrCursor)
	}
	plain, err := c.aead.Open(nil, nil, raw, c.data(subject))
	if err != nil {
		return "", fmt.Errorf("%w: does not open", ErrCursor)
	}
	return string(plain), nil
}

// isSealed reports whether a caller's cursor claims to be one this
// installation wrote.
func isSealed(cursor string) bool { return strings.HasPrefix(cursor, cursorPrefix) }
