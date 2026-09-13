// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// call posts one contract-2 envelope and returns the status and the
// answer. The shared stub decodes it; this package's tests exercise the
// additions Origo makes over it (Origo spec 028).
func call(t *testing.T, s *authorizer.Server, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", s.URL(), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func control(t *testing.T, s *authorizer.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, s.URL()+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// probe is the reserved id every authorizer denies.
const probe = `{"subject":"alice","action":"repo.read","resource":{"kind":"Repository","id":"` + authorizer.ProbeID + `"}}`

// envelope is one contract-2 request naming a repository by id and
// owner/slug.
func envelope(subject, action string) string {
	return `{"subject":"` + subject + `","issuer":"https://iss","sub":"` + subject +
		`","action":"` + action + `","resource":{"kind":"Repository","id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"acme","slug":"app"}}`
}

func TestOwnerSlugRuleAndFigures(t *testing.T) {
	s := authorizer.New(t)
	if s.Token() != authorizer.DefaultToken {
		t.Fatalf("token %q", s.Token())
	}
	// The bearer is required, and the probe id is denied whatever the
	// rules.
	if status, _ := call(t, s, "wrong", envelope("alice", "repo.read")); status != 401 {
		t.Fatalf("wrong bearer: %d", status)
	}
	if _, out := call(t, s, s.Token(), probe); out["allow"] != false || out["reason"] == "" {
		t.Fatalf("probe not denied: %v", out)
	}
	// A rule that names the repository by owner/slug matches a request
	// that carries the id, and its figures travel under limits.
	s.Allow(authorizer.Rule{Subject: "*", Resource: "acme/app", Action: "repo.read", TTL: 5, Limits: map[string]any{"replicas": 3, "quota_bytes": 1024}})
	_, out := call(t, s, s.Token(), envelope("carol", "repo.read"))
	if out["allow"] != true || out["ttl"] != 5.0 {
		t.Fatalf("allow by owner/slug: %v", out)
	}
	limits, ok := out["limits"].(map[string]any)
	if !ok || limits["replicas"] != 3.0 || limits["quota_bytes"] != 1024.0 {
		t.Fatalf("figures under limits: %v", out)
	}
	// The request is recorded as the shared envelope, subject and
	// resource apart.
	reqs := s.Requests()
	if len(reqs) == 0 || reqs[len(reqs)-1].Subject != "carol" || reqs[len(reqs)-1].Resource.String("owner") != "acme" {
		t.Fatalf("recorded request: %+v", reqs)
	}
}

func TestDirectory(t *testing.T) {
	s := authorizer.New(t)
	list := `{"subject":"alice","issuer":"https://iss","sub":"alice","action":"repo.list","resource":{"kind":"Repository"}}`
	// With no directory set the stub answers {"directory": false}.
	if _, out := call(t, s, s.Token(), list); out["directory"] != false {
		t.Fatalf("no directory: %v", out)
	}
	// A directory of two, both readable under the default allow.
	s.SetDirectory(true,
		authorizer.DirectoryEntry{ID: "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f", Owner: "acme", Slug: "app"},
		authorizer.DirectoryEntry{ID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", Owner: "acme", Slug: "lib"},
	)
	_, out := call(t, s, s.Token(), list)
	repos, ok := out["repos"].([]any)
	if !ok || len(repos) != 2 {
		t.Fatalf("directory page: %v", out)
	}
	// A pageful cursors: a limit of one returns the first and a cursor.
	page := `{"subject":"alice","action":"repo.list","resource":{"kind":"Repository","limit":1}}`
	_, out = call(t, s, s.Token(), page)
	if repos, _ := out["repos"].([]any); len(repos) != 1 || out["next_cursor"] != "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f" {
		t.Fatalf("first page: %v", out)
	}
	// A rule that denies one repository drops it from the directory.
	s.Deny(authorizer.Rule{Subject: "alice", Resource: "acme/lib", Action: "repo.read"}, "no")
	_, out = call(t, s, s.Token(), list)
	if repos, _ := out["repos"].([]any); len(repos) != 1 {
		t.Fatalf("denied entry still listed: %v", out)
	}
	// A subject denied the list action at all is refused, not paged.
	s.Deny(authorizer.Rule{Subject: "bob", Action: "repo.list"}, "not you")
	if _, out := call(t, s, s.Token(), `{"subject":"bob","action":"repo.list","resource":{"kind":"Repository"}}`); out["allow"] != false || out["reason"] != "not you" {
		t.Fatalf("list denied: %v", out)
	}
}

func TestOutageAndControlAPI(t *testing.T) {
	s := authorizer.New(t)
	body := envelope("alice", "repo.read")
	// PUT /fail and POST /hang, POST /resume drive the outage over HTTP.
	if status, _ := control(t, s, "PUT", "/fail", `{"status":503}`); status != 204 {
		t.Fatalf("PUT /fail: %d", status)
	}
	if status, _ := call(t, s, s.Token(), body); status != 503 {
		t.Fatalf("after PUT /fail: %d", status)
	}
	if status, _ := control(t, s, "PUT", "/fail", `{"status":0}`); status != 204 {
		t.Fatalf("clear fail: %d", status)
	}
	if status, _ := control(t, s, "POST", "/hang", ""); status != 204 {
		t.Fatalf("POST /hang: %d", status)
	}
	client := &http.Client{Timeout: 200 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", s.URL(), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	if _, err := client.Do(req); err == nil {
		t.Fatal("a hung endpoint answered")
	}
	if status, _ := control(t, s, "POST", "/resume", ""); status != 204 {
		t.Fatalf("POST /resume: %d", status)
	}
	if status, out := call(t, s, s.Token(), body); status != 200 || out["allow"] != true {
		t.Fatalf("after resume: %d %v", status, out)
	}
	// PUT /directory sets the directory over HTTP.
	if status, _ := control(t, s, "PUT", "/directory", `{"supported":true,"repos":[{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"acme","slug":"app"}]}`); status != 204 {
		t.Fatalf("PUT /directory: %d", status)
	}
	if _, out := call(t, s, s.Token(), `{"subject":"alice","action":"repo.list","resource":{"kind":"Repository"}}`); out["repos"] == nil {
		t.Fatalf("directory over HTTP: %v", out)
	}
	if status, _ := control(t, s, "PUT", "/directory", `nope`); status != 400 {
		t.Fatalf("malformed directory: %d", status)
	}
	// GET and DELETE /requests list and clear the record.
	status, raw := control(t, s, "GET", "/requests", "")
	var listed []authz.Request
	if err := json.Unmarshal(raw, &listed); err != nil || status != 200 || len(listed) == 0 {
		t.Fatalf("GET /requests: %d %s %v", status, raw, err)
	}
	if status, _ := control(t, s, "DELETE", "/requests", ""); status != 204 || len(s.Requests()) != 0 {
		t.Fatalf("DELETE /requests: %d", status)
	}
}

func TestHandlerServesWithoutAListener(t *testing.T) {
	s := authorizer.NewHandler(authorizer.WithToken("x"))
	if s.Handler() == nil || s.Token() != "x" {
		t.Fatal("handler")
	}
	s.Close()
}
