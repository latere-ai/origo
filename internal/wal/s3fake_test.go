// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an S3 endpoint over a MemStore, enough of the REST API for
// the client to run the whole store suite hermetically. It asserts every
// request is signed with the same signature the client would compute
// from what arrived on the wire, and that no request ever carries
// If-Match: the design never issues one.
type fakeS3 struct {
	t      *testing.T
	store  *MemStore
	bucket string
	signer *S3

	mu       sync.Mutex
	requests []*http.Request
	failNext int
	failCode int
}

func newFakeS3(t *testing.T, pathStyle bool) (*fakeS3, *S3) {
	t.Helper()
	f := &fakeS3{t: t, store: NewMemStore(), bucket: "origo", failCode: http.StatusInternalServerError}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	endpoint := srv.URL
	if !pathStyle {
		// Virtual-host style needs the bucket as a host label; the fake
		// answers any host, so the client is pointed at a name that
		// resolves to the loopback listener.
		endpoint = strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	}
	client, err := NewS3(S3Options{
		Endpoint: endpoint, Region: "us-east-1", Bucket: f.bucket, Key: "AKIDEXAMPLE", Secret: "secret",
		PathStyle: pathStyle, Client: srv.Client(), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.sleep = func(context.Context, time.Duration) {}
	f.signer = client
	return f, client
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	fail := f.failNext > 0
	if fail {
		f.failNext--
	}
	f.mu.Unlock()
	if r.Header.Get("If-Match") != "" {
		f.t.Errorf("%s %s carries If-Match, which the design never issues", r.Method, r.URL)
	}
	f.checkSignature(r)
	if fail {
		w.WriteHeader(f.failCode)
		_, _ = w.Write([]byte(`<Error><Code>InternalError</Code><Message>injected</Message></Error>`))
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	if strings.HasPrefix(key, f.bucket+"/") || key == f.bucket {
		key = strings.TrimPrefix(strings.TrimPrefix(key, f.bucket), "/")
	}
	ctx := r.Context()
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		q := r.URL.Query()
		max, _ := strconv.Atoi(q.Get("max-keys"))
		res, _ := f.store.List(ctx, ListOptions{Prefix: q.Get("prefix"), StartAfter: q.Get("start-after"), Max: max, Delimiter: q.Get("delimiter")})
		var out listBucketResult
		out.IsTruncated = res.Truncated
		for _, o := range res.Objects {
			out.Contents = append(out.Contents, struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				ETag         string    `xml:"ETag"`
				LastModified time.Time `xml:"LastModified"`
			}{o.Key, o.Size, o.ETag, o.LastModified})
		}
		for _, p := range res.Prefixes {
			out.CommonPrefixes = append(out.CommonPrefixes, struct {
				Prefix string `xml:"Prefix"`
			}{p})
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"ListBucketResult"`
			listBucketResult
		}{listBucketResult: out})
	case r.Method == http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		if got := len(data); int64(got) != r.ContentLength {
			f.t.Errorf("PUT %s: %d bytes, Content-Length %d", key, got, r.ContentLength)
		}
		if r.Header.Get("Content-MD5") != BytesBody(data).MD5 {
			f.t.Errorf("PUT %s: Content-MD5 %q does not match the body", key, r.Header.Get("Content-MD5"))
		}
		if h := r.Header.Get("x-amz-content-sha256"); h != BytesBody(data).SHA256 && h != unsignedPayload {
			f.t.Errorf("PUT %s: x-amz-content-sha256 %q does not match the body", key, h)
		}
		var etag string
		var err error
		if r.Header.Get("If-None-Match") == "*" {
			etag, err = f.store.Create(ctx, key, BytesBody(data))
		} else {
			etag, err = f.store.Put(ctx, key, BytesBody(data))
		}
		if err == ErrExists { //nolint:errorlint // sentinel from the fake
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`<Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message></Error>`))
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		rc, o, err := f.store.Get(ctx, key, r.Header.Get("If-None-Match"))
		switch err {
		case ErrNotFound: //nolint:errorlint // sentinel from the fake
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		case ErrNotModified: //nolint:errorlint // sentinel from the fake
			w.WriteHeader(http.StatusNotModified)
			return
		}
		defer rc.Close()
		w.Header().Set("ETag", o.ETag)
		w.Header().Set("Last-Modified", o.LastModified.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, rc)
		}
	case r.Method == http.MethodDelete:
		_ = f.store.Delete(ctx, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// checkSignature recomputes the signature from the request as it arrived
// and compares it with the one sent, which is what a real endpoint does.
func (f *fakeS3) checkSignature(r *http.Request) {
	auth := r.Header.Get("Authorization")
	const prefix = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"
	if !strings.HasPrefix(auth, prefix) {
		f.t.Errorf("%s %s: Authorization %q", r.Method, r.URL, auth)
		return
	}
	var signed, sig string
	for part := range strings.SplitSeq(auth[len("AWS4-HMAC-SHA256 "):], ", ") {
		if v, ok := strings.CutPrefix(part, "SignedHeaders="); ok {
			signed = v
		}
		if v, ok := strings.CutPrefix(part, "Signature="); ok {
			sig = v
		}
	}
	at, err := time.Parse(amzDateFormat, r.Header.Get("x-amz-date"))
	if err != nil {
		f.t.Errorf("x-amz-date: %v", err)
		return
	}
	copy := &http.Request{Method: r.Method, URL: r.URL, Host: r.Host, Header: http.Header{}}
	for name := range strings.SplitSeq(signed, ";") {
		if name == "host" {
			continue
		}
		copy.Header.Set(name, r.Header.Get(name))
	}
	f.signer.sign(copy, r.Header.Get("x-amz-content-sha256"), at)
	if want := copy.Header.Get("Authorization"); !strings.HasSuffix(want, "Signature="+sig) {
		f.t.Errorf("%s %s: signature %s does not verify (%s)", r.Method, r.URL.RequestURI(), sig, want)
	}
}

func (f *fakeS3) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method {
			n++
		}
	}
	return n
}

func (f *fakeS3) failNextRequests(n, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext, f.failCode = n, code
}
