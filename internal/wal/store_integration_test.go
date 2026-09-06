// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build integration

package wal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
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
