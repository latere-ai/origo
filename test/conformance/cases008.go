// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"testing"
	"time"
)

// The rows of spec 008: one signed push event per acknowledged push
// with the payload table's fields, and origo.event=off suppressing that
// push's event alone. Without a sink the target gives no way to read a
// delivery, so the cases assert the pushes and report the deliveries
// unverified.

func cases008() []testCase {
	return []testCase{
		{name: "push", run: case008Push},
		{name: "event-off", run: case008EventOff},
	}
}

func case008Push(t *testing.T, s *session) {
	id := s.create(t, "events")
	work := clone(t, s.repoURL(id))
	c1 := commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	d, ok := s.expectEvent(t, id, "push", 1)
	if !ok {
		return
	}
	e := event(t, d)
	for _, k := range []string{"id", "kind", "repo", "seq", "owner", "slug", "pusher", "updates", "at"} {
		if _, present := e[k]; !present {
			t.Fatalf("the push event lacks %s: %s", k, d.Body)
		}
	}
	check(t, !(e["id"] != d.ID || e["kind"] != "push" || e["repo"] != id || e["owner"] != Owner || e["seq"] != float64(1)), "push event: %s", d.Body)
	if _, err := time.Parse(time.RFC3339Nano, str(e["at"])); err != nil {
		t.Fatalf("at: %v", err)
	}
	updates, _ := e["updates"].([]any)
	u, _ := updates[0].(map[string]any)
	check(t, !(len(updates) != 1 || u["ref"] != "refs/heads/main" || u["after"] != c1 || u["forced"] != false || len(str(u["before"])) != 40), "updates: %v", updates)
	pusher, _ := e["pusher"].(map[string]any)
	check(t, !(pusher["sub"] == ""), "pusher: %v", pusher)
	// A forced update is flagged.
	mustGit(t, work, "commit", "-q", "--amend", "-m", "rewritten")
	mustGit(t, work, "push", "-q", "--force", "origin", "HEAD:refs/heads/main")
	d, _ = s.expectEvent(t, id, "push", 2)
	updates, _ = event(t, d)["updates"].([]any)
	if u, _ := updates[0].(map[string]any); u["forced"] != true || u["before"] != c1 {
		t.Fatalf("forced update: %v", updates)
	}
}

func case008EventOff(t *testing.T, s *session) {
	id := s.create(t, "event-off")
	work := clone(t, s.repoURL(id))
	commitFile(t, work, "a.txt", []byte("a"), "first")
	mustGit(t, work, "push", "-q", "-o", "origo.event=off", "origin", "HEAD:refs/heads/main")
	commitFile(t, work, "b.txt", []byte("b"), "second")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	// The second push's event arrives and the first's never does.
	if d, ok := s.expectEvent(t, id, "push", 1); ok {
		if e := event(t, d); e["seq"] != float64(2) {
			t.Fatalf("the suppressed push was delivered: %s", d.Body)
		}
	}
	s.expectNoEvent(t, id, "push", 1)
}
