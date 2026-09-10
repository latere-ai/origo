// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/origocli"
)

// fake is a stand-in Origo with the shapes internal/api answers: the same
// envelopes, the same headers, the same exclusive cursors, and the same rule
// that a {sha} segment holding a slash is a refusal. A command tested against
// it is tested against the contract rather than against a mock of one.
type fake struct {
	mu sync.Mutex

	repos   []map[string]any
	refs    []map[string]any
	commits []map[string]any
	entries []map[string]any
	blobs   map[string][]byte
	diff    string

	refsTruncated    bool
	compareTruncated bool
	stale            string
	directoryCode    string
	treePage         int
	commitPage       int

	calls  []string
	bodies []map[string]any
	answer func(w http.ResponseWriter, r *http.Request) bool
}

func newFake() *fake {
	return &fake{
		repos:    []map[string]any{repoJSON("id-1", "acme", "web"), repoJSON("id-2", "acme", "api")},
		blobs:    map[string][]byte{},
		treePage: 5000, commitPage: 200,
	}
}

func repoJSON(id, owner, slug string) map[string]any {
	return map[string]any{
		"id": id, "owner": owner, "slug": slug, "default_branch": "main",
		"size_bytes": 4096, "head": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"updated_at": "2026-09-10T12:00:00Z", "pushed_at": "2026-09-10T11:00:00Z",
		"frozen_at": nil, "verified_at": nil, "verified_equal": nil,
	}
}

func (f *fake) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if r.Method == http.MethodPost {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bodies = append(f.bodies, body)
	}
	f.mu.Unlock()

	if f.answer != nil && f.answer(w, r) {
		return
	}
	if f.stale != "" {
		w.Header().Set(contract.HeaderStale, f.stale)
	}
	// The node's own segment rule, so a test fails the way the node would.
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	last := parts[len(parts)-1]
	for _, route := range []string{"commits", "tree", "blob"} {
		if len(parts) >= 4 && parts[3] == route && len(parts) == 5 && last != "-" && strings.Contains(last, "%2F") {
			f.refuse(w, 400, contract.CodeInvalid, map[string]any{"reason": "ref: the segment"})
			return
		}
	}
	switch {
	case r.URL.Path == "/v1/repos" && r.Method == http.MethodGet:
		f.collection(w, r)
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "repos" && r.Method == http.MethodGet:
		f.one(w, parts[2])
	case strings.HasSuffix(r.URL.Path, "/refs"):
		f.serveRefs(w)
	case strings.Contains(r.URL.Path, "/commits/"):
		f.serveCommit(w, r)
	case strings.HasSuffix(r.URL.Path, "/commits") && r.Method == http.MethodGet:
		f.serveCommits(w, r)
	case strings.Contains(r.URL.Path, "/tree/"):
		f.serveTree(w, r)
	case strings.Contains(r.URL.Path, "/blob/"):
		f.serveBlob(w, r)
	case strings.Contains(r.URL.Path, "/compare/"):
		f.serveCompare(w, r)
	case r.Method == http.MethodPost:
		f.serveOperation(w, r)
	default:
		f.refuse(w, 404, contract.CodeRepoNotFound, map[string]any{"id": r.URL.Path})
	}
}

func (f *fake) collection(w http.ResponseWriter, r *http.Request) {
	if owner := r.URL.Query().Get("owner"); owner != "" {
		slug := r.URL.Query().Get("slug")
		for _, repo := range f.repos {
			if repo["owner"] == owner && repo["slug"] == slug {
				writeJSON(w, 200, repo)
				return
			}
		}
		f.refuse(w, 404, contract.CodeRepoNotFound, map[string]any{"owner": owner, "slug": slug})
		return
	}
	if f.directoryCode != "" {
		f.refuse(w, contract.Status(f.directoryCode), f.directoryCode, map[string]any{"reason": "the authorizer has no directory"})
		return
	}
	// One repository per page, so the paging is exercised rather than asserted.
	cursor := r.URL.Query().Get("cursor")
	at := 0
	if cursor != "" {
		at, _ = strconv.Atoi(cursor)
	}
	body := map[string]any{"repos": []any{}, "next_cursor": nil}
	if at < len(f.repos) {
		body["repos"] = []any{f.repos[at]}
		if at+1 < len(f.repos) {
			body["next_cursor"] = strconv.Itoa(at + 1)
		}
	}
	writeJSON(w, 200, body)
}

func (f *fake) one(w http.ResponseWriter, id string) {
	for _, repo := range f.repos {
		if repo["id"] == id {
			writeJSON(w, 200, repo)
			return
		}
	}
	f.refuse(w, 404, contract.CodeRepoNotFound, map[string]any{"id": id})
}

func (f *fake) serveRefs(w http.ResponseWriter) {
	if f.refsTruncated {
		w.Header().Set("Origo-Truncated", "true")
	}
	out := f.refs
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (f *fake) serveCommit(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("ref")
	for _, c := range f.commits {
		sha, _ := c["sha"].(string)
		if sha == want || strings.HasPrefix(sha, want) {
			writeJSON(w, 200, c)
			return
		}
	}
	if want == "HEAD" && len(f.commits) > 0 {
		writeJSON(w, 200, f.commits[0])
		return
	}
	f.refuse(w, 404, contract.CodeRefNotFound, map[string]any{"ref": want})
}

func (f *fake) serveCommits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Origo-Commit", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, f.commitPage)
	at := 0
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		for i, c := range f.commits {
			if c["sha"] == cursor {
				at = i + 1 // the cursor is exclusive
				break
			}
		}
	}
	page := []any{}
	for i := at; i < len(f.commits) && len(page) < limit; i++ {
		page = append(page, f.commits[i])
	}
	body := map[string]any{"commits": page, "next_cursor": nil}
	if len(page) == limit && at+len(page) < len(f.commits) {
		body["next_cursor"] = f.commits[at+len(page)-1]["sha"]
	}
	writeJSON(w, 200, body)
}

func (f *fake) serveTree(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Origo-Commit", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	dir := r.URL.Query().Get("path")
	recursive := r.URL.Query().Get("recursive") == "1"
	var in []map[string]any
	for _, e := range f.entries {
		path, _ := e["path"].(string)
		if !strings.HasPrefix(path, prefixOf(dir)) {
			continue
		}
		rest := strings.TrimPrefix(path, prefixOf(dir))
		if !recursive && strings.Contains(rest, "/") {
			continue
		}
		in = append(in, e)
	}
	at := 0
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		for i, e := range in {
			if e["path"] == cursor {
				at = i + 1
				break
			}
		}
	}
	page := []any{}
	for i := at; i < len(in) && len(page) < f.treePage; i++ {
		page = append(page, in[i])
	}
	body := map[string]any{"entries": page, "next_cursor": nil}
	if len(page) == f.treePage && at+len(page) < len(in) {
		body["next_cursor"] = in[at+len(page)-1]["path"]
	}
	writeJSON(w, 200, body)
}

// prefixOf mirrors the node: ?path=d lists the entries under d/, never d.
func prefixOf(dir string) string {
	if dir == "" {
		return ""
	}
	return dir + "/"
}

func (f *fake) serveBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.URL.Query().Get("ref")
	body, ok := f.blobs[sha]
	if !ok {
		f.refuse(w, 404, contract.CodeRefNotFound, map[string]any{"ref": sha})
		return
	}
	w.Header().Set("Origo-Commit", sha)
	if rng := r.Header.Get("Range"); rng != "" {
		var first, last int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &first, &last); err == nil {
			last = min(last, int64(len(body))-1)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[first : last+1])
			return
		}
	}
	_, _ = w.Write(body)
}

func (f *fake) serveCompare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Origo-Commit", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if f.compareTruncated {
		w.Header().Set("Origo-Truncated", "true")
	}
	w.Header().Set("Content-Type", "text/x-diff")
	body := f.diff
	if p := r.URL.Query().Get("path"); p != "" {
		body = onlyFile(f.diff, p)
	}
	_, _ = w.Write([]byte(body))
}

// onlyFile keeps the one file's patch, the way ?path= narrows the node's own
// git diff.
func onlyFile(diff, path string) string {
	var out []string
	keep := false
	for line := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			keep = strings.Contains(line, " b/"+path)
		}
		if keep {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func (f *fake) serveOperation(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	body := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if dry, _ := body["dry_run"].(bool); dry {
		writeJSON(w, 200, map[string]any{
			"commit": "cccccccccccccccccccccccccccccccccccccccc", "branch": "refs/heads/main",
			"entry_seq": nil, "tree": "t", "committed": false,
		})
		return
	}
	writeJSON(w, 201, map[string]any{
		"commit": "cccccccccccccccccccccccccccccccccccccccc", "branch": "refs/heads/main",
		"entry_seq": 42, "tree": "t",
	})
}

// sentenceOf holds every code this stand-in refuses with, beside its sentence.
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

func (f *fake) refuse(w http.ResponseWriter, status int, code string, details map[string]any) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": sentenceOf[code], "details": details,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// result is one run of the command: what a pipeline would read, what a person
// would read, and the exit code.
type result struct {
	stdout string
	stderr string
	code   int
}

// run drives origocli.Run with an environment of its own.
func run(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := origocli.Run(context.Background(), args, func(k string) string { return env[k] }, &stdout, &stderr)
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// envFor is the four variables pointed at one stand-in.
func envFor(url string) map[string]string {
	return map[string]string{
		"ORIGO_URL": url, "ORIGO_TOKEN": "a-very-distinctive-bearer",
		"ORIGO_REPO": "id-1", "ORIGO_AUTHOR": "Ada Lovelace <ada@example.com>",
	}
}

func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// commitJSON builds one history entry.
func commitJSON(sha, name, at, message string) map[string]any {
	return map[string]any{
		"sha": sha, "parents": []string{"0000000000000000000000000000000000000000"},
		"author":    map[string]any{"name": name, "email": "a@example.com", "at": at},
		"committer": map[string]any{"name": "Origo", "email": "origo@example.com", "at": at},
		"message":   message, "trailers": []any{},
	}
}

func entryJSON(path, mode, kind string, size int64) map[string]any {
	e := map[string]any{"path": path, "mode": mode, "type": kind, "sha": "sha-" + path}
	if kind == "tree" {
		e["size"] = nil
	} else {
		e["size"] = size
	}
	return e
}
