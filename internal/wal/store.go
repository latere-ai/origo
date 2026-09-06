// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package wal is the write-ahead log of spec 004: entries in object
// storage, an immutable index object per committed entry, and the
// create-if-absent commit that linearizes writers without a lock.
//
// The Store interface names exactly the object storage primitives the
// design relies on. There is no compare-and-swap on an ETag anywhere in
// it: the spike under docs/spikes found PUT If-Match is not portable, and
// the design uses create-if-absent alone.
package wal

import (
	"context"
	"errors"
	"io"
	"time"

	"latere.ai/x/pkg/s3"
)

// Errors a Store reports. Anything else is a transport or server failure.
var (
	// ErrNotFound is a 404: the key does not exist.
	ErrNotFound = errors.New("wal: object not found")
	// ErrExists is a 412 on Create: the key already exists.
	ErrExists = errors.New("wal: object exists")
	// ErrNotModified is a 304 on a conditional Get.
	ErrNotModified = errors.New("wal: not modified")
)

// Object is one listed or headed key.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// ListOptions selects a page of keys.
type ListOptions struct {
	// Prefix restricts the listing to keys that start with it.
	Prefix string
	// StartAfter continues a listing after this key.
	StartAfter string
	// Max bounds the page; the store's own limit applies when zero.
	Max int
	// Delimiter groups keys after the prefix up to the first delimiter
	// into Prefixes, the way a directory listing does.
	Delimiter string
}

// ListResult is one page of a listing.
type ListResult struct {
	Objects   []Object
	Prefixes  []string
	Truncated bool
}

// Store is the object storage the log runs on. Every method is one
// request; the Log composes them.
type Store interface {
	// Create writes key only if it is absent (PUT If-None-Match: *) and
	// returns ErrExists when it is present. It is the one primitive that
	// linearizes writers.
	Create(ctx context.Context, key string, body Body) (etag string, err error)
	// Put writes key unconditionally.
	Put(ctx context.Context, key string, body Body) (etag string, err error)
	// Get reads key. With ifNoneMatch set to an ETag the store answers
	// ErrNotModified when the object still carries it.
	Get(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, Object, error)
	// Head reports the object's metadata without a body, or ErrNotFound.
	Head(ctx context.Context, key string) (Object, error)
	// List returns a page of keys in lexical order.
	List(ctx context.Context, opts ListOptions) (ListResult, error)
	// Delete removes key. A missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// Body is an object's content with the integrity values the store sends;
// it is pkg/s3's type, so a Store over that client passes it through.
type Body = s3.Body

// BytesBody wraps content held in memory.
func BytesBody(b []byte) Body { return s3.BytesBody(b) }

// FileBody wraps a file, hashing it once now so a large pack is read
// twice at most: once here and once per upload attempt.
func FileBody(path string) (Body, error) { return s3.FileBody(path) }
