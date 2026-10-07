// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"latere.ai/x/origo/internal/contract"
	"latere.ai/x/origo/test/stubs/authorizer"
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
	// route.
	r := s.call(t, "GET", "/v1/repos?limit=50", "")
	got := ids(r)
	failIf(t, !got[seen] || !got[hidden], "directory %v lacks %s or %s", got, seen, hidden)
	repos, _ := r.json["repos"].([]any)
	first, _ := repos[0].(map[string]any)
	failIf(t, first["owner"] != Owner || first["default_branch"] == nil, "directory entry is not the representation: %v", first)

	// A walk at limit=1 goes through cursors the node sealed, which carry
	// nothing of the authorizer's, and finds each repository once.
	walked := map[string]int{}
	cursor := ""
	for page := 1; ; page++ {
		failIf(t, page > 2, "the walk of two did not end: %v", walked)
		path := "/v1/repos?limit=1"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := s.call(t, "GET", path, "")
		for id := range ids(r) {
			walked[id]++
		}
		if r.json["next_cursor"] == nil {
			break
		}
		cursor, _ = r.json["next_cursor"].(string)
		failIf(t, !strings.HasPrefix(cursor, "v1.") || strings.Contains(cursor, seen) || strings.Contains(cursor, hidden),
			"next_cursor %q is not one the node sealed", cursor)
	}
	failIf(t, len(walked) != 2 || walked[seen] != 1 || walked[hidden] != 1, "the walk at limit=1 found %v", walked)

	// A read the authorizer denies on one repository hides it from the
	// page: the directory is the authorizer's answer, filtered by the
	// same rules a read is.
	s.setRules(t, authorizer.Rule{Resource: hidden, Action: "repo.read", Allow: false, Reason: "hidden"})
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
