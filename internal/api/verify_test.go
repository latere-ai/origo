// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sink"
	"github.com/latere-ai/origo/test/stubs/source"
)

// verifyResultOf reads a verify response into the document.
func verifyResultOf(t *testing.T, out map[string]any) VerifyResult {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var res VerifyResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// sourceOfCopy exports the repository the harness holds and adds it to
// the stub under name, so the two sides start identical.
func sourceOfCopy(t *testing.T, h *harness, stub *source.Server, id, name string) string {
	t.Helper()
	res := h.get("/v1/repos/" + id + "/export.bundle")
	if res.status != 200 {
		t.Fatalf("export: %d", res.status)
	}
	if err := stub.AddRepo(name, res.body); err != nil {
		t.Fatal(err)
	}
	return sourceURL(t, stub, name)
}

// TestVerifyDetectsADivergedReference is spec 014's first criterion:
// verify on an identical fixture answers equal with the counts equal
// and the reachable-object count of the copy, records the verdict in
// meta where GET /v1/repos/{id} serves it, and after one commit on the
// source answers not equal, naming the reference with both hashes.
func TestVerifyDetectsADivergedReference(t *testing.T) {
	f := loadFixture(t)
	stub, egress := importStub(t)
	s := sink.New(t)
	h := newHarness(t, egress, withSink(s), withNow(fixedClock()))
	h.as(auth.Principal{Subject: "alice", Actor: "svc"})
	h.seed(f)
	src := sourceOfCopy(t, h, stub, repoA, "mirror")

	// Never verified: the representation carries both fields as null.
	if _, out := h.do("GET", "/v1/repos/"+repoA, ""); out["verified_at"] != nil || out["verified_equal"] != nil {
		t.Fatalf("a repository never verified: %v", out)
	}

	body := `{"source":"` + src + `","token":"` + stub.Token() + `"}`
	status, out := h.do("POST", "/v1/repos/"+repoA+"/verify", body)
	if status != 200 {
		t.Fatalf("verify: %d %v", status, out)
	}
	res := verifyResultOf(t, out)
	if !res.Equal || len(res.Refs.Differing) != 0 {
		t.Fatalf("the two sides differ: %+v", res)
	}
	if res.Refs.Source == 0 || res.Refs.Source != res.Refs.Origo {
		t.Fatalf("reference counts %d and %d", res.Refs.Source, res.Refs.Origo)
	}
	if want := reachableObjects(t, f.Dir); res.Objects.Origo != want {
		t.Fatalf("verify counted %d objects, the fixture has %d", res.Objects.Origo, want)
	}
	if res.CheckedAt.IsZero() {
		t.Fatal("checked_at is not set")
	}

	// The verdict is in meta, which the representation serves.
	_, repr := h.do("GET", "/v1/repos/"+repoA, "")
	if repr["verified_at"] == nil || repr["verified_equal"] != true {
		t.Fatalf("the verdict is not on the representation: %v", repr)
	}

	// The event carries the response's own fields.
	ev := waitEvent(t, s, repoA, KindVerified)
	if ev["equal"] != true || ev["checked_at"] == nil {
		t.Fatalf("verified %v", ev)
	}
	refs, _ := ev["refs"].(map[string]any)
	objects, _ := ev["objects"].(map[string]any)
	if refs == nil || refs["source"] != float64(res.Refs.Source) || objects == nil || objects["origo"] != float64(res.Objects.Origo) {
		t.Fatalf("verified refs and objects: %v", ev)
	}

	// One commit on the source and the two sides differ on that
	// reference alone, with both hashes.
	before := res.Refs.Origo
	commit, err := stub.Commit("mirror", "main")
	if err != nil {
		t.Fatal(err)
	}
	status, out = h.do("POST", "/v1/repos/"+repoA+"/verify", body)
	if status != 200 {
		t.Fatalf("second verify: %d %v", status, out)
	}
	res = verifyResultOf(t, out)
	if res.Equal || len(res.Refs.Differing) != 1 {
		t.Fatalf("the late write was not detected: %+v", res)
	}
	d := res.Refs.Differing[0]
	if d.Name != "refs/heads/main" || d.Source != commit || d.Origo == "" || d.Origo == commit {
		t.Fatalf("differing: %+v, the source committed %s", d, commit)
	}
	if res.Refs.Source != before || res.Refs.Origo != before {
		t.Fatalf("a commit changed a reference count: %+v", res.Refs)
	}
	if _, repr := h.do("GET", "/v1/repos/"+repoA, ""); repr["verified_equal"] != false {
		t.Fatalf("the second verdict is not on the representation: %v", repr)
	}
}

// reachableObjects is rev-list --objects --all over a repository, the
// figure verify reports for Origo's copy.
func reachableObjects(t *testing.T, dir string) int64 {
	t.Helper()
	out := gittest.Run(t, dir, nil, "rev-list", "--objects", "--all")
	return countLines([]byte(out))
}

// TestVerifyRefusals covers what verify refuses before it reads either
// side: a body that is not the shape, a source that is not https, one
// the egress rules do not admit, one that does not answer, and a caller
// the authorizer gives no admin.
func TestVerifyRefusals(t *testing.T) {
	stub, egress := importStub(t)
	h := newHarness(t, egress)
	h.create(repoA, "acme", "app")

	for name, body := range map[string]string{
		"not json":  `{`,
		"no url":    `{"source":"not a url"}`,
		"not https": `{"source":"http://localhost/x.git"}`,
	} {
		if status, out := h.do("POST", "/v1/repos/"+repoA+"/verify", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("verify %s: %d %v", name, status, out)
		}
	}
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/verify", `{"source":"https://elsewhere.example/x.git"}`); status != 400 || details(out)["reason"] != "egress" {
		t.Fatalf("verify a host not on the list: %d %v", status, out)
	}
	// A source that answers nothing under that name is the caller's
	// input, so it is 400 with the step in the developer register.
	if status, out := h.do("POST", "/v1/repos/"+repoA+"/verify", `{"source":"`+sourceURL(t, stub, "absent")+`"}`); status != 400 ||
		code(out) != contract.CodeInvalid || details(out)["field"] != "source" {
		t.Fatalf("verify an absent repository: %d %v", status, out)
	}
	if status, out := h.do("POST", "/v1/repos/"+unknown+"/verify", `{"source":"`+sourceURL(t, stub, "fixture")+`"}`); status != 404 || code(out) != contract.CodeRepoNotFound {
		t.Fatalf("verify an unknown repository: %d %v", status, out)
	}
	h.authz.SetRules(authorizer.Rule{Allow: true}, authorizer.Rule{Subject: "eve", Allow: false, Reason: "no"})
	h.as(auth.Principal{Subject: "eve"})
	if status, _ := h.do("POST", "/v1/repos/"+repoA+"/verify", `{"source":"`+sourceURL(t, stub, "fixture")+`"}`); status != 403 {
		t.Fatalf("verify without admin: %d", status)
	}
}

// TestParseLsRemote holds the two lines the advertisement carries that
// name no comparable reference: the peeled line of an annotated tag,
// which names the tag's target, and HEAD, whose value on the other side
// is a name and not a hash.
func TestParseLsRemote(t *testing.T) {
	const out = "aaa\tHEAD\nbbb\trefs/heads/main\nccc\trefs/tags/v1\nddd\trefs/tags/v1^{}\n\n"
	refs := parseLsRemote(out)
	want := map[string]string{"refs/heads/main": "bbb", "refs/tags/v1": "ccc"}
	if len(refs) != len(want) {
		t.Fatalf("parsed %v", refs)
	}
	for name, sha := range want {
		if refs[name] != sha {
			t.Fatalf("%s is %q, want %q", name, refs[name], sha)
		}
	}
	if got := compareRefs(refs, map[string]string{"refs/heads/main": "bbb"}); got.Source != 2 || got.Origo != 1 ||
		len(got.Differing) != 1 || got.Differing[0].Name != "refs/tags/v1" || got.Differing[0].Origo != "" {
		t.Fatalf("compare: %+v", got)
	}
}

// logBuffer collects the node's lines for a test that reads them.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestSourceTokenIsNeverLogged is spec 014's third criterion, which
// covers the bearer of verify and the bearer of spec 019's import
// alike: neither reaches a log line, a process argument, or a URL the
// node builds, while the stub's request list shows every request
// carried it.
func TestSourceTokenIsNeverLogged(t *testing.T) {
	f := loadFixture(t)
	stub, egress := importStub(t)
	buf := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	spy := newSpyGit(t)
	s := sink.New(t)
	h := newHarness(t, egress, withSink(s), withLogger(logger), withGit(spy.bin()), withNow(fixedClock()))
	h.as(auth.Principal{Subject: "alice"})
	h.seed(f)
	src := sourceOfCopy(t, h, stub, repoA, "secrets")
	token := stub.Token()

	// The import of spec 019 and the verify of spec 014, both with the
	// bearer in the body.
	h.create(repoB, "acme", "fresh")
	body := `{"source":"` + src + `","token":"` + token + `"}`
	if status, out := h.do("POST", "/v1/repos/"+repoB+"/import", body); status != 202 {
		t.Fatalf("import: %d %v", status, out)
	}
	if st := h.waitImport(repoB); st.State != ImportDone {
		t.Fatalf("import state: %+v", st)
	}
	if status, out := h.do("POST", "/v1/repos/"+repoB+"/verify", body); status != 200 || out["equal"] != true {
		t.Fatalf("verify: %d %v", status, out)
	}

	// Nothing the node wrote carries the bearer, and no URL it built
	// carries a credential.
	lines := buf.String()
	if lines == "" {
		t.Fatal("the node wrote no line")
	}
	if strings.Contains(lines, token) {
		t.Fatalf("a log line carries the source bearer: %s", lines)
	}
	for _, call := range spy.calls() {
		if strings.Contains(call, token) {
			t.Fatalf("a git argument carries the source bearer: %q", call)
		}
	}
	// The stub saw the bearer on every request and records only that
	// it was carried.
	reqs := stub.Requests()
	if len(reqs) == 0 {
		t.Fatal("the stub saw no request")
	}
	for _, r := range reqs {
		if !r.Bearer {
			t.Fatalf("a request without the bearer: %+v", r)
		}
	}
	raw, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("the stub's request list carries the bearer: %s", raw)
	}
	// The event's source is the URL without credentials or a query.
	for _, kind := range []string{KindImported, KindVerified} {
		ev := waitEvent(t, s, repoB, kind)
		if raw, err := json.Marshal(ev); err != nil || strings.Contains(string(raw), token) {
			t.Fatalf("the %s event carries the bearer: %v %v", kind, ev, err)
		}
	}
	// A ls-remote of the source ran through the proxy's http:// URL,
	// never the https one, so the source's own host is not in an
	// argument either.
	var lsRemote string
	for _, call := range spy.calls() {
		if strings.Contains(call, " ls-remote ") {
			lsRemote = call
		}
	}
	if !strings.HasPrefix(lsRemote, "-c transfer.fsckObjects=true ls-remote --end-of-options http://") {
		t.Fatalf("verify's ls-remote was %q", lsRemote)
	}
}

// TestVerifyReadsTheCurrentCopy proves the comparison is against the
// log's newest state and not a stale copy: a push after a verification
// changes the answer on a second node that never served the push.
func TestVerifyReadsTheCurrentCopy(t *testing.T) {
	f := loadFixture(t)
	stub, egress := importStub(t)
	store := wal.NewMemStore()
	h := newHarness(t, egress, withStore(store), withNow(fixedClock()))
	h.seed(f)
	src := sourceOfCopy(t, h, stub, repoA, "second")

	other := newHarness(t, egress, withStore(store), withNow(fixedClock()))
	body := `{"source":"` + src + `","token":"` + stub.Token() + `"}`
	status, out := other.do("POST", "/v1/repos/"+repoA+"/verify", body)
	if status != 200 || out["equal"] != true {
		t.Fatalf("verify on a node that never served the repository: %d %v", status, out)
	}
	res := verifyResultOf(t, out)
	if res.Objects.Origo == 0 || res.Refs.Origo == 0 {
		t.Fatalf("the copy was not materialized from the log: %+v", res)
	}
}
