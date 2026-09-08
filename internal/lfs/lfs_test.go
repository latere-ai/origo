// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package lfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/s3"
	"latere.ai/x/pkg/s3/s3test"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
)

// fakeAuthorizer is the consumer's authorizer: it records every request
// and answers what the test set.
type fakeAuthorizer struct {
	mu       sync.Mutex
	requests []auth.Request
	decision auth.Decision
	err      error
}

func (f *fakeAuthorizer) Authorize(_ context.Context, req auth.Request) (auth.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return auth.Decision{}, f.err
	}
	return f.decision, nil
}

func (f *fakeAuthorizer) seen() []auth.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Request(nil), f.requests...)
}

// recordingTB is the testing.TB an s3test.Server reports a signature
// failure on: it records Errorf instead of failing, so a test asserts
// the message beside the 403 the store answers. It embeds the real TB,
// because testing.TB cannot be implemented outside the testing package
// and Helper and Cleanup must still reach the running test.
type recordingTB struct {
	testing.TB
	mu       sync.Mutex
	messages []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *recordingTB) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messages...)
}

const testRepo = "5f2a1b3c-4d5e-4f60-8a9b-0c1d2e3f4a5b"

// env is one handler over an in-process bucket with one repository.
type env struct {
	t      *testing.T
	bucket *s3test.Server
	log    *wal.Log
	authz  *fakeAuthorizer
	mux    *http.ServeMux
	now    time.Time
}

func newEnv(t *testing.T) *env { return newEnvOn(t, s3test.New(t, "origo-test")) }

func newEnvOn(t *testing.T, bucket *s3test.Server) *env {
	t.Helper()
	store, err := wal.NewS3(wal.S3Options{
		Endpoint: bucket.URL(), Region: s3test.Region, Bucket: "origo-test",
		Key: s3test.Key, Secret: s3test.Secret, PathStyle: true, Client: bucket.HTTPClient(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, bucket: bucket,
		log:   wal.New(wal.Options{Store: store, Prefix: config.Prefix, Logger: slog.New(slog.DiscardHandler)}),
		authz: &fakeAuthorizer{decision: auth.Decision{Allow: true, QuotaBytes: auth.DefaultQuotaBytes}},
		mux:   http.NewServeMux(),
		now:   time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC),
	}
	presigner, err := NewPresigner(PresignerOptions{
		Endpoint: bucket.URL(), Region: s3test.Region, Bucket: "origo-test",
		Key: s3test.Key, Secret: s3test.Secret, PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	New(Options{
		Log: e.log, Guard: auth.NewGuard(e.authz, slog.New(slog.DiscardHandler)),
		Presigner: presigner, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return e.now },
	}).Register(e.mux)
	if _, err := e.log.CreateRepo(t.Context(), wal.Meta{ID: testRepo, Owner: "dev", Slug: "hello"}, "main"); err != nil {
		t.Fatal(err)
	}
	return e
}

// do sends one request for the dev subject through the mux.
func (e *env) do(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.as(auth.Principal{Subject: "dev"}, method, path, body)
}

func (e *env) as(p auth.Principal, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(e.t.Context(), p))
	r.Header.Set("Content-Type", MediaType)
	r.Header.Set("Authorization", "Bearer dev-token")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, r)
	return rec
}

// batchPath is the batch endpoint of the test repository, id form.
const batchPath = "/r/" + testRepo + ".git/info/lfs/objects/batch"
const verifyPath = "/r/" + testRepo + ".git/info/lfs/verify"

func (e *env) key(rel string) string { return e.log.RepoPrefix(testRepo) + rel }

// seed writes an object under lfs/ straight into the bucket, the way a
// client's presigned PUT leaves it.
func (e *env) seed(oid string, size int) {
	e.t.Helper()
	e.bucket.PutWithContentType(e.key("lfs/"+oid), bytes.Repeat([]byte("x"), size), ObjectContentType)
}

func (e *env) mark(oid string, size int64) {
	e.t.Helper()
	body, err := json.Marshal(marker{Size: size, At: e.now, Subject: "dev"})
	if err != nil {
		e.t.Fatal(err)
	}
	e.bucket.Put(e.key("lfs/verified/"+oid), body)
}

func oidOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// decodeBatch reads a batch response, failing the test on any other shape.
func decodeBatch(t *testing.T, rec *httptest.ResponseRecorder) batchResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != MediaType {
		t.Fatalf("Content-Type %q", ct)
	}
	var out batchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("batch body: %v: %s", err, rec.Body.String())
	}
	return out
}

// decodeError reads the LFS error body: the sentence of the code, a
// request id, this spec's URL, and no details object.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != MediaType {
		t.Errorf("Content-Type %q, want %q", ct, MediaType)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body: %v: %s", err, rec.Body.String())
	}
	if got := body["message"]; got != contract.Sentence(code) {
		t.Errorf("message %q, want %q", got, contract.Sentence(code))
	}
	if id, _ := body["request_id"].(string); id == "" {
		t.Error("no request_id")
	}
	if got := body["documentation_url"]; got != DocumentationURL {
		t.Errorf("documentation_url %q", got)
	}
	if _, ok := body["details"]; ok {
		t.Error("the LFS shape carries no details")
	}
	if _, ok := body["error"]; ok {
		t.Error("the LFS shape is not the envelope of spec 003")
	}
}

// TestDownloadNeedsVerification is spec 010's download rule: a download
// action is given only for an object with a lfs/verified/<oid> marker,
// and the URL it names serves the bytes.
func TestDownloadNeedsVerification(t *testing.T) {
	e := newEnv(t)
	data := []byte("the object's bytes")
	stored, unstored := oidOf(data), oidOf([]byte("never uploaded"))
	e.seed(stored, len(data))
	e.bucket.PutWithContentType(e.key("lfs/"+stored), data, ObjectContentType)

	body := fmt.Sprintf(`{"operation":"download","transfers":["basic"],"objects":[{"oid":%q,"size":%d},{"oid":%q,"size":7}]}`,
		stored, len(data), unstored)

	// Without the marker both objects are a per-object 404.
	before := decodeBatch(t, e.do("POST", batchPath, body))
	for _, o := range before.Objects {
		if o.Error == nil || o.Error.Code != http.StatusNotFound {
			t.Fatalf("oid %s: error %+v, want 404", o.OID, o.Error)
		}
		if o.Error.Message != contract.Sentence(contract.CodeLFSObjectNotStored) {
			t.Errorf("oid %s: message %q", o.OID, o.Error.Message)
		}
		if len(o.Actions) != 0 {
			t.Errorf("oid %s: actions %v on an unstored object", o.OID, o.Actions)
		}
	}

	// With the marker the stored object gets a URL that serves the bytes.
	e.mark(stored, int64(len(data)))
	after := decodeBatch(t, e.do("POST", batchPath, body))
	if len(after.Objects) != 2 {
		t.Fatalf("%d objects", len(after.Objects))
	}
	if after.Transfer != BasicTransfer {
		t.Errorf("transfer %q", after.Transfer)
	}
	act, ok := after.Objects[0].Actions["download"]
	if !ok {
		t.Fatalf("no download action: %+v", after.Objects[0])
	}
	if act.ExpiresIn != int(PresignTTL.Seconds()) {
		t.Errorf("expires_in %d", act.ExpiresIn)
	}
	got := fetch(t, e.bucket.HTTPClient(), act.Href)
	if !bytes.Equal(got, data) {
		t.Errorf("the presigned URL served %q", got)
	}
	if after.Objects[1].Error == nil || after.Objects[1].Error.Code != http.StatusNotFound {
		t.Errorf("the unmarked object: %+v", after.Objects[1].Error)
	}
}

func fetch(t *testing.T, client *http.Client, url string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, raw)
	}
	return raw
}

// TestVerifyRefusesASizeMismatch is spec 010's verify rule: an upload
// whose stored size differs from the declared size is 422 with the
// lfs_object_mismatch sentence and the object is deleted; one that was
// never uploaded is the same answer; a match writes the marker and a
// repeated verify is a success.
func TestVerifyRefusesASizeMismatch(t *testing.T) {
	e := newEnv(t)
	data := []byte("nine byte")
	oid := oidOf(data)
	e.seed(oid, len(data))

	rec := e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":%d}`, oid, len(data)+1))
	decodeError(t, rec, http.StatusUnprocessableEntity, contract.CodeLFSObjectMismatch)
	if _, ok := e.bucket.Get(e.key("lfs/" + oid)); ok {
		t.Error("the mismatched object was not deleted")
	}

	// An object that was never uploaded is the same refusal.
	decodeError(t, e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":9}`, oid)), http.StatusUnprocessableEntity, contract.CodeLFSObjectMismatch)

	// The matching size writes the marker, and a repeat is a success.
	e.seed(oid, len(data))
	for range 2 {
		if rec := e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":%d}`, oid, len(data))); rec.Code != http.StatusOK {
			t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
		}
	}
	raw, ok := e.bucket.Get(e.key("lfs/verified/" + oid))
	if !ok {
		t.Fatal("no marker")
	}
	var m marker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Size != int64(len(data)) || !m.At.Equal(e.now) || m.Subject != "dev" {
		t.Errorf("marker %+v", m)
	}
}

// TestVerifyIsAWrite is spec 007's action mapping on the LFS routes:
// verify and an upload batch ask for write, a download batch for read.
func TestVerifyIsAWrite(t *testing.T) {
	e := newEnv(t)
	oid := oidOf([]byte("x"))
	e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":1}`, oid))
	e.do("POST", batchPath, fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":1}]}`, oid))
	e.do("POST", batchPath, fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":1}]}`, oid))
	var actions []auth.Action
	for _, req := range e.authz.seen() {
		actions = append(actions, req.Action)
		if req.Repo.ID != testRepo {
			t.Errorf("the authorizer saw repo %+v", req.Repo)
		}
	}
	want := []auth.Action{auth.ActionWrite, auth.ActionWrite, auth.ActionRead}
	if len(actions) != len(want) {
		t.Fatalf("actions %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("actions %v, want %v", actions, want)
		}
	}
	// A read-scoped repository-bound token is refused on verify.
	rec := e.as(auth.Principal{Subject: "build", Bound: &auth.Bound{Repo: testRepo, Scope: auth.ScopeRead}},
		"POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":1}`, oid))
	decodeError(t, rec, http.StatusForbidden, contract.CodeForbidden)
}

// TestBatchBodyLimit is spec 012's LFS body bound, enforced here: a
// body over 1 MiB is 400 with the invalid_request sentence, and the
// authorizer is never asked.
func TestBatchBodyLimit(t *testing.T) {
	e := newEnv(t)
	oid := oidOf([]byte("x"))
	var b strings.Builder
	b.WriteString(`{"operation":"upload","objects":[`)
	for b.Len() < MaxBatchBytes {
		fmt.Fprintf(&b, `{"oid":%q,"size":1},`, oid)
	}
	b.WriteString(`{"oid":"` + oid + `","size":1}]}`)
	if b.Len() <= MaxBatchBytes {
		t.Fatalf("the body is %d bytes", b.Len())
	}
	decodeError(t, e.do("POST", batchPath, b.String()), http.StatusBadRequest, contract.CodeInvalid)
	if n := len(e.authz.seen()); n != 0 {
		t.Errorf("%d authorizer calls on an oversized body", n)
	}
	// One byte under the limit parses and is answered.
	decodeBatch(t, e.do("POST", batchPath, fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":1}]}`, oid)))
}

// TestLocksAre501 is spec 010's locks rule: every path under locks,
// whatever the method, is 501 with the lfs_locks_unsupported sentence.
func TestLocksAre501(t *testing.T) {
	e := newEnv(t)
	base := []string{"/r/" + testRepo + ".git/info/lfs/locks", "/dev/hello.git/info/lfs/locks"}
	for _, prefix := range base {
		for _, suffix := range []string{"", "/verify", "/42/unlock", "/a/b/c"} {
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
				rec := e.as(auth.Principal{Subject: "dev"}, method, prefix+suffix, "{}")
				decodeError(t, rec, http.StatusNotImplemented, contract.CodeLFSLocksUnsupported)
			}
		}
	}
	if n := len(e.authz.seen()); n != 0 {
		t.Errorf("%d authorizer calls: locks needs no repository", n)
	}
}

// TestUploadOverQuota is spec 012's repository size rule on an upload
// batch: the bytes under lfs/, an uploaded but unverified object
// included, plus the batch's sizes, against the authorizer's
// quota_bytes.
func TestUploadOverQuota(t *testing.T) {
	e := newEnv(t)
	e.authz.decision = auth.Decision{Allow: true, QuotaBytes: 1000}
	// 600 bytes uploaded and never verified: they hold bytes until spec
	// 019's sweep removes them, so they count.
	unverified := oidOf([]byte("unverified"))
	e.seed(unverified, 600)
	wanted := oidOf([]byte("wanted"))

	over := fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":401}]}`, wanted)
	decodeError(t, e.do("POST", batchPath, over), http.StatusRequestEntityTooLarge, contract.CodeOverQuota)

	// Exactly at the limit is allowed and answers the upload action.
	at := fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":400}]}`, wanted)
	res := decodeBatch(t, e.do("POST", batchPath, at))
	if _, ok := res.Objects[0].Actions["upload"]; !ok {
		t.Fatalf("no upload action at the limit: %+v", res.Objects[0])
	}
	// A download batch is not bounded by the quota.
	decodeBatch(t, e.do("POST", batchPath, fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":600}]}`, unverified)))
}

// TestQuotaCountsEveryPageOfTheListing walks the lfs/ listing past one
// page, with the markers grouped into a prefix by the delimiter.
func TestQuotaCountsEveryPageOfTheListing(t *testing.T) {
	e := newEnv(t)
	e.authz.decision = auth.Decision{Allow: true, QuotaBytes: 2000}
	for i := range 1200 {
		oid := oidOf(fmt.Appendf(nil, "object-%d", i))
		e.seed(oid, 1)
		e.mark(oid, 1)
	}
	stored, err := e.handlerBytes()
	if err != nil {
		t.Fatal(err)
	}
	if stored != 1200 {
		t.Fatalf("the listing summed %d bytes, want 1200", stored)
	}
	wanted := oidOf([]byte("wanted"))
	decodeError(t, e.do("POST", batchPath, fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":801}]}`, wanted)),
		http.StatusRequestEntityTooLarge, contract.CodeOverQuota)
}

// handlerBytes sums lfs/ the way the quota rule does.
func (e *env) handlerBytes() (int64, error) {
	h := New(Options{Log: e.log, Guard: auth.NewGuard(e.authz, slog.New(slog.DiscardHandler)), Presigner: stubPresigner{}})
	r := httptest.NewRequest("POST", batchPath, nil).WithContext(e.t.Context())
	return h.lfsBytes(r, testRepo)
}

// stubPresigner signs nothing; it is for a test that never reads a URL.
type stubPresigner struct{}

func (stubPresigner) PresignGet(key string, _ time.Duration) (string, error) {
	return "http://example.invalid/" + key, nil
}

func (stubPresigner) PresignPut(key string, _ time.Duration, _ int64, _ ...s3.PresignOption) (string, error) {
	return "http://example.invalid/" + key, nil
}

// failingPresigner is a signer whose every call fails, for the
// per-object 503 of a signing failure.
type failingPresigner struct{}

var errPresign = errors.New("no credential")

func (failingPresigner) PresignGet(string, time.Duration) (string, error) { return "", errPresign }

func (failingPresigner) PresignPut(string, time.Duration, int64, ...s3.PresignOption) (string, error) {
	return "", errPresign
}

// TestPresignedPutPinsLengthAndType is spec 010's upload action: the
// URL signs Content-Length and Content-Type, so the store refuses a PUT
// of another size or type, and the action's header names the type the
// basic transfer must send.
func TestPresignedPutPinsLengthAndType(t *testing.T) {
	rec := &recordingTB{TB: t}
	e := newEnvOn(t, s3test.New(rec, "origo-test"))
	data := []byte("the object's bytes")
	oid := oidOf(data)

	res := decodeBatch(t, e.do("POST", batchPath, fmt.Sprintf(`{"operation":"upload","transfers":["basic"],"objects":[{"oid":%q,"size":%d}]}`, oid, len(data))))
	upload, ok := res.Objects[0].Actions["upload"]
	if !ok {
		t.Fatalf("no upload action: %+v", res.Objects[0])
	}
	if upload.Header["Content-Type"] != ObjectContentType {
		t.Errorf("the action's header names %q, want %q", upload.Header["Content-Type"], ObjectContentType)
	}
	verify, ok := res.Objects[0].Actions["verify"]
	if !ok {
		t.Fatalf("no verify action: %+v", res.Objects[0])
	}
	if !strings.HasSuffix(verify.Href, "/info/lfs/verify") {
		t.Errorf("verify href %q", verify.Href)
	}
	if verify.Header["Authorization"] != "Bearer dev-token" {
		t.Errorf("the verify action carries %q", verify.Header["Authorization"])
	}

	put := func(body []byte, contentType string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, upload.Href, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := e.bucket.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// Another length and another type are each refused, and the store
	// reports the signature failure the way a provider would.
	if status := put(append(data, '!'), ObjectContentType); status != http.StatusForbidden {
		t.Errorf("a PUT of another length answered %d", status)
	}
	if status := put(data, "text/plain"); status != http.StatusForbidden {
		t.Errorf("a PUT of another type answered %d", status)
	}
	messages := rec.recorded()
	if len(messages) != 2 {
		t.Fatalf("the store recorded %d failures: %v", len(messages), messages)
	}
	for _, m := range messages {
		if !strings.Contains(strings.ToLower(m), "signature") {
			t.Errorf("recorded %q, want a signature failure", m)
		}
	}
	// Both pinned values land.
	if status := put(data, ObjectContentType); status != http.StatusOK {
		t.Fatalf("the pinned PUT answered %d", status)
	}
	stored, ok := e.bucket.Get(e.key("lfs/" + oid))
	if !ok || !bytes.Equal(stored, data) {
		t.Fatalf("the object did not land: %v %q", ok, stored)
	}
	if ct, _ := e.bucket.ContentType(e.key("lfs/" + oid)); ct != ObjectContentType {
		t.Errorf("stored Content-Type %q", ct)
	}
	if rest := rec.recorded(); len(rest) != 2 {
		t.Errorf("the pinned PUT was reported: %v", rest)
	}
}

// TestBatchRefusesAMalformedRequest covers the request rules the batch
// applies before the authorizer: the body, the operation, the transfer
// adapter, and the per-object oid and size.
func TestBatchRefusesAMalformedRequest(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ name, body string }{
		{"not json", "{"},
		{"no operation", `{"objects":[]}`},
		{"unknown operation", `{"operation":"delete","objects":[]}`},
		{"no adapter in common", `{"operation":"download","transfers":["tus"],"objects":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decodeError(t, e.do("POST", batchPath, tc.body), http.StatusBadRequest, contract.CodeInvalid)
		})
	}
	if n := len(e.authz.seen()); n != 0 {
		t.Errorf("%d authorizer calls on a malformed batch", n)
	}
	// A malformed object is a per-object 400 and the rest is answered.
	good := oidOf([]byte("good"))
	e.seed(good, 4)
	e.mark(good, 4)
	body := fmt.Sprintf(`{"operation":"download","objects":[{"oid":"../escape","size":1},{"oid":%q,"size":-1},{"oid":%q,"size":4}]}`,
		strings.ToUpper(good), good)
	res := decodeBatch(t, e.do("POST", batchPath, body))
	for i := range 2 {
		if res.Objects[i].Error == nil || res.Objects[i].Error.Code != http.StatusBadRequest {
			t.Errorf("object %d: %+v", i, res.Objects[i].Error)
		}
	}
	if _, ok := res.Objects[2].Actions["download"]; !ok {
		t.Errorf("the well-formed object: %+v", res.Objects[2])
	}
	// A malformed verify body is the same refusal.
	for _, body := range []string{"{", `{"oid":"nope","size":1}`, `{"oid":"` + good + `","size":-1}`} {
		decodeError(t, e.do("POST", verifyPath, body), http.StatusBadRequest, contract.CodeInvalid)
	}
}

// TestLabelFormAndUnknownRepository covers the second URL form and the
// two answers spec 007 fixes: a deny is 403 whether or not the
// repository exists, and 404 reaches only an allowed caller.
func TestLabelFormAndUnknownRepository(t *testing.T) {
	e := newEnv(t)
	good := oidOf([]byte("good"))
	e.seed(good, 4)
	e.mark(good, 4)
	body := fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":4}]}`, good)
	res := decodeBatch(t, e.do("POST", "/dev/hello.git/info/lfs/objects/batch", body))
	if _, ok := res.Objects[0].Actions["download"]; !ok {
		t.Fatalf("the label form: %+v", res.Objects[0])
	}
	// The slug without .git works the same way.
	decodeBatch(t, e.do("POST", "/dev/hello/info/lfs/objects/batch", body))

	// An unresolved name reaches the authorizer with the owner and slug
	// and answers 404 once it allowed.
	decodeError(t, e.do("POST", "/dev/nothing.git/info/lfs/objects/batch", body), http.StatusNotFound, contract.CodeRepoNotFound)
	last := e.authz.seen()
	req := last[len(last)-1]
	if req.Repo.ID != "" || req.Repo.Owner != "dev" || req.Repo.Slug != "nothing" {
		t.Errorf("the authorizer saw %+v", req.Repo)
	}
	// An unknown id answers 404 too, after the allow.
	unknown := "00000000-0000-4000-8000-000000000000"
	decodeError(t, e.do("POST", "/r/"+unknown+".git/info/lfs/objects/batch", body), http.StatusNotFound, contract.CodeRepoNotFound)
	decodeError(t, e.do("POST", "/r/"+unknown+".git/info/lfs/verify", fmt.Sprintf(`{"oid":%q,"size":4}`, good)), http.StatusNotFound, contract.CodeRepoNotFound)

	// A deny is 403 in the LFS shape.
	e.authz.decision = auth.Decision{Allow: false, Reason: "no"}
	decodeError(t, e.do("POST", batchPath, body), http.StatusForbidden, contract.CodeForbidden)

	// No decision is 503 with the sentence of authorizer_unavailable.
	e.authz.err = &auth.Unavailable{URL: "http://authorizer.invalid", Status: 500}
	decodeError(t, e.do("POST", batchPath, body), http.StatusServiceUnavailable, contract.CodeAuthorizerUnavailable)
	decodeError(t, e.do("POST", "/dev/hello.git/info/lfs/verify", fmt.Sprintf(`{"oid":%q,"size":4}`, good)), http.StatusServiceUnavailable, contract.CodeAuthorizerUnavailable)
}

// TestDeletedRepositoryIsNotFound: a deleted repository answers 404 on
// every LFS path, the way every other endpoint does.
func TestDeletedRepositoryIsNotFound(t *testing.T) {
	e := newEnv(t)
	ix, _, err := e.log.Newest(t.Context(), testRepo, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.Commit(t.Context(), testRepo, ix, wal.Entry{Kind: wal.KindDelete, Deleted: true},
		func(context.Context, *wal.Index) error { return nil }); err != nil {
		t.Fatal(err)
	}
	oid := oidOf([]byte("x"))
	decodeError(t, e.do("POST", batchPath, fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":1}]}`, oid)),
		http.StatusNotFound, contract.CodeRepoNotFound)
	decodeError(t, e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":1}`, oid)), http.StatusNotFound, contract.CodeRepoNotFound)
}

// TestStorageFailuresAreServiceUnavailable: a store that fails is 503
// with the storage_unavailable sentence at the top level, and a
// per-object 503 inside a batch, whichever call failed.
func TestStorageFailuresAreServiceUnavailable(t *testing.T) {
	store := wal.NewMemStore()
	log := wal.New(wal.Options{Store: store, Prefix: config.Prefix, Logger: slog.New(slog.DiscardHandler)})
	if _, err := log.CreateRepo(t.Context(), wal.Meta{ID: testRepo, Owner: "dev", Slug: "hello"}, "main"); err != nil {
		t.Fatal(err)
	}
	authz := &fakeAuthorizer{decision: auth.Decision{Allow: true, QuotaBytes: auth.DefaultQuotaBytes}}
	mux := http.NewServeMux()
	New(Options{
		Log: log, Guard: auth.NewGuard(authz, slog.New(slog.DiscardHandler)),
		Presigner: stubPresigner{}, Logger: slog.New(slog.DiscardHandler),
	}).Register(mux)
	e := &env{t: t, log: log, authz: authz, mux: mux}

	oid := oidOf([]byte("x"))
	down := fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":1}]}`, oid)
	up := fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":1}]}`, oid)
	broken := errors.New("the bucket is unreachable")
	fail := func(wantOp, contains string) {
		t.Helper()
		store.SetFault(func(op, key string) error {
			if op == wantOp && strings.Contains(key, contains) {
				return broken
			}
			return nil
		})
	}
	// failKey breaks every call that touches a key, whatever the verb:
	// the newest index is read through a hint, a HEAD, and a listing.
	failKey := func(contains string) {
		t.Helper()
		store.SetFault(func(_, key string) error {
			if strings.Contains(key, contains) {
				return broken
			}
			return nil
		})
	}

	// The HEAD of the marker fails: a per-object 503.
	fail("Head", "lfs/verified/")
	res := decodeBatch(t, e.do("POST", batchPath, down))
	if res.Objects[0].Error == nil || res.Objects[0].Error.Code != http.StatusServiceUnavailable {
		t.Fatalf("object error %+v", res.Objects[0].Error)
	}
	if res.Objects[0].Error.Message != contract.Sentence(contract.CodeStorageUnavailable) {
		t.Errorf("message %q", res.Objects[0].Error.Message)
	}

	// The newest index cannot be read: a top-level 503 on both routes.
	failKey("index/")
	decodeError(t, e.do("POST", batchPath, down), http.StatusServiceUnavailable, contract.CodeStorageUnavailable)
	failKey("index/")
	decodeError(t, e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":1}`, oid)), http.StatusServiceUnavailable, contract.CodeStorageUnavailable)

	// The name lookup fails before the guard is asked.
	failKey("names/")
	before := len(e.authz.seen())
	decodeError(t, e.do("POST", "/dev/hello.git/info/lfs/objects/batch", down), http.StatusServiceUnavailable, contract.CodeStorageUnavailable)
	if len(e.authz.seen()) != before {
		t.Error("the guard was asked after the name lookup failed")
	}

	// The lfs/ listing of the quota rule fails.
	fail("List", "lfs/")
	decodeError(t, e.do("POST", batchPath, up), http.StatusServiceUnavailable, contract.CodeStorageUnavailable)

	// The HEAD of the object on verify fails, and so does the marker
	// write after a matching size.
	fail("Head", "lfs/")
	decodeError(t, e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":1}`, oid)), http.StatusServiceUnavailable, contract.CodeStorageUnavailable)
	store.SetFault(nil)
	if _, err := store.Put(t.Context(), log.RepoPrefix(testRepo)+"lfs/"+oid, wal.BytesBody([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	fail("Create", "lfs/verified/")
	decodeError(t, e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":1}`, oid)), http.StatusServiceUnavailable, contract.CodeStorageUnavailable)

	// A mismatched size whose delete fails is still 422: the object is
	// left for spec 019's sweep and the failure goes to the log.
	fail("Delete", "lfs/")
	decodeError(t, e.do("POST", verifyPath, fmt.Sprintf(`{"oid":%q,"size":2}`, oid)), http.StatusUnprocessableEntity, contract.CodeLFSObjectMismatch)
	store.SetFault(nil)
}

// TestSigningFailureIsAPerObjectError: a signer that cannot sign leaves
// the rest of the batch answered.
func TestSigningFailureIsAPerObjectError(t *testing.T) {
	e := newEnv(t)
	oid := oidOf([]byte("x"))
	e.seed(oid, 1)
	e.mark(oid, 1)
	mux := http.NewServeMux()
	New(Options{
		Log: e.log, Guard: auth.NewGuard(e.authz, slog.New(slog.DiscardHandler)),
		Presigner: failingPresigner{}, Logger: slog.New(slog.DiscardHandler),
	}).Register(mux)
	e.mux = mux
	for _, op := range []string{"download", "upload"} {
		res := decodeBatch(t, e.do("POST", batchPath, fmt.Sprintf(`{"operation":%q,"objects":[{"oid":%q,"size":1}]}`, op, oid)))
		if res.Objects[0].Error == nil || res.Objects[0].Error.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %+v", op, res.Objects[0])
		}
	}
}

// TestVerifyHrefFollowsTheRequest: the verify action names the origin
// the client reached, so a reverse proxy in front of the node stays in
// the path, and the scheme follows X-Forwarded-Proto or TLS.
func TestVerifyHrefFollowsTheRequest(t *testing.T) {
	e := newEnv(t)
	oid := oidOf([]byte("x"))
	body := fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":1}]}`, oid)

	req := httptest.NewRequest("POST", "http://proxy.example:9999"+batchPath, strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(t.Context(), auth.Principal{Subject: "dev"}))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	res := decodeBatch(t, rec)
	want := "https://proxy.example:9999/r/" + testRepo + ".git/info/lfs/verify"
	if got := res.Objects[0].Actions["verify"].Href; got != want {
		t.Errorf("verify href %q, want %q", got, want)
	}
	if h := res.Objects[0].Actions["verify"].Header; h != nil {
		t.Errorf("no Authorization on the request, so no header: %v", h)
	}
	// Over TLS with no forwarded header the scheme is https too.
	req = httptest.NewRequest("POST", "https://node.example"+batchPath, strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(t.Context(), auth.Principal{Subject: "dev"}))
	rec = httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	res = decodeBatch(t, rec)
	if got := res.Objects[0].Actions["verify"].Href; !strings.HasPrefix(got, "https://node.example/") {
		t.Errorf("verify href %q", got)
	}
}

// TestNewRefusesAnIncompleteHandler and the presigner's own validation.
func TestNewRefusesAnIncompleteHandler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New accepted a handler with no log")
		}
	}()
	New(Options{})
}

func TestNewPresignerRefusesABadEndpoint(t *testing.T) {
	if _, err := NewPresigner(PresignerOptions{Endpoint: "://", Region: "us-east-1", Bucket: "b", Key: "k", Secret: "s"}); err == nil {
		t.Fatal("NewPresigner accepted a malformed endpoint")
	}
	if _, err := NewPresigner(PresignerOptions{Endpoint: "http://localhost:9000", Region: "us-east-1", Bucket: "b", Key: "k", Secret: "s", PathStyle: true}); err != nil {
		t.Fatal(err)
	}
}

// TestRequestIDIsAlwaysPresent: with no trace on the request the id is
// a fresh UUID, so two failures carry two ids.
func TestRequestIDIsAlwaysPresent(t *testing.T) {
	e := newEnv(t)
	ids := map[string]bool{}
	for range 2 {
		rec := e.do("POST", "/r/"+testRepo+".git/info/lfs/locks", "{}")
		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		ids[body.RequestID] = true
	}
	if len(ids) != 2 {
		t.Errorf("two failures carried %d request ids", len(ids))
	}
}

// TestDefaultsAreTheSpecValues holds the constants a client and the
// operator read.
func TestDefaultsAreTheSpecValues(t *testing.T) {
	if PresignTTL != 15*time.Minute {
		t.Errorf("PresignTTL %s", PresignTTL)
	}
	if MaxBatchBytes != 1<<20 {
		t.Errorf("MaxBatchBytes %d", MaxBatchBytes)
	}
	h := New(Options{Log: wal.New(wal.Options{Store: wal.NewMemStore()}), Guard: auth.NewGuard(&fakeAuthorizer{}, nil), Presigner: stubPresigner{}})
	if h.now().IsZero() {
		t.Error("the default clock is not set")
	}
	if h.logger == nil {
		t.Error("the default logger is not set")
	}
}
