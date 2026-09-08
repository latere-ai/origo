// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
)

// TestReadAPIServesStaleAndRepositoryUnavailable: the read API carries
// Origo-Stale the way the git routes do while the read breaker is
// open, answers 503 storage_unavailable with the breaker's details and
// Retry-After for what the copy cannot answer, and 503
// repository_unavailable naming the key for an integrity error of the
// log (spec 015).
func TestReadAPIServesStaleAndRepositoryUnavailable(t *testing.T) {
	var bs *wal.BreakerStore
	h := newHarness(t, withWrap(func(s wal.Store) wal.Store {
		bs = wal.NewBreakerStore(wal.BreakerOptions{Store: s, Threshold: 1, OpenFor: 3 * time.Second})
		return bs
	}))
	f := loadFixture(t)
	h.seed(f)
	ctx := context.Background()
	if r := h.get("/v1/repos/" + repoA + "/refs"); r.status != 200 || r.header.Get(contract.HeaderStale) != "" {
		t.Fatalf("warm read: %d %q", r.status, r.header.Get(contract.HeaderStale))
	}
	// The bucket goes away for reads: the check that runs into it fails
	// on its own error and opens the breaker.
	h.store.SetFault(func(op, _ string) error {
		if op == "Head" || op == "Get" || op == "List" {
			return errors.New("unreachable")
		}
		return nil
	})
	if r := h.get("/v1/repos/" + repoA + "/refs"); r.status != 503 || r.json()["error"].(map[string]any)["code"] != contract.CodeStorageUnavailable || r.header.Get(contract.HeaderStale) != "" {
		t.Fatalf("the failing check: %d %s", r.status, r.body)
	}
	// Open: every read endpoint is served from the copy with the header.
	for _, path := range []string{"/refs", "/commits", "/tree/" + f.Refs["refs/heads/main"]} {
		r := h.get("/v1/repos/" + repoA + path)
		if age, err := strconv.Atoi(r.header.Get(contract.HeaderStale)); r.status != 200 || err != nil || age < 0 || age > 60 {
			t.Fatalf("%s under the open breaker: %d Origo-Stale %q", path, r.status, r.header.Get(contract.HeaderStale))
		}
	}
	// The lifecycle API reads the log itself: refused with the breaker's
	// details and Retry-After.
	status, out, header := h.doHeader("GET", "/v1/repos/"+repoA, "")
	retry, err := strconv.Atoi(header.Get("Retry-After"))
	if d := details(out); status != 503 || d["error"] != "breaker open" || d["op"] == nil || err != nil || retry < 1 || retry > 3 {
		t.Fatalf("GET under the open breaker: %d %v Retry-After %q", status, out, header.Get("Retry-After"))
	}
	// The bucket returns and the window passes: consistent again.
	h.store.SetFault(nil)
	time.Sleep(3100 * time.Millisecond)
	if r := h.get("/v1/repos/" + repoA + "/refs"); r.status != 200 || r.header.Get(contract.HeaderStale) != "" {
		t.Fatalf("after recovery: %d %q", r.status, r.header.Get(contract.HeaderStale))
	}
	// An entry the index names is gone: 503 repository_unavailable with
	// the key, and GET /v1/repos/{id}, which needs no entry, still
	// answers.
	ix, _, err := h.log.Newest(ctx, repoA, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	key := h.log.RepoPrefix(repoA) + ix.Entry
	if err := h.store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	h.cache.Evict(repoA)
	r := h.get("/v1/repos/" + repoA + "/refs")
	e, _ := r.json()["error"].(map[string]any)
	if d, _ := e["details"].(map[string]any); r.status != 503 || e["code"] != contract.CodeRepositoryUnavailable || d["key"] != key || e["message"] != contract.Sentence(contract.CodeRepositoryUnavailable) {
		t.Fatalf("missing entry: %d %s", r.status, r.body)
	}
	if status, _ := h.do("GET", "/v1/repos/"+repoA, ""); status != 200 {
		t.Fatalf("GET with the entry gone: %d", status)
	}
}

// TestWriteRefusedByTheWriteBreakerCarriesRetryAfter: a create refused
// by the write breaker answers Retry-After from that breaker's open
// window, while the read breaker stays closed. Reading the closed read
// breaker, whose window is zero, sends no header at all, which is what
// this held before the class was taken from the operation.
func TestWriteRefusedByTheWriteBreakerCarriesRetryAfter(t *testing.T) {
	var bs *wal.BreakerStore
	h := newHarness(t, withWrap(func(s wal.Store) wal.Store {
		bs = wal.NewBreakerStore(wal.BreakerOptions{Store: s, Threshold: 1, OpenFor: 30 * time.Second})
		return bs
	}))
	// Writes go away; reads keep answering, so only the write breaker
	// opens.
	h.store.SetFault(func(op, _ string) error {
		if op == "Create" || op == "Put" || op == "Delete" {
			return errors.New("unreachable")
		}
		return nil
	})
	body := `{"id":"` + repoA + `","owner":"acme","slug":"app"}`
	if status, out := h.do("POST", "/v1/repos", body); status != 503 {
		t.Fatalf("the failing create: %d %v", status, out)
	}
	if bs.Admits(wal.ClassWrite) {
		t.Fatal("the write breaker did not open")
	}
	if !bs.Admits(wal.ClassRead) {
		t.Fatal("the read breaker opened; the test needs it closed")
	}
	status, out, header := h.doHeader("POST", "/v1/repos", body)
	retry, err := strconv.Atoi(header.Get("Retry-After"))
	d := details(out)
	if status != 503 || d["error"] != "breaker open" || d["op"] != "create" {
		t.Fatalf("the refused create: %d %v", status, out)
	}
	if err != nil || retry < 1 || retry > 30 {
		t.Fatalf("Retry-After %q on a write refused by the write breaker", header.Get("Retry-After"))
	}
	// A read is unaffected and carries no Retry-After.
	if status, _, header := h.doHeader("GET", "/v1/repos/"+repoA, ""); header.Get("Retry-After") != "" {
		t.Fatalf("a read carried Retry-After: %d %q", status, header.Get("Retry-After"))
	}
}
