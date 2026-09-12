// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"latere.ai/x/pkg/retry"
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

func TestS3AttemptTimeoutRetriesBeforeParentDeadline(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store, err := NewS3(S3Options{Endpoint: server.URL, Region: "region", Bucket: "bucket", Key: "key", Secret: "secret", PathStyle: true, Client: server.Client(), RetryPolicy: retry.Policy{Timeout: 25 * time.Millisecond, MaxAttempts: 2, Base: time.Nanosecond}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := store.Head(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || ctx.Err() != nil {
		t.Fatal("attempt deadline consumed parent budget")
	}
}

type retryTimingTransport func(*http.Request) (*http.Response, error)

func (f retryTimingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestS3AttemptPolicyKeepsExistingBackoff(t *testing.T) {
	for _, policy := range []retry.Policy{{}, {Timeout: time.Second}} {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			attempts := 0
			client := &http.Client{Transport: retryTimingTransport(func(*http.Request) (*http.Response, error) {
				attempts++
				status := http.StatusServiceUnavailable
				if attempts == 2 {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			store, err := NewS3(S3Options{Endpoint: "http://storage.invalid", Region: "region", Bucket: "bucket", Key: "key", Secret: "secret", Client: client, RetryPolicy: policy})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Head(t.Context(), "key"); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			if elapsed < 40*time.Millisecond || elapsed > 50*time.Millisecond {
				t.Fatalf("default S3 retry backoff changed: %v", elapsed)
			}
		})
	}
}
