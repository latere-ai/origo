// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package openapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/origo/internal/contract"
)

// moduleRoot is the checkout, resolved from this file rather than from
// the working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(file))
}

// TestDocumentIsTheCommittedFile: the bytes the node serves are the
// bytes a consumer vendors, which is the whole of spec 030's second
// option. A test that read the document through a parser could not say
// this.
func TestDocumentIsTheCommittedFile(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(moduleRoot(t), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(Document) != string(want) {
		t.Error("the embedded document is not api/openapi.yaml")
	}
	if !strings.HasPrefix(string(Document), "# Origo's HTTP surface") {
		t.Error("the document does not open with the comment that says what it is")
	}
	if !strings.Contains(string(Document), "\nopenapi: 3.1.0\n") {
		t.Error("the document does not declare OpenAPI 3.1")
	}
	// A document in a repository describes no one installation.
	if strings.Contains(string(Document), "\nservers:\n") {
		t.Error("the document names a server")
	}
}

// TestDocumentStatesTheContractVersion is spec 030's criterion 4 from
// the node's side: what the document pins is the contract version of
// spec 003, the one internal/contract stamps on every response.
func TestDocumentStatesTheContractVersion(t *testing.T) {
	if want := "\n  version: \"" + contract.Version + "\"\n"; !strings.Contains(string(Document), want) {
		t.Errorf("the document does not state contract version %s", contract.Version)
	}
}

// TestDocumentNamesEveryContractCode is criterion 3: the document
// declares one response per error code the node can write and no other,
// under the statuses that code's row lists, so the deck's Code tables,
// which the document is rendered from, and internal/contract's table,
// which the handlers write from, are held to each other.
func TestDocumentNamesEveryContractCode(t *testing.T) {
	declared := declaredResponses(t)
	codes := contract.Codes()
	for _, code := range codes {
		description, ok := declared[code]
		if !ok {
			t.Errorf("the document declares no response for %s", code)
			continue
		}
		statuses, sentence, ok := strings.Cut(description, " "+code+": ")
		if !ok || strings.TrimSpace(sentence) == "" {
			t.Errorf("%s is declared as %q, which names neither the code nor its sentence", code, description)
			continue
		}
		// The document takes each status from the Status column of the
		// row, so it holds what the deck states: every status of the
		// package's row, or the first alone where a later spec added one
		// in prose (invalid_request answers 416 on a Range past the end
		// of a blob, spec 009, which spec 003's Status column does not
		// carry). A status the package does not list is drift.
		listed := contract.Statuses(code)
		for status := range strings.SplitSeq(statuses, " or ") {
			n, err := strconv.Atoi(status)
			if err != nil || !slices.Contains(listed, n) {
				t.Errorf("%s is declared under %s, and internal/contract lists %v", code, status, listed)
			}
		}
		if first, _, _ := strings.Cut(statuses, " "); first != strconv.Itoa(contract.Status(code)) {
			t.Errorf("%s is declared under %s first, and the node answers it with %d", code, first, contract.Status(code))
		}
	}
	for code := range declared {
		if !slices.Contains(codes, code) {
			t.Errorf("the document declares %s, which internal/contract does not", code)
		}
	}
	// Two rows in full: the sentence the document carries is the
	// sentence the node writes, not a paraphrase of it.
	for _, tc := range []struct{ code, sentence string }{
		{contract.CodeRepoNotFound, contract.Sentence(contract.CodeRepoNotFound)},
		{contract.CodeRateLimited, contract.Sentence(contract.CodeRateLimited)},
	} {
		if got := declared[tc.code]; !strings.HasSuffix(got, " "+tc.sentence) {
			t.Errorf("%s is declared as %q and the node writes %q", tc.code, got, tc.sentence)
		}
	}
}

// declaredResponses are the entries of components.responses with their
// descriptions, read from the committed document by indentation. The
// file is generated in one shape by one renderer, so the reading needs
// no parser and the node carries none.
func declaredResponses(t *testing.T) map[string]string {
	t.Helper()
	_, components, ok := strings.Cut(string(Document), "\ncomponents:\n")
	if !ok {
		t.Fatal("the document has no components block")
	}
	_, responses, ok := strings.Cut(components, "\n  responses:\n")
	if !ok {
		t.Fatal("the document declares no responses")
	}
	out := map[string]string{}
	code := ""
	for line := range strings.SplitSeq(responses, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		switch {
		case trimmed == "":
		case indent <= 2:
			code = ""
		case indent == 4 && strings.HasSuffix(trimmed, ":"):
			code = strings.TrimSuffix(trimmed, ":")
			out[code] = ""
		case indent == 6 && strings.HasPrefix(trimmed, "description:") && code != "":
			out[code] = unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "description:")))
		}
		if indent <= 2 && trimmed != "" && code == "" && len(out) > 0 {
			break
		}
	}
	if len(out) == 0 {
		t.Fatal("no response is declared")
	}
	return out
}

// unquote reads a YAML scalar the renderer wrote: a plain one as it is,
// a double-quoted one through the JSON reader, which is the same
// escaping.
func unquote(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	var out string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return s
	}
	return out
}

// TestHandlerAnswersTheDocumentToAnyone: the handler writes the file and
// nothing else, with the media type of an OpenAPI document and no
// credential asked for.
func TestHandlerAnswersTheDocumentToAnyone(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != ContentType {
		t.Errorf("content type %q", got)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Error("the document asks for a credential")
	}
	if rec.Body.String() != string(Document) {
		t.Error("the body is not the document")
	}
}
