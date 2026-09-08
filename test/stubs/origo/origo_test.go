// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/origo"
)

const repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

func call(t *testing.T, url, token, method, path, body string) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, url+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

// code is the error code of an envelope, empty when the body is not one.
func code(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func cloneURL(s *origo.Server, token, id string) string {
	return strings.Replace(s.URL(), "http://", "http://x:"+token+"@", 1) + "/r/" + id + ".git"
}

func commitFile(t *testing.T, dir, name, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, dir, nil, "add", "--", name)
	gittest.Run(t, dir, nil, "commit", "-q", "-m", message)
}

// TestStubServesTheContract is spec 013's criterion for the contract
// stub: a clone, a push, and the lifecycle table of spec 003 against the
// real git and net/http in-process, with the stubs deciding.
func TestStubServesTheContract(t *testing.T) {
	s := origo.New(t)
	token := s.Token("dev", "")
	// The unauthenticated paths and the contract header.
	if status, body, h := call(t, s.URL(), "", "GET", "/version", ""); status != 200 || body["version"] == nil || h.Get("Origo-Contract") == "" {
		t.Fatalf("/version: %d %v %v", status, body, h)
	}
	if status, _, _ := call(t, s.URL(), "", "GET", "/readyz", ""); status != 200 {
		t.Fatalf("/readyz: %d", status)
	}
	if status, body, _ := call(t, s.URL(), "", "GET", "/.well-known/jwks.json", ""); status != 200 || body["keys"] == nil {
		t.Fatalf("jwks: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), "", "GET", "/v1/repos/"+repoA, ""); status != 401 || code(body) != "unauthenticated" {
		t.Fatalf("no token: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), token, "GET", "/nope", ""); status != 400 || code(body) != "invalid_request" {
		t.Fatalf("unknown route: %d %v", status, body)
	}
	// The lifecycle: create, read, rename, delete, undelete, a token.
	if status, body, _ := call(t, s.URL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"acme","slug":"app"}`, repoA)); status != 201 || body["id"] != repoA {
		t.Fatalf("create: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"acme","slug":"app"}`, repoA)); status != 409 || code(body) != "repo_exists" {
		t.Fatalf("duplicate: %d %v", status, body)
	}
	// A clone of the empty repository, a push, and a clone from another
	// directory that sees the pushed history.
	work := filepath.Join(t.TempDir(), "work")
	gittest.Run(t, t.TempDir(), nil, "clone", "-q", cloneURL(s, token, repoA), work)
	commitFile(t, work, "a.txt", "one", "first")
	commitFile(t, work, "b.txt", "two", "second")
	gittest.Run(t, work, nil, "push", "-q", "origin", "HEAD:refs/heads/main")
	other := filepath.Join(t.TempDir(), "other")
	gittest.Run(t, t.TempDir(), nil, "clone", "-q", cloneURL(s, token, repoA), other)
	if gittest.RevList(t, work) != gittest.RevList(t, other) {
		t.Fatal("the second clone differs from the pushed history")
	}
	if status, body, _ := call(t, s.URL(), token, "GET", "/v1/repos/"+repoA, ""); status != 200 || body["head"] == "" || body["default_branch"] != "main" {
		t.Fatalf("get: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), token, "PATCH", "/v1/repos/"+repoA, `{"slug":"renamed"}`); status != 200 || body["slug"] != "renamed" {
		t.Fatalf("rename: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), token, "POST", "/v1/repos/"+repoA+"/tokens", `{"scope":"read","ttl":60}`); status != 201 || body["token"] == nil {
		t.Fatalf("tokens: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), token, "DELETE", "/v1/repos/"+repoA, ""); status != 202 || body["purge_after"] == nil {
		t.Fatalf("delete: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), token, "GET", "/v1/repos/"+repoA, ""); status != 404 || code(body) != "repo_not_found" {
		t.Fatalf("deleted: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), token, "POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 || body["id"] != repoA {
		t.Fatalf("undelete: %d %v", status, body)
	}
	// The stubs decide: a deny is a 403, the sink is there for spec 008,
	// and a failing bucket is a 503 storage_unavailable.
	s.Authorizer().Deny(authorizer.Rule{Subject: "eve"}, "not welcome")
	if status, body, _ := call(t, s.URL(), s.Token("eve", ""), "GET", "/v1/repos/"+repoA, ""); status != 403 || code(body) != "forbidden" {
		t.Fatalf("deny: %d %v", status, body)
	}
	if status, body, _ := call(t, s.URL(), s.Token("bob", "svc"), "GET", "/v1/repos/"+repoA, ""); status != 200 || body["id"] != repoA {
		t.Fatalf("acting for: %d %v", status, body)
	}
	// sub is the caller and act the subject it acts for (spec 007).
	if reqs := s.Authorizer().Requests(); reqs[len(reqs)-1].Subject != "svc" || reqs[len(reqs)-1].Actor != "bob" {
		t.Fatalf("act on the authorizer request: %+v", reqs[len(reqs)-1])
	}
	if s.Sink().URL() == "" || s.Issuer().URL() == "" || s.Store() == nil || s.Log() == nil {
		t.Fatal("handles")
	}
	s.Bucket().Fail(math.MaxInt, 503)
	if status, body, _ := call(t, s.URL(), token, "GET", "/v1/repos/"+repoA, ""); status != 503 || code(body) != "storage_unavailable" {
		t.Fatalf("bucket down: %d %v", status, body)
	}
	if status, _, _ := call(t, s.URL(), "", "GET", "/readyz", ""); status != 503 {
		t.Fatalf("/readyz with the bucket down: %d", status)
	}
	s.Bucket().Fail(0, 0)
	if status, _, _ := call(t, s.URL(), token, "GET", "/v1/repos/"+repoA, ""); status != 200 {
		t.Fatalf("bucket back: %d", status)
	}
	if keys := s.Bucket().Keys(); len(keys) == 0 || !strings.HasPrefix(keys[0], "origo/") {
		t.Fatalf("bucket keys: %v", keys)
	}
}
