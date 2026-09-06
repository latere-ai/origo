// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// Now stamps the signature. The wall clock by default.
	Now func() time.Time
	// MaxAttempts bounds retries of a request that failed in transport or
	// with a 5xx. 3 by default.
	MaxAttempts int
}

// S3 is a Store over the S3 REST API signed with Signature Version 4 by
// the standard library. It sends the five verbs the design needs and no
// conditional header but If-None-Match: a compare-and-swap never leaves
// this file.
type S3 struct {
	endpoint    *url.URL
	region      string
	bucket      string
	key, secret string
	pathStyle   bool
	client      *http.Client
	now         func() time.Time
	maxAttempts int
	sleep       func(context.Context, time.Duration)
}

const (
	emptySHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	amzDateFormat   = "20060102T150405Z"
)

// NewS3 validates the options and returns the store. It sends nothing.
func NewS3(o S3Options) (*S3, error) {
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("wal: S3 endpoint %q is not an absolute URL", o.Endpoint)
	}
	if o.Bucket == "" || o.Region == "" || o.Key == "" || o.Secret == "" {
		return nil, errors.New("wal: S3 bucket, region, key, and secret are required")
	}
	if o.Client == nil {
		return nil, errors.New("wal: S3 needs an HTTP client")
	}
	s := &S3{
		endpoint: u, region: o.Region, bucket: o.Bucket, key: o.Key, secret: o.Secret,
		pathStyle: o.PathStyle, client: o.Client, now: o.Now, maxAttempts: o.MaxAttempts,
		sleep: sleepContext,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxAttempts <= 0 {
		s.maxAttempts = 3
	}
	return s, nil
}

// objectURL builds the request URL for key. The path is escaped the way
// the signature expects, so the request line and the canonical URI agree.
func (s *S3) objectURL(key string, query url.Values) *url.URL {
	u := *s.endpoint
	segments := []string{}
	if s.pathStyle {
		segments = append(segments, s.bucket)
	} else {
		u.Host = s.bucket + "." + u.Host
	}
	if key != "" {
		segments = append(segments, strings.Split(key, "/")...)
	}
	escaped := make([]string, len(segments))
	for i, seg := range segments {
		escaped[i] = awsEscape(seg)
	}
	u.Path = "/" + strings.Join(segments, "/")
	u.RawPath = "/" + strings.Join(escaped, "/")
	if key == "" && len(segments) > 0 {
		// The bucket itself: a listing addresses the bucket root.
		u.Path += "/"
		u.RawPath += "/"
	}
	u.RawQuery = canonicalQuery(query)
	return &u
}

// awsEscape percent-encodes everything but the unreserved characters, as
// the signing specification requires; url.PathEscape leaves more alone.
func awsEscape(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// sign adds the Signature Version 4 headers to req. Every header already
// on the request is signed, so a store cannot be handed a header the
// signature does not cover.
func (s *S3) sign(req *http.Request, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format(amzDateFormat)
	date := amzDate[:8]
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	names := []string{"host"}
	values := map[string]string{"host": req.Host}
	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		names = append(names, lower)
		values[lower] = strings.TrimSpace(strings.Join(vs, ","))
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + values[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	path := req.URL.RawPath
	if path == "" {
		path = req.URL.EscapedPath()
	}
	canonical := strings.Join([]string{
		req.Method, path, req.URL.RawQuery, canonHeaders.String(), signedHeaders, payloadHash,
	}, "\n")
	scope := date + "/" + s.region + "/s3/aws4_request"
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256([]byte(canonical)),
	}, "\n")
	k := hmacSHA256([]byte("AWS4"+s.secret), date)
	k = hmacSHA256(k, s.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.key, scope, signedHeaders, signature))
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// request is one attempt-independent description of a call.
type request struct {
	method  string
	key     string
	query   url.Values
	headers map[string]string
	body    *Body
}

// do sends the request with retries. The response is returned with its
// body open on success; every status the caller did not list as accepted
// is an error. A 5xx or a transport failure is retried up to maxAttempts
// times, a 4xx never.
func (s *S3) do(ctx context.Context, r request, accept ...int) (*http.Response, error) {
	var last error
	for attempt := range s.maxAttempts {
		if attempt > 0 {
			s.sleep(ctx, backoff(attempt))
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		resp, err := s.once(ctx, r)
		if err != nil {
			last = err
			continue
		}
		if slices.Contains(accept, resp.StatusCode) {
			return resp, nil
		}
		err = s3Error(resp)
		_ = resp.Body.Close()
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return nil, err
		}
		last = err
	}
	return nil, fmt.Errorf("wal: %s %s failed after %d attempts: %w", r.method, r.key, s.maxAttempts, last)
}

func (s *S3) once(ctx context.Context, r request) (*http.Response, error) {
	var body io.Reader
	payloadHash := emptySHA256
	var size int64
	if r.body != nil {
		rc, err := r.body.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		body = rc
		size = r.body.Size
		payloadHash = r.body.SHA256
		if payloadHash == "" {
			payloadHash = unsignedPayload
		}
	}
	req, err := http.NewRequestWithContext(ctx, r.method, s.objectURL(r.key, r.query).String(), body)
	if err != nil {
		return nil, err
	}
	if r.body != nil {
		req.ContentLength = size
		if r.body.MD5 != "" {
			req.Header.Set("Content-MD5", r.body.MD5)
		}
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	s.sign(req, payloadHash, s.now())
	return s.client.Do(req)
}

func backoff(attempt int) time.Duration {
	base := 50 * time.Millisecond << (attempt - 1)
	return base + time.Duration(rand.Int64N(int64(base))) //nolint:gosec // jitter
}

// s3Error turns a refused response into an error naming the status and
// the code the body carries.
func s3Error(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var body struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(raw, &body)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusPreconditionFailed:
		return ErrExists
	case http.StatusNotModified:
		return ErrNotModified
	}
	if body.Code != "" {
		return fmt.Errorf("wal: %s: %s: %s", resp.Status, body.Code, body.Message)
	}
	return fmt.Errorf("wal: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
}

// Create implements Store with PUT If-None-Match: *.
func (s *S3) Create(ctx context.Context, key string, body Body) (string, error) {
	resp, err := s.do(ctx, request{method: http.MethodPut, key: key, body: &body, headers: map[string]string{"If-None-Match": "*"}}, http.StatusOK)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get("ETag"), nil
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key string, body Body) (string, error) {
	resp, err := s.do(ctx, request{method: http.MethodPut, key: key, body: &body}, http.StatusOK)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get("ETag"), nil
}

// Get implements Store.
func (s *S3) Get(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, Object, error) {
	r := request{method: http.MethodGet, key: key}
	if ifNoneMatch != "" {
		r.headers = map[string]string{"If-None-Match": ifNoneMatch}
	}
	resp, err := s.do(ctx, r, http.StatusOK)
	if err != nil {
		return nil, Object{}, err
	}
	return resp.Body, objectFrom(key, resp), nil
}

// Head implements Store.
func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	resp, err := s.do(ctx, request{method: http.MethodHead, key: key}, http.StatusOK)
	if err != nil {
		return Object{}, err
	}
	_ = resp.Body.Close()
	return objectFrom(key, resp), nil
}

func objectFrom(key string, resp *http.Response) Object {
	o := Object{Key: key, ETag: resp.Header.Get("ETag")}
	if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil {
		o.Size = n
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		o.LastModified = t
	}
	return o
}

// Delete implements Store. A 404 is success: the key is gone either way.
func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, request{method: http.MethodDelete, key: key}, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

type listBucketResult struct {
	IsTruncated bool `xml:"IsTruncated"`
	Contents    []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		ETag         string    `xml:"ETag"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

// List implements Store with ListObjectsV2.
func (s *S3) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	q := url.Values{"list-type": {"2"}}
	if opts.Prefix != "" {
		q.Set("prefix", opts.Prefix)
	}
	if opts.StartAfter != "" {
		q.Set("start-after", opts.StartAfter)
	}
	if opts.Max > 0 {
		q.Set("max-keys", strconv.Itoa(opts.Max))
	}
	if opts.Delimiter != "" {
		q.Set("delimiter", opts.Delimiter)
	}
	resp, err := s.do(ctx, request{method: http.MethodGet, query: q}, http.StatusOK)
	if err != nil {
		return ListResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed listBucketResult
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&parsed); err != nil {
		return ListResult{}, fmt.Errorf("wal: list: %w", err)
	}
	res := ListResult{Truncated: parsed.IsTruncated}
	for _, c := range parsed.Contents {
		res.Objects = append(res.Objects, Object{Key: c.Key, Size: c.Size, ETag: c.ETag, LastModified: c.LastModified})
	}
	for _, p := range parsed.CommonPrefixes {
		res.Prefixes = append(res.Prefixes, p.Prefix)
	}
	return res, nil
}

func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
