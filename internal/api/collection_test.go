// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/origo/internal/auth"
	"latere.ai/x/origo/internal/contract"
	"latere.ai/x/origo/internal/placement"
	"latere.ai/x/origo/test/stubs/authorizer"
)

// makeRepo creates one repository through the route and returns nothing;
// the tests below read it back through the collection route.
func makeRepo(t *testing.T, h *harness, id, owner, slug string) {
	t.Helper()
	status, out := h.do(http.MethodPost, "/v1/repos", `{"id":"`+id+`","owner":"`+owner+`","slug":"`+slug+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("create %s: %d %v", id, status, out)
	}
}

func entries(out map[string]any) []any {
	list, _ := out["repos"].([]any)
	return list
}

// TestDirectoryServesWhatSurvives is spec 026's directory mode: one
// authorizer call, the representation of every id that still exists, the
// authorizer's next_cursor sealed by the node (spec 031), and no entry for
// an id the log dropped.
func TestDirectoryServesWhatSurvives(t *testing.T) {
	h := newHarness(t)
	makeRepo(t, h, repoA, "acme", "app")
	makeRepo(t, h, repoB, "acme", "lib")
	// repoB is deleted, so the log no longer holds it; unknown was never
	// created at all.
	if status, out := h.do(http.MethodDelete, "/v1/repos/"+repoB, ""); status != http.StatusAccepted {
		t.Fatalf("delete: %d %v", status, out)
	}
	h.authz.SetDirectory(true,
		authorizer.DirectoryEntry{ID: repoA, Owner: "acme", Slug: "app"},
		authorizer.DirectoryEntry{ID: repoB, Owner: "acme", Slug: "lib"},
		authorizer.DirectoryEntry{ID: unknown, Owner: "acme", Slug: "gone"},
	)
	h.authz.ClearRequests()

	status, out := h.do(http.MethodGet, "/v1/repos", "")
	if status != http.StatusOK {
		t.Fatalf("directory: %d %v", status, out)
	}
	list := entries(out)
	if len(list) != 1 {
		t.Fatalf("the page holds %d entries: %v", len(list), list)
	}
	row, _ := list[0].(map[string]any)
	if row["id"] != repoA || row["owner"] != "acme" || row["slug"] != "app" || row["default_branch"] != "main" {
		t.Fatalf("the entry is %v", row)
	}
	if _, ok := row["size_bytes"]; !ok {
		t.Errorf("the entry is not the repository representation: %v", row)
	}
	if out["next_cursor"] != nil {
		t.Errorf("next_cursor is %v on a last page", out["next_cursor"])
	}

	// One authorizer call for the whole page, and it named no repository.
	seen := h.authz.Requests()
	if len(seen) != 1 || seen[0].Action != "repo.list" || seen[0].Resource.ID != "" {
		t.Fatalf("the page cost %d authorizer calls: %+v", len(seen), seen)
	}

	// The response carries the authorizer's cursor sealed, never as the
	// authorizer wrote it.
	h.authz.SetDirectory(true,
		authorizer.DirectoryEntry{ID: repoA, Owner: "acme", Slug: "app"},
		authorizer.DirectoryEntry{ID: unknown, Owner: "acme", Slug: "gone"},
	)
	h.authz.ClearRequests()
	status, out = h.do(http.MethodGet, "/v1/repos?limit=1", "")
	next, _ := out["next_cursor"].(string)
	if status != http.StatusOK || !strings.HasPrefix(next, "v1.") || strings.Contains(next, repoA) {
		t.Fatalf("the paged answer is %d %v", status, out)
	}
	if len(entries(out)) != 1 {
		t.Fatalf("the first page holds %d entries", len(entries(out)))
	}
	// The second page's only id no longer exists, so the page is empty
	// and the cursor still ended: a page may be shorter than the
	// authorizer's without paging losing its place.
	status, out = h.do(http.MethodGet, "/v1/repos?cursor="+next, "")
	if status != http.StatusOK || len(entries(out)) != 0 || out["next_cursor"] != nil {
		t.Fatalf("the second page is %d %v", status, out)
	}
	// One authorizer call a page, and the second carried the authorizer's
	// cursor as it wrote it.
	if seen = h.authz.Requests(); len(seen) != 2 || seen[1].Resource.String("cursor") != repoA {
		t.Fatalf("the two pages cost %+v", seen)
	}
}

// TestDirectoryRefusals: the three things that are not a page.
func TestDirectoryRefusals(t *testing.T) {
	h := newHarness(t)

	// The authorizer has no directory: one 501 of its own, distinct from
	// the 503 of an outage, so a consumer stops asking.
	status, out := h.do(http.MethodGet, "/v1/repos", "")
	if status != http.StatusNotImplemented || code(out) != contract.CodeDirectoryUnsupported {
		t.Fatalf("no directory: %d %v", status, out)
	}
	if got := out["error"].(map[string]any)["message"]; got != contract.Sentence(contract.CodeDirectoryUnsupported) {
		t.Errorf("the sentence is %v", got)
	}

	// A deny is 403 with the authorizer's reason.
	h.authz.SetDirectory(true)
	h.authz.Deny(authorizer.Rule{Subject: "alice", Action: "repo.list"}, "not_a_member")
	status, out = h.do(http.MethodGet, "/v1/repos", "")
	if status != http.StatusForbidden || code(out) != contract.CodeForbidden {
		t.Fatalf("a denied directory: %d %v", status, out)
	}
	if d := details(out); d["reason"] != "not_a_member" || d["action"] != "repo.list" {
		t.Fatalf("the refusal details are %v", d)
	}

	// An outage is the same 503 every other route answers, never a 501.
	h.authz.Fail(http.StatusInternalServerError)
	status, out = h.do(http.MethodGet, "/v1/repos", "")
	if status != http.StatusServiceUnavailable || code(out) != contract.CodeAuthorizerUnavailable {
		t.Fatalf("an authorizer outage: %d %v", status, out)
	}
	h.authz.Resume()
}

// TestDirectoryDenyIsTheAuthorizersOwn checks the deny path answers from
// a {"allow": false} directory answer and not from the rule table alone.
func TestDirectoryDenyIsTheAuthorizersOwn(t *testing.T) {
	h := newHarness(t)
	h.authz.SetDirectory(true, authorizer.DirectoryEntry{ID: repoA, Owner: "acme", Slug: "app"})
	// A bound token never reaches the authorizer at all.
	h.as(auth.Principal{Subject: "builder", Bound: &auth.Bound{Repo: repoA, Scope: auth.ScopeRead}})
	h.authz.ClearRequests()
	status, out := h.do(http.MethodGet, "/v1/repos", "")
	if status != http.StatusForbidden || code(out) != contract.CodeForbidden {
		t.Fatalf("a bound token's directory: %d %v", status, out)
	}
	if d := details(out); d["reason"] != auth.ReasonScope {
		t.Fatalf("the reason is %v", details(out))
	}
	if len(h.authz.Requests()) != 0 {
		t.Errorf("a bound token reached the authorizer")
	}
}

// TestNameModeMatchesTheIdRoute is spec 026's name mode: the same body
// as the id route, and spec 007's authorization before lookup kept, so a
// refused caller cannot tell an existing name from a free one.
func TestNameModeMatchesTheIdRoute(t *testing.T) {
	h := newHarness(t, withPlacement(placement.NewSet("n1", nil)))
	makeRepo(t, h, repoA, "acme", "app")

	status, byName, hdr := h.doHeader(http.MethodGet, "/v1/repos?owner=acme&slug=app", "")
	if status != http.StatusOK {
		t.Fatalf("the name mode: %d %v", status, byName)
	}
	_, byID := h.do(http.MethodGet, "/v1/repos/"+repoA, "")
	for _, field := range []string{"id", "owner", "slug", "default_branch", "head", "size_bytes", "updated_at"} {
		if byName[field] != byID[field] {
			t.Errorf("%s: name mode %v, id route %v", field, byName[field], byID[field])
		}
	}
	if hdr.Get("Origo-Prefer") == "" {
		t.Error("an allowed name mode carries no Origo-Prefer")
	}

	// An allowed caller learns that a name is free.
	status, out := h.do(http.MethodGet, "/v1/repos?owner=acme&slug=nothing", "")
	if status != http.StatusNotFound || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("a free name: %d %v", status, out)
	}

	// A denied caller learns nothing: the same 403 for both, and no
	// Origo-Prefer on either, whether or not the name resolved.
	h.authz.Deny(authorizer.Rule{Subject: "mallory"}, "insufficient_role")
	h.as(auth.Principal{Subject: "mallory"})
	h.authz.ClearRequests()
	for _, name := range []string{"acme&slug=app", "acme&slug=nothing"} {
		status, out, hdr := h.doHeader(http.MethodGet, "/v1/repos?owner="+name, "")
		if status != http.StatusForbidden || code(out) != contract.CodeForbidden {
			t.Fatalf("%s: %d %v", name, status, out)
		}
		if hdr.Get("Origo-Prefer") != "" {
			t.Errorf("%s: a refused name mode carries Origo-Prefer", name)
		}
	}
	// The authorizer saw the owner and the slug both times, before any
	// store read could have told the caller anything.
	seen := h.authz.Requests()
	if len(seen) != 2 {
		t.Fatalf("the two refusals cost %d authorizer calls", len(seen))
	}
	if seen[0].Resource.String("owner") != "acme" || seen[0].Resource.ID != repoA || seen[1].Resource.ID != "" {
		t.Fatalf("the authorizer saw %+v", seen)
	}
}

// TestCollectionQueryIsValidated: the two modes are exclusive and each
// is bounded, and a cursor this installation did not write is refused
// before the authorizer is called (spec 031).
func TestCollectionQueryIsValidated(t *testing.T) {
	h := newHarness(t)
	sealed, err := h.cursors.Seal("alice", repoA)
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "v1."))
	if err != nil {
		t.Fatal(err)
	}
	flipped[len(flipped)/2] ^= 0x01
	for _, row := range []struct{ query, reason, field string }{
		{"?owner=acme", "owner and slug are given together", "slug"},
		{"?slug=app", "owner and slug are given together", "owner"},
		{"?owner=acme&slug=app&cursor=x", "modes", ""},
		{"?owner=acme&slug=app&limit=10", "modes", ""},
		{"?limit=0", "limit", "limit"},
		{"?limit=201", "limit", "limit"},
		{"?limit=many", "limit", "limit"},
		// A cursor without the sealed prefix, one that is not base64url,
		// one shorter than a nonce and a tag, one with a byte flipped, one
		// longer than the longest cursor this installation writes, and a
		// bare repository id, which is what an authorizer pages by.
		{"?cursor=" + strings.TrimPrefix(sealed, "v1."), "cursor", "cursor"},
		{"?cursor=v1.not*base64url", "cursor", "cursor"},
		{"?cursor=v1." + base64.RawURLEncoding.EncodeToString(make([]byte, 27)), "cursor", "cursor"},
		{"?cursor=v1." + base64.RawURLEncoding.EncodeToString(flipped), "cursor", "cursor"},
		{"?cursor=v1." + strings.Repeat("A", 721), "cursor", "cursor"},
		{"?cursor=" + strings.Repeat("A", 724), "cursor", "cursor"},
		{"?cursor=" + repoA, "cursor", "cursor"},
	} {
		h.authz.ClearRequests()
		status, out := h.do(http.MethodGet, "/v1/repos"+row.query, "")
		if status != http.StatusBadRequest || code(out) != contract.CodeInvalid {
			t.Errorf("%s: %d %v", row.query, status, out)
			continue
		}
		d := details(out)
		if row.reason == "modes" || row.reason == "limit" || row.reason == "cursor" {
			if d["reason"] != row.reason {
				t.Errorf("%s: reason %v, want %q", row.query, d["reason"], row.reason)
			}
		}
		if row.field != "" && d["field"] != row.field {
			t.Errorf("%s: field %v, want %q", row.query, d["field"], row.field)
		}
		if n := len(h.authz.Requests()); n != 0 {
			t.Errorf("%s: the authorizer was called %d times", row.query, n)
		}
	}
	// A limit inside the bounds is not a refusal.
	h.authz.SetDirectory(true)
	if status, out := h.do(http.MethodGet, "/v1/repos?limit=200", ""); status != http.StatusOK {
		t.Errorf("limit=200: %d %v", status, out)
	}
}

// sealedFor seals the authorizer's cursor for the subject under the
// harness's key, the cursor a node of the next release hands out.
func sealedFor(t *testing.T, c *auth.Cursors, subject, cursor string) string {
	t.Helper()
	out, err := c.Seal(subject, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestADirectoryCursorOpensForItsSubjectAndAuthorizerAlone: a sealed
// cursor opens only on a request from the subject it was sealed for, and
// only on a node asking the authorizer it was sealed under. Anything else
// is the 400 of a cursor this installation did not write, never a 503,
// and the authorizer is not called.
func TestADirectoryCursorOpensForItsSubjectAndAuthorizerAlone(t *testing.T) {
	h := newHarness(t)
	makeRepo(t, h, repoA, "acme", "app")
	makeRepo(t, h, repoB, "acme", "lib")
	h.authz.SetDirectory(true,
		authorizer.DirectoryEntry{ID: repoA, Owner: "acme", Slug: "app"},
		authorizer.DirectoryEntry{ID: repoB, Owner: "acme", Slug: "lib"},
	)
	another, err := auth.NewCursors(h.key, "http://another-authorizer.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	none, err := auth.NewCursors(h.key, "")
	if err != nil {
		t.Fatal(err)
	}
	// The cursor the node handed bob.
	h.as(auth.Principal{Subject: "bob"})
	status, out := h.do(http.MethodGet, "/v1/repos?limit=1", "")
	forBob, _ := out["next_cursor"].(string)
	if status != http.StatusOK || forBob == "" {
		t.Fatalf("bob's first page: %d %v", status, out)
	}
	h.as(auth.Principal{Subject: "alice"})
	refused := func(h *harness, name, cursor string) {
		t.Helper()
		h.authz.ClearRequests()
		status, out := h.do(http.MethodGet, "/v1/repos?cursor="+cursor, "")
		if status != http.StatusBadRequest || code(out) != contract.CodeInvalid {
			t.Fatalf("%s: %d %v", name, status, out)
		}
		if d := details(out); d["reason"] != "cursor" || d["field"] != "cursor" {
			t.Fatalf("%s: the details are %v", name, d)
		}
		if n := len(h.authz.Requests()); n != 0 {
			t.Fatalf("%s: the authorizer was called %d times", name, n)
		}
	}
	refused(h, "another subject", forBob)
	refused(h, "another authorizer", sealedFor(t, another, "alice", repoA))
	refused(h, "no authorizer", sealedFor(t, none, "alice", repoA))

	// The subject it was sealed for opens it.
	h.as(auth.Principal{Subject: "bob"})
	if status, out := h.do(http.MethodGet, "/v1/repos?cursor="+forBob, ""); status != http.StatusOK || len(entries(out)) != 1 {
		t.Fatalf("bob's own cursor: %d %v", status, out)
	}

	// A node bound to no authorizer, the owner policy's, refuses a cursor
	// sealed under one with the same key.
	bare := newHarness(t, withCursorAuthorizer(""))
	stubs, err := auth.NewCursors(bare.key, bare.authz.URL())
	if err != nil {
		t.Fatal(err)
	}
	bare.authz.SetDirectory(true)
	refused(bare, "a node holding none", sealedFor(t, stubs, "alice", repoA))
}

// TestTheDirectoryCursorCarriesNoLimit: a caller may change limit between
// pages, as the agent client does, so the cursor carries a position and
// nothing else. A walk at 1, then 2, then 1 returns each repository once
// and ends with a null cursor.
func TestTheDirectoryCursorCarriesNoLimit(t *testing.T) {
	const (
		repoC = "2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6d"
		repoD = "3c4d5e6f-7081-4a9b-8c0d-1e2f3a4b5c6d"
	)
	h := newHarness(t)
	var directory []authorizer.DirectoryEntry
	for i, id := range []string{repoA, repoB, repoC, repoD} {
		slug := "repo-" + string(rune('a'+i))
		makeRepo(t, h, id, "acme", slug)
		directory = append(directory, authorizer.DirectoryEntry{ID: id, Owner: "acme", Slug: slug})
	}
	h.authz.SetDirectory(true, directory...)

	seen := map[string]int{}
	cursor := ""
	for i, limit := range []int{1, 2, 1} {
		query := "/v1/repos?limit=" + strconv.Itoa(limit)
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		status, out := h.do(http.MethodGet, query, "")
		if status != http.StatusOK || len(entries(out)) != limit {
			t.Fatalf("page %d at limit %d: %d %v", i+1, limit, status, out)
		}
		for _, e := range entries(out) {
			row, _ := e.(map[string]any)
			id, _ := row["id"].(string)
			seen[id]++
		}
		next, _ := out["next_cursor"].(string)
		if i < 2 && !strings.HasPrefix(next, "v1.") {
			t.Fatalf("page %d: next_cursor %v is not one the node sealed", i+1, out["next_cursor"])
		}
		if i == 2 && out["next_cursor"] != nil {
			t.Fatalf("the last page carries next_cursor %v", out["next_cursor"])
		}
		cursor = next
	}
	for _, e := range directory {
		if seen[e.ID] != 1 {
			t.Errorf("%s was listed %d times: %v", e.ID, seen[e.ID], seen)
		}
	}
}
