// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origo

import (
	"context"
	"math"
	"net/http"
	"testing"

	"latere.ai/x/pkg/s3/s3test"

	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/conformance"
)

// Fault is spec 021's Fault on the stub: the bucket cut through the
// s3test server's Fail, every request answered 503 until the test
// ends, and an object deleted through the adapter.
func (s *Server) Fault() conformance.Fault { return &fault{bucket: s.bucket, store: s.store} }

type fault struct {
	bucket *s3test.Server
	store  *wal.S3
}

// CutStorage makes every request to the bucket fail with 503 until the
// test ends, which is what Fail(math.MaxInt, 503) with Fail(0, 0) on
// the cleanup does.
func (f *fault) CutStorage(t testing.TB) {
	t.Helper()
	f.bucket.Fail(math.MaxInt, http.StatusServiceUnavailable)
	t.Cleanup(func() { f.bucket.Fail(0, 0) })
}

// DeleteObject deletes the one object under the prefix and reports its
// key.
func (f *fault) DeleteObject(t testing.TB, prefix string) string {
	t.Helper()
	res, err := f.store.List(context.Background(), wal.ListOptions{Prefix: prefix, Max: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("%d objects under %s, want one", len(res.Objects), prefix)
	}
	key := res.Objects[0].Key
	if err := f.store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return key
}
