// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"latere.ai/x/pkg/httpjson"

	versionpkg "github.com/latere-ai/origo/internal/version"
)

// landingFacts are what spec 022 says the page carries, in either form.
var landingFacts = []string{
	"Origo",
	"git remote, not a website",
	"git clone https://{host}/{owner}/{slug}.git",
	"/r/{id}.git",
	"bearer token",
	"https://github.com/latere-ai/origo",
}

// startLandingNode brings a node up against the fake bucket and answers
// its public address.
func startLandingNode(t *testing.T, env map[string]string) string {
	t.Helper()
	env["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
	env["ORIGO_S3_PATH_STYLE"] = "1"
	n, stop := startNode(t, env)
	t.Cleanup(func() { _ = stop() })
	public, _, _ := n.addrs()
	return public
}

// fetch performs one request against the public listener and answers the
// response with its body read.
func fetch(t *testing.T, method, url string, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
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

// TestLandingPageAnswersTheRoot is spec 022's first criterion: a person
// who opens the root without a token reads a page rather than being
// asked for credentials that cannot exist. curl sends no Accept, so the
// plain text form is what a terminal gets.
func TestLandingPageAnswersTheRoot(t *testing.T) {
	public := startLandingNode(t, testEnv(t))
	resp, body := fetch(t, "GET", "http://"+public+"/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("GET / asks for credentials: %q", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "Accept" {
		t.Errorf("Vary = %q", got)
	}
	if got := resp.Header.Get("Origo-Contract"); got != "1" {
		t.Errorf("Origo-Contract = %q", got)
	}
	for _, fact := range append(landingFacts, versionpkg.Version) {
		if !strings.Contains(body, fact) {
			t.Errorf("the text page does not carry %q:\n%s", fact, body)
		}
	}
	if strings.ContainsAny(body, "<>") {
		t.Errorf("the text page carries markup:\n%s", body)
	}
	// The page is the same document for every visitor: a second request
	// with a different Host and query answers byte for byte.
	resp2, body2 := fetch(t, "GET", "http://"+public+"/?q=x", map[string]string{"Host": "evil.example"})
	if resp2.StatusCode != 200 || body2 != body {
		t.Errorf("the page varies with the request: %d", resp2.StatusCode)
	}
}

// TestLandingPageNegotiatesHTML is spec 022's second criterion: a browser
// asks for text/html and reads the same facts as markup, with the
// stylesheet and the icon inline and nothing fetched from anywhere.
func TestLandingPageNegotiatesHTML(t *testing.T) {
	public := startLandingNode(t, testEnv(t))
	accept := "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	resp, body := fetch(t, "GET", "http://"+public+"/", map[string]string{"Accept": accept})
	if resp.StatusCode != 200 {
		t.Fatalf("GET /: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "Accept" {
		t.Errorf("Vary = %q", got)
	}
	for _, fact := range append(landingFacts, versionpkg.Version) {
		if !strings.Contains(body, fact) {
			t.Errorf("the HTML page does not carry %q", fact)
		}
	}
	for _, want := range []string{"<!doctype html>", "<h1>Origo</h1>", "<style>", `<link rel="icon" href="data:,">`} {
		if !strings.Contains(body, want) {
			t.Errorf("the HTML page does not carry %q", want)
		}
	}
	// Nothing is fetched: no script at all, no src attribute, and every
	// href is the icon or the project.
	for _, forbidden := range []string{"<script", " src=", "@import", "<iframe"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the HTML page carries %q", forbidden)
		}
	}
	for _, m := range regexp.MustCompile(`href="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if m[1] != "data:," && m[1] != projectURL {
			t.Errorf("the HTML page links to %q", m[1])
		}
	}
	// Any other Accept is the text form.
	if _, plain := fetch(t, "GET", "http://"+public+"/", map[string]string{"Accept": "application/json"}); strings.Contains(plain, "<h1>") {
		t.Error("Accept: application/json answered HTML")
	}
}

// TestLandingPageShadowsNoRoute is spec 022's shadowing criterion. With
// the page registered, the git routes in both URL forms and the API
// still reach their own handlers: each answers the contract's error
// envelope for a repository the fake bucket does not hold, which only
// the handler behind the verifier produces, and none of them answers the
// page. The root answers the page for the same caller, and a POST of the
// root falls through to the unknown-route handler, so the page takes the
// one method and the one path it is registered for.
func TestLandingPageShadowsNoRoute(t *testing.T) {
	env, id := newEnv(t)
	public := startLandingNode(t, env)
	const repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	bearer := map[string]string{"Authorization": "Bearer " + id.token()}
	for _, path := range []string{
		"/v1/repos/" + repoA,
		"/r/" + repoA + ".git/info/refs?service=git-upload-pack",
		"/acme/app.git/info/refs?service=git-upload-pack",
	} {
		resp, body := fetch(t, "GET", "http://"+public+path, bearer)
		var env httpjson.ErrorEnvelope
		_ = json.Unmarshal([]byte(body), &env)
		if resp.StatusCode == 200 || env.Error.Code == "" {
			t.Errorf("%s did not reach its handler: %d %s", path, resp.StatusCode, body)
		}
		if strings.Contains(body, "not a website") {
			t.Errorf("%s answered the landing page", path)
		}
	}
	if resp, body := fetch(t, "GET", "http://"+public+"/", bearer); resp.StatusCode != 200 || !strings.Contains(body, "not a website") {
		t.Errorf("GET / with a token: %d %s", resp.StatusCode, body)
	}
	resp, body := fetch(t, "POST", "http://"+public+"/", bearer)
	var envelope httpjson.ErrorEnvelope
	_ = json.Unmarshal([]byte(body), &envelope)
	if resp.StatusCode != 400 || envelope.Error.Details["reason"] != "no such route" {
		t.Errorf("POST / : %d %s", resp.StatusCode, body)
	}
}

// TestLandingPageLeaksNoConfiguration is spec 022's no-leak criterion.
// The page is readable by anyone who can reach the installation, so no
// value the node was configured with may appear in either form of it.
func TestLandingPageLeaksNoConfiguration(t *testing.T) {
	env := testEnv(t)
	env["ORIGO_S3_BUCKET"] = "origo-private-bucket"
	env["ORIGO_S3_KEY"] = "landing-access-key"
	env["ORIGO_S3_SECRET"] = "landing-secret-value"
	env["ORIGO_NODE_NAME"] = "origod-landing-7"
	n, stop := func() (*node, func() error) {
		env["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
		env["ORIGO_S3_PATH_STYLE"] = "1"
		return startNode(t, env)
	}()
	defer func() { _ = stop() }()
	public, internal, gossip := n.addrs()
	_, text := fetch(t, "GET", "http://"+public+"/", nil)
	_, html := fetch(t, "GET", "http://"+public+"/", map[string]string{"Accept": "text/html"})
	secrets := []string{internal, gossip}
	for name, value := range env {
		if len(value) >= 8 {
			secrets = append(secrets, value)
		} else {
			t.Logf("%s is too short to search for: %q", name, value)
		}
	}
	for _, body := range []string{text, html} {
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("the page carries the configured value %q", secret)
			}
		}
	}
}

// TestFaviconAnswersNoContent is spec 022's favicon criterion: the
// request a browser makes on its own after it renders the page is
// answered without a token, so the credential dialog the page exists to
// remove does not arrive a moment behind it.
func TestFaviconAnswersNoContent(t *testing.T) {
	public := startLandingNode(t, testEnv(t))
	resp, body := fetch(t, "GET", "http://"+public+"/favicon.ico", nil)
	if resp.StatusCode != http.StatusNoContent || body != "" {
		t.Fatalf("GET /favicon.ico: %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("GET /favicon.ico asks for credentials: %q", got)
	}
}
