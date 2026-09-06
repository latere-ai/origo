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
	"testing"
	"time"
)

func TestS3StoreSuiteOverTheFakeEndpoint(t *testing.T) {
	for _, pathStyle := range []bool{true, false} {
		t.Run(map[bool]string{true: "path-style", false: "virtual-host"}[pathStyle], func(t *testing.T) {
			runStoreSuite(t, func(t *testing.T) Store {
				_, s := newFakeS3(t, pathStyle)
				return s
			})
		})
	}
}

// The GET Object example from the Signature Version 4 documentation for
// S3, with its published signature.
func TestSignatureMatchesTheDocumentedVector(t *testing.T) {
	s, err := NewS3(S3Options{
		Endpoint: "https://s3.amazonaws.com", Region: "us-east-1", Bucket: "examplebucket",
		Key: "AKIAIOSFODNN7EXAMPLE", Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Client: &http.Client{Transport: &http.Transport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, s.objectURL("test.txt", nil).String(), nil)
	req.Header.Set("Range", "bytes=0-9")
	s.sign(req, emptySHA256, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
	}
	if req.Host != "examplebucket.s3.amazonaws.com" {
		t.Fatalf("Host = %q", req.Host)
	}
}

func TestObjectURLEscapesLikeTheSigner(t *testing.T) {
	s, _ := NewS3(S3Options{Endpoint: "http://minio:9000", Region: "r", Bucket: "b", Key: "k", Secret: "s", PathStyle: true, Client: &http.Client{Transport: &http.Transport{}}})
	u := s.objectURL("origo/names/o w/s+l~ug", nil)
	if u.String() != "http://minio:9000/b/origo/names/o%20w/s%2Bl~ug" {
		t.Fatalf("url = %s", u)
	}
	u = s.objectURL("", map[string][]string{"prefix": {"a b"}, "list-type": {"2"}})
	if u.String() != "http://minio:9000/b/?list-type=2&prefix=a%20b" {
		t.Fatalf("list url = %s", u)
	}
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

func TestS3RetriesServerFailuresAndNotClientOnes(t *testing.T) {
	ctx := context.Background()
	f, s := newFakeS3(t, true)
	f.failNextRequests(2, http.StatusServiceUnavailable)
	if _, err := s.Put(ctx, "k", BytesBody([]byte("v"))); err != nil {
		t.Fatalf("put after two 503s: %v", err)
	}
	if n := f.count(http.MethodPut); n != 3 {
		t.Fatalf("%d PUTs, want 3", n)
	}
	f.failNextRequests(3, http.StatusInternalServerError)
	if _, err := s.Head(ctx, "k"); err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("head after three 500s: %v", err)
	}
	f.failNextRequests(1, http.StatusTooManyRequests)
	if _, err := s.Head(ctx, "k"); err != nil {
		t.Fatalf("head after a 429: %v", err)
	}
	f.failNextRequests(1, http.StatusForbidden)
	if _, _, err := s.Get(ctx, "k", ""); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "InternalError") {
		t.Fatalf("get with a 403: %v", err)
	}
	if n := f.count(http.MethodHead); n != 5 {
		t.Fatalf("%d HEADs, want 5", n)
	}
	// A body that cannot be opened fails before a request is sent.
	broken := Body{Open: func() (io.ReadCloser, error) { return nil, errors.New("no body") }, Size: 1}
	if _, err := s.Put(ctx, "k2", broken); err == nil || !strings.Contains(err.Error(), "no body") {
		t.Fatalf("broken body: %v", err)
	}
	// A cancelled context stops the retry loop.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	f.failNextRequests(2, http.StatusInternalServerError)
	if _, err := s.Head(cctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled head: %v", err)
	}
}

func TestS3ErrorsWithoutAnXMLBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("plain text"))
	}))
	defer srv.Close()
	s, _ := NewS3(S3Options{Endpoint: srv.URL, Region: "r", Bucket: "b", Key: "k", Secret: "s", PathStyle: true, Client: srv.Client()})
	if _, _, err := s.Get(context.Background(), "k", ""); err == nil || !strings.Contains(err.Error(), "plain text") {
		t.Fatalf("err = %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<not xml"))
	}))
	defer bad.Close()
	s, _ = NewS3(S3Options{Endpoint: bad.URL, Region: "r", Bucket: "b", Key: "k", Secret: "s", PathStyle: true, Client: bad.Client()})
	if _, err := s.List(context.Background(), ListOptions{}); err == nil {
		t.Fatal("malformed listing accepted")
	}
	// A transport failure is retried and then reported.
	s, _ = NewS3(S3Options{Endpoint: "http://127.0.0.1:1", Region: "r", Bucket: "b", Key: "k", Secret: "s", PathStyle: true, Client: &http.Client{Transport: &http.Transport{}}, MaxAttempts: 2})
	s.sleep = func(context.Context, time.Duration) {}
	if _, err := s.Head(context.Background(), "k"); err == nil || !strings.Contains(err.Error(), "after 2 attempts") {
		t.Fatalf("unreachable endpoint: %v", err)
	}
}

func TestBackoffGrowsWithJitter(t *testing.T) {
	if d := backoff(1); d < 50*time.Millisecond || d >= 100*time.Millisecond {
		t.Fatalf("attempt 1: %v", d)
	}
	if d := backoff(3); d < 200*time.Millisecond || d >= 400*time.Millisecond {
		t.Fatalf("attempt 3: %v", d)
	}
	done := make(chan struct{})
	go func() { sleepContext(context.Background(), time.Millisecond); close(done) }()
	<-done
}
