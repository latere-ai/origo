// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

// cases026 is the collection route of spec 026: the directory the
// authorizer answers, filtered by its own rule table, the name mode
// beside it, and the 501 of an authorizer with no directory. The
// directory is the stub's, set over its control endpoint, so the case
// needs Authorizer and skips with the deny-flipping group; it is what
// proves the populated path on the stub and the stack, which the
// overlay's default of no directory cannot.
func cases026() []testCase {
	return []testCase{
		{name: "directory", group: GroupDeny, run: case026Directory},
	}
}

func case026Directory(t *testing.T, s *session) {
	seen, hidden := s.create(t, "seen"), s.create(t, "hidden")
	s.setDirectory(t, true,
		authorizer.DirectoryEntry{ID: seen, Owner: Owner, Slug: SlugPrefix + "seen-" + seen[:8]},
		authorizer.DirectoryEntry{ID: hidden, Owner: Owner, Slug: SlugPrefix + "hidden-" + hidden[:8]},
	)
	ids := func(r response) map[string]bool {
		expectStatus(t, r, http.StatusOK)
		repos, _ := r.json["repos"].([]any)
		out := map[string]bool{}
		for _, repo := range repos {
			m, _ := repo.(map[string]any)
			id, _ := m["id"].(string)
			out[id] = true
		}
		return out
	}
	// The directory lists both, each as the representation of the id
	// route, with the authorizer's cursor passed through.
	r := s.call(t, "GET", "/v1/repos?limit=50", "")
	got := ids(r)
	failIf(t, !got[seen] || !got[hidden], "directory %v lacks %s or %s", got, seen, hidden)
	repos, _ := r.json["repos"].([]any)
	first, _ := repos[0].(map[string]any)
	failIf(t, first["owner"] != Owner || first["default_branch"] == nil, "directory entry is not the representation: %v", first)

	// A read the authorizer denies on one repository hides it from the
	// page: the directory is the authorizer's answer, filtered by the
	// same rules a read is.
	s.setRules(t, authorizer.Rule{Repo: hidden, Action: "read", Allow: false, Reason: "hidden"})
	got = ids(s.call(t, "GET", "/v1/repos?limit=50", ""))
	failIf(t, !got[seen] || got[hidden], "directory after a deny %v", got)

	// The name mode answers the id route's body for the id the name
	// resolves to.
	byName := s.call(t, "GET", "/v1/repos?owner="+Owner+"&slug="+SlugPrefix+"seen-"+seen[:8], "")
	expectStatus(t, byName, http.StatusOK)
	failIf(t, byName.json["id"] != seen, "name mode resolved %v, want %s", byName.json["id"], seen)

	// An authorizer with no directory is 501 directory_unsupported.
	s.setDirectory(t, false)
	expectError(t, s.call(t, "GET", "/v1/repos", ""), http.StatusNotImplemented, contract.CodeDirectoryUnsupported)
}
