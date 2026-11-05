// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"
)

var benchAt = time.Date(2026, 9, 6, 11, 52, 0, 0, time.UTC)

func benchSHA(n int) string { return fmt.Sprintf("%040x", n*7919+13) }

func benchSource(lines int) string {
	var sb strings.Builder
	for i := range lines {
		fmt.Fprintf(&sb, "func handler%04d(w http.ResponseWriter, r *http.Request) { serve(w, r, %d) }\n", i, i)
	}
	return sb.String()
}

// BenchmarkJSONCreateRequestDecode is POST /v1/repos: the 64 KiB
// bounded strict decode every small request body goes through.
func BenchmarkJSONCreateRequestDecode(b *testing.B) {
	body := []byte(`{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"acme","slug":"billing-service","default_branch":"main"}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		r := httptest.NewRequest(http.MethodPost, "/v1/repos", bytes.NewReader(body))
		var req createRequest
		if err := decode(r, &req); err != nil || req.Slug != "billing-service" {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONCommitsRequestDecode is POST /v1/repos/{id}/commits, the
// write operation: the strict decode of a request whose changes carry
// file contents.
func BenchmarkJSONCommitsRequestDecode(b *testing.B) {
	for _, s := range []struct {
		name         string
		files, lines int
	}{{"small", 1, 3}, {"medium", 4, 30}, {"large", 20, 120}} {
		b.Run(s.name, func(b *testing.B) {
			head := benchSHA(1)
			req := commitsRequest{
				Branch: "refs/heads/main", ExpectedHead: &head, Message: "billing: round invoice totals half to even",
				Author:       &identityRequest{Name: "Ada Lovelace", Email: "ada@acme.example"},
				CreateBranch: true, From: json.RawMessage(`"main"`),
			}
			for i := 0; i < s.files; i++ {
				content := benchSource(s.lines)
				req.Changes = append(req.Changes, change{Path: fmt.Sprintf("internal/billing/handler_%02d.go", i), Content: &content, Mode: "100644"})
			}
			body, err := json.Marshal(req)
			if err != nil {
				b.Fatal(err)
			}
			h := &Handler{}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/v1/repos/x/commits", bytes.NewReader(body))
				var got commitsRequest
				if !h.decodeOperation(w, r, &got) || len(got.Changes) != s.files {
					b.Fatalf("decode refused: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}

func benchCommits(n int) []commitJSON {
	out := make([]commitJSON, 0, n)
	for i := range n {
		at := benchAt.Add(-time.Duration(i) * time.Minute)
		out = append(out, commitJSON{
			SHA: benchSHA(i), Parents: []string{benchSHA(i + 1)},
			Author:    identity{Name: "Ada Lovelace", Email: "ada@acme.example", At: at},
			Committer: identity{Name: "Origo", Email: "origo@git.latere.ai", At: at},
			Message:   fmt.Sprintf("billing: round invoice totals half to even (%d)\n\nThe previous rounding drifted by one cent on large invoices.\n", i),
			Trailers:  []trailer{{Key: "Reviewed-by", Value: "Grace Hopper <grace@acme.example>"}},
			Stats:     &stats{Files: 3, Additions: 41, Deletions: 7},
		})
	}
	return out
}

// BenchmarkJSONCommitsPageEncode is GET /v1/repos/{id}/commits.
func BenchmarkJSONCommitsPageEncode(b *testing.B) {
	for _, s := range []struct {
		name string
		n    int
	}{{"small", 1}, {"medium", 22}, {"large", 430}} {
		b.Run(s.name, func(b *testing.B) {
			next := benchSHA(s.n)
			page := map[string]any{"commits": benchCommits(s.n), "next_cursor": &next}
			benchWrite(b, page)
		})
	}
}

// BenchmarkJSONTreePageEncode is GET /v1/repos/{id}/tree/{sha} with the
// recursive listing of a large repository.
func BenchmarkJSONTreePageEncode(b *testing.B) {
	for _, s := range []struct {
		name string
		n    int
	}{{"small", 5}, {"medium", 100}, {"large", 2000}} {
		b.Run(s.name, func(b *testing.B) {
			entries := make([]treeEntry, 0, s.n)
			for i := 0; i < s.n; i++ {
				size := int64(512 + i*13)
				entries = append(entries, treeEntry{Path: fmt.Sprintf("internal/billing/invoices/handler_%04d.go", i), Mode: "100644", Type: "blob", SHA: benchSHA(i), Size: &size})
			}
			last := entries[len(entries)-1].Path
			benchWrite(b, map[string]any{"entries": entries, "next_cursor": &last})
		})
	}
}

// BenchmarkJSONRefsListEncode is GET /v1/repos/{id}/refs. The node
// declares the element type inside the handler, so this mirrors it.
func BenchmarkJSONRefsListEncode(b *testing.B) {
	type ref struct {
		Name   string  `json:"name"`
		SHA    string  `json:"sha"`
		Peeled *string `json:"peeled"`
	}
	for _, s := range []struct {
		name string
		n    int
	}{{"small", 4}, {"medium", 80}, {"large", 1700}} {
		b.Run(s.name, func(b *testing.B) {
			refs := make([]ref, 0, s.n)
			for i := 0; i < s.n; i++ {
				e := ref{Name: fmt.Sprintf("refs/tags/v1.%d.0", i), SHA: benchSHA(i)}
				if i%2 == 0 {
					p := benchSHA(i + 100000)
					e.Peeled = &p
				}
				refs = append(refs, e)
			}
			benchWrite(b, refs)
		})
	}
}

// BenchmarkJSONRepositoryListEncode is GET /v1/repos, the listing page.
func BenchmarkJSONRepositoryListEncode(b *testing.B) {
	for _, s := range []struct {
		name string
		n    int
	}{{"small", 1}, {"medium", 28}, {"large", 560}} {
		b.Run(s.name, func(b *testing.B) {
			page := collectionPage{Repos: make([]Repository, 0, s.n)}
			pushed := benchAt
			for i := 0; i < s.n; i++ {
				page.Repos = append(page.Repos, Repository{
					ID: fmt.Sprintf("0f5c1d2e-3a4b-4c5d-8e6f-%012d", i), Owner: "acme", Slug: fmt.Sprintf("service-%03d", i),
					DefaultBranch: "main", SizeBytes: int64(1<<20 + i*4096), Head: benchSHA(i), UpdatedAt: benchAt, PushedAt: &pushed,
				})
			}
			cursor := page.Repos[len(page.Repos)-1].ID
			page.NextCursor = &cursor
			benchWrite(b, page)
		})
	}
}

// benchWrite runs the node's own response writer over a recorder.
func benchWrite(b *testing.B, v any) {
	b.Helper()
	probe := httptest.NewRecorder()
	httpjson.Write(probe, http.StatusOK, v)
	if probe.Code != http.StatusOK || probe.Body.Len() == 0 {
		b.Fatalf("write: %d", probe.Code)
	}
	b.ReportAllocs()
	b.SetBytes(int64(probe.Body.Len()))
	for b.Loop() {
		httpjson.Write(httptest.NewRecorder(), http.StatusOK, v)
	}
}
