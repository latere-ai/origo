// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origoclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/origoclient"
)

// recorder is a stand-in Origo: it records every request and answers what the
// test told it to. It stands where a node would, so a test proves the wire and
// not a mock of it.
type recorder struct {
	mu      sync.Mutex
	seen    []*http.Request
	targets []string
	handler http.HandlerFunc
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	rec.seen = append(rec.seen, r)
	rec.targets = append(rec.targets, r.URL.RequestURI())
	rec.mu.Unlock()
	rec.handler(w, r)
}

func (rec *recorder) calls() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.targets...)
}

func serve(t *testing.T, h http.HandlerFunc) (*origoclient.Client, *recorder) {
	t.Helper()
	rec := &recorder{handler: h}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := origoclient.New(origoclient.Config{
		URL: srv.URL + "/", Token: "bearer-value",
		Sleep: func(time.Duration) {},
	})
	return c, rec
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sentenceOf holds every code these tests refuse with, beside its sentence.
//
// It exists because spec 021's code table is checked at the call site: every
// contract.Sentence call names a contract.Code* constant, so a sentence keeps
// exactly one owner and a call passing a variable could name any of them. A
// stand-in that answered contract.Sentence(code) for whatever code it was
// handed would be that call.
var sentenceOf = map[string]string{
	contract.CodeInvalid:               contract.Sentence(contract.CodeInvalid),
	contract.CodeUnauthenticated:       contract.Sentence(contract.CodeUnauthenticated),
	contract.CodeForbidden:             contract.Sentence(contract.CodeForbidden),
	contract.CodeRepoNotFound:          contract.Sentence(contract.CodeRepoNotFound),
	contract.CodeRefNotFound:           contract.Sentence(contract.CodeRefNotFound),
	contract.CodeNonFastForward:        contract.Sentence(contract.CodeNonFastForward),
	contract.CodeMergeConflict:         contract.Sentence(contract.CodeMergeConflict),
	contract.CodeInvalidChange:         contract.Sentence(contract.CodeInvalidChange),
	contract.CodeOverQuota:             contract.Sentence(contract.CodeOverQuota),
	contract.CodeRateLimited:           contract.Sentence(contract.CodeRateLimited),
	contract.CodeStorageUnavailable:    contract.Sentence(contract.CodeStorageUnavailable),
	contract.CodeRepositoryUnavailable: contract.Sentence(contract.CodeRepositoryUnavailable),
	contract.CodeAuthorizerUnavailable: contract.Sentence(contract.CodeAuthorizerUnavailable),
	contract.CodeBlobTooLarge:          contract.Sentence(contract.CodeBlobTooLarge),
	contract.CodeOperationTimeout:      contract.Sentence(contract.CodeOperationTimeout),
	contract.CodeDirectoryUnsupported:  contract.Sentence(contract.CodeDirectoryUnsupported),
	contract.CodeRepoFrozen:            contract.Sentence(contract.CodeRepoFrozen),
	contract.CodeGone:                  contract.Sentence(contract.CodeGone),
}

func sentence(t *testing.T, code string) string {
	t.Helper()
	s, ok := sentenceOf[code]
	if !ok {
		t.Fatalf("%s is not in sentenceOf; add it with its constant", code)
	}
	return s
}

func refuse(w http.ResponseWriter, status int, code string, details map[string]any) {
	body := map[string]any{"error": map[string]any{
		"code": code, "message": sentenceOf[code], "details": details,
	}}
	writeJSON(w, status, body)
}

// TestEveryRouteSendsThePlaceholderSegment is the first acceptance criterion.
// internal/api refuses any {sha} segment holding a slash, so a client that put
// a reference name there would answer 400 on every branch named feature/x.
func TestEveryRouteSendsThePlaceholderSegment(t *testing.T) {
	const ref = "feature/x"
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// The node's own rule, enforced here so the test fails the way the
		// node would: a segment that is not the placeholder and holds a
		// slash, or a query name beside a real segment, is 400.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		last := parts[len(parts)-1]
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/repos/r/commits/"), strings.HasPrefix(r.URL.Path, "/v1/repos/r/tree/"),
			strings.HasPrefix(r.URL.Path, "/v1/repos/r/blob/"):
			if last != origoclient.Placeholder {
				refuse(w, 400, contract.CodeInvalid, map[string]any{"reason": "ref: the segment"})
				return
			}
		case strings.HasPrefix(r.URL.Path, "/v1/repos/r/compare/"):
			if last != origoclient.Placeholder+"..."+origoclient.Placeholder {
				refuse(w, 400, contract.CodeInvalid, map[string]any{"reason": "ref: the segment"})
				return
			}
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/repos/r/tree/"):
			writeJSON(w, 200, map[string]any{"entries": []any{}, "next_cursor": nil})
		case strings.HasPrefix(r.URL.Path, "/v1/repos/r/blob/"):
			_, _ = w.Write([]byte("bytes"))
		case strings.HasPrefix(r.URL.Path, "/v1/repos/r/compare/"):
			_, _ = w.Write([]byte("diff"))
		case strings.HasPrefix(r.URL.Path, "/v1/repos/r/commits/"):
			writeJSON(w, 200, map[string]any{"sha": "abc", "parents": []string{}})
		default:
			writeJSON(w, 200, map[string]any{"commits": []any{}, "next_cursor": nil})
		}
	})
	ctx := t.Context()
	if _, err := c.Commit(ctx, "r", ref); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := c.Tree(ctx, "r", origoclient.TreeOptions{Ref: ref}); err != nil {
		t.Fatalf("tree: %v", err)
	}
	if _, err := c.Blob(ctx, "r", ref, nil); err != nil {
		t.Fatalf("blob: %v", err)
	}
	if _, err := c.Compare(ctx, "r", ref, "main", ""); err != nil {
		t.Fatalf("compare: %v", err)
	}
	if _, err := c.Commits(ctx, "r", origoclient.CommitOptions{Ref: ref}); err != nil {
		t.Fatalf("commits: %v", err)
	}
	// Assert positively, route by route: the name is in the query and the
	// segment is the placeholder. Filtering and checking what survives would
	// pass a request that sent the name in neither.
	want := []struct{ path, query string }{
		{"/v1/repos/r/commits/-", "ref=feature%2Fx"},
		{"/v1/repos/r/tree/-", "ref=feature%2Fx"},
		{"/v1/repos/r/blob/-", "ref=feature%2Fx"},
		{"/v1/repos/r/compare/-...-", "base=feature%2Fx"},
		{"/v1/repos/r/commits", "ref=feature%2Fx"},
	}
	calls := rec.calls()
	if len(calls) != len(want) {
		t.Fatalf("%d calls, want %d: %v", len(calls), len(want), calls)
	}
	for i, w := range want {
		path, query, _ := strings.Cut(calls[i], "?")
		if path != w.path {
			t.Fatalf("call %d went to %q, want %q", i, path, w.path)
		}
		if !strings.Contains(query, w.query) {
			t.Fatalf("call %d query %q does not carry %q", i, query, w.query)
		}
		if strings.Contains(path, "feature") {
			t.Fatalf("a reference reached a path segment: %s", calls[i])
		}
	}
}

// TestRecursiveListingPagesThroughTheWholeTree is the paging the documented
// pipeline stands on. The cursor of a recursive listing is the last full path
// served, so a second page continues rather than restarting.
func TestRecursiveListingPagesThroughTheWholeTree(t *testing.T) {
	pages := [][]string{
		{"a/one.go", "a/two.go"},
		{"b/three.go", "b/four.go"},
		{"c/five.go"},
	}
	var asked []string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "1" {
			t.Errorf("the listing was not recursive: %s", r.URL.RawQuery)
		}
		cursor := r.URL.Query().Get("cursor")
		asked = append(asked, cursor)
		n := 0
		for i, p := range pages {
			if i > 0 && cursor == p[len(p)-1] {
				n = i + 1
			}
		}
		if cursor != "" && n == 0 {
			for i, p := range pages {
				if cursor == p[len(p)-1] {
					n = i + 1
				}
			}
		}
		entries := []any{}
		for _, path := range pages[n] {
			entries = append(entries, map[string]any{"path": path, "mode": "100644", "type": "blob", "sha": "x", "size": 1})
		}
		body := map[string]any{"entries": entries, "next_cursor": nil}
		if n+1 < len(pages) {
			body["next_cursor"] = pages[n][len(pages[n])-1]
		}
		writeJSON(w, 200, body)
	})
	var got []string
	opt := origoclient.TreeOptions{Ref: "main", Recursive: true}
	for {
		page, err := c.Tree(t.Context(), "r", opt)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			got = append(got, e.Path)
		}
		if page.NextCursor == nil {
			break
		}
		opt.Cursor = *page.NextCursor
	}
	var flat []string
	for _, p := range pages {
		flat = append(flat, p...)
	}
	if strings.Join(got, ",") != strings.Join(flat, ",") {
		t.Fatalf("walked %v, want %v", got, flat)
	}
	if len(asked) != len(pages) {
		t.Fatalf("%d requests, want %d", len(asked), len(pages))
	}
	if asked[0] != "" || asked[1] != "a/two.go" || asked[2] != "b/four.go" {
		t.Fatalf("the cursors handed back were %v", asked)
	}
}

// TestFileResolvesThroughTheParentDirectory holds fact 2: ?path= lists a
// directory, so a file is found by listing its parent and matching the
// basename, paged until the basename is seen.
func TestFileResolvesThroughTheParentDirectory(t *testing.T) {
	size := int64(4096)
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "internal/api" {
			t.Errorf("the listing was not of the parent directory: %q", r.URL.Query().Get("path"))
		}
		if r.URL.Query().Get("cursor") == "" {
			next := "internal/api/page1"
			writeJSON(w, 200, map[string]any{
				"entries":     []any{map[string]any{"path": "internal/api/admin.go", "mode": "100644", "type": "blob", "sha": "aa", "size": 1}},
				"next_cursor": next,
			})
			return
		}
		writeJSON(w, 200, map[string]any{
			"entries": []any{
				map[string]any{"path": "internal/api/read.go", "mode": "100644", "type": "blob", "sha": "bb", "size": size},
			},
			"next_cursor": nil,
		})
	})
	e, err := c.File(t.Context(), "r", "main", "internal/api/read.go")
	if err != nil {
		t.Fatal(err)
	}
	if e.SHA != "bb" || e.Size == nil || *e.Size != size {
		t.Fatalf("entry: %+v", e)
	}
	if n := len(rec.calls()); n != 2 {
		t.Fatalf("resolution took %d calls, want 2 (one page, then the cursor)", n)
	}
	for _, got := range rec.calls() {
		if strings.Contains(got, "read.go") {
			t.Fatalf("the file path reached ?path=: %s", got)
		}
	}
}

func TestFileMissingIsRefNotFoundAndReadsNoBlob(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blob/") {
			t.Error("a blob was read for a path that does not resolve")
		}
		writeJSON(w, 200, map[string]any{"entries": []any{}, "next_cursor": nil})
	})
	_, err := c.File(t.Context(), "r", "main", "internal/api/missing.go")
	ref, ok := origoclient.AsRefusal(err)
	if !ok || ref.Code != contract.CodeRefNotFound {
		t.Fatalf("err = %v, want ref_not_found", err)
	}
	if v, _ := ref.Detail("ref"); v != "internal/api/missing.go" {
		t.Fatalf("details: %+v", ref.Details)
	}
	if n := len(rec.calls()); n != 1 {
		t.Fatalf("%d calls, want 1", n)
	}
}

func TestFileAtTheRootAndAnEmptyBasename(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Query().Get("path"); p != "" {
			t.Errorf("a root path listed %q", p)
		}
		writeJSON(w, 200, map[string]any{
			"entries":     []any{map[string]any{"path": "go.mod", "mode": "100644", "type": "blob", "sha": "cc", "size": 9}},
			"next_cursor": nil,
		})
	})
	if _, err := c.File(t.Context(), "r", "main", "./go.mod"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.File(t.Context(), "r", "main", "internal/"); err == nil {
		t.Fatal("a path with no basename resolved")
	}
}

// TestBlobRangeAvoidsTheBlobLimit: the node measures the requested length, so
// a caller that knows the size never provokes blob_too_large.
func TestBlobRangeAvoidsTheBlobLimit(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			refuse(w, 413, contract.CodeBlobTooLarge, map[string]any{"size": 60 << 20, "max": origoclient.MaxBlobBytes})
			return
		}
		w.Header().Set(origoclient.HeaderCommit, "blobsha")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("window"))
	})
	b, err := c.Blob(t.Context(), "r", "blobsha", &origoclient.ByteRange{First: 0, Last: 5})
	if err != nil {
		t.Fatal(err)
	}
	if string(b.Bytes) != "window" || b.Meta.Commit != "blobsha" {
		t.Fatalf("blob: %+v", b)
	}
	if !strings.Contains(rec.calls()[0], "ref=blobsha") {
		t.Fatalf("the object id did not travel in the query: %s", rec.calls()[0])
	}
	if _, err := c.Blob(t.Context(), "r", "blobsha", nil); err == nil {
		t.Fatal("a whole read of an oversized blob was served")
	}
}

// TestShortRetryAfterIsWaitedOnce: five seconds or less is slept through here,
// because the caller's next turn costs more; a longer wait is returned.
func TestShortRetryAfterIsWaitedOnce(t *testing.T) {
	var slept []time.Duration
	var n int
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "3")
			w.Header().Set(contract.HeaderRateLimit, "60")
			refuse(w, 429, contract.CodeRateLimited, map[string]any{"limit": "repository"})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "r", "owner": "o", "slug": "s"})
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := origoclient.New(origoclient.Config{URL: srv.URL, Token: "t", Sleep: func(d time.Duration) { slept = append(slept, d) }})
	repo, err := c.Repository(t.Context(), "r")
	if err != nil {
		t.Fatalf("the short wait was not retried: %v", err)
	}
	if repo.ID != "r" || len(slept) != 1 || slept[0] != 3*time.Second {
		t.Fatalf("repo %+v slept %v", repo, slept)
	}
	if n != 2 {
		t.Fatalf("%d calls, want exactly 2: once, never twice", n)
	}
}

func TestLongRetryAfterIsReturnedWithTheLimitInForce(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set(contract.HeaderRateLimit, "120")
		refuse(w, 429, contract.CodeRateLimited, map[string]any{"limit": "repository"})
	})
	_, err := c.Repository(t.Context(), "r")
	ref, ok := origoclient.AsRefusal(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if ref.RetryAfter != "60" || ref.RateLimit != "120" || ref.Code != contract.CodeRateLimited {
		t.Fatalf("refusal: %+v", ref)
	}
	if ref.Error() != "429 rate_limited" {
		t.Fatalf("Error() = %q, want the developer register", ref.Error())
	}
}

func TestRetryAfterThatIsNotAWaitIsNotSleptThrough(t *testing.T) {
	for _, header := range []string{"", "0", "later"} {
		c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			refuse(w, 503, contract.CodeStorageUnavailable, nil)
		})
		if _, err := c.Repository(t.Context(), "r"); err == nil {
			t.Fatalf("Retry-After %q was served", header)
		}
	}
}

// TestRefusalsDecodeWithTheirDetails walks the error table: every code becomes
// a typed refusal carrying the code, the sentence and the details, and nothing
// is rendered into a line here.
func TestRefusalsDecodeWithTheirDetails(t *testing.T) {
	cases := []struct {
		status  int
		code    string
		details map[string]any
	}{
		{409, contract.CodeNonFastForward, map[string]any{"expected": "aaa", "actual": "bbb"}},
		{409, contract.CodeMergeConflict, map[string]any{"commit": "c", "paths": []any{"a", "b"}}},
		{400, contract.CodeInvalidChange, map[string]any{"index": float64(2), "reason": "path"}},
		{413, contract.CodeOverQuota, map[string]any{"limit": "repository", "bytes": float64(1), "max": float64(2)}},
		{404, contract.CodeRefNotFound, map[string]any{"ref": "main"}},
		{404, contract.CodeRepoNotFound, map[string]any{"id": "r"}},
		{403, contract.CodeRepoFrozen, map[string]any{"frozen_at": "now"}},
		{410, contract.CodeGone, map[string]any{"id": "r"}},
		{401, contract.CodeUnauthenticated, map[string]any{"reason": "expired"}},
		{403, contract.CodeForbidden, map[string]any{"action": "write", "reason": "no"}},
		{501, contract.CodeDirectoryUnsupported, map[string]any{"reason": "the authorizer has no directory"}},
		{504, contract.CodeOperationTimeout, map[string]any{"operation": "merge", "budget_seconds": float64(300)}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
				refuse(w, tc.status, tc.code, tc.details)
			})
			_, err := c.Repository(t.Context(), "r")
			ref, ok := origoclient.AsRefusal(err)
			if !ok {
				t.Fatalf("err = %v", err)
			}
			if ref.Code != tc.code || ref.Status != tc.status || ref.Message != sentence(t, tc.code) {
				t.Fatalf("refusal: %+v", ref)
			}
			for k, want := range tc.details {
				got, ok := ref.Detail(k)
				if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("detail %q = %v (%v), want %v", k, got, ok, want)
				}
			}
			if _, ok := ref.Detail("absent"); ok {
				t.Fatal("an absent detail reported present")
			}
		})
	}
}

func TestABodyThatIsNotTheEnvelopeIsStillARefusal(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>a proxy</html>"))
	})
	_, err := c.Repository(t.Context(), "r")
	ref, ok := origoclient.AsRefusal(err)
	if !ok || ref.Code != "unexpected_status" || !strings.Contains(ref.Message, "502") {
		t.Fatalf("refusal: %+v (%v)", ref, err)
	}
	if ref.Details == nil {
		t.Fatal("Details is nil, so a caller reading it panics")
	}
}

// TestClientCannotMintOrAdminister is the write-safety property asserted over
// the package rather than left to review: no method reaches the token route or
// any administration route of spec 019.
func TestClientCannotMintOrAdminister(t *testing.T) {
	forbidden := []string{"/tokens", "/freeze", "/unfreeze", "/transfer", "/gc",
		"/import", "/export.bundle", "/undelete", "/verify", "/stats", "/archive/"}
	var reached []string
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		for _, f := range forbidden {
			if strings.Contains(r.URL.Path, f) {
				reached = append(reached, r.URL.Path)
			}
		}
		if r.Method == http.MethodPost {
			writeJSON(w, 201, map[string]any{"commit": "c", "branch": "refs/heads/main", "tree": "t"})
			return
		}
		if strings.Contains(r.URL.Path, "/refs") {
			writeJSON(w, 200, []any{})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "r", "entries": []any{}, "commits": []any{}, "repos": []any{}})
	})
	ctx := t.Context()
	exercise(ctx, t, c)
	if len(reached) > 0 {
		t.Fatalf("the client reached an admin route: %v", reached)
	}
	for _, got := range rec.calls() {
		if strings.Contains(got, "token") {
			t.Fatalf("the client called a token route: %s", got)
		}
	}
}

// exercise calls every exported method once, which is what makes the two
// assertions above cover the surface rather than a sample of it.
func exercise(ctx context.Context, t *testing.T, c *origoclient.Client) {
	t.Helper()
	_, _ = c.Directory(ctx, origoclient.DirectoryOptions{Cursor: "c", Limit: 10})
	_, _ = c.Resolve(ctx, "o", "s")
	_, _ = c.Repository(ctx, "r")
	_, _ = c.Refs(ctx, "r", "refs/heads/")
	_, _ = c.Commits(ctx, "r", origoclient.CommitOptions{Ref: "main", Path: "p", Since: "s", Until: "u", Cursor: "cur", Limit: 20})
	_, _ = c.Commit(ctx, "r", "main")
	_, _ = c.Expand(ctx, "r", "abcdefg")
	_, _ = c.Compare(ctx, "r", "a", "b", "p")
	_, _ = c.Tree(ctx, "r", origoclient.TreeOptions{Ref: "main", Path: "d", Cursor: "cur", Recursive: true})
	_, _ = c.Blob(ctx, "r", "sha", nil)
	head := "abc"
	common := origoclient.Common{Branch: "main", ExpectedHead: &head, Author: &origoclient.Author{Name: "N", Email: "n@e"}}
	_, _ = c.CreateCommit(ctx, "r", origoclient.CommitRequest{Common: common, Changes: []origoclient.Change{{Path: "p", Content: []byte("x")}}})
	_, _ = c.Merge(ctx, "r", origoclient.MergeRequest{Common: common, Source: "b"})
	_, _ = c.CherryPick(ctx, "r", origoclient.PickRequest{Common: common, Commits: []string{"a"}})
	_, _ = c.Revert(ctx, "r", origoclient.PickRequest{Common: common, Commits: []string{"a"}})
}

func TestReadRoutesCarryTheirQueryAndMeta(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(origoclient.HeaderCommit, "headsha")
		w.Header().Set(origoclient.HeaderTruncated, "true")
		w.Header().Set(contract.HeaderStale, "12")
		switch {
		case strings.HasSuffix(r.URL.Path, "/refs"):
			writeJSON(w, 200, []map[string]any{{"name": "refs/heads/main", "sha": "aa", "peeled": nil}})
		case strings.Contains(r.URL.Path, "/tree/"):
			writeJSON(w, 200, map[string]any{"entries": []any{}, "next_cursor": "last"})
		case strings.Contains(r.URL.Path, "/compare/"):
			_, _ = w.Write([]byte("diff --git a/x b/x\n"))
		case strings.Contains(r.URL.Path, "/commits"):
			writeJSON(w, 200, map[string]any{"commits": []any{}, "next_cursor": "cur"})
		default:
			writeJSON(w, 200, map[string]any{"repos": []any{}, "next_cursor": nil})
		}
	})
	ctx := t.Context()
	refs, err := c.Refs(ctx, "r", "refs/tags/")
	if err != nil || len(refs.Refs) != 1 || !refs.Meta.Truncated || refs.Meta.Stale != "12" {
		t.Fatalf("refs %+v err %v", refs, err)
	}
	tree, err := c.Tree(ctx, "r", origoclient.TreeOptions{Ref: "main", Path: "d", Cursor: "c", Recursive: true})
	if err != nil || tree.NextCursor == nil || *tree.NextCursor != "last" {
		t.Fatalf("tree %+v err %v", tree, err)
	}
	cmp, err := c.Compare(ctx, "r", "a", "b", "p")
	if err != nil || !strings.HasPrefix(string(cmp.Diff), "diff --git") || cmp.Meta.Commit != "headsha" {
		t.Fatalf("compare %+v err %v", cmp, err)
	}
	page, err := c.Commits(ctx, "r", origoclient.CommitOptions{Ref: "main", Path: "p", Since: "s", Until: "u", Cursor: "k", Limit: 20})
	if err != nil || page.NextCursor == nil {
		t.Fatalf("commits %+v err %v", page, err)
	}
	dir, err := c.Directory(ctx, origoclient.DirectoryOptions{Cursor: "c", Limit: 5})
	if err != nil || dir.NextCursor != nil {
		t.Fatalf("directory %+v err %v", dir, err)
	}
	want := []string{"prefix=refs%2Ftags%2F", "recursive=1", "path=p", "since=s", "limit=20", "limit=5", "cursor=c"}
	all := strings.Join(rec.calls(), "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Fatalf("%q missing from:\n%s", w, all)
		}
	}
}

func TestResolveAndRepositoryReadTheSameRepresentation(t *testing.T) {
	body := map[string]any{"id": "id-1", "owner": "o", "slug": "s", "default_branch": "main", "head": "abc", "size_bytes": 12}
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, body) })
	byName, err := c.Resolve(t.Context(), "o", "s")
	if err != nil || byName.ID != "id-1" || byName.DefaultBranch != "main" {
		t.Fatalf("resolve %+v err %v", byName, err)
	}
	byID, err := c.Repository(t.Context(), "id-1")
	if err != nil || byID != byName {
		t.Fatalf("the two modes answered differently: %+v %+v", byID, byName)
	}
	if got := rec.calls()[0]; !strings.Contains(got, "owner=o") || !strings.Contains(got, "slug=s") {
		t.Fatalf("name mode query: %s", got)
	}
}

func TestOperationsSendTheFlatBodyAndReadTheThreeStateReceipt(t *testing.T) {
	var bodies []map[string]any
	var paths []string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		paths = append(paths, r.URL.Path)
		if dry, _ := body["dry_run"].(bool); dry {
			writeJSON(w, 200, map[string]any{"commit": "c", "branch": "refs/heads/main", "entry_seq": nil, "tree": "t", "committed": false})
			return
		}
		writeJSON(w, 201, map[string]any{"commit": "c", "branch": "refs/heads/main", "entry_seq": 7, "tree": "t"})
	})
	head := "abcdef0123456789abcdef0123456789abcdef01"
	common := origoclient.Common{Branch: "main", ExpectedHead: &head, Author: &origoclient.Author{Name: "N", Email: "n@example.com"}}
	ctx := t.Context()

	got, err := c.CreateCommit(ctx, "r", origoclient.CommitRequest{
		Common:  common,
		Changes: []origoclient.Change{{Path: "a.txt", Content: []byte("hello"), Mode: "100644"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != 201 || got.Committed != nil || got.EntrySeq == nil || *got.EntrySeq != 7 || !got.Landed() {
		t.Fatalf("a real write read as %+v; committed is absent, never true", got)
	}
	// The common fields are flat, and content is base64 the client made.
	first := bodies[0]
	if first["branch"] != "main" || first["expected_head"] != head {
		t.Fatalf("the common fields did not flatten: %v", first)
	}
	author, _ := first["author"].(map[string]any)
	if author["name"] != "N" || author["email"] != "n@example.com" {
		t.Fatalf("author: %v", first["author"])
	}
	changes, _ := first["changes"].([]any)
	change, _ := changes[0].(map[string]any)
	if change["content"] != "aGVsbG8=" {
		t.Fatalf("content was not base64 encoded by the client: %v", change["content"])
	}

	dry := common
	dry.DryRun = true
	plan, err := c.Merge(ctx, "r", origoclient.MergeRequest{Common: dry, Source: "topic", Strategy: origoclient.FastForwardOnly})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != 200 || plan.Committed == nil || *plan.Committed || plan.Landed() || plan.EntrySeq != nil {
		t.Fatalf("a dry run read as %+v", plan)
	}
	if _, err := c.CherryPick(ctx, "r", origoclient.PickRequest{Common: common, Commits: []string{"a"}, Mainline: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Revert(ctx, "r", origoclient.PickRequest{Common: common, Commits: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/v1/repos/r/commits", "/v1/repos/r/merge", "/v1/repos/r/cherry-pick", "/v1/repos/r/revert"}
	for i, p := range want {
		if paths[i] != p {
			t.Fatalf("call %d went to %s, want %s", i, paths[i], p)
		}
	}
}

func TestExpandTurnsAShortIdIntoTheWholeOne(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") == "deadbee" {
			writeJSON(w, 200, map[string]any{"sha": "deadbeef00000000000000000000000000000000", "parents": []string{}})
			return
		}
		refuse(w, 404, contract.CodeRefNotFound, map[string]any{"ref": r.URL.Query().Get("ref")})
	})
	sha, err := c.Expand(t.Context(), "r", "deadbee")
	if err != nil || sha != "deadbeef00000000000000000000000000000000" {
		t.Fatalf("sha %q err %v", sha, err)
	}
	if _, err := c.Expand(t.Context(), "r", "0000000"); err == nil {
		t.Fatal("a short id that does not resolve expanded")
	}
	if len(rec.calls()) != 2 {
		t.Fatalf("calls: %v", rec.calls())
	}
}

func TestTheTokenTravelsOnlyInTheAuthorizationHeader(t *testing.T) {
	const token = "a-very-distinctive-bearer"
	var seen []string
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		writeJSON(w, 200, map[string]any{"id": "r"})
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := origoclient.New(origoclient.Config{URL: srv.URL, Token: token})
	if _, err := c.Repository(t.Context(), "r"); err != nil {
		t.Fatal(err)
	}
	for _, got := range seen {
		if strings.Contains(got, token) {
			t.Fatalf("the bearer reached a URL: %s", got)
		}
	}
}

func TestAnUnreachableInstallationNamesNoURL(t *testing.T) {
	c := origoclient.New(origoclient.Config{URL: "http://127.0.0.1:1", Token: "t"})
	_, err := c.Repository(t.Context(), "r")
	var u *origoclient.Unreachable
	if !errors.As(err, &u) {
		t.Fatalf("err = %#v, want Unreachable", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1:1/v1") {
		t.Fatalf("the URL reached the error: %v", err)
	}
	if u.Unwrap() == nil {
		t.Fatal("Unreachable wraps nothing")
	}
}

func TestATimeoutIsAnOperationTimeoutNamingTheRoute(t *testing.T) {
	block := make(chan struct{})
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) { <-block }}
	srv := httptest.NewServer(rec)
	defer func() { close(block); srv.Close() }()
	c := origoclient.New(origoclient.Config{URL: srv.URL, Token: "t", Timeout: 20 * time.Millisecond})
	_, err := c.Tree(t.Context(), "r", origoclient.TreeOptions{Ref: "main"})
	ref, ok := origoclient.AsRefusal(err)
	if !ok || ref.Code != contract.CodeOperationTimeout {
		t.Fatalf("err = %v, want operation_timeout", err)
	}
	if v, _ := ref.Detail("operation"); v != "tree" {
		t.Fatalf("operation = %v, want the route", v)
	}
	if v, _ := ref.Detail("budget_seconds"); v == nil {
		t.Fatal("the budget is not named")
	}
}

func TestABodyThatIsNotWhatItClaimsIsUnreachableAndNotAServedAnswer(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	})
	_, err := c.Repository(t.Context(), "r")
	var u *origoclient.Unreachable
	if !errors.As(err, &u) || u.Op != "decode" {
		t.Fatalf("err = %#v", err)
	}
}

func TestNewFillsItsDefaultsAndTrimsTheURL(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": "r"})
	})
	if _, err := c.Repository(t.Context(), "r"); err != nil {
		t.Fatal(err)
	}
	if got := rec.calls()[0]; got != "/v1/repos/r" {
		t.Fatalf("a trailing slash survived: %q", got)
	}
	bare := origoclient.New(origoclient.Config{URL: "https://example.invalid", Token: "t"})
	if bare == nil {
		t.Fatal("New returned nil")
	}
}

func TestAnIdWithASlashCannotEscapeItsSegment(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": "x"})
	})
	if _, err := c.Repository(t.Context(), "../../etc"); err != nil {
		t.Fatal(err)
	}
	if got := rec.calls()[0]; strings.Contains(got, "/etc") {
		t.Fatalf("the id escaped its segment: %q", got)
	}
}

// TestAChangeSaysWhichKindItIs covers the hand-written encoding: presence, not
// length, decides the content key, so an empty file is a change with zero
// bytes rather than a change with nothing in it.
func TestAChangeSaysWhichKindItIs(t *testing.T) {
	cases := []struct {
		name   string
		change origoclient.Change
		want   map[string]any
	}{
		{"text", origoclient.Change{Path: "a", Content: []byte("hi"), Mode: "100644"},
			map[string]any{"path": "a", "content": "aGk=", "mode": "100644"}},
		{"empty file", origoclient.Change{Path: "a", Content: []byte{}},
			map[string]any{"path": "a", "content": ""}},
		{"delete", origoclient.Change{Path: "a", Delete: true},
			map[string]any{"path": "a", "delete": true}},
		{"content ref", origoclient.Change{Path: "a", ContentRef: "abc"},
			map[string]any{"path": "a", "content_ref": "abc"}},
		{"executable", origoclient.Change{Path: "a", Content: nil, Mode: "100755"},
			map[string]any{"path": "a", "mode": "100755"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.change)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEveryReadRouteReturnsItsRefusal: a refusal on any route reaches the
// caller as a Refusal and never as a zero value with a nil error.
func TestEveryReadRouteReturnsItsRefusal(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		refuse(w, 403, contract.CodeForbidden, map[string]any{"action": "read"})
	})
	ctx := t.Context()
	calls := map[string]func() error{
		"directory": func() error { _, err := c.Directory(ctx, origoclient.DirectoryOptions{}); return err },
		"resolve":   func() error { _, err := c.Resolve(ctx, "o", "s"); return err },
		"repo":      func() error { _, err := c.Repository(ctx, "r"); return err },
		"refs":      func() error { _, err := c.Refs(ctx, "r", ""); return err },
		"commits":   func() error { _, err := c.Commits(ctx, "r", origoclient.CommitOptions{}); return err },
		"commit":    func() error { _, err := c.Commit(ctx, "r", "main"); return err },
		"compare":   func() error { _, err := c.Compare(ctx, "r", "a", "b", ""); return err },
		"tree":      func() error { _, err := c.Tree(ctx, "r", origoclient.TreeOptions{Ref: "main"}); return err },
		"blob":      func() error { _, err := c.Blob(ctx, "r", "sha", nil); return err },
		"blob range": func() error {
			_, err := c.Blob(ctx, "r", "sha", &origoclient.ByteRange{Last: 1})
			return err
		},
		"file":   func() error { _, err := c.File(ctx, "r", "main", "a/b.go"); return err },
		"expand": func() error { _, err := c.Expand(ctx, "r", "abc"); return err },
	}
	for name, call := range calls {
		ref, ok := origoclient.AsRefusal(call())
		if !ok || ref.Code != contract.CodeForbidden {
			t.Fatalf("%s did not return the refusal", name)
		}
	}
}

// TestEveryWriteRouteReturnsItsRefusal is the same over the four operations.
func TestEveryWriteRouteReturnsItsRefusal(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		refuse(w, 409, contract.CodeNonFastForward, map[string]any{"expected": "a", "actual": "b"})
	})
	ctx := t.Context()
	head := "abc"
	common := origoclient.Common{Branch: "main", ExpectedHead: &head, Author: &origoclient.Author{Name: "N", Email: "n@e.com"}}
	calls := map[string]func() error{
		"commit": func() error {
			_, err := c.CreateCommit(ctx, "r", origoclient.CommitRequest{Common: common, Changes: []origoclient.Change{{Path: "a", Delete: true}}})
			return err
		},
		"merge": func() error {
			_, err := c.Merge(ctx, "r", origoclient.MergeRequest{Common: common, Source: "b"})
			return err
		},
		"cherry-pick": func() error {
			_, err := c.CherryPick(ctx, "r", origoclient.PickRequest{Common: common, Commits: []string{"a"}})
			return err
		},
		"revert": func() error {
			_, err := c.Revert(ctx, "r", origoclient.PickRequest{Common: common, Commits: []string{"a"}})
			return err
		},
	}
	for name, call := range calls {
		ref, ok := origoclient.AsRefusal(call())
		if !ok || ref.Code != contract.CodeNonFastForward {
			t.Fatalf("%s did not return the refusal", name)
		}
	}
}

// TestAServedAnswerThatCannotBeDecodedIsNotReadAsAWrite closes the operate
// path: a 201 whose body is not a receipt is a failure, not an empty receipt.
func TestAServedAnswerThatCannotBeDecodedIsNotReadAsAWrite(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte("not a receipt"))
	})
	got, err := c.Merge(t.Context(), "r", origoclient.MergeRequest{Branch: "main", Source: "b"})
	if _, ok := errors.AsType[*origoclient.Unreachable](err); !ok {
		t.Fatalf("err = %#v", err)
	}
	if got.Commit != "" {
		t.Fatalf("receipt: %+v", got)
	}
}
