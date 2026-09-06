// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build integration

package wal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestS3Suite runs the store suite against the bucket ORIGO_TEST_S3_*
// names, under a random prefix it removes at the end. `make
// test-integration` points it at the MinIO of the dev stack.
func TestS3Suite(t *testing.T) {
	endpoint := os.Getenv("ORIGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ORIGO_TEST_S3_ENDPOINT is not set")
	}
	base, err := NewS3(S3Options{
		Endpoint: endpoint, Region: os.Getenv("ORIGO_TEST_S3_REGION"), Bucket: os.Getenv("ORIGO_TEST_S3_BUCKET"),
		Key: os.Getenv("ORIGO_TEST_S3_KEY"), Secret: os.Getenv("ORIGO_TEST_S3_SECRET"),
		PathStyle: os.Getenv("ORIGO_TEST_S3_PATH_STYLE") == "1",
		Client:    &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	})
	if err != nil {
		t.Fatal(err)
	}
	runStoreSuite(t, func(t *testing.T) Store {
		var b [8]byte
		_, _ = rand.Read(b[:])
		prefix := "origo-test/" + hex.EncodeToString(b[:]) + "/"
		t.Cleanup(func() {
			ctx := context.Background()
			for {
				res, err := base.List(ctx, ListOptions{Prefix: prefix, Max: 1000})
				if err != nil || len(res.Objects) == 0 {
					return
				}
				for _, o := range res.Objects {
					_ = base.Delete(ctx, o.Key)
				}
			}
		})
		return &prefixed{Store: base, prefix: prefix}
	})
}

// prefixed namespaces a store under a prefix so a test runs in a shared
// bucket without meeting another test's keys.
type prefixed struct {
	Store
	prefix string
}

func (p *prefixed) Create(ctx context.Context, key string, body Body) (string, error) {
	return p.Store.Create(ctx, p.prefix+key, body)
}

func (p *prefixed) Put(ctx context.Context, key string, body Body) (string, error) {
	return p.Store.Put(ctx, p.prefix+key, body)
}

func (p *prefixed) Get(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, Object, error) {
	rc, o, err := p.Store.Get(ctx, p.prefix+key, ifNoneMatch)
	o.Key = strings.TrimPrefix(o.Key, p.prefix)
	return rc, o, err
}

func (p *prefixed) Head(ctx context.Context, key string) (Object, error) {
	o, err := p.Store.Head(ctx, p.prefix+key)
	o.Key = strings.TrimPrefix(o.Key, p.prefix)
	return o, err
}

func (p *prefixed) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	opts.Prefix = p.prefix + opts.Prefix
	if opts.StartAfter != "" {
		opts.StartAfter = p.prefix + opts.StartAfter
	}
	res, err := p.Store.List(ctx, opts)
	for i := range res.Objects {
		res.Objects[i].Key = strings.TrimPrefix(res.Objects[i].Key, p.prefix)
	}
	for i := range res.Prefixes {
		res.Prefixes[i] = strings.TrimPrefix(res.Prefixes[i], p.prefix)
	}
	return res, err
}

func (p *prefixed) Delete(ctx context.Context, key string) error {
	return p.Store.Delete(ctx, p.prefix+key)
}
