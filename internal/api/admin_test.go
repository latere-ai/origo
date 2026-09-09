// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/compact"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sink"
)

// create makes a repository through the API and fails the test when it
// is not created.
func (h *harness) create(id, owner, slug string) {
	h.t.Helper()
	if status, out := h.do("POST", "/v1/repos", `{"id":"`+id+`","owner":"`+owner+`","slug":"`+slug+`"}`); status != 201 {
		h.t.Fatalf("create %s: %d %v", id, status, out)
	}
}

// TestAdministrationOperations is spec 019's first criterion for the
// operations this file serves: the success path, the 403 of a caller
// the authorizer does not give admin, and the state conflict of each.
func TestAdministrationOperations(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, withNow(func() time.Time { return now }),
		withCompactor(stubCompactor{res: compact.Result{Primary: "origod-1"}}))
	h.create(repoA, "acme", "app")

	// stats reports what the log holds for a repository with no push.
	status, out := h.do("GET", "/v1/repos/"+repoA+"/stats", "")
	if status != 200 || out["size_bytes"] != float64(0) || out["lfs_bytes"] != float64(0) ||
		out["packs"] != float64(0) || out["entries_since_compaction"] != float64(0) ||
		out["refs"] != float64(1) || out["pushed_at"] != nil || out["compacted_at"] != nil {
		t.Fatalf("stats: %d %v", status, out)
	}

	// gc on a node that is not the primary schedules and says so.
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/gc", ""); status != 202 || out["status"] != "scheduled" {
		t.Fatalf("gc: %d %v", status, out)
	}

	// transfer moves the owner and leaves the id and the slug.
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/transfer", `{"owner":"beta"}`); status != 200 || out["owner"] != "beta" || out["slug"] != "app" || out["id"] != repoA {
		t.Fatalf("transfer: %d %v", status, out)
	}
	if id, err := h.log.Resolve(context.Background(), "beta", "app"); err != nil || id != repoA {
		t.Fatalf("the new name does not resolve: %v", err)
	}
	if _, err := h.log.Resolve(context.Background(), "acme", "app"); err == nil {
		t.Fatal("the old name still resolves")
	}
	// A transfer onto a name another repository holds is 409.
	h.create(repoB, "gamma", "app")
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/transfer", `{"owner":"gamma"}`); status != 409 || code(out) != contract.CodeRepoExists {
		t.Fatalf("transfer onto a taken name: %d %v", status, out)
	}
	for name, body := range map[string]string{
		"not json":  `{`,
		"bad label": `{"owner":"a b"}`,
		"reserved":  `{"owner":"r"}`,
	} {
		if status, out := h.do("POST", "/v1/repos/"+repoA+"/transfer", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("transfer %s: %d %v", name, status, out)
		}
	}

	// freeze sets frozen_at, GET reports it, and a second freeze is 409.
	status, out = h.do("POST", "/v1/repos/"+repoA+"/freeze", "")
	if status != 200 || out["frozen_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("freeze: %d %v", status, out)
	}
	if status, out := h.do("GET", "/v1/repos/"+repoA, ""); status != 200 || out["frozen_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("get after freeze: %d %v", status, out)
	}
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/freeze", ""); status != 409 || code(out) != contract.CodeRepoFrozen || details(out)["frozen_at"] == nil {
		t.Fatalf("second freeze: %d %v", status, out)
	}
	// unfreeze clears it and is 200 again on a repository that is not
	// frozen.
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/unfreeze", ""); status != 200 || out["frozen_at"] != nil {
		t.Fatalf("unfreeze: %d %v", status, out)
	}
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/unfreeze", ""); status != 200 || out["frozen_at"] != nil {
		t.Fatalf("second unfreeze: %d %v", status, out)
	}

	// A caller the authorizer does not give admin is 403 on every one
	// of them, and on the three operations of spec 003 beside them.
	// A later rule wins, so the catch-all allow goes first.
	h.authz.SetRules(
		authorizer.Rule{Allow: true},
		authorizer.Rule{Subject: "eve", Allow: false, Reason: "not an administrator"},
		authorizer.Rule{Subject: "eve", Action: "read", Allow: true},
	)
	h.as(auth.Principal{Subject: "eve"})
	for _, path := range []string{"/transfer", "/freeze", "/unfreeze", "/gc"} {
		if status, out := h.do("POST", "/v1/repos/"+repoA+path, `{"owner":"beta"}`); status != 403 || code(out) != contract.CodeForbidden {
			t.Errorf("%s without admin: %d %v", path, status, out)
		}
	}
	// stats asks for read, which this caller has.
	if status, _ := h.do("GET", "/v1/repos/"+repoA+"/stats", ""); status != 200 {
		t.Errorf("stats with read: %d", status)
	}
	h.as(auth.Principal{Subject: "alice"})

	// An operation on a repository that does not exist is 404.
	for _, path := range []string{"/transfer", "/freeze", "/unfreeze", "/gc"} {
		if status, out := h.do("POST", "/v1/repos/"+unknown+path, `{"owner":"beta"}`); status != 404 || code(out) != contract.CodeRepoNotFound {
			t.Errorf("%s of an unknown repository: %d %v", path, status, out)
		}
	}
	if status, out := h.do("GET", "/v1/repos/"+unknown+"/stats", ""); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Errorf("stats of an unknown repository: %d %v", status, out)
	}
}

// TestPurgedRepositoryIsGone is spec 019's tombstone on the API: every
// endpoint of a purged repository answers 410 gone, its id is refused
// by POST /v1/repos, and its name is free again.
func TestPurgedRepositoryIsGone(t *testing.T) {
	now := newTestClock(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, withNow(now.Now))
	h.create(repoA, "acme", "app")
	if status, _ := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 {
		t.Fatal("delete")
	}
	now.Add(wal.DeleteHold)
	if rep, err := h.log.Sweep(context.Background(), repoA, time.Hour); err != nil || !rep.Purged {
		t.Fatalf("purge: %+v, %v", rep, err)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/repos/" + repoA, ""},
		{"PATCH", "/v1/repos/" + repoA, `{"slug":"other"}`},
		{"DELETE", "/v1/repos/" + repoA, ""},
		{"POST", "/v1/repos/" + repoA + "/undelete", ""},
		{"POST", "/v1/repos/" + repoA + "/tokens", `{"scope":"read","ttl":60}`},
		{"POST", "/v1/repos/" + repoA + "/transfer", `{"owner":"beta"}`},
		{"POST", "/v1/repos/" + repoA + "/freeze", ""},
		{"POST", "/v1/repos/" + repoA + "/unfreeze", ""},
		{"POST", "/v1/repos/" + repoA + "/gc", ""},
		{"GET", "/v1/repos/" + repoA + "/stats", ""},
		{"GET", "/v1/repos/" + repoA + "/refs", ""},
		{"GET", "/v1/repos/" + repoA + "/commits", ""},
		{"GET", "/v1/repos/" + repoA + "/archive/main.tar.gz", ""},
	} {
		status, out := h.do(c.method, c.path, c.body)
		if status != 410 || code(out) != contract.CodeGone || details(out)["purged_at"] == nil || details(out)["id"] != repoA {
			t.Errorf("%s %s: %d %v", c.method, c.path, status, out)
		}
	}
	// The id stays taken forever and the name is reusable.
	if status, out := h.do("POST", "/v1/repos", `{"id":"`+repoA+`","owner":"acme","slug":"other"}`); status != 409 || code(out) != contract.CodeRepoExists {
		t.Fatalf("create of a purged id: %d %v", status, out)
	}
	h.create(repoB, "acme", "app")
}

// TestRenameAndTransferEvents is spec 019's event criterion for the two
// kinds one operation can carry: PATCH with owner emits renamed with
// both labels, transfer emits transferred with the owner alone, and
// both carry the caller as the pusher.
func TestRenameAndTransferEvents(t *testing.T) {
	s := sink.New(t)
	h := newHarness(t, withSink(s))
	h.as(auth.Principal{Subject: "alice", Actor: "svc"})
	h.create(repoA, "acme", "app")

	if status, _ := h.do("PATCH", "/v1/repos/"+repoA, `{"owner":"beta"}`); status != 200 {
		t.Fatal("patch owner")
	}
	renamed := waitEvent(t, s, repoA, KindRenamed)
	from, to := renamed["from"].(map[string]any), renamed["to"].(map[string]any)
	if from["owner"] != "acme" || from["slug"] != "app" || to["owner"] != "beta" || to["slug"] != "app" {
		t.Fatalf("renamed %v", renamed)
	}
	if p := renamed["pusher"].(map[string]any); p["sub"] != "alice" || p["actor"] != "svc" {
		t.Fatalf("renamed pusher %v", renamed["pusher"])
	}

	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/transfer", `{"owner":"gamma"}`); status != 200 {
		t.Fatal("transfer")
	}
	transferred := waitEvent(t, s, repoA, KindTransferred)
	from, to = transferred["from"].(map[string]any), transferred["to"].(map[string]any)
	if from["owner"] != "beta" || len(from) != 1 || to["owner"] != "gamma" || len(to) != 1 {
		t.Fatalf("transferred %v", transferred)
	}
	if transferred["owner"] != "gamma" || transferred["slug"] != "app" || transferred["repo"] != repoA {
		t.Fatalf("transferred shared fields %v", transferred)
	}
	if p := transferred["pusher"].(map[string]any); p["sub"] != "alice" || p["actor"] != "svc" {
		t.Fatalf("transferred pusher %v", transferred["pusher"])
	}
	// A rename that changes nothing emits nothing.
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/transfer", `{"owner":"gamma"}`); status != 200 {
		t.Fatal("second transfer")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(s.Deliveries(repoA, KindTransferred)); n != 1 {
		t.Fatalf("%d transferred events", n)
	}
}

// waitEvent waits for one delivery of the kind and decodes its payload.
func waitEvent(t *testing.T, s *sink.Server, repo, kind string) map[string]any {
	t.Helper()
	got, ok := s.Wait(repo, kind, 1, 10*time.Second)
	if !ok {
		t.Fatalf("%s was not delivered", kind)
	}
	var body map[string]any
	if err := json.Unmarshal(got[len(got)-1].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["kind"] != kind || body["repo"] != repo || body["id"] == nil || body["at"] == nil {
		t.Fatalf("%s shared fields: %s", kind, got[len(got)-1].Body)
	}
	return body
}

// TestAdministrationEvents is spec 019's last criterion: every
// operation delivers one event of its kind with the fields the table
// lists.
func TestAdministrationEvents(t *testing.T) {
	now := newTestClock(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
	s := sink.New(t)
	h := newHarness(t, withSink(s), withNow(now.Now))
	h.as(auth.Principal{Subject: "alice"})
	h.create(repoA, "acme", "app")

	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/freeze", ""); status != 200 {
		t.Fatal("freeze")
	}
	frozen := waitEvent(t, s, repoA, KindFrozen)
	if _, ok := frozen["from"]; ok {
		t.Fatalf("frozen carries extra fields: %v", frozen)
	}
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/unfreeze", ""); status != 200 {
		t.Fatal("unfreeze")
	}
	waitEvent(t, s, repoA, KindUnfrozen)

	if status, _ := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 {
		t.Fatal("delete")
	}
	deleted := waitEvent(t, s, repoA, KindDeleted)
	if deleted["purge_after"] == nil {
		t.Fatalf("deleted %v", deleted)
	}
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/undelete", ""); status != 200 {
		t.Fatal("undelete")
	}
	waitEvent(t, s, repoA, KindUndeleted)

	// A repeated DELETE of a deleted repository is one event, because
	// the id is derived from the repository, the kind, and at, and a
	// second DELETE does not move deleted_at.
	now.Add(time.Minute)
	if status, _ := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 {
		t.Fatal("second delete")
	}
	if _, ok := s.Wait(repoA, KindDeleted, 2, 10*time.Second); !ok {
		t.Fatal("the second delete emitted nothing")
	}
	if status, _ := h.do("DELETE", "/v1/repos/"+repoA, ""); status != 202 {
		t.Fatal("third delete")
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(s.Deliveries(repoA, KindDeleted)); n != 2 {
		t.Fatalf("%d deleted events, want 2", n)
	}
}
