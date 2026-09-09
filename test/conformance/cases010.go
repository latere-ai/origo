// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/lfs"
)

// The rows of spec 010, driven over the batch API with net/http so the
// run needs no git-lfs: an upload through the presigned URL and its
// verify, a download, the deduplication of a held object, and the
// three refusals in the LFS body shape.

func cases010() []testCase {
	return []testCase{
		{name: "upload-download", run: case010UploadDownload},
		{name: "lfs_object_mismatch", run: case010Mismatch},
		{name: "lfs_object_not_stored", run: case010NotStored},
		{name: "lfs_locks_unsupported", run: case010Locks},
	}
}

// lfsCall posts to an LFS route with the LFS media type.
func (s *session) lfsCall(t *testing.T, id, route, body string) response {
	t.Helper()
	return s.do(t, request{method: "POST", path: "/r/" + id + ".git/info/lfs/" + route, body: body, token: s.target.Token,
		header: map[string]string{"Accept": lfs.MediaType, "Content-Type": lfs.MediaType}})
}

// lfsError asserts an LFS body: the sentence of the code, a request
// id, and the documentation URL.
func lfsError(t *testing.T, r response, status int, code string) {
	t.Helper()
	failIf(t, r.status != status || r.json["message"] != sentence(t, code) || r.json["request_id"] == "" || r.json["documentation_url"] != lfs.DocumentationURL || r.header.Get("Content-Type") != lfs.MediaType, "want %d %q, got %d %v %s", status, sentence(t, code), r.status, r.header.Get("Content-Type"), r.body)
}

// batch decodes a batch response's objects.
func batch(t *testing.T, r response) []map[string]any {
	t.Helper()
	expectStatus(t, r, http.StatusOK)
	objects, _ := r.json["objects"].([]any)
	var out []map[string]any
	for _, o := range objects {
		m, _ := o.(map[string]any)
		out = append(out, m)
	}
	return out
}

// action is one action of a batch object.
func action(t *testing.T, o map[string]any, name string) (href string, header map[string]string) {
	t.Helper()
	actions, _ := o["actions"].(map[string]any)
	a, ok := actions[name].(map[string]any)
	failIf(t, !ok, "no %s action: %v", name, o)
	href, _ = a["href"].(string)
	header = map[string]string{}
	if h, _ := a["header"].(map[string]any); h != nil {
		for k, v := range h {
			header[k], _ = v.(string)
		}
	}
	return href, header
}

// transfer performs one presigned transfer against the bucket.
func transfer(t *testing.T, method, href string, header map[string]string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, href, bytes.NewReader(body))
	failIf(t, err != nil, "%v", err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := bucketClient.Do(req)
	failIf(t, err != nil, "%s %s: %v", method, href, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// bucketClient carries the presigned transfers to the bucket.
var bucketClient = &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Minute}

func oidOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func case010UploadDownload(t *testing.T, s *session) {
	id := s.create(t, "lfs")
	content := gittest.Bytes(64<<10, 11)
	oid := oidOf(content)
	up := fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":%d}],"transfers":["basic"]}`, oid, len(content))
	objects := batch(t, s.lfsCall(t, id, "objects/batch", up))
	failIf(t, len(objects) != 1 || objects[0]["oid"] != oid, "upload batch: %v", objects)
	href, header := action(t, objects[0], "upload")
	if status, body := transfer(t, "PUT", href, header, content); status/100 != 2 {
		t.Fatalf("presigned PUT: %d %s", status, body)
	}
	verifyHref, verifyHeader := action(t, objects[0], "verify")
	req := request{method: "POST", path: verifyHref, body: mustJSON(t, map[string]any{"oid": oid, "size": len(content)}), header: verifyHeader}
	if req.header["Authorization"] == "" {
		req.token = s.target.Token
	}
	expectStatus(t, s.do(t, req), http.StatusOK)
	// The held object needs no upload.
	objects = batch(t, s.lfsCall(t, id, "objects/batch", up))
	if _, ok := objects[0]["actions"]; ok {
		t.Fatalf("a held object was given actions: %v", objects[0])
	}
	// The download brings the bytes back from the bucket.
	down := fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":%d}],"transfers":["basic"]}`, oid, len(content))
	objects = batch(t, s.lfsCall(t, id, "objects/batch", down))
	href, header = action(t, objects[0], "download")
	status, got := transfer(t, "GET", href, header, nil)
	failIf(t, status != http.StatusOK || !bytes.Equal(got, content), "presigned GET: %d, %d bytes", status, len(got))
}

func case010Mismatch(t *testing.T, s *session) {
	id := s.create(t, "lfs-mismatch")
	content := []byte("mismatch")
	oid := oidOf(content)
	objects := batch(t, s.lfsCall(t, id, "objects/batch", fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":%d}]}`, oid, len(content))))
	href, header := action(t, objects[0], "upload")
	if status, body := transfer(t, "PUT", href, header, content); status/100 != 2 {
		t.Fatalf("presigned PUT: %d %s", status, body)
	}
	lfsError(t, s.lfsCall(t, id, "verify", fmt.Sprintf(`{"oid":%q,"size":%d}`, oid, len(content)+1)), http.StatusUnprocessableEntity, contract.CodeLFSObjectMismatch)
	lfsError(t, s.lfsCall(t, id, "verify", fmt.Sprintf(`{"oid":%q,"size":1}`, oidOf([]byte("never uploaded")))), http.StatusUnprocessableEntity, contract.CodeLFSObjectMismatch)
}

func case010NotStored(t *testing.T, s *session) {
	id := s.create(t, "lfs-missing")
	oid := oidOf([]byte("missing"))
	objects := batch(t, s.lfsCall(t, id, "objects/batch", fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":7}]}`, oid)))
	e, _ := objects[0]["error"].(map[string]any)
	failIf(t, e["code"] != float64(http.StatusNotFound) || e["message"] != sentence(t, contract.CodeLFSObjectNotStored), "per-object error: %v", objects[0])
	if _, ok := objects[0]["actions"]; ok {
		t.Fatalf("a missing object was given actions: %v", objects[0])
	}
}

func case010Locks(t *testing.T, s *session) {
	id := s.fixture.id
	lfsError(t, s.lfsCall(t, id, "locks", `{"path":"a.txt"}`), http.StatusNotImplemented, contract.CodeLFSLocksUnsupported)
	lfsError(t, s.lfsCall(t, id, "locks/verify", `{}`), http.StatusNotImplemented, contract.CodeLFSLocksUnsupported)
	r := s.do(t, request{method: "GET", path: "/r/" + id + ".git/info/lfs/locks", token: s.target.Token, header: map[string]string{"Accept": lfs.MediaType}})
	lfsError(t, r, http.StatusNotImplemented, contract.CodeLFSLocksUnsupported)
	// git-lfs, when on PATH, reads the 501 and goes on.
	if _, err := exec.LookPath("git-lfs"); err == nil {
		work := clone(t, s.repoURL(id))
		if out, err := git(t, work, "lfs", "locks"); err == nil {
			t.Logf("git lfs locks: %s", out)
		}
	}
}
