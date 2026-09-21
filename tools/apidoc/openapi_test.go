// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/latere-ai/origo/tools/specindex/specs"
)

// document builds the document from the deck, failing the test on a deck
// the generator refuses.
func document(t *testing.T) *Document {
	t.Helper()
	doc, err := Build(index(t))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestOpenAPIDocumentIsCurrent is spec 030's first criterion: the
// committed document is what `make docs` renders from the deck, so a
// spec table changed without regenerating fails here and in the
// specindex job's diff.
func TestOpenAPIDocumentIsCurrent(t *testing.T) {
	want, err := document(t).YAML()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root(t), "api", "openapi.yaml")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("api/openapi.yaml differs from the specs; run make docs")
	}

	// A deck one row behind renders a different document, which is what
	// makes the comparison above a check and not a tautology.
	idx := index(t)
	for i, n := range idx.Names {
		if n.Kind == specs.KindEndpoint && n.Name == "GET /v1/repos/{id}" {
			idx.Names[i].Row[2] = "the representation, one day"
		}
	}
	stale, err := Build(idx)
	if err != nil {
		t.Fatal(err)
	}
	body, err := stale.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == string(got) {
		t.Error("a changed row renders the committed document")
	}
}

// TestDocumentCarriesEveryEndpoint is criterion 2: one operation per
// endpoint the deck defines and none besides, each with a stable id, a
// summary, one group, and a parameter per wildcard of its path.
func TestDocumentCarriesEveryEndpoint(t *testing.T) {
	idx := index(t)
	doc, err := Build(idx)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range idx.Names {
		if n.Kind == specs.KindEndpoint {
			want = append(want, n.Name)
		}
	}
	got := doc.Operations()
	if missing, extra := diff(want, got), diff(got, want); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("the document is missing %v and carries %v the deck does not define", missing, extra)
	}

	ids := map[string]string{}
	for path, item := range doc.Paths {
		for method, op := range item {
			where := strings.ToUpper(method) + " " + path
			switch {
			case op.OperationID == "":
				t.Errorf("%s has no operationId", where)
			case ids[op.OperationID] != "":
				t.Errorf("%s and %s share the operationId %s", ids[op.OperationID], where, op.OperationID)
			}
			ids[op.OperationID] = where
			if op.Summary == "" {
				t.Errorf("%s has no summary", where)
			}
			if len(op.Tags) != 1 || tagDescriptions[op.Tags[0]] == "" {
				t.Errorf("%s is in the groups %v", where, op.Tags)
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s answers nothing", where)
			}
			for _, m := range wildcardRe.FindAllStringSubmatch(path, -1) {
				if !hasParameter(op, m[1], "path") {
					t.Errorf("%s does not carry the path parameter %s", where, m[1])
				}
			}
		}
	}
	// The groups a reader is offered are the ones the document uses.
	used := map[string]bool{}
	for _, tag := range doc.Tags {
		used[tag.Name] = true
	}
	for _, op := range doc.Paths["/v1/repos/{id}/freeze"] {
		if !used[op.Tags[0]] {
			t.Errorf("the group %s is not declared", op.Tags[0])
		}
	}
}

// Navigation labels name actions; the description retains the complete
// endpoint row, including rows that used to live only in the summary.
func TestDocumentUsesActionLabelsAndPreservesDescriptions(t *testing.T) {
	doc := document(t)
	verbs := []string{"Get", "List", "Create", "Update", "Delete", "Restore", "Download", "Check", "Export", "Freeze", "Unfreeze", "Compact", "Import", "Merge", "Revert", "Transfer", "Verify", "Push", "Fetch", "Request", "Advertise", "Cherry-pick", "Compare"}
	for _, n := range index(t).Names {
		if n.Kind != specs.KindEndpoint {
			continue
		}
		method, path, _ := strings.Cut(n.Name, " ")
		op := doc.Paths[path][strings.ToLower(method)]
		words := strings.Fields(op.Summary)
		if len(words) < 2 || len(words) > 4 || !slices.Contains(verbs, words[0]) {
			t.Errorf("%s needs a verb-first action of two to four words, got %q", n.Name, op.Summary)
		}
		for _, cell := range n.Row[2:] {
			if !strings.Contains(op.Description, cell) {
				t.Errorf("%s description lost endpoint detail %q", n.Name, cell)
			}
		}
	}
	for _, tc := range []struct{ method, path, summary string }{
		{"get", "/v1/repos", "List repositories"},
		{"get", "/v1/repos/{id}/tree/{sha}", "List files"},
		{"post", "/v1/repos/{id}/commits", "Create commit"},
		{"post", "/v1/repos/{id}/cherry-pick", "Cherry-pick commits"},
		{"get", "/v1/repos/{id}/import", "Get import status"},
		{"post", "/v1/repos/{id}/import", "Import repository"},
		{"get", "/livez", "Check liveness"},
	} {
		if got := doc.Paths[tc.path][tc.method].Summary; got != tc.summary {
			t.Errorf("%s %s summary = %q, want %q", tc.method, tc.path, got, tc.summary)
		}
	}
}

// TestDocumentStatesTheContractVersion is criterion 4: the version is
// spec 003's contract version, and a deck that states none renders no
// document rather than a guess.
func TestDocumentStatesTheContractVersion(t *testing.T) {
	idx := index(t)
	version, err := contract(idx)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Build(idx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Info.Version != version {
		t.Errorf("the document names version %q and the deck states %q", doc.Info.Version, version)
	}
	if doc.OpenAPI != "3.1.0" || doc.Info.Title != "Origo" {
		t.Errorf("the document is %s %q", doc.OpenAPI, doc.Info.Title)
	}
	for i, n := range idx.Names {
		if n.Kind == specs.KindHeader && n.Name == "Origo-Contract" {
			idx.Names[i].Row[1] = "the contract version, on every response"
		}
	}
	if _, err := Build(idx); err == nil {
		t.Error("a deck that states no contract version renders a document anyway")
	}
}

// TestDocumentMarksWhatACallerDoesNotSend holds the two marks a consumer
// hides a group by, and the bearer scheme everything else carries.
func TestDocumentMarksWhatACallerDoesNotSend(t *testing.T) {
	doc := document(t)
	for _, tc := range []struct {
		method, path, tag, scope string
		secured                  bool
	}{
		{"get", "/readyz", "service", "operator", false},
		{"get", "/", "service", "operator", false},
		{"get", "/openapi.yaml", "service", "", false},
		{"get", "/.well-known/jwks.json", "tokens", "", false},
		{"get", "/{repo}/info/refs", "transport", "", true},
		{"post", "/{repo}/info/lfs/objects/batch", "transport", "", true},
		{"get", "/v1/repos", "repositories", "", true},
		{"get", "/v1/repos/{id}/blob/{sha}", "read", "", true},
		{"post", "/v1/repos/{id}/merge", "operations", "", true},
		{"post", "/v1/repos/{id}/gc", "administration", "", true},
	} {
		op, ok := doc.Paths[tc.path][tc.method]
		if !ok {
			t.Errorf("%s %s is not in the document", tc.method, tc.path)
			continue
		}
		if op.Tags[0] != tc.tag {
			t.Errorf("%s %s is in %s and not %s", tc.method, tc.path, op.Tags[0], tc.tag)
		}
		if op.Scope != tc.scope {
			t.Errorf("%s %s carries the scope %q", tc.method, tc.path, op.Scope)
		}
		if secured := len(op.Security) > 0; secured != tc.secured {
			t.Errorf("%s %s is secured: %v", tc.method, tc.path, secured)
		}
		if tc.secured {
			for _, status := range []string{"401", "403", "429"} {
				if _, ok := op.Responses[status]; !ok {
					t.Errorf("%s %s does not answer %s", tc.method, tc.path, status)
				}
			}
		}
	}
}

// TestDocumentReadsEachRowForItsAnswers holds the rules the operations
// are rendered by: the status a row states, the refusals it names, the
// query fields it states, and the row itself as the description.
func TestDocumentReadsEachRowForItsAnswers(t *testing.T) {
	doc := document(t)
	for _, tc := range []struct {
		method, path string
		success      []string // empty for a row that only refuses
		refusals     []string
		query        []string
	}{
		{method: "post", path: "/v1/repos", success: []string{"201"}, refusals: []string{"409", "400"}},
		{method: "delete", path: "/v1/repos/{id}", success: []string{"202"}},
		{method: "get", path: "/favicon.ico", success: []string{"204"}},
		{method: "post", path: "/{repo}/info/lfs/locks", refusals: []string{"501"}},
		{method: "get", path: "/v1/repos/{id}/blob/{sha}", success: []string{"200"}, refusals: []string{"413"}},
		{method: "get", path: "/v1/repos/{id}/commits", success: []string{"200"}, query: []string{"ref", "path", "since", "until", "limit", "cursor"}},
		{method: "get", path: "/v1/repos", success: []string{"200"}, query: []string{"cursor", "limit", "owner", "slug"}},
		// Two answers on one row: the operation commits and answers
		// 201, and a dry run answers 200 (spec 020).
		{method: "post", path: "/v1/repos/{id}/commits", success: []string{"200", "201"}, refusals: []string{"401", "403", "429"}},
		// And two on another: the node that compacted answers 200, the
		// one that scheduled 202 (spec 019).
		{method: "post", path: "/v1/repos/{id}/gc", success: []string{"200", "202"}, refusals: []string{"429"}},
	} {
		op, ok := doc.Paths[tc.path][tc.method]
		if !ok {
			t.Errorf("%s %s is not in the document", tc.method, tc.path)
			continue
		}
		for status := 200; status < 300; status++ {
			_, answered := op.Responses[strconv.Itoa(status)]
			if want := slices.Contains(tc.success, strconv.Itoa(status)); answered != want {
				t.Errorf("%s %s answers %d: %v", tc.method, tc.path, status, answered)
			}
		}
		for _, status := range tc.refusals {
			if _, ok := op.Responses[status]; !ok {
				t.Errorf("%s %s does not answer %s", tc.method, tc.path, status)
			}
		}
		for _, name := range tc.query {
			if !hasParameter(op, name, "query") {
				t.Errorf("%s %s does not carry the query field %s", tc.method, tc.path, name)
			}
		}
		if op.Description == "" {
			t.Errorf("%s %s carries no description", tc.method, tc.path)
		}
	}
	// The envelope and one code's declared response, from the deck's own
	// Code tables.
	if _, ok := doc.Components.Schemas["Error"]; !ok {
		t.Error("the document declares no error envelope")
	}
	if got := doc.Components.Responses["repo_not_found"].Description; !strings.HasPrefix(got, "404 repo_not_found: ") {
		t.Errorf("repo_not_found is declared as %q", got)
	}
	if got := doc.Components.Responses["repo_frozen"].Description; !strings.HasPrefix(got, "403 or 409 repo_frozen: ") {
		t.Errorf("repo_frozen is declared as %q", got)
	}
}

// TestADeckWithoutAGroupRendersNothing: a spec that defines a route and
// names no group is an error, so a new spec's routes are placed by a
// person rather than falling into an unnamed group.
func TestADeckWithoutAGroupRendersNothing(t *testing.T) {
	dir := t.TempDir()
	body := "---\ntitle: t\n---\n\n| Code | Status | Message |\n|---|---|---|\n| `repo_not_found` | 404 | Repository not found. |\n\n" +
		"| Method | Path | Body |\n|---|---|---|\n| GET | `/v1/nowhere` | 200 the answer |\n\n" +
		"| Header | Meaning |\n|---|---|\n| `Origo-Contract` | the contract version, `1` |\n"
	if err := os.WriteFile(filepath.Join(dir, "099-a.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := specs.Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(idx); err == nil || !strings.Contains(err.Error(), "names no group") {
		t.Errorf("a spec with no group rendered a document: %v", err)
	}
}

// Every new endpoint needs an authored action name, even when its prose
// happens to start with a short sentence.
func TestEndpointWithoutActionSummaryRendersNothing(t *testing.T) {
	idx := index(t)
	for i, n := range idx.Names {
		if n.Kind == specs.KindEndpoint && n.Name == "GET /v1/repos/{id}" {
			idx.Names[i].Name = "GET /v1/new-operation"
		}
	}
	if _, err := Build(idx); err == nil || !strings.Contains(err.Error(), "has no action summary") {
		t.Errorf("an endpoint without action metadata rendered a document: %v", err)
	}
}

// Removed routes must not leave stale naming metadata behind.
func TestActionSummariesNameOnlyDefinedEndpoints(t *testing.T) {
	routes := document(t).Operations()
	for route := range summaries {
		if !slices.Contains(routes, route) {
			t.Errorf("summary names an undefined endpoint: %s", route)
		}
	}
}

// TestOperationIDsReadAsOneWordPerSegment holds the naming a generated
// client's method names come from, including the two paths a plain
// segment rule would not survive: a wildcard beside literal text, and
// two wildcards in one segment.
func TestOperationIDsReadAsOneWordPerSegment(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/v1/repos/{id}", "getV1ReposById"},
		{"POST", "/v1/repos/{id}/cherry-pick", "postV1ReposByIdCherryPick"},
		{"GET", "/v1/repos/{id}/archive/{sha}.tar.gz", "getV1ReposByIdArchiveByShaTarGz"},
		{"GET", "/v1/repos/{id}/compare/{base}...{head}", "getV1ReposByIdCompareByBaseByHead"},
		{"GET", "/.well-known/jwks.json", "getWellKnownJwksJson"},
		{"GET", "/{repo}/info/refs", "getByRepoInfoRefs"},
		{"GET", "/", "getRoot"},
	} {
		if got := operationID(tc.method, tc.path); got != tc.want {
			t.Errorf("operationID(%s %s) = %s, want %s", tc.method, tc.path, got, tc.want)
		}
	}
}

// hasParameter reports whether the operation carries one parameter of a
// name in a place.
func hasParameter(op Operation, name, in string) bool {
	for _, p := range op.Parameters {
		if p.Name == name && p.In == in {
			return true
		}
	}
	return false
}
