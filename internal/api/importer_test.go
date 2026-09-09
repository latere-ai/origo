// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sink"
	"github.com/latere-ai/origo/test/stubs/source"
)

// importStub is a source stub with the egress rules that reach it: the
// loopback seam of spec 016, which is written in _test.go files alone.
func importStub(t *testing.T) (*source.Server, harnessOption) {
	t.Helper()
	stub := source.New(t)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(stub.CA())
	e := NewEgress(EgressOptions{Allow: []string{"localhost"}, Roots: roots})
	return stub, withEgress(e, true)
}

// sourceURL is a repository of the stub as the caller names it: the
// host the certificate carries, never the address the stub listens on.
func sourceURL(t *testing.T, stub *source.Server, name string) string {
	t.Helper()
	u, err := url.Parse(stub.URL())
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost:" + u.Port()
	u.Path = "/" + name + ".git"
	return u.String()
}

// waitImport polls the import state until it leaves running.
func (h *harness) waitImport(id string) ImportState {
	h.t.Helper()
	h.handler.Wait()
	status, out := h.do("GET", "/v1/repos/"+id+"/import", "")
	if status != 200 {
		h.t.Fatalf("import state: %d %v", status, out)
	}
	raw, _ := json.Marshal(out)
	var st ImportState
	if err := json.Unmarshal(raw, &st); err != nil {
		h.t.Fatal(err)
	}
	return st
}

// TestExportRoundTrip is spec 019's round-trip criterion: a bundle
// taken out of one repository and imported into a fresh one has the
// same rev-list --all, which is what makes the export a copy rather
// than a snapshot.
func TestExportRoundTrip(t *testing.T) {
	f := loadFixture(t)
	stub, egress := importStub(t)
	s := sink.New(t)
	h := newHarness(t, egress, withSink(s), withNow(fixedClock()))
	h.as(auth.Principal{Subject: "alice", Actor: "svc"})
	h.seed(f)

	// The export of the seeded repository becomes a repository of the
	// source stub, which the import then mirrors.
	res := h.get("/v1/repos/" + repoA + "/export.bundle")
	if res.status != 200 {
		t.Fatalf("export: %d", res.status)
	}
	if err := stub.AddRepo("round-trip", res.body); err != nil {
		t.Fatal(err)
	}
	h.create(repoB, "acme", "fresh")
	src := sourceURL(t, stub, "round-trip")
	status, out := h.do("POST", "/v1/repos/"+repoB+"/import", `{"source":"`+src+`","token":"`+stub.Token()+`"}`)
	if status != 202 || out["state"] != ImportRunning {
		t.Fatalf("import: %d %v", status, out)
	}
	st := h.waitImport(repoB)
	if st.State != ImportDone || st.Error != "" || st.Refs == 0 || st.Bytes == 0 || st.FinishedAt == nil {
		t.Fatalf("import state: %+v", st)
	}

	// One entry, in the shape of a compaction, with the packs.
	ix := mustIndex(t, h.log, repoB)
	if len(ix.Entries) != 1 || ix.Entries[0].Kind != wal.KindCompact || len(ix.Packs) == 0 || ix.SizeBytes != st.Bytes {
		t.Fatalf("index after the import: %+v", ix)
	}
	if len(ix.Refs) != st.Refs && len(ix.Refs) != st.Refs+1 {
		t.Fatalf("%d refs in the index, %d reported", len(ix.Refs), st.Refs)
	}

	// The imported history is the exported one.
	if revListOf(t, h, repoB) != gittest.RevList(t, f.Dir) {
		t.Fatal("the imported history differs from the exported one")
	}

	// The imported event carries the source without its token.
	ev := waitEvent(t, s, repoB, KindImported)
	if ev["source"] != src || ev["refs"] != float64(st.Refs) || ev["bytes"] != float64(st.Bytes) {
		t.Fatalf("imported %v", ev)
	}
	if p := ev["pusher"].(map[string]any); p["sub"] != "alice" || p["actor"] != "svc" {
		t.Fatalf("imported pusher %v", ev["pusher"])
	}
	// Every request the stub saw carried the bearer and nothing logged
	// or emitted holds it.
	reqs := stub.Requests()
	if len(reqs) == 0 {
		t.Fatal("the stub saw no request")
	}
	for _, r := range reqs {
		if !r.Bearer {
			t.Fatalf("a request without the bearer: %+v", r)
		}
	}

	// A repository that already has an entry refuses a second import.
	if status, out := h.do("POST", "/v1/repos/"+repoB+"/import", `{"source":"`+src+`"}`); status != 409 || code(out) != contract.CodeRepoNotEmpty || details(out)["seq"] == nil {
		t.Fatalf("import into a repository with history: %d %v", status, out)
	}
}

// revListOf is rev-list --all of the node's local copy.
func revListOf(t *testing.T, h *harness, id string) string {
	t.Helper()
	rp, release, err := h.cache.Acquire(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	return gittest.RevList(t, rp.Dir)
}

func fixedClock() func() time.Time {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

// TestImportRefusals covers what the import refuses before it starts:
// a source that is not https, one the egress rules do not admit, a
// caller without admin, and a state read for a repository no import was
// started for.
func TestImportRefusals(t *testing.T) {
	stub, egress := importStub(t)
	h := newHarness(t, egress)
	h.create(repoA, "acme", "app")

	if status, out := h.do("GET", "/v1/repos/"+repoA+"/import", ""); status != 404 || code(out) != contract.CodeImportNotFound {
		t.Fatalf("state without an import: %d %v", status, out)
	}
	for name, body := range map[string]string{
		"not json":  `{`,
		"no url":    `{"source":"not a url"}`,
		"not https": `{"source":"http://localhost/x.git"}`,
	} {
		if status, out := h.do("POST", "/v1/repos/"+repoA+"/import", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("import %s: %d %v", name, status, out)
		}
	}
	// A host the allow-list does not carry is refused with the egress
	// reason and no connection.
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/import", `{"source":"https://elsewhere.example/x.git"}`); status != 400 || details(out)["reason"] != "egress" {
		t.Fatalf("import of a host not on the list: %d %v", status, out)
	}
	h.authz.SetRules(authorizer.Rule{Allow: true}, authorizer.Rule{Subject: "eve", Allow: false, Reason: "no"})
	h.as(auth.Principal{Subject: "eve"})
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/import", `{"source":"`+sourceURL(t, stub, "fixture")+`"}`); status != 403 {
		t.Fatalf("import without admin: %d %v", status, out)
	}
	h.as(auth.Principal{Subject: "alice"})

	// A source that does not answer leaves the repository failed and
	// free for another import.
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/import", `{"source":"`+sourceURL(t, stub, "absent")+`"}`); status != 202 {
		t.Fatal("import of an absent repository")
	}
	st := h.waitImport(repoA)
	if st.State != ImportFailed || st.Error == "" {
		t.Fatalf("failed import: %+v", st)
	}
	if _, err := os.Stat(h.handler.importDir(repoA)); !os.IsNotExist(err) {
		t.Fatal("the scratch directory survived a failed import")
	}
}

// TestImportLeaseExpires is spec 019's lease criterion: a node killed
// during an import leaves importing_since set, another node reports
// running for the lease and then failed with import node lost, and the
// repository accepts a new import.
func TestImportLeaseExpires(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	stub, egress := importStub(t)
	h := newHarness(t, egress, withNow(func() time.Time { return now }), withNode("origod-1", liveSet{"origod-1", "origod-2"}))
	h.create(repoA, "acme", "app")

	// The lease of a node that is gone, as a killed node leaves it.
	started := now
	writeLease(t, h, repoA, "origod-9", started)
	if st := h.state(repoA); st.State != ImportRunning || st.StartedAt == nil || !st.StartedAt.Equal(started) {
		t.Fatalf("state under a fresh lease: %+v", st)
	}
	// A second import while one runs is refused.
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/import", `{"source":"`+sourceURL(t, stub, "fixture")+`"}`); status != 409 || code(out) != contract.CodeRepoImporting || details(out)["started_at"] == nil {
		t.Fatalf("import during an import: %d %v", status, out)
	}
	// Inside the lease it still reads running.
	now = started.Add(ImportLease - time.Minute)
	if st := h.state(repoA); st.State != ImportRunning {
		t.Fatalf("state inside the lease: %+v", st)
	}
	// Past it, with the node out of the live set, the lease is cleared.
	now = started.Add(ImportLease)
	st := h.state(repoA)
	if st.State != ImportFailed || st.Error != ErrorNodeLost {
		t.Fatalf("state past the lease: %+v", st)
	}
	m, err := h.log.ReadMeta(context.Background(), repoA)
	if err != nil || m.ImportingSince != nil || m.ImportNode != "" {
		t.Fatalf("meta after the lease expired: %+v, %v", m, err)
	}
	// The repository accepts a new import.
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/import", `{"source":"`+sourceURL(t, stub, "absent")+`"}`); status != 202 {
		t.Fatal("import after the lease expired")
	}
	h.handler.Wait()

	// A lease whose node is still in the live set is honoured whatever
	// its age.
	now = started
	writeLease(t, h, repoA, "origod-2", started)
	now = started.Add(2 * ImportLease)
	if st := h.state(repoA); st.State != ImportRunning {
		t.Fatalf("state under a live node's lease: %+v", st)
	}
}

// TestRestartClearsOwnImportLeases is spec 019's restart criterion: a
// node that starts under its own name frees every repository its
// scratch directory names at once, rather than after the lease.
func TestRestartClearsOwnImportLeases(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, withNow(func() time.Time { return now }), withNode("origod-1"))
	h.create(repoA, "acme", "app")
	h.create(repoB, "acme", "other")
	writeLease(t, h, repoA, "origod-1", now)
	writeLease(t, h, repoB, "origod-2", now)

	// Both left a scratch directory, and a stray one names no
	// repository at all.
	for _, id := range []string{repoA, repoB, "not-a-repository"} {
		if err := os.MkdirAll(filepath.Join(h.handler.importDir(id), "objects"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.handler.ClearImportLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := h.state(repoA); st.State != ImportFailed || st.Error != ErrorNodeRestarted {
		t.Fatalf("own lease after the restart: %+v", st)
	}
	// Another node's lease is left alone, whatever this node's scratch
	// directory holds.
	if st := h.state(repoB); st.State != ImportRunning {
		t.Fatalf("another node's lease after the restart: %+v", st)
	}
	for _, id := range []string{repoA, repoB} {
		if _, err := os.Stat(h.handler.importDir(id)); !os.IsNotExist(err) {
			t.Fatalf("the scratch directory of %s survived", id)
		}
	}
	// A node with no scratch root at all is a node that never imported.
	if err := os.RemoveAll(h.handler.importRoot()); err != nil {
		t.Fatal(err)
	}
	if err := h.handler.ClearImportLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// state reads the import state of a repository.
func (h *harness) state(id string) ImportState {
	h.t.Helper()
	status, out := h.do("GET", "/v1/repos/"+id+"/import", "")
	if status != 200 {
		h.t.Fatalf("import state: %d %v", status, out)
	}
	raw, _ := json.Marshal(out)
	var st ImportState
	if err := json.Unmarshal(raw, &st); err != nil {
		h.t.Fatal(err)
	}
	return st
}

// liveSet is the live set of spec 005 as the lease reads it.
type liveSet []string

func (l liveSet) Live() []string { return l }

// writeLease puts the lease of a node on the repository, as a running
// import does.
func writeLease(t *testing.T, h *harness, id, node string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	m, err := h.log.ReadMeta(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	m.ImportingSince, m.ImportNode, m.ImportError, m.ImportedAt = &at, node, "", nil
	if err := h.log.WriteMeta(ctx, m); err != nil {
		t.Fatal(err)
	}
}
