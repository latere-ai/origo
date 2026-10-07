// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/origo/test/stubs/authorizer"
)

// directoryNode starts a node of env and answers its public base URL;
// the node stops with the test.
func directoryNode(t *testing.T, env map[string]string) string {
	t.Helper()
	n, stop := startNode(t, env)
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	public, _, _ := n.addrs()
	return "http://" + public
}

// directoryCall sends one request with the bearer and decodes the body.
func directoryCall(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	header := map[string]string{"Authorization": "Bearer " + token}
	if body != "" {
		header["Content-Type"] = "application/json"
	}
	resp, raw := fetchBody(t, method, url, header, body)
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return resp.StatusCode, out
}

// fetchBody is fetch with a request body.
func fetchBody(t *testing.T, method, url string, header map[string]string, body string) (*http.Response, string) {
	t.Helper()
	if body == "" {
		return fetch(t, method, url, header)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

// sharedBucketEnv is a configuration against the stubs and one bucket
// that every node built from it shares.
func sharedBucketEnv(t *testing.T) (map[string]string, *identity) {
	t.Helper()
	env, id := newEnv(t)
	bucket := s3test.New(t, "origo")
	env["ORIGO_S3_ENDPOINT"] = bucket.URL()
	env["ORIGO_S3_REGION"] = s3test.Region
	env["ORIGO_S3_KEY"], env["ORIGO_S3_SECRET"] = s3test.Key, s3test.Secret
	env["ORIGO_S3_PATH_STYLE"] = "1"
	return env, id
}

// anotherNode is env for a second node of the same installation: its own
// data directory and listeners, the same bucket, stubs and key.
func anotherNode(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	out := maps.Clone(env)
	out["ORIGO_DATA_DIR"] = t.TempDir()
	return out
}

// TestNodesSharingTheTokenKeyShareCursors: every node of an installation
// holds one ORIGO_TOKEN_KEY, so a directory cursor one node sealed opens
// on any of them and reaches the authorizer as the authorizer wrote it.
// A node holding another key, and one holding the same key and no
// authorizer, refuse it with the 400 of a cursor they did not write
// (spec 031).
func TestNodesSharingTheTokenKeyShareCursors(t *testing.T) {
	const (
		one = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
		two = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	)
	env, id := sharedBucketEnv(t)
	stranger := anotherNode(t, env)
	elsewhere, _ := newEnv(t)
	stranger["ORIGO_TOKEN_KEY"] = elsewhere["ORIGO_TOKEN_KEY"]
	owned := anotherNode(t, env)
	delete(owned, "ORIGO_AUTHORIZER_URL")
	delete(owned, "ORIGO_AUTHORIZER_TOKEN")
	a, b := directoryNode(t, env), directoryNode(t, anotherNode(t, env))
	c, d := directoryNode(t, stranger), directoryNode(t, owned)
	token := id.token()
	for _, r := range []struct{ id, slug string }{{one, "app"}, {two, "lib"}} {
		if status, out := directoryCall(t, http.MethodPost, a+"/v1/repos", token, `{"id":"`+r.id+`","owner":"acme","slug":"`+r.slug+`"}`); status != http.StatusCreated {
			t.Fatalf("create %s: %d %v", r.slug, status, out)
		}
	}
	id.authz.SetDirectory(true,
		authorizer.DirectoryEntry{ID: one, Owner: "acme", Slug: "app"},
		authorizer.DirectoryEntry{ID: two, Owner: "acme", Slug: "lib"},
	)
	status, out := directoryCall(t, http.MethodGet, a+"/v1/repos?limit=1", token, "")
	sealed, _ := out["next_cursor"].(string)
	if status != http.StatusOK || !strings.HasPrefix(sealed, "v1.") || strings.Contains(sealed, one) {
		t.Fatalf("node a's first page: %d %v", status, out)
	}

	// Both nodes holding the key open it, and the authorizer receives its
	// own cursor byte for byte.
	for name, base := range map[string]string{"a": a, "b": b} {
		id.authz.ClearRequests()
		status, out := directoryCall(t, http.MethodGet, base+"/v1/repos?limit=1&cursor="+sealed, token, "")
		repos, _ := out["repos"].([]any)
		if status != http.StatusOK || len(repos) != 1 || repos[0].(map[string]any)["id"] != two {
			t.Fatalf("node %s: %d %v", name, status, out)
		}
		if seen := id.authz.Requests(); len(seen) != 1 || seen[0].Resource.String("cursor") != one {
			t.Fatalf("node %s: the authorizer saw %+v", name, seen)
		}
	}

	// A node holding another key wrote none of it, and one holding the
	// same key with no authorizer never hands the owner policy a cursor
	// the endpoint wrote.
	id.authz.ClearRequests()
	for name, base := range map[string]string{"another key": c, "no authorizer": d} {
		status, out := directoryCall(t, http.MethodGet, base+"/v1/repos?cursor="+sealed, token, "")
		refusal, _ := out["error"].(map[string]any)
		details, _ := refusal["details"].(map[string]any)
		if status != http.StatusBadRequest || details["reason"] != "cursor" || details["field"] != "cursor" {
			t.Fatalf("the node holding %s: %d %v", name, status, out)
		}
	}
	if n := len(id.authz.Requests()); n != 0 {
		t.Fatalf("the refused cursor reached the authorizer %d times", n)
	}
}

// TestTheOwnerPolicyDirectoryWalks: with no authorizer configured, the
// owner policy's directory is sealed by the same path as an endpoint's,
// and a walk of three repositories at limit=1 returns each once and ends
// with a null cursor (spec 031).
func TestTheOwnerPolicyDirectoryWalks(t *testing.T) {
	env, id := sharedBucketEnv(t)
	delete(env, "ORIGO_AUTHORIZER_URL")
	delete(env, "ORIGO_AUTHORIZER_TOKEN")
	base := directoryNode(t, env)
	token := id.token()
	ids := []string{
		"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f",
		"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
		"2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6d",
	}
	for i, repo := range ids {
		body := `{"id":"` + repo + `","owner":"acme","slug":"repo-` + string(rune('a'+i)) + `"}`
		if status, out := directoryCall(t, http.MethodPost, base+"/v1/repos", token, body); status != http.StatusCreated {
			t.Fatalf("create %s: %d %v", repo, status, out)
		}
	}

	seen := map[string]int{}
	cursor := ""
	for page := 1; ; page++ {
		if page > len(ids) {
			t.Fatalf("the walk did not end after %d pages: %v", len(ids), seen)
		}
		query := base + "/v1/repos?limit=1"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		status, out := directoryCall(t, http.MethodGet, query, token, "")
		repos, _ := out["repos"].([]any)
		if status != http.StatusOK || len(repos) != 1 {
			t.Fatalf("page %d: %d %v", page, status, out)
		}
		row, _ := repos[0].(map[string]any)
		listed, _ := row["id"].(string)
		seen[listed]++
		if out["next_cursor"] == nil {
			break
		}
		next, _ := out["next_cursor"].(string)
		if !strings.HasPrefix(next, "v1.") || strings.Contains(next, listed) {
			t.Fatalf("page %d: next_cursor %q is not one the node sealed", page, next)
		}
		cursor = next
	}
	for _, repo := range ids {
		if seen[repo] != 1 {
			t.Errorf("%s was listed %d times: %v", repo, seen[repo], seen)
		}
	}
}
