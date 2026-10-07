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

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/origo/internal/auth"
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
// holds one ORIGO_TOKEN_KEY, so a directory cursor sealed under it opens
// on any of them and reaches the authorizer as the authorizer wrote it,
// and a node holding another key refuses it with the 400 of a cursor it
// did not write (spec 031).
func TestNodesSharingTheTokenKeyShareCursors(t *testing.T) {
	const (
		one = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
		two = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	)
	env, id := sharedBucketEnv(t)
	stranger := anotherNode(t, env)
	elsewhere, _ := newEnv(t)
	stranger["ORIGO_TOKEN_KEY"] = elsewhere["ORIGO_TOKEN_KEY"]
	a, b, c := directoryNode(t, env), directoryNode(t, anotherNode(t, env)), directoryNode(t, stranger)
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
	cursors, err := auth.NewCursors(id.key, env["ORIGO_AUTHORIZER_URL"])
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cursors.Seal(authz.Subject(id.issuer.URL(), "dev"), one)
	if err != nil {
		t.Fatal(err)
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

	// A node holding another key wrote none of it.
	id.authz.ClearRequests()
	status, out := directoryCall(t, http.MethodGet, c+"/v1/repos?cursor="+sealed, token, "")
	refusal, _ := out["error"].(map[string]any)
	details, _ := refusal["details"].(map[string]any)
	if status != http.StatusBadRequest || details["reason"] != "cursor" || details["field"] != "cursor" {
		t.Fatalf("the node holding another key: %d %v", status, out)
	}
	if n := len(id.authz.Requests()); n != 0 {
		t.Fatalf("the refused cursor reached the authorizer %d times", n)
	}
}
