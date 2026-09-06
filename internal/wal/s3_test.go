// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/s3"
	"latere.ai/x/pkg/s3/s3test"
)

// The store suite over pkg/s3 against its in-process endpoint, under
// both addressing styles. The endpoint verifies every signature and
// digest and answers 412 to any If-Match, so the suite proves the store
// never sends one.
func TestS3StoreSuiteOverTheFakeEndpoint(t *testing.T) {
	for _, pathStyle := range []bool{true, false} {
		t.Run(map[bool]string{true: "path-style", false: "virtual-host"}[pathStyle], func(t *testing.T) {
			runStoreSuite(t, func(t *testing.T) Store {
				return newFakeS3(t, pathStyle)
			})
		})
	}
}

func newFakeS3(t *testing.T, pathStyle bool) *S3 {
	t.Helper()
	f := s3test.New(t, "origo")
	endpoint := f.URL()
	if !pathStyle {
		endpoint = f.VirtualHostURL()
	}
	store, err := NewS3(S3Options{
		Endpoint: endpoint, Region: s3test.Region, Bucket: "origo", Key: s3test.Key, Secret: s3test.Secret,
		PathStyle: pathStyle, Client: f.HTTPClient(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestNewS3RejectsBadOptions(t *testing.T) {
	client := &http.Client{Transport: &http.Transport{}}
	for name, o := range map[string]S3Options{
		"relative endpoint": {Endpoint: "minio:9000", Region: "r", Bucket: "b", Key: "k", Secret: "s", Client: client},
		"no bucket":         {Endpoint: "http://minio:9000", Region: "r", Key: "k", Secret: "s", Client: client},
		"no client":         {Endpoint: "http://minio:9000", Region: "r", Bucket: "b", Key: "k", Secret: "s"},
	} {
		if _, err := NewS3(o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Every outcome the Log branches on arrives as the Store's own sentinel,
// and anything else as the client's error with its status.
func TestS3TranslatesTheClientErrors(t *testing.T) {
	ctx := context.Background()
	f := s3test.New(t, "origo")
	store, err := NewS3(S3Options{Endpoint: f.URL(), Region: s3test.Region, Bucket: "origo", Key: s3test.Key, Secret: s3test.Secret, PathStyle: true, Client: f.HTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, "absent"); !errors.Is(err, ErrNotFound) || errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("head absent: %v", err)
	}
	if _, err := store.List(ctx, ListOptions{Prefix: "absent/"}); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	f.Fail(1, http.StatusForbidden)
	_, err = store.List(ctx, ListOptions{})
	var se *s3.Error
	if !errors.As(err, &se) || se.Status != 403 || !strings.Contains(err.Error(), "403") {
		t.Fatalf("refused list: %v", err)
	}
	f.Fail(1, http.StatusForbidden)
	if err := store.Delete(ctx, "k"); err == nil {
		t.Fatal("refused delete accepted")
	}
	if _, _, err := store.Get(ctx, "absent", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get absent: %v", err)
	}
}
