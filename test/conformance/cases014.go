// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"latere.ai/x/origo/internal/api"
)

// The row of spec 014 the suite carries: verify, in the source group
// beside spec 019's import, because it needs the same source.

func cases014() []testCase {
	return []testCase{
		{name: "verify", group: GroupSource, run: case014Verify},
	}
}

// case014Verify imports the source and verifies the copy against it:
// equal, with the same reference count on both sides, the copy's
// object count, verified_at and verified_equal on the repository, and
// the verified event. One commit pushed to the copy, which the source
// does not have, makes the next verify answer not equal, naming the
// branch with the hash on each side, and the repository records it.
func case014Verify(t *testing.T, s *session) {
	id := s.create(t, "verify")
	body := fmt.Sprintf(`{"source":%q,"token":%q}`, s.target.Source, s.target.SourceToken)
	expectStatus(t, s.call(t, "POST", "/v1/repos/"+id+"/import", body), http.StatusAccepted)
	var st response
	waitFor(t, 15*time.Minute, "the import", func() bool {
		st = s.call(t, "GET", "/v1/repos/"+id+"/import", "")
		return st.status == http.StatusOK && st.json["state"] != api.ImportRunning
	})
	failIf(t, st.json["state"] != api.ImportDone, "import state: %s", st.body)

	r := s.call(t, "POST", "/v1/repos/"+id+"/verify", body)
	expectStatus(t, r, http.StatusOK)
	refs := obj(r.json["refs"])
	differing, listed := refs["differing"].([]any)
	failIf(t, r.json["equal"] != true || num(refs["source"]) < 1 || refs["source"] != refs["origo"] || !listed || len(differing) != 0 || num(obj(r.json["objects"])["origo"]) < 1 || r.json["checked_at"] == nil, "verify of the import: %s", r.body)
	repo := s.call(t, "GET", "/v1/repos/"+id, "")
	failIf(t, repo.json["verified_equal"] != true || repo.json["verified_at"] != r.json["checked_at"], "the repository after an equal verify: %s", repo.body)
	if d, ok := s.expectEvent(t, id, "verified", 1); ok {
		e := event(t, d)
		failIf(t, e["equal"] != true || e["checked_at"] != r.json["checked_at"], "verified: %s", d.Body)
	}

	work := clone(t, s.repoURL(id))
	before := mustGit(t, work, "rev-parse", "HEAD")
	after := commitFile(t, work, "verify.txt", []byte("only on the copy"), "only on the copy")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	r = s.call(t, "POST", "/v1/repos/"+id+"/verify", body)
	expectStatus(t, r, http.StatusOK)
	differing, _ = obj(r.json["refs"])["differing"].([]any)
	failIf(t, r.json["equal"] != false || len(differing) != 1, "verify after a push to the copy: %s", r.body)
	diff := obj(differing[0])
	failIf(t, diff["name"] != "refs/heads/main" || diff["source"] != before || diff["origo"] != after, "the differing reference: %v, want refs/heads/main from %s to %s", diff, before, after)
	repo = s.call(t, "GET", "/v1/repos/"+id, "")
	failIf(t, repo.json["verified_equal"] != false || repo.json["verified_at"] != r.json["checked_at"], "the repository after a verify that differs: %s", repo.body)
}
