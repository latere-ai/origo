// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

const repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

type harness struct {
	t     *testing.T
	store *wal.MemStore
	log   *wal.Log
	cache *repo.Cache
	srv   *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := wal.NewMemStore()
	logger := slog.New(slog.DiscardHandler)
	l := wal.New(wal.Options{Store: store, Logger: logger})
	cache, err := repo.New(repo.Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(cache, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, store: store, log: l, cache: cache, srv: srv}
}

func (h *harness) do(method, path, body string) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, h.srv.URL+path, strings.NewReader(body))
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func code(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

func TestRepositoryLifecycle(t *testing.T) {
	h := newHarness(t)
	status, out := h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app","default_branch":"trunk"}`)
	if status != 201 || out["id"] != repoA || out["default_branch"] != "trunk" || out["head"] != "" || out["size_bytes"] != float64(0) {
		t.Fatalf("create: %d %v", status, out)
	}
	if status, out := h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"other"}`); status != 409 || code(out) != contract.CodeRepoExists {
		t.Fatalf("duplicate id: %d %v", status, out)
	}
	if status, out := h.do("POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"acme","slug":"app"}`); status != 409 || code(out) != contract.CodeRepoExists {
		t.Fatalf("duplicate name: %d %v", status, out)
	}
	for name, body := range map[string]string{
		"not json":       `{`,
		"unknown field":  `{"id":"` + repoA + `","owner":"a","slug":"b","x":1}`,
		"bad id":         `{"id":"x","owner":"a","slug":"b"}`,
		"reserved owner": `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"r","slug":"b"}`,
		"bad slug":       `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"b c"}`,
		"bad branch":     `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"b","default_branch":"a..b"}`,
	} {
		if status, out := h.do("POST", "/v1/repos", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	if status, out := h.do("GET", "/v1/repos/"+repoA, ""); status != 200 || out["owner"] != "acme" || out["slug"] != "app" {
		t.Fatalf("get: %d %v", status, out)
	}
	if status, out := h.do("GET", "/v1/repos/00000000-0000-4000-8000-000000000000", ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("get unknown: %d %v", status, out)
	}
	if status, out := h.do("GET", "/v1/repos/nope", ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("get invalid: %d %v", status, out)
	}

	// Rename, then the default branch through the log.
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"slug":"app2"}`); status != 200 || out["slug"] != "app2" || out["owner"] != "acme" {
		t.Fatalf("rename: %d %v", status, out)
	}
	if id, err := h.log.Resolve(context.Background(), "acme", "app2"); err != nil || id != repoA {
		t.Fatal("new name does not resolve")
	}
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"owner":"acme2","default_branch":"main"}`); status != 200 || out["owner"] != "acme2" || out["default_branch"] != "main" {
		t.Fatalf("rename owner and branch: %d %v", status, out)
	}
	ix, _, _ := h.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 1 || ix.Refs["HEAD"] != "ref: refs/heads/main" {
		t.Fatalf("HEAD not moved through the log: %+v", ix)
	}
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"default_branch":"main"}`); status != 200 {
		t.Fatalf("same branch: %d %v", status, out)
	}
	if ix, _, _ := h.log.Newest(context.Background(), repoA, 0, false); ix.Seq != 1 {
		t.Fatal("a no-op patch moved the log")
	}
	h.do("POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"acme","slug":"taken"}`)
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"owner":"acme","slug":"taken"}`); status != 409 || code(out) != contract.CodeRepoExists {
		t.Fatalf("rename onto taken: %d %v", status, out)
	}
	for name, body := range map[string]string{
		"not json":   `{`,
		"bad label":  `{"slug":"a b"}`,
		"reserved":   `{"owner":"v1"}`,
		"bad branch": `{"default_branch":"a..b"}`,
	} {
		if status, out := h.do("PATCH", "/v1/repos/"+repoA, body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("patch %s: %d %v", name, status, out)
		}
	}
	if status, out := h.do("PATCH", "/v1/repos/00000000-0000-4000-8000-000000000000", `{"slug":"x"}`); status != 404 {
		t.Fatalf("patch unknown: %d %v", status, out)
	}

	// Delete holds the objects and answers 404 until an undelete.
	if status, out := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 || out["deleted_at"] == nil || out["purge_after"] == nil {
		t.Fatalf("delete: %d %v", status, out)
	}
	if status, _ := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 {
		t.Fatalf("second delete: %d", status)
	}
	if status, out := h.do("GET", "/v1/repos/"+repoA, ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("get deleted: %d %v", status, out)
	}
	if status, out := h.do("PATCH", "/v1/repos/"+repoA, `{"slug":"x"}`); status != 404 {
		t.Fatalf("patch deleted: %d %v", status, out)
	}
	if _, _, err := h.cache.Acquire(context.Background(), repoA, false); !errors.Is(err, repo.ErrDeleted) {
		t.Fatalf("cache after delete: %v", err)
	}
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 || out["id"] != repoA {
		t.Fatalf("undelete: %d %v", status, out)
	}
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 {
		t.Fatal("second undelete")
	}
	if status, _ := h.do("GET", "/v1/repos/"+repoA, ""); status != 200 {
		t.Fatal("get after undelete")
	}
	if status, _ := h.do("POST", "/v1/repos/00000000-0000-4000-8000-000000000000/undelete", ""); status != 404 {
		t.Fatal("undelete unknown")
	}
	if status, _ := h.do("DELETE", "/v1/repos/00000000-0000-4000-8000-000000000000", ""); status != 404 {
		t.Fatal("delete unknown")
	}
}

func TestStorageFailuresAnswer503(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"app"}`)
	fail := func(ops ...string) {
		h.store.SetFault(func(op, key string) error {
			for _, o := range ops {
				if o == op || (strings.HasPrefix(o, "!") && op != o[1:]) {
					return errors.New("storage down")
				}
			}
			return nil
		})
	}
	check := func(name, method, path, body string) {
		t.Helper()
		if status, out := h.do(method, path, body); status != 503 || code(out) != contract.CodeStorageUnavailable {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	fail("Create")
	check("create", "POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"b"}`)
	fail("Get")
	check("get meta", "GET", "/v1/repos/"+repoA, "")
	check("patch", "PATCH", "/v1/repos/"+repoA, `{"slug":"x"}`)
	check("delete", "DELETE", "/v1/repos/"+repoA, "")
	check("undelete", "POST", "/v1/repos/"+repoA+"/undelete", "")
	fail("Head")
	check("get index", "GET", "/v1/repos/"+repoA, "")
	fail("Put")
	check("patch branch", "PATCH", "/v1/repos/"+repoA, `{"default_branch":"dev"}`)
	check("delete entry", "DELETE", "/v1/repos/"+repoA, "")
	check("rename meta", "PATCH", "/v1/repos/"+repoA, `{"slug":"y"}`)
	h.store.SetFault(nil)
	h.do("DELETE", "/v1/repos/"+repoA, "")
	fail("Put")
	check("undelete entry", "POST", "/v1/repos/"+repoA+"/undelete", "")
	h.store.SetFault(nil)
	// A repository whose metadata exists but whose index vanished is
	// not found, and a create whose read-back fails is 503.
	_ = h.store.Delete(context.Background(), h.log.RepoPrefix(repoA)+wal.LatestKey)
	for _, k := range h.store.Keys() {
		if strings.Contains(k, "/index/") {
			_ = h.store.Delete(context.Background(), k)
		}
	}
	if status, out := h.do("GET", "/v1/repos/"+repoA, ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("no index: %d %v", status, out)
	}
	h.store.SetFault(func(op, key string) error {
		if op == "Get" && strings.HasSuffix(key, "/meta") {
			return errors.New("meta unreadable")
		}
		return nil
	})
	check("create read-back", "POST", "/v1/repos", `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"a","slug":"c"}`)
}
