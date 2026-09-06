// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"latere.ai/x/pkg/s3"
)

// S3Options configures an S3 compatible store. Endpoint is the service
// URL; with PathStyle the bucket is a path segment, otherwise a host
// label. Client is the HTTP client every request goes through and must
// carry a Transport.
type S3Options struct {
	Endpoint  string
	Region    string
	Bucket    string
	Key       string
	Secret    string
	PathStyle bool
	Client    *http.Client
}

// S3 is a Store over pkg/s3, which sends the five verbs the design needs
// and no conditional header but If-None-Match: a compare-and-swap never
// leaves this file, and the client has no If-Match to send.
type S3 struct {
	c *s3.Client
}

// NewS3 validates the options and returns the store. It sends nothing.
func NewS3(o S3Options) (*S3, error) {
	if o.Client == nil {
		return nil, errors.New("wal: S3 needs an HTTP client")
	}
	opts := []s3.Option{s3.WithHTTPClient(o.Client)}
	if o.PathStyle {
		opts = append(opts, s3.WithPathStyle())
	}
	c, err := s3.New(o.Endpoint, o.Region, o.Bucket, o.Key, o.Secret, opts...)
	if err != nil {
		return nil, fmt.Errorf("wal: %w", err)
	}
	return &S3{c: c}, nil
}

// translate maps the client's sentinels to the Store's, so the Log and
// the in-process MemStore report the same errors.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, s3.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, s3.ErrPreconditionFailed):
		return ErrExists
	case errors.Is(err, s3.ErrNotModified):
		return ErrNotModified
	}
	return err
}

func object(o s3.Object) Object {
	return Object{Key: o.Key, Size: o.Size, ETag: o.ETag, LastModified: o.LastModified}
}

// Create implements Store with PUT If-None-Match: *.
func (s *S3) Create(ctx context.Context, key string, body Body) (string, error) {
	etag, err := s.c.CreateObject(ctx, key, body)
	return etag, translate(err)
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key string, body Body) (string, error) {
	etag, err := s.c.PutObject(ctx, key, body)
	return etag, translate(err)
}

// Get implements Store.
func (s *S3) Get(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, Object, error) {
	rc, o, err := s.c.GetObject(ctx, key, ifNoneMatch)
	if err != nil {
		return nil, Object{}, translate(err)
	}
	return rc, object(o), nil
}

// Head implements Store.
func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	o, err := s.c.HeadObject(ctx, key)
	return object(o), translate(err)
}

// Delete implements Store. A missing key is not an error.
func (s *S3) Delete(ctx context.Context, key string) error {
	return translate(s.c.DeleteObject(ctx, key))
}

// List implements Store with ListObjectsV2.
func (s *S3) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	res, err := s.c.ListObjects(ctx, s3.ListOptions{
		Prefix: opts.Prefix, StartAfter: opts.StartAfter, MaxKeys: opts.Max, Delimiter: opts.Delimiter,
	})
	if err != nil {
		return ListResult{}, translate(err)
	}
	out := ListResult{Truncated: res.Truncated, Prefixes: res.Prefixes}
	for _, o := range res.Objects {
		out.Objects = append(out.Objects, object(o))
	}
	return out, nil
}
