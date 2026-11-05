// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origoclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/origo/internal/origoclient"
)

const benchRepo = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

var benchAt = time.Date(2026, 9, 6, 11, 52, 0, 0, time.UTC)

func benchSHA(n int) string { return fmt.Sprintf("%040x", n*7919+13) }

// benchTransport answers every request in process: it reads the whole
// request body, as a node does, and replies with the canned body.
type benchTransport struct {
	status  int
	reply   []byte
	sawBody int
}

func (t *benchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return nil, err
		}
		t.sawBody = int(n)
		_ = r.Body.Close()
	}
	return &http.Response{
		StatusCode: t.status, Status: http.StatusText(t.status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:  http.Header{"Content-Type": []string{"application/json"}},
		Body:    io.NopCloser(bytes.NewReader(t.reply)),
		Request: r,
	}, nil
}

func benchClient(t *benchTransport) *origoclient.Client {
	return origoclient.New(origoclient.Config{
		URL: "http://origo.invalid", Token: "bearer-value", HTTP: &http.Client{Transport: t},
		Sleep: func(time.Duration) {},
	})
}

// BenchmarkJSONCommitRequestMarshal is the CLI and agent write path:
// CreateCommit encodes the request with its hand-written Change encoder
// and decodes the receipt.
func BenchmarkJSONCommitRequestMarshal(b *testing.B) {
	for _, s := range []struct {
		name         string
		files, lines int
	}{{"small", 1, 3}, {"medium", 4, 30}, {"large", 20, 120}} {
		b.Run(s.name, func(b *testing.B) {
			var src strings.Builder
			for i := 0; i < s.lines; i++ {
				fmt.Fprintf(&src, "func handler%04d(w http.ResponseWriter, r *http.Request) { serve(w, r, %d) }\n", i, i)
			}
			head := benchSHA(1)
			req := origoclient.CommitRequest{
				Branch: "refs/heads/main", ExpectedHead: &head, Message: "billing: round invoice totals half to even",
				Author: &origoclient.Author{Name: "Ada Lovelace", Email: "ada@acme.example"},
			}
			for i := 0; i < s.files; i++ {
				req.Changes = append(req.Changes, origoclient.Change{Path: fmt.Sprintf("internal/billing/handler_%02d.go", i), Content: []byte(src.String()), Mode: "100644"})
			}
			tr := &benchTransport{status: http.StatusCreated, reply: []byte(`{"commit":"` + benchSHA(2) + `","branch":"refs/heads/main","entry_seq":1043,"tree":"` + benchSHA(3) + `"}`)}
			c := benchClient(tr)
			ctx := context.Background()
			if _, err := c.CreateCommit(ctx, benchRepo, req); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(tr.sawBody))
			for b.Loop() {
				if _, err := c.CreateCommit(ctx, benchRepo, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONCommitPageUnmarshal is the read path: Commits decodes a
// commit listing page into the client's types.
func BenchmarkJSONCommitPageUnmarshal(b *testing.B) {
	for _, s := range []struct {
		name string
		n    int
	}{{"small", 1}, {"medium", 22}, {"large", 430}} {
		b.Run(s.name, func(b *testing.B) {
			next := benchSHA(s.n)
			page := origoclient.CommitPage{NextCursor: &next}
			for i := 0; i < s.n; i++ {
				at := benchAt.Add(-time.Duration(i) * time.Minute)
				page.Commits = append(page.Commits, origoclient.Commit{
					SHA: benchSHA(i), Parents: []string{benchSHA(i + 1)},
					Author:    origoclient.Identity{Name: "Ada Lovelace", Email: "ada@acme.example", At: at},
					Committer: origoclient.Identity{Name: "Origo", Email: "origo@git.latere.ai", At: at},
					Message:   fmt.Sprintf("billing: round invoice totals half to even (%d)\n\nThe previous rounding drifted by one cent on large invoices.\n", i),
					Trailers:  []origoclient.Trailer{{Key: "Reviewed-by", Value: "Grace Hopper <grace@acme.example>"}},
					Stats:     &origoclient.Stats{Files: 3, Additions: 41, Deletions: 7},
				})
			}
			body, err := json.Marshal(page)
			if err != nil {
				b.Fatal(err)
			}
			c := benchClient(&benchTransport{status: http.StatusOK, reply: body})
			ctx := context.Background()
			if got, err := c.Commits(ctx, benchRepo, origoclient.CommitOptions{}); err != nil || len(got.Commits) != s.n {
				b.Fatalf("%d commits: %v", len(got.Commits), err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, err := c.Commits(ctx, benchRepo, origoclient.CommitOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
