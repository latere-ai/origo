// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"latere.ai/x/origo/test/stubs/authorizer"
)

// listReq builds a contract-2 list envelope: no repository is named, so
// the resource carries the kind alone, and the cursor and limit ride in
// its fields (Origo spec 028).
func listReq(subject, cursor string, limit int) authz.Request {
	fields := map[string]any{}
	if cursor != "" {
		fields["cursor"] = cursor
	}
	if limit > 0 {
		fields["limit"] = limit
	}
	return authz.Request{Subject: subject, Action: string(ActionList), Resource: authz.NewResource(ResourceKind, "", fields)}
}

// answering serves one fixed body and records every request body it saw.
type answering struct {
	status int
	body   string
	seen   []string
}

func (a *answering) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 4<<10)
	n, _ := r.Body.Read(buf)
	a.seen = append(a.seen, string(buf[:n]))
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(a.body))
}

func newAnswering(t *testing.T, status int, body string) (*Client, *answering) {
	t.Helper()
	a := &answering{status: status, body: body}
	return newClient(t, "http://authorizer.invalid/", "tok", &handlerTransport{h: a}, newClock(), pkgmetrics.NewRegistry()), a
}

// TestListRequestCarriesNoRepo is spec 026's distinguishing property: a
// list call names no repository, and the three actions still do.
func TestListRequestCarriesNoRepo(t *testing.T) {
	c, a := newAnswering(t, http.StatusOK, `{"repos":[],"next_cursor":""}`)
	ctx := context.Background()
	if _, err := c.List(ctx, listReq("alice", "c1", 7)); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(a.seen[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["repo"]; ok {
		t.Errorf("the list request carries a contract-1 repo object: %s", a.seen[0])
	}
	if sent["subject"] != "alice" || sent["action"] != "repo.list" {
		t.Errorf("the list envelope is %s", a.seen[0])
	}
	// A list names no repository: its resource carries the kind, and the
	// cursor and limit ride in the resource (Origo spec 028).
	if res, _ := sent["resource"].(map[string]any); res == nil || res["kind"] != "Repository" || res["id"] != nil || res["cursor"] != "c1" || res["limit"] != float64(7) {
		t.Errorf("the list resource is %v", sent["resource"])
	}

	c2, a2 := newAnswering(t, http.StatusOK, `{"allow":true}`)
	if _, err := c2.Authorize(ctx, request("alice", repoA, ActionRead)); err != nil {
		t.Fatal(err)
	}
	var read map[string]any
	if err := json.Unmarshal([]byte(a2.seen[0]), &read); err != nil {
		t.Fatal(err)
	}
	if _, ok := read["repo"]; ok {
		t.Errorf("the read request carries a contract-1 repo object: %s", a2.seen[0])
	}
	if rr, _ := read["resource"].(map[string]any); rr == nil || rr["id"] != repoA || rr["kind"] != "Repository" {
		t.Errorf("the read resource is %v", read["resource"])
	}

	// The limit defaults to spec 026's figure when the caller sends none:
	// the guard fills it before the envelope is built (Origo spec 028).
	c3, a3 := newAnswering(t, http.StatusOK, `{"repos":[]}`)
	g := NewGuard(c3, nil)
	if _, err := g.Directory(ctx, Principal{Subject: "alice"}, "", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a3.seen[0], `"limit":50`) {
		t.Errorf("the default limit is not %d: %s", DefaultListLimit, a3.seen[0])
	}
}

// TestDirectoryAnswersAreDistinguished: the three answers of spec 026
// are told apart by one key each, and anything else fails closed.
func TestDirectoryAnswersAreDistinguished(t *testing.T) {
	ctx := context.Background()
	page := `{"repos":[{"id":"` + repoA + `","owner":"acme","slug":"app"}],"next_cursor":"n1"}`
	c, _ := newAnswering(t, http.StatusOK, page)
	got, err := c.List(ctx, listReq("alice", "", 0))
	switch {
	case err != nil:
		t.Fatal(err)
	case !got.Supported || !got.Allowed:
		t.Fatalf("a page read as supported=%v allowed=%v", got.Supported, got.Allowed)
	case len(got.Repos) != 1 || got.Repos[0].ID != repoA || got.Repos[0].Owner != "acme" || got.Repos[0].Slug != "app":
		t.Fatalf("the page is %+v", got.Repos)
	case got.NextCursor != "n1":
		t.Fatalf("next_cursor is %q", got.NextCursor)
	}

	// An empty page is a valid answer and not an absent directory.
	c, _ = newAnswering(t, http.StatusOK, `{"repos":[]}`)
	got, err = c.List(ctx, listReq("alice", "", 0))
	if err != nil || !got.Supported || !got.Allowed || len(got.Repos) != 0 {
		t.Fatalf("the empty page is %+v, %v", got, err)
	}

	c, _ = newAnswering(t, http.StatusOK, `{"allow":false,"reason":"no_directory_for_you"}`)
	got, err = c.List(ctx, listReq("alice", "", 0))
	if err != nil || !got.Supported || got.Allowed || got.Reason != "no_directory_for_you" {
		t.Fatalf("the deny is %+v, %v", got, err)
	}

	c, _ = newAnswering(t, http.StatusOK, `{"directory":false}`)
	got, err = c.List(ctx, listReq("alice", "", 0))
	if err != nil || got.Supported {
		t.Fatalf("{\"directory\": false} is %+v, %v", got, err)
	}

	// Everything else is an outage, never an answer.
	for _, body := range []string{`{}`, `{"allow":true}`, `{"directory":true}`, `not json`} {
		c, _ = newAnswering(t, http.StatusOK, body)
		if _, err := c.List(ctx, listReq("alice", "", 0)); !isUnavailable(err) {
			t.Errorf("body %q gave %v, want an *Unavailable", body, err)
		}
	}
	for _, status := range []int{http.StatusNotImplemented, http.StatusInternalServerError, http.StatusForbidden} {
		c, _ = newAnswering(t, status, `{"repos":[]}`)
		if _, err := c.List(ctx, listReq("alice", "", 0)); !isUnavailable(err) {
			t.Errorf("status %d gave %v, want an *Unavailable", status, err)
		}
	}
}

func isUnavailable(err error) bool {
	_, ok := errors.AsType[*Unavailable](err)
	return err != nil && ok
}

// TestDecisionStillNeedsAllow: spec 026 widened no parser but its own.
// A decision without an allow field is still an outage, which is the
// fail-closed rule of spec 007.
func TestDecisionStillNeedsAllow(t *testing.T) {
	ctx := context.Background()
	for _, body := range []string{`{}`, `{"reason":"nope"}`, `{"repos":[]}`, `{"directory":false}`, `{"ttl":60}`} {
		c, _ := newAnswering(t, http.StatusOK, body)
		if _, err := c.Authorize(ctx, request("alice", repoA, ActionRead)); !isUnavailable(err) {
			t.Errorf("a decision body %q gave %v, want an *Unavailable", body, err)
		}
	}
	// And a directory answer never reaches the decision path as an allow.
	c, _ := newAnswering(t, http.StatusOK, `{"repos":[{"id":"`+repoA+`"}]}`)
	if d, err := c.Authorize(ctx, request("alice", repoA, ActionWrite)); err == nil || d.Allow {
		t.Errorf("a directory body decided a write: %+v, %v", d, err)
	}
}

// TestDirectoryIsNotCached: a list answer is a page, not a verdict, so
// two reads are two calls and neither touches the decision cache.
func TestDirectoryIsNotCached(t *testing.T) {
	a := &answering{body: `{"repos":[]}`}
	transport := &handlerTransport{h: a}
	c := newClient(t, "http://authorizer.invalid/", "tok", transport, newClock(), pkgmetrics.NewRegistry())
	ctx := context.Background()
	for range 3 {
		if _, err := c.List(ctx, listReq("alice", "", 0)); err != nil {
			t.Fatal(err)
		}
	}
	if got := transport.attempts.Load(); got != 3 {
		t.Errorf("three directory reads made %d calls", got)
	}
	if got := c.CacheLen(); got != 0 {
		t.Errorf("the decision cache holds %d entries after three list calls", got)
	}
}

// TestBoundTokenCannotList: a repository-bound token was minted for one
// repository, so there is no subject-wide answer and the authorizer is
// never asked.
func TestBoundTokenCannotList(t *testing.T) {
	a := &answering{body: `{"repos":[]}`}
	transport := &handlerTransport{h: a}
	c := newClient(t, "http://authorizer.invalid/", "tok", transport, newClock(), pkgmetrics.NewRegistry())
	g := NewGuard(c, nil)
	p := Principal{Subject: "builder", Bound: &Bound{Repo: repoA, Scope: ScopeWrite}}
	_, err := g.Directory(context.Background(), p, "", 50)
	denied, ok := errors.AsType[*Denied](err)
	if !ok || denied.Reason != ReasonScope || denied.Action != ActionList {
		t.Fatalf("a bound token's directory gave %v", err)
	}
	if got := transport.attempts.Load(); got != 0 {
		t.Errorf("the authorizer was called %d times for a bound token", got)
	}
}

// TestGuardWithoutAListerHasNoDirectory: an authorizer that does not
// implement the question is an installation with no directory, which is
// the same answer as {"directory": false} and never an outage.
func TestGuardWithoutAListerHasNoDirectory(t *testing.T) {
	g := NewGuard(allowAll{}, nil)
	got, err := g.Directory(context.Background(), Principal{Subject: "alice"}, "", 50)
	if err != nil || got.Supported {
		t.Fatalf("a guard over a plain authorizer gave %+v, %v", got, err)
	}
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, authz.Request) (Decision, error) {
	return Decision{Allow: true, TTL: DefaultTTL, Replicas: DefaultReplicas, QuotaBytes: DefaultQuotaBytes}, nil
}

// TestDirectoryMetricRecordsRefusalsAsDenies keeps the label set of spec
// 011: a page is an allow, both refusals are denies, and only an
// *Unavailable is an error.
func TestDirectoryMetricRecordsRefusalsAsDenies(t *testing.T) {
	ctx := context.Background()
	for _, row := range []struct {
		body, want string
		status     int
	}{
		{body: `{"repos":[]}`, want: "allow"},
		{body: `{"allow":false,"reason":"x"}`, want: "deny"},
		{body: `{"directory":false}`, want: "deny"},
		{body: `{}`, want: "error"},
	} {
		reg := pkgmetrics.NewRegistry()
		c := newClient(t, "http://authorizer.invalid/", "tok", &handlerTransport{h: &answering{status: row.status, body: row.body}}, newClock(), reg)
		_, _ = c.List(ctx, listReq("alice", "", 0))
		hist := reg.Histogram("origo_authorizer_seconds", "", nil)
		if got := hist.Count(map[string]string{"result": row.want}); got != 1 {
			t.Errorf("body %q recorded result=%s %d times", row.body, row.want, got)
		}
	}
}

// TestStubDirectory is the stub's half of spec 026: no directory until
// one is set, the rule table filters it, and it pages at limit.
func TestStubDirectory(t *testing.T) {
	stub := authorizer.New(t)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, newClock(), pkgmetrics.NewRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := c.List(ctx, listReq("alice", "", 0))
	if err != nil || got.Supported {
		t.Fatalf("an unset directory gave %+v, %v", got, err)
	}

	entries := []authorizer.DirectoryEntry{
		{ID: repoA, Owner: "acme", Slug: "app"},
		{ID: repoB, Owner: "acme", Slug: "lib"},
	}
	stub.SetDirectory(true, entries...)
	got, err = c.List(ctx, listReq("alice", "", 0))
	if err != nil || len(got.Repos) != 2 {
		t.Fatalf("the directory is %+v, %v", got, err)
	}

	// A rule that denies one repository denies it in the directory too.
	stub.Deny(authorizer.Rule{Subject: "alice", Resource: repoB}, "insufficient_role")
	got, err = c.List(ctx, listReq("alice", "", 0))
	if err != nil || len(got.Repos) != 1 || got.Repos[0].ID != repoA {
		t.Fatalf("the filtered directory is %+v, %v", got, err)
	}
	// Another subject the rule does not name still sees both.
	got, err = c.List(ctx, listReq("bob", "", 0))
	if err != nil || len(got.Repos) != 2 {
		t.Fatalf("bob's directory is %+v, %v", got, err)
	}

	// Paging: one entry a page, and the cursor carries the reader on.
	first, err := c.List(ctx, listReq("bob", "", 1))
	if err != nil || len(first.Repos) != 1 || first.Repos[0].ID != repoA || first.NextCursor != repoA {
		t.Fatalf("the first page is %+v, %v", first, err)
	}
	second, err := c.List(ctx, listReq("bob", first.NextCursor, 1))
	if err != nil || len(second.Repos) != 1 || second.Repos[0].ID != repoB || second.NextCursor != "" {
		t.Fatalf("the second page is %+v, %v", second, err)
	}

	// The stub records a list request like any other.
	seen := stub.Requests()
	if len(seen) == 0 || seen[len(seen)-1].Action != "repo.list" || seen[len(seen)-1].Resource.ID != "" {
		t.Fatalf("the last recorded request is %+v", seen[len(seen)-1])
	}

	stub.SetDirectory(false)
	if got, err := c.List(ctx, listReq("bob", "", 0)); err != nil || got.Supported {
		t.Fatalf("a withdrawn directory gave %+v, %v", got, err)
	}
}
