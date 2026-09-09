// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
)

// The row of spec 015 a caller outside the installation cannot cause:
// a pack object gone.

func cases015() []testCase {
	return []testCase{
		{name: "repository_unavailable", group: GroupRepository, run: case015RepositoryUnavailable},
	}
}

// case015RepositoryUnavailable pushes one entry, deletes and undeletes
// the repository so every node materializes it again, deletes the
// entry object that carries the push's pack through the Fault, and
// asserts 503 repository_unavailable naming the key. Behind a load
// balancer the node that made the push may still hold its copy, so the
// request is repeated until a node materializes.
func case015RepositoryUnavailable(t *testing.T, s *session) {
	id := s.create(t, "partial")
	work := clone(t, s.repoURL(id))
	commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	expectStatus(t, s.call(t, "DELETE", "/v1/repos/"+id, ""), http.StatusAccepted)
	key := s.target.Fault.DeleteObject(t, "origo/repos/"+id+"/wal/"+wal.SeqString(1)+".")
	expectStatus(t, s.call(t, "POST", "/v1/repos/"+id+"/undelete", ""), http.StatusOK)
	var r response
	for range 20 {
		r = s.call(t, "GET", "/v1/repos/"+id+"/refs", "")
		if r.status == http.StatusServiceUnavailable {
			break
		}
	}
	d := expectError(t, r, http.StatusServiceUnavailable, contract.CodeRepositoryUnavailable)
	check(t, !(d["key"] != key), "details.key %v, want %s", d["key"], key)
	// Another repository is served.
	expectStatus(t, s.call(t, "GET", "/v1/repos/"+s.fixture.id+"/refs", ""), http.StatusOK)
}
