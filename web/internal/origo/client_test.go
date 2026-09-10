// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// server is an installation that answers from a table and records what it was
// asked.
type server struct {
	*httptest.Server
	paths   []string
	queries []url.Values
	auth    []string
	rangeHd []string
	handler func(w http.ResponseWriter, r *http.Request)
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *server {
	t.Helper()
	s := &server{handler: h}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.paths = append(s.paths, r.URL.Path)
		s.queries = append(s.queries, r.URL.Query())
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.rangeHd = append(s.rangeHd, r.Header.Get("Range"))
		s.handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) client(t *testing.T) *Client {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return New(u, s.Client())
}

func TestReadsCarryTheTokenOnlyWhenThereIsOne(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Repo{ID: "r1", Owner: "infra", Slug: "origo"})
	})
	c := s.client(t)

	if _, _, err := c.Repo(t.Context(), "abc", "r1"); err != nil {
		t.Fatal(err)
	}
	if s.auth[0] != "Bearer abc" {
		t.Errorf("a read with a token carried %q", s.auth[0])
	}
	if _, _, err := c.Repo(t.Context(), "", "r1"); err != nil {
		t.Fatal(err)
	}
	if s.auth[1] != "" {
		t.Errorf("a read with no token carried %q; it must carry no header at all", s.auth[1])
	}
}

func TestReadsAndTheirShapes(t *testing.T) {
	at := time.Date(2026, 9, 10, 9, 41, 0, 0, time.UTC)
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Origo-Commit", "9f3c1ab")
		switch {
		case strings.HasSuffix(r.URL.Path, "/refs"):
			_ = json.NewEncoder(w).Encode([]Ref{
				{Name: "refs/tags/v1", SHA: "aaa", Peeled: "bbb"},
				{Name: "refs/heads/main", SHA: "ccc"},
			})
		case strings.Contains(r.URL.Path, "/commits/"):
			_ = json.NewEncoder(w).Encode(Commit{
				SHA: "9f3c1ab", Message: "a subject\n\na body\n",
				Author: Person{Name: "aki", At: at}, Stats: &Stats{Files: 1},
			})
		case strings.HasSuffix(r.URL.Path, "/commits"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commits": []Commit{{SHA: "9f3c1ab"}}, "next_cursor": "9f3c1ab",
			})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entries": []Entry{{Path: "a/b.go", Type: "blob", Size: 3}}, "next_cursor": "a/b.go",
			})
		case strings.Contains(r.URL.Path, "/compare/"):
			w.Header().Set("Origo-Truncated", "true")
			_, _ = w.Write([]byte("diff --git a/x b/x\n"))
		}
	})
	c := s.client(t)

	refs, meta, err := c.Refs(t.Context(), "t", "r1", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Commit != "9f3c1ab" {
		t.Errorf("Origo-Commit did not reach the caller: %q", meta.Commit)
	}
	if s.queries[0].Get("prefix") != "refs/heads/" {
		t.Errorf("the prefix was not sent: %v", s.queries[0])
	}
	if refs[0].Short() != "v1" || refs[0].Target() != "bbb" {
		t.Errorf("a tag reads as %q pointing at %q", refs[0].Short(), refs[0].Target())
	}
	if refs[1].Short() != "main" || refs[1].Target() != "ccc" {
		t.Errorf("a branch reads as %q pointing at %q", refs[1].Short(), refs[1].Target())
	}

	log, err := c.Commits(t.Context(), "t", "r1", CommitsOptions{Ref: "main", Path: "a", Limit: 50, Cursor: "z"})
	if err != nil {
		t.Fatal(err)
	}
	if log.Next != "9f3c1ab" {
		t.Errorf("the cursor did not reach the caller: %q", log.Next)
	}
	q := s.queries[len(s.queries)-1]
	for k, want := range map[string]string{"ref": "main", "path": "a", "limit": "50", "cursor": "z"} {
		if q.Get(k) != want {
			t.Errorf("%s was sent as %q, want %q", k, q.Get(k), want)
		}
	}

	commit, _, err := c.Commit(t.Context(), "t", "r1", "9f3c1ab")
	if err != nil {
		t.Fatal(err)
	}
	if commit.Subject() != "a subject" || commit.Body() != "a body" {
		t.Errorf("the message split as %q / %q", commit.Subject(), commit.Body())
	}

	tree, err := c.Tree(t.Context(), "t", "r1", "main", TreeOptions{Path: "a", Cursor: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if tree.Items[0].Name() != "b.go" || tree.Items[0].IsDir() {
		t.Errorf("an entry reads as %+v", tree.Items[0])
	}

	text, dm, err := c.Compare(t.Context(), "t", "r1", "a", "b", "one/path")
	if err != nil {
		t.Fatal(err)
	}
	if !dm.Truncated {
		t.Error("Origo-Truncated did not reach the caller")
	}
	if !strings.HasPrefix(string(text), "diff --git") {
		t.Errorf("the diff text came back as %q", text)
	}
	if got := s.paths[len(s.paths)-1]; got != "/v1/repos/r1/compare/a...b" {
		t.Errorf("compare asked for %q", got)
	}
	if s.queries[len(s.queries)-1].Get("path") != "one/path" {
		t.Error("the path was not sent")
	}
}

func TestBlobReadsAreBounded(t *testing.T) {
	body := strings.Repeat("x", 4096)
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write([]byte(body))
	})
	c := s.client(t)

	blob, err := c.BlobRange(t.Context(), "t", "r1", "sha", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if s.rangeHd[0] != "bytes=0-1023" {
		t.Errorf("the cap was not put on the request: Range %q", s.rangeHd[0])
	}
	if len(blob.Body) != 1024 {
		t.Errorf("read %d bytes past a 1024 byte cap", len(blob.Body))
	}
	if !blob.Partial || blob.ContentType != "text/plain" {
		t.Errorf("the response reads as %+v", blob)
	}

	// A stream is handed over unread, so the raw route copies it through.
	resp, err := c.BlobStream(t.Context(), "t", "r1", "sha")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the stream came back %d", resp.StatusCode)
	}
}

func TestRefusalsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		check  func(error) bool
		name   string
	}{
		{401, "unauthenticated", Unauthenticated, "a refused credential"},
		{403, "forbidden", Absent, "a refusal"},
		{404, "repo_not_found", Absent, "an absence"},
		{410, "gone", Absent, "a purged repository"},
		{413, "blob_too_large", TooLarge, "a file over the cap"},
		{503, "storage_unavailable", Unavailable, "a degraded installation"},
	} {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": tc.code, "message": "a developer sentence"},
			})
		})
		_, _, err := s.client(t).Repo(t.Context(), "t", "r1")
		if err == nil {
			t.Fatalf("%s answered no error", tc.name)
		}
		if !tc.check(err) {
			t.Errorf("%s was not classified: %v", tc.name, err)
		}
		var e *Error
		if !As(err, &e) || e.Code != tc.code || e.Status != tc.status {
			t.Errorf("%s reads as %v", tc.name, err)
		}
		// The installation's own message is for a developer reading an
		// API, and never for a screen: it is not kept.
		if strings.Contains(err.Error(), "a developer sentence") {
			t.Errorf("%s carried the installation's message into the interface", tc.name)
		}
	}
}

func TestAnUnreachableInstallationIsUnavailable(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	_, _, err := New(u, &http.Client{Timeout: time.Second}).Repo(t.Context(), "t", "r1")
	if err == nil || !Unavailable(err) {
		t.Errorf("an unreachable installation reads as %v", err)
	}
	if Absent(err) || Unauthenticated(err) {
		t.Error("an unreachable installation was read as a refusal")
	}
}

// TestListDegradesRatherThanFails asserts the three answers an installation
// without a directory can give, each of which is the same fact: this
// installation cannot enumerate repositories.
func TestListDegradesRatherThanFails(t *testing.T) {
	for _, status := range []int{http.StatusNotImplemented, http.StatusNotFound, http.StatusBadRequest, http.StatusMethodNotAllowed} {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "invalid_request"}})
		})
		_, err := s.client(t).List(t.Context(), "t", "", 50)
		if err != ErrNoDirectory {
			t.Errorf("an installation answering %d reads as %v, want no directory", status, err)
		}
	}

	// An installation that grows the collection route is read as one.
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repos": []Repo{{ID: "r1", Owner: "infra", Slug: "origo"}}, "next_cursor": "r1",
		})
	})
	page, err := s.client(t).List(t.Context(), "t", "c", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Next != "r1" {
		t.Errorf("a directory answer read as %+v", page)
	}
	if s.queries[0].Get("cursor") != "c" || s.queries[0].Get("limit") != "50" {
		t.Errorf("the page was asked for as %v", s.queries[0])
	}
}

func TestStaleIsReadFromTheHeader(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Origo-Stale", "180")
		_ = json.NewEncoder(w).Encode(Repo{ID: "r1"})
	})
	_, meta, err := s.client(t).Repo(t.Context(), "t", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.StaleSet || meta.Stale != 3*time.Minute {
		t.Errorf("the staleness reads as %v (set %v)", meta.Stale, meta.StaleSet)
	}
}

func TestShortAbbreviates(t *testing.T) {
	if got := Short("9f3c1abf20d4e7c8"); got != "9f3c1ab" {
		t.Errorf("Short is %q", got)
	}
	if got := Short("abc"); got != "abc" {
		t.Errorf("a short id was cut to %q", got)
	}
}
