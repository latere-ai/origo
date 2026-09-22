// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	openapi "latere.ai/x/origo/api"
)

// startDocumentNode brings a node up against the fake bucket and answers
// both listeners, since the document describes routes on each.
func startDocumentNode(t *testing.T) (public, internal string) {
	t.Helper()
	env, _ := newEnv(t)
	env["ORIGO_S3_ENDPOINT"], _ = fakeBucket(t)
	env["ORIGO_S3_PATH_STYLE"] = "1"
	n, stop := startNode(t, env)
	t.Cleanup(func() { _ = stop() })
	public, internal, _ = n.addrs()
	return public, internal
}

// TestOpenAPIDocumentIsServed is spec 030's criterion 5: the node
// answers the document to a caller with no token, as the document it
// committed and with the media type of one.
func TestOpenAPIDocumentIsServed(t *testing.T) {
	public, _ := startDocumentNode(t)
	resp, body := fetch(t, http.MethodGet, "http://"+public+"/openapi.yaml", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.yaml: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != openapi.ContentType {
		t.Errorf("content type %q", got)
	}
	// Every response of the public listener carries the contract version
	// (spec 003), this one included.
	if resp.Header.Get("Origo-Contract") == "" {
		t.Error("the document is served without the contract header")
	}
	want, err := os.ReadFile(filepath.Join(moduleRoot(t), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if body != string(want) {
		t.Error("the served document is not api/openapi.yaml")
	}
}

// TestTheDocumentsUnauthenticatedRoutesAreTheNodes is criterion 6: the
// rows the document serves without a bearer are the routes the node
// serves without one, and a route the document secures refuses an
// unauthenticated request with the contract's own code. The document is
// rendered from the specs, so this is where that reading meets the
// router.
func TestTheDocumentsUnauthenticatedRoutesAreTheNodes(t *testing.T) {
	// The two listeners of spec 002: /livez and /metrics are internal,
	// the rest of the open surface is published.
	internalOnly := []string{"GET /livez", "GET /metrics"}
	open := []string{
		"GET /", "GET /.well-known/jwks.json", "GET /favicon.ico",
		"GET /livez", "GET /metrics", "GET /openapi.yaml", "GET /readyz", "GET /version",
	}
	got := openRoutes(t)
	if !slices.Equal(got, open) {
		t.Fatalf("the document serves %v without a token, want %v", got, open)
	}

	public, internal := startDocumentNode(t)
	for _, route := range open {
		method, path, _ := strings.Cut(route, " ")
		host := public
		if slices.Contains(internalOnly, route) {
			host = internal
		}
		resp, _ := fetch(t, method, "http://"+host+path, nil)
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s answers 401 and the document serves it without a token", route)
		}
	}
	// The other side of the claim: a route the document secures asks for
	// a token, with the code of spec 003's table.
	resp, body := fetch(t, http.MethodGet, "http://"+public+"/v1/repos", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `"unauthenticated"`) {
		t.Errorf("GET /v1/repos without a token: %d %s", resp.StatusCode, body)
	}
}

// openRoutes are the operations the document marks as taking no bearer,
// read from the committed file by indentation: the paths block holds one
// path per two spaces and one method per four, and a security of no
// scheme is the empty list. The file is generated in one shape by one
// renderer, so this needs no parser.
func openRoutes(t *testing.T) []string {
	t.Helper()
	_, paths, ok := strings.Cut(string(openapi.Document), "\npaths:\n")
	if !ok {
		t.Fatal("the document has no paths block")
	}
	var out []string
	var path, method string
	for line := range strings.SplitSeq(paths, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		switch {
		case trimmed == "":
		case indent == 0:
			path, method = "", ""
		case indent == 2 && strings.HasSuffix(trimmed, ":"):
			path = strings.TrimSuffix(trimmed, ":")
		case indent == 4 && strings.HasSuffix(trimmed, ":"):
			method = strings.TrimSuffix(trimmed, ":")
		case indent == 6 && trimmed == "security: []" && path != "":
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(out)
	return out
}

// TestTheProdIngressPublishesTheDocument is criterion 7: Latere's
// overlay publishes the document at code.latere.ai, as a regular
// expression because that object turns every path into one and because
// the nginx admission webhook refuses a path holding a dot under Exact
// or Prefix. The origin's object does not grow: one probe surface per
// host (spec 029).
func TestTheProdIngressPublishesTheDocument(t *testing.T) {
	prod := manifest(t, "deploy/prod/ingress.yaml")
	if !strings.Contains(prod, `- path: /openapi\.yaml$`) {
		t.Error("deploy/prod/ingress.yaml does not publish /openapi.yaml")
	}
	if strings.Contains(manifest(t, originIngress), "openapi") {
		t.Error("the origin's Ingress claims the document; spec 029 keeps one probe surface per host")
	}
	if strings.Contains(manifest(t, baseIngress), "openapi") {
		t.Error("the base Ingress names the document; a self-hoster's overlay takes / already")
	}
}
