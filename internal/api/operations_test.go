// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/sink"
)

// The tests of spec 020. Every one of them runs against a small history
// pushed as one entry, so the node materializes the copy from the log
// the way it does for any repository.

// ops is one seeded repository the operations run against.
type ops struct {
	h   *harness
	id  string
	src *gittest.Source
	// main and feature are the tips of the two branches, and base the
	// commit they share.
	base, main, feature string
}

// seedOps creates the repository and commits a history with two
// branches as one push entry: main with a shared root and one commit of
// its own, feature with another commit over the same root.
func seedOps(t *testing.T, h *harness, id string) *ops {
	t.Helper()
	src := gittest.NewSource(t)
	o := &ops{h: h, id: id, src: src}
	o.base = src.Commit("README.md", "# fixture\n", "Initial commit")
	src.Commit("src/a.txt", "alpha\n", "Add a")
	o.main = src.Rev("HEAD")
	gittest.Run(t, src.Dir, nil, "branch", "feature", o.base)
	src.Checkout("feature")
	src.Commit("src/b.txt", "beta\n", "Add b")
	o.feature = src.Rev("HEAD")
	src.Checkout("main")

	ctx := context.Background()
	ix, err := h.log.CreateRepo(ctx, wal.Meta{ID: id, Owner: "acme", Slug: "app-" + id[len(id)-6:]}, "main")
	if err != nil {
		t.Fatal(err)
	}
	e := wal.Entry{Kind: wal.KindPush, Subject: "alice", Pack: wal.BytesBody(src.PackAll())}
	for name, sha := range src.Refs() {
		if name != "HEAD" {
			e.Refs = append(e.Refs, wal.RefUpdate{Ref: name, Old: wal.ZeroSHA, New: sha})
		}
	}
	if _, err := h.log.Commit(ctx, id, ix, e, noCatchUp); err != nil {
		t.Fatal(err)
	}
	return o
}

// post sends one operation and returns the status and the body.
func (o *ops) post(path, body string) (int, map[string]any) {
	o.h.t.Helper()
	return o.h.do("POST", "/v1/repos/"+o.id+"/"+path, body)
}

// b64 is a change's content on the wire.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// author is the author field every request carries.
const author = `"author":{"name":"Ada","email":"ada@example.com"}`

// newID makes a repository id of its own for a test, so the per
// repository bucket of one test never refuses another's operations.
func newID(n int) string {
	return fmt.Sprintf("2a2b3c4d-5e6f-4a7b-8c9d-%012d", n)
}

// entryHeader reads the header of the entry the newest index names.
func (o *ops) entryHeader() wal.Header {
	o.h.t.Helper()
	ctx := context.Background()
	ix, _, err := o.h.log.Newest(ctx, o.id, 0, false)
	if err != nil {
		o.h.t.Fatal(err)
	}
	rc, _, err := o.h.store.Get(ctx, o.h.log.RepoPrefix(o.id)+ix.Entry, "")
	if err != nil {
		o.h.t.Fatal(err)
	}
	hdr, _, _, err := wal.ReadEntryHead(rc)
	_ = rc.Close()
	if err != nil {
		o.h.t.Fatal(err)
	}
	return hdr
}

// commitJSON reads one commit of the repository through the read API of
// spec 009, which is how a consumer sees what the operation wrote.
func (o *ops) commit(sha string) map[string]any {
	o.h.t.Helper()
	r := o.h.get("/v1/repos/" + o.id + "/commits/" + sha)
	if r.status != 200 {
		o.h.t.Fatalf("read commit %s: %d %s", sha, r.status, r.body)
	}
	return r.json()
}

// TestCommitsWritesOneEntryAndRefusesStaleHead is spec 020's first
// criterion: three changes on a fixture branch produce one commit whose
// tree equals a client-side commit of the same changes, one entry
// carrying origo.operation=commits, and one push event with the
// operation; a stale expected_head is 409 and a request without an
// author is 400.
func TestCommitsWritesOneEntryAndRefusesStaleHead(t *testing.T) {
	s := sink.New(t)
	h := newHarness(t, withSink(s))
	o := seedOps(t, h, repoA)

	// The same three changes, made in a clone: add src/c.txt, replace
	// src/a.txt with the blob README.md already holds, and delete
	// README.md.
	blob := gittest.Run(t, o.src.Dir, nil, "rev-parse", "HEAD:README.md")
	o.src.Write("src/c.txt", []byte("gamma\n"))
	gittest.Run(t, o.src.Dir, nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+",src/a.txt")
	gittest.Run(t, o.src.Dir, nil, "rm", "-q", "--cached", "--", "README.md")
	gittest.Run(t, o.src.Dir, nil, "add", "--", "src/c.txt")
	wantTree := gittest.Run(t, o.src.Dir, nil, "write-tree")

	body := `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"Three changes","changes":[
		{"path":"src/c.txt","content":"` + b64("gamma\n") + `"},
		{"path":"src/a.txt","content_ref":"` + blob + `"},
		{"path":"README.md","delete":true}]}`
	status, out := o.post("commits", body)
	if status != 201 {
		t.Fatalf("commits: %d %v", status, out)
	}
	if out["tree"] != wantTree {
		t.Fatalf("tree %v, want %s", out["tree"], wantTree)
	}
	if out["branch"] != "refs/heads/main" || out["entry_seq"] != float64(2) || out["committed"] != nil {
		t.Fatalf("result: %v", out)
	}
	created, _ := out["commit"].(string)

	// The commit git wrote: the request's author, Origo as the
	// committer, and the subject in the trailer.
	c := o.commit(created)
	a, _ := c["author"].(map[string]any)
	cm, _ := c["committer"].(map[string]any)
	if a["name"] != "Ada" || a["email"] != "ada@example.com" {
		t.Fatalf("author: %v", a)
	}
	if cm["name"] != "Origo" || cm["email"] != "origo@git.example.com" {
		t.Fatalf("committer: %v", cm)
	}
	trailers, _ := c["trailers"].([]any)
	if len(trailers) != 1 || trailers[0].(map[string]any)["key"] != "Origo-Subject" || trailers[0].(map[string]any)["value"] != "alice" {
		t.Fatalf("trailers: %v", trailers)
	}

	// One entry, whose header carries the push option, and one push
	// event naming the operation.
	hdr := o.entryHeader()
	if len(hdr.PushOptions) != 1 || hdr.PushOptions[0] != "origo.operation=commits" {
		t.Fatalf("push options: %v", hdr.PushOptions)
	}
	delivered, ok := s.Wait(o.id, "push", 1, 10*time.Second)
	if !ok {
		t.Fatal("no push event")
	}
	var payload events.Push
	if err := json.Unmarshal(delivered[0].Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Operation != OpCommits || len(payload.Updates) != 1 || payload.Updates[0].After != created {
		t.Fatalf("push event: %+v", payload)
	}

	// A second request with the head the caller read before is refused.
	status, out = o.post("commits", body)
	if status != 409 || code(out) != contract.CodeNonFastForward {
		t.Fatalf("stale head: %d %v", status, out)
	}
	if details(out)["ref"] != "refs/heads/main" || details(out)["expected"] != o.main || details(out)["actual"] != created {
		t.Fatalf("stale details: %v", details(out))
	}

	// A request without an author names the field.
	status, out = o.post("commits", `{"branch":"main","expected_head":"`+created+`","message":"x","changes":[{"path":"x","content":"`+b64("x")+`"}]}`)
	if status != 400 || code(out) != contract.CodeInvalid || details(out)["field"] != "author" {
		t.Fatalf("no author: %d %v", status, out)
	}
}

// TestCreateBranchFromAndDryRunWritesNothing is spec 020's second
// criterion.
func TestCreateBranchFromAndDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	o := seedOps(t, h, repoA)

	body := func(branch string, dry bool) string {
		return `{"branch":"` + branch + `","create_branch":true,"from":"main","expected_head":null,` + author +
			`,"message":"New branch","dry_run":` + fmt.Sprint(dry) + `,"changes":[{"path":"n.txt","content":"` + b64("n\n") + `"}]}`
	}
	status, out := o.post("commits", body("release", false))
	if status != 201 {
		t.Fatalf("create branch: %d %v", status, out)
	}
	created, _ := out["commit"].(string)
	parents := o.commit(created)["parents"].([]any)
	if len(parents) != 1 || parents[0] != o.main {
		t.Fatalf("parent: %v, want %s", parents, o.main)
	}
	if r := h.get("/v1/repos/" + o.id + "/refs?prefix=refs/heads/release"); !strings.Contains(string(r.body), created) {
		t.Fatalf("branch not created: %s", r.body)
	}

	// The same request on a branch that exists is 409 with a null
	// expected.
	status, out = o.post("commits", body("release", false))
	if status != 409 || code(out) != contract.CodeNonFastForward || details(out)["expected"] != nil || details(out)["actual"] != created {
		t.Fatalf("existing branch: %d %v", status, out)
	}

	// create_branch without from names the field, and from without
	// create_branch names the other.
	status, out = o.post("commits", `{"branch":"x","create_branch":true,"expected_head":null,`+author+`,"message":"m","changes":[{"path":"a","content":"`+b64("a")+`"}]}`)
	if status != 400 || code(out) != contract.CodeInvalid || details(out)["field"] != "from" {
		t.Fatalf("no from: %d %v", status, out)
	}
	status, out = o.post("commits", `{"branch":"main","from":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"a","content":"`+b64("a")+`"}]}`)
	if status != 400 || code(out) != contract.CodeInvalid || details(out)["field"] != "create_branch" {
		t.Fatalf("from alone: %d %v", status, out)
	}

	// A dry run answers 200 with committed false and entry_seq null,
	// writes nothing into the repository's objects, and leaves no
	// directory under spool/.
	dir := filepath.Join(h.cache.Dir(), "repos", o.id+".git", "objects")
	before := treeOf(t, dir)
	status, out = o.post("commits", body("draft", true))
	if status != 200 || out["committed"] != false || out["entry_seq"] != nil || out["commit"] == "" {
		t.Fatalf("dry run: %d %v", status, out)
	}
	if after := treeOf(t, dir); after != before {
		t.Fatalf("a dry run wrote into objects/:\n%s\n%s", before, after)
	}
	entries, err := os.ReadDir(h.cache.SpoolDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a dry run left %d entries under spool/", len(entries))
	}
	ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
	if _, ok := ix.Refs["refs/heads/draft"]; ok {
		t.Fatal("a dry run created the branch")
	}
}

// treeOf lists every file under dir with its size, so a test compares a
// directory before and after.
func treeOf(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			fmt.Fprintf(&b, "%s %d\n", path, info.Size())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestMergeStrategiesAndConflict is spec 020's third criterion.
func TestMergeStrategiesAndConflict(t *testing.T) {
	h := newHarness(t)
	o := seedOps(t, h, repoA)

	// A branch behind feature fast-forwards: the branch moves and the
	// answer is the source's own sha, with no new commit.
	gittest.Run(t, o.src.Dir, nil, "branch", "behind", o.base)
	if err := pushRef(h, o, "refs/heads/behind", wal.ZeroSHA, o.base); err != nil {
		t.Fatal(err)
	}
	status, out := o.post("merge", `{"branch":"behind","expected_head":"`+o.base+`","source":"feature",`+author+`}`)
	if status != 201 || out["commit"] != o.feature {
		t.Fatalf("fast-forward: %d %v", status, out)
	}
	if len(o.commit(o.feature)["parents"].([]any)) != 1 {
		t.Fatal("the fast-forward wrote a commit")
	}

	// main and feature have diverged, so the same strategy writes a
	// merge commit with two parents.
	status, out = o.post("merge", `{"branch":"main","expected_head":"`+o.main+`","source":"feature",`+author+`}`)
	if status != 201 {
		t.Fatalf("merge: %d %v", status, out)
	}
	merge, _ := out["commit"].(string)
	c := o.commit(merge)
	parents := c["parents"].([]any)
	if len(parents) != 2 || parents[0] != o.main || parents[1] != o.feature {
		t.Fatalf("parents: %v", parents)
	}
	if !strings.HasPrefix(c["message"].(string), "Merge feature into main") {
		t.Fatalf("message: %q", c["message"])
	}

	// fast_forward_only over a diverged branch is refused.
	status, out = o.post("merge", `{"branch":"main","expected_head":"`+merge+`","source":"`+o.base+`","strategy":"fast_forward_only",`+author+`}`)
	if status != 409 || code(out) != contract.CodeNonFastForward {
		t.Fatalf("fast_forward_only: %d %v", status, out)
	}

	// A conflicting merge names the paths and leaves the branch where
	// it was.
	o.src.Checkout("feature")
	o.src.Commit("src/a.txt", "conflicting\n", "Change a on feature")
	clash := o.src.Rev("HEAD")
	if err := pushRef(h, o, "refs/heads/feature", o.feature, clash); err != nil {
		t.Fatal(err)
	}
	status, out = o.post("merge", `{"branch":"main","expected_head":"`+merge+`","source":"feature","strategy":"merge_commit",`+author+`}`)
	if status != 409 || code(out) != contract.CodeMergeConflict {
		t.Fatalf("conflict: %d %v", status, out)
	}
	paths, _ := details(out)["paths"].([]any)
	if len(paths) != 1 || paths[0] != "src/a.txt" {
		t.Fatalf("paths: %v", details(out))
	}
	ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
	if ix.Refs["refs/heads/main"] != merge {
		t.Fatalf("the branch moved: %s", ix.Refs["refs/heads/main"])
	}
}

// pushRef commits one reference update with the objects the source
// holds, the way a client push would.
func pushRef(h *harness, o *ops, ref, old, want string) error {
	ctx := context.Background()
	ix, _, err := h.log.Newest(ctx, o.id, 0, false)
	if err != nil {
		return err
	}
	var have []string
	for name, sha := range ix.Refs {
		// Only what the source itself holds is a have: a commit the
		// server wrote is not in this working repository.
		if name == "HEAD" || sha == "" {
			continue
		}
		if _, err := gittest.Try(o.src.Dir, nil, "cat-file", "-e", sha+"^{commit}"); err == nil {
			have = append(have, sha)
		}
	}
	e := wal.Entry{Kind: wal.KindPush, Subject: "alice", Refs: []wal.RefUpdate{{Ref: ref, Old: old, New: want}}}
	if pack := o.src.Pack(want, have...); len(pack) > 0 {
		e.Pack = wal.BytesBody(pack)
	}
	_, err = h.log.Commit(ctx, o.id, ix, e, noCatchUp)
	return err
}

// TestCherryPickIsAtomic is spec 020's fourth criterion: three commits
// of which the second conflicts commit nothing and name the second.
func TestCherryPickIsAtomic(t *testing.T) {
	h := newHarness(t)
	o := seedOps(t, h, repoA)

	// Three commits on feature: the second changes the file main
	// changed, so it conflicts when picked onto main.
	o.src.Checkout("feature")
	one := o.src.Commit("src/one.txt", "one\n", "Pick one")
	two := o.src.Commit("src/a.txt", "conflicting\n", "Pick two")
	three := o.src.Commit("src/three.txt", "three\n", "Pick three")
	if err := pushRef(h, o, "refs/heads/feature", o.feature, three); err != nil {
		t.Fatal(err)
	}

	status, out := o.post("cherry-pick", `{"branch":"main","expected_head":"`+o.main+`","commits":["`+one+`","`+two+`","`+three+`"],`+author+`}`)
	if status != 409 || code(out) != contract.CodeMergeConflict {
		t.Fatalf("conflict: %d %v", status, out)
	}
	if details(out)["commit"] != two {
		t.Fatalf("commit: %v, want %s", details(out)["commit"], two)
	}
	ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
	if ix.Refs["refs/heads/main"] != o.main || ix.Seq != 2 {
		t.Fatalf("something was committed: %d %s", ix.Seq, ix.Refs["refs/heads/main"])
	}

	// The two that do not conflict land as two commits in one entry.
	status, out = o.post("cherry-pick", `{"branch":"main","expected_head":"`+o.main+`","commits":["`+one+`","`+three+`"],`+author+`}`)
	if status != 201 || out["entry_seq"] != float64(3) {
		t.Fatalf("cherry-pick: %d %v", status, out)
	}
	tip, _ := out["commit"].(string)
	c := o.commit(tip)
	if !strings.HasPrefix(c["message"].(string), "Pick three\n") {
		t.Fatalf("message: %q", c["message"])
	}
	parent := c["parents"].([]any)[0].(string)
	if !strings.HasPrefix(o.commit(parent)["message"].(string), "Pick one\n") {
		t.Fatal("the picks are not in order")
	}
	if o.commit(parent)["parents"].([]any)[0] != o.main {
		t.Fatal("the first pick is not on the branch")
	}

	// A revert of the tip undoes it and leaves the tree of its parent.
	status, out = o.post("revert", `{"branch":"main","expected_head":"`+tip+`","commits":["`+tip+`"],`+author+`}`)
	if status != 201 {
		t.Fatalf("revert: %d %v", status, out)
	}
	if out["tree"] != o.commit(parent)["sha"] && out["tree"] == "" {
		t.Fatalf("revert result: %v", out)
	}
	reverted := o.commit(out["commit"].(string))
	if !strings.HasPrefix(reverted["message"].(string), `Revert "Pick three"`) {
		t.Fatalf("revert message: %q", reverted["message"])
	}
}

// TestServerSideOperationsHonourLimits is spec 020's fifth criterion.
func TestServerSideOperationsHonourLimits(t *testing.T) {
	// The clock stands still, so the repository's bucket refills for no
	// request of the sweep below and the 61st is the one that is
	// refused.
	h := newHarness(t, withLimits(limits.Options{}), withNow(fixedClock()))
	o := seedOps(t, h, repoA)

	// A pack the authorizer's quota_bytes cannot hold is over_quota and
	// commits nothing.
	h.authz.Allow(authorizer.Rule{Repo: repoA, Action: "write", QuotaBytes: 1})
	status, out := o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"q.txt","content":"`+b64(strings.Repeat("q", 4096))+`"}]}`)
	if status != 413 || code(out) != contract.CodeOverQuota || details(out)["limit"] != limits.LimitRepository {
		t.Fatalf("over quota: %d %v", status, out)
	}
	ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
	if ix.Seq != 1 {
		t.Fatalf("the refusal committed an entry: %d", ix.Seq)
	}
	h.authz.SetRules()

	// 1 001 changes, one 11 MiB file, and six 10 MiB files, each with
	// the code and the reason its row names.
	var many strings.Builder
	for i := range MaxChanges + 1 {
		if i > 0 {
			many.WriteString(",")
		}
		fmt.Fprintf(&many, `{"path":"f%04d","content":"%s"}`, i, b64("x"))
	}
	status, out = o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[`+many.String()+`]}`)
	if status != 400 || code(out) != contract.CodeInvalidChange || details(out)["reason"] != ReasonTooMany {
		t.Fatalf("too many: %d %v", status, out)
	}
	big := b64(strings.Repeat("b", MaxContentBytes+1))
	status, out = o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"big","content":"`+big+`"}]}`)
	if status != 400 || code(out) != contract.CodeInvalidChange || details(out)["reason"] != ReasonTooLarge || details(out)["index"] != float64(0) {
		t.Fatalf("too large: %d %v", status, out)
	}
	// Six 10 MiB files are 80 MiB of base64, which the body limit
	// refuses before any change is read.
	var six strings.Builder
	one := b64(strings.Repeat("s", MaxContentBytes))
	for i := range 6 {
		if i > 0 {
			six.WriteString(",")
		}
		fmt.Fprintf(&six, `{"path":"s%d","content":"%s"}`, i, one)
	}
	status, out = o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[`+six.String()+`]}`)
	if status != 400 || code(out) != contract.CodeInvalid {
		t.Fatalf("body limit: %d %v", status, out)
	}

	// The 61st operation on one repository in a minute is refused, and
	// another repository is not; each half runs on a repository whose
	// bucket no earlier request of this test has drawn on.
	busy := seedOps(t, h, newID(2))
	other := seedOps(t, h, newID(3))
	miss := func(r *ops) string {
		return `{"branch":"main","expected_head":"` + r.main + `","source":"nosuch",` + author + `}`
	}
	for i := range OperationsPerMinute {
		if status, out := busy.post("merge", miss(busy)); status != 404 {
			t.Fatalf("operation %d: %d %v", i, status, out)
		}
	}
	status, out = busy.post("merge", miss(busy))
	if status != 429 || code(out) != contract.CodeRateLimited || details(out)["limit"] != limits.LimitRepository {
		t.Fatalf("rate limit: %d %v", status, out)
	}
	if _, _, header := h.doHeader("POST", "/v1/repos/"+busy.id+"/merge", miss(busy)); header.Get("Retry-After") == "" {
		t.Fatalf("no Retry-After: %v", header)
	}
	if status, out := other.post("merge", miss(other)); status != 404 {
		t.Fatalf("another repository was refused: %d %v", status, out)
	}
}

// TestChangePathsUseTheReadRules is spec 020's sixth criterion: a path
// the rules of spec 009 refuse and an empty one are invalid_change with
// the index and the reason, and no subprocess starts.
func TestChangePathsUseTheReadRules(t *testing.T) {
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()))
	o := seedOps(t, h, repoA)
	// The copy is materialized by the first request; the sweep counts
	// from what that cost.
	if status, out := o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"warm","changes":[{"path":"w.txt","content":"`+b64("w")+`"}]}`); status != 201 {
		t.Fatalf("warm: %d %v", status, out)
	}
	spy.reset()
	before := len(spy.calls())
	for _, path := range []string{"", ".git/config", "a/../b", "/etc/passwd", "a//b", "x/.GIT/y", strings.Repeat("p", MaxPathBytes+1)} {
		body, err := json.Marshal(map[string]any{
			"branch": "main", "expected_head": o.main, "message": "m",
			"author":  map[string]string{"name": "Ada", "email": "ada@example.com"},
			"changes": []any{map[string]any{"path": "ok.txt", "content": b64("ok")}, map[string]any{"path": path, "content": b64("x")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		status, out := o.post("commits", string(body))
		if status != 400 || code(out) != contract.CodeInvalidChange {
			t.Fatalf("path %q: %d %v", path, status, out)
		}
		if details(out)["reason"] != ReasonPath || details(out)["index"] != float64(1) {
			t.Fatalf("path %q details: %v", path, details(out))
		}
	}
	// A symbolic link's target is checked by the same rules.
	status, out := o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"link","mode":"120000","content":"`+b64("../etc/passwd")+`"}]}`)
	if status != 400 || code(out) != contract.CodeInvalidChange || details(out)["reason"] != ReasonPath {
		t.Fatalf("symlink target: %d %v", status, out)
	}
	if after := len(spy.calls()); after != before {
		t.Fatalf("%d subprocesses started for a refused change", after-before)
	}
}

// TestConcurrentCommitsSerializeOnExpectedHead is spec 020's seventh
// criterion: twenty requests on one branch with the same expected_head
// produce one commit and nineteen non_fast_forward answers.
func TestConcurrentCommitsSerializeOnExpectedHead(t *testing.T) {
	h := newHarness(t)
	o := seedOps(t, h, repoA)
	const n = 20
	var wg sync.WaitGroup
	results := make([]int, n)
	codes := make([]string, n)
	for i := range n {
		wg.Go(func() {
			body := fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"c%d","changes":[{"path":"c%d.txt","content":%q}]}`,
				o.main, author, i, i, b64(fmt.Sprintf("%d\n", i)))
			results[i], codes[i] = statusCode(h, "/v1/repos/"+o.id+"/commits", body)
		})
	}
	wg.Wait()
	created, refused := 0, 0
	for i := range n {
		switch {
		case results[i] == 201:
			created++
		case results[i] == 409 && codes[i] == contract.CodeNonFastForward:
			refused++
		default:
			t.Errorf("request %d: %d %s", i, results[i], codes[i])
		}
	}
	if created != 1 || refused != n-1 {
		t.Fatalf("%d created, %d refused", created, refused)
	}
	ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
	if ix.Seq != 2 {
		t.Fatalf("the log holds %d entries", ix.Seq)
	}
}

func statusCode(h *harness, path, body string) (int, string) {
	status, out := h.do("POST", path, body)
	return status, code(out)
}

// TestOperationRefusalsAndBudget covers the refusals the criteria above
// do not reach: a frozen repository, an unknown branch, a body that is
// not a request, a merge of an unknown source, a mainline that does not
// fit the commit, a content_ref the repository does not hold, and the
// budget of a merge that runs long.
func TestOperationRefusalsAndBudget(t *testing.T) {
	h := newHarness(t)
	o := seedOps(t, h, repoA)

	for name, body := range map[string]string{
		"not json":       `{`,
		"unknown field":  `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[],"x":1}`,
		"bad branch":     `{"branch":"a..b","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"option branch":  `{"branch":"--upload-pack=x","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"bad head":       `{"branch":"main","expected_head":"nope",` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"no head":        `{"branch":"main",` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"no changes":     `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[]}`,
		"no message":     `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"bad email":      `{"branch":"main","expected_head":"` + o.main + `","author":{"name":"A","email":"a"},"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"long message":   `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"` + strings.Repeat("m", MaxMessageBytes+1) + `","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"head on create": `{"branch":"x","create_branch":true,"from":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
		"from option":    `{"branch":"x","create_branch":true,"from":"-x","expected_head":null,` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`,
	} {
		if status, out := o.post("commits", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	for name, body := range map[string]string{
		"bad mode":      `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","mode":"040000","content":"` + b64("a") + `"}]}`,
		"delete a mode": `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","mode":"100644","delete":true}]}`,
		"two forms":     `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `","delete":true}]}`,
		"no form":       `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a"}]}`,
		"bad base64":    `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","content":"!!!"}]}`,
		"bad ref":       `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","content_ref":"zz"}]}`,
	} {
		if status, out := o.post("commits", body); status != 400 || code(out) != contract.CodeInvalidChange {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}

	// A branch the repository does not hold is ref_not_found.
	if status, out := o.post("commits", `{"branch":"nosuch","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"a","content":"`+b64("a")+`"}]}`); status != 404 || code(out) != contract.CodeRefNotFound {
		t.Fatalf("unknown branch: %d %v", status, out)
	}
	// A content_ref the repository does not hold is invalid_change.
	missing := strings.Repeat("a", 40)
	if status, out := o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"a","content_ref":"`+missing+`"}]}`); status != 400 || code(out) != contract.CodeInvalidChange || details(out)["reason"] != ReasonContent {
		t.Fatalf("missing content_ref: %d %v", status, out)
	}
	// A merge of a source the repository does not hold is ref_not_found.
	if status, out := o.post("merge", `{"branch":"main","expected_head":"`+o.main+`","source":"nosuch",`+author+`}`); status != 404 || code(out) != contract.CodeRefNotFound {
		t.Fatalf("unknown source: %d %v", status, out)
	}
	for name, body := range map[string]string{
		"bad strategy": `{"branch":"main","expected_head":"` + o.main + `","source":"feature","strategy":"rebase",` + author + `}`,
		"no source":    `{"branch":"main","expected_head":"` + o.main + `",` + author + `}`,
	} {
		if status, out := o.post("merge", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("merge %s: %d %v", name, status, out)
		}
	}
	for name, body := range map[string]string{
		"no commits":   `{"branch":"main","expected_head":"` + o.main + `","commits":[],` + author + `}`,
		"bad commit":   `{"branch":"main","expected_head":"` + o.main + `","commits":["x"],` + author + `}`,
		"bad mainline": `{"branch":"main","expected_head":"` + o.main + `","commits":["` + o.feature + `"],"mainline":-1,` + author + `}`,
		"no merge":     `{"branch":"main","expected_head":"` + o.main + `","commits":["` + o.feature + `"],"mainline":2,` + author + `}`,
	} {
		if status, out := o.post("cherry-pick", body); status != 400 || code(out) != contract.CodeInvalid {
			t.Errorf("cherry-pick %s: %d %v", name, status, out)
		}
	}
	if status, out := o.post("cherry-pick", `{"branch":"main","expected_head":"`+o.main+`","commits":["`+strings.Repeat("b", 40)+`"],`+author+`}`); status != 404 {
		t.Fatalf("unknown pick: %d %v", status, out)
	}
	if status, out := o.post("revert", `{"branch":"main","expected_head":"`+o.main+`","commits":["`+o.base+`"],`+author+`}`); status != 400 {
		t.Fatalf("root revert: %d %v", status, out)
	}

	// A frozen repository refuses every operation with repo_frozen.
	if status, out := h.do("POST", "/v1/repos/"+o.id+"/freeze", ""); status != 200 {
		t.Fatalf("freeze: %d %v", status, out)
	}
	if status, out := o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"a","content":"`+b64("a")+`"}]}`); status != 403 || code(out) != contract.CodeRepoFrozen {
		t.Fatalf("frozen: %d %v", status, out)
	}
}

// TestOperationBudgetIsAnswered holds the 504 of an operation whose
// subprocesses run past the budget, with the seconds of the row.
func TestOperationBudgetIsAnswered(t *testing.T) {
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()))
	o := seedOps(t, h, repoA)
	body := `{"branch":"main","expected_head":"` + o.main + `",` + author + `,"message":"m","changes":[{"path":"a","content":"` + b64("a") + `"}]}`
	// The copy is materialized by a request that answers, and it runs
	// under the default budget: materializing a repository is work the
	// budget below is not measuring, and a loaded machine takes seconds
	// over it. The short budget is set after it, for the one request
	// this test is about, where the fake git holds read-tree open until
	// the deadline cuts it. Requests here are sequential, so the field
	// is read by no other goroutine while it is written.
	status, out := o.post("commits", body)
	if status != 201 {
		t.Fatalf("warm: %d %v", status, out)
	}
	h.handler.readTimeout = 2 * time.Second
	spy.hold("read-tree")
	next := `{"branch":"main","expected_head":"` + out["commit"].(string) + `",` + author + `,"message":"m","changes":[{"path":"b","content":"` + b64("b") + `"}]}`
	status, out = o.post("commits", next)
	if status != 504 || code(out) != contract.CodeOperationTimeout {
		t.Fatalf("budget: %d %v", status, out)
	}
	if details(out)["operation"] != OpCommits || details(out)["budget_seconds"] != float64(2) {
		t.Fatalf("details: %v", details(out))
	}
}

// FuzzOperationBody is spec 020's eighth criterion: no request body
// panics a handler, whatever the mutation produced.
func FuzzOperationBody(f *testing.F) {
	f.Add("commits", `{"branch":"main","expected_head":"`+strings.Repeat("a", 40)+`","author":{"name":"A","email":"a@b"},"message":"m","changes":[{"path":"a","content":"YQ=="}]}`)
	f.Add("commits", `{"branch":"x","create_branch":true,"from":null,"expected_head":null,"author":{"name":"A","email":"a@b"},"message":"m","changes":[{"path":"a","delete":true}]}`)
	f.Add("merge", `{"branch":"main","expected_head":"`+strings.Repeat("a", 40)+`","source":"feature","strategy":"merge_commit","author":{"name":"A","email":"a@b"}}`)
	f.Add("cherry-pick", `{"branch":"main","expected_head":"`+strings.Repeat("a", 40)+`","commits":["`+strings.Repeat("b", 40)+`"],"mainline":1,"author":{"name":"A","email":"a@b"}}`)
	f.Add("revert", `{"branch":"main","expected_head":null,"commits":[],"author":null,"message":"","dry_run":true}`)
	f.Add("commits", `{}`)
	f.Add("merge", ``)

	f.Fuzz(func(t *testing.T, route, body string) {
		switch route {
		case OpCommits, OpMerge, OpCherryPick, OpRevert:
		default:
			return
		}
		h := newHarness(t)
		o := seedOps(t, h, repoA)
		status, _ := o.post(route, body)
		if status < 200 || status >= 600 {
			t.Fatalf("%s: status %d", route, status)
		}
	})
}

// TestAuthorizerRateBucketsTheSubject is spec 012's item, which spec
// 020 carries: a subject the authorizer names a requests_per_minute for
// is bucketed at that figure, recorded wherever a decision arrives, and
// every other subject stays on the node's own figure.
func TestAuthorizerRateBucketsTheSubject(t *testing.T) {
	h := newHarness(t, withLimits(limits.Options{}))
	o := seedOps(t, h, repoA)
	if h.limits.SubjectRate("alice") != limits.RequestsPerMinute {
		t.Fatalf("before any decision alice is at %d", h.limits.SubjectRate("alice"))
	}
	h.authz.Allow(authorizer.Rule{Subject: "alice", RequestsPerMinute: 6000})
	if status, out := h.do("GET", "/v1/repos/"+o.id, ""); status != 200 {
		t.Fatalf("read: %d %v", status, out)
	}
	if h.limits.SubjectRate("alice") != 6000 {
		t.Fatalf("alice is at %d, want the authorizer's 6000", h.limits.SubjectRate("alice"))
	}

	// The smart HTTP surface reads the same decision.
	h.as(auth.Principal{Subject: "bob"})
	h.authz.Allow(authorizer.Rule{Subject: "bob", RequestsPerMinute: 1200})
	if r := h.get("/r/" + o.id + ".git/info/refs?service=git-upload-pack"); r.status != 200 {
		t.Fatalf("advertisement: %d %s", r.status, r.body)
	}
	if h.limits.SubjectRate("bob") != 1200 {
		t.Fatalf("bob is at %d, want the authorizer's 1200", h.limits.SubjectRate("bob"))
	}

	// A subject the authorizer names no figure for keeps the node's.
	h.as(auth.Principal{Subject: "carol"})
	if status, _ := h.do("GET", "/v1/repos/"+o.id, ""); status != 200 {
		t.Fatal("carol's read")
	}
	if h.limits.SubjectRate("carol") != limits.RequestsPerMinute {
		t.Fatalf("carol is at %d", h.limits.SubjectRate("carol"))
	}
	h.as(auth.Principal{Subject: "alice"})
}

// TestAuthorizerRateIsWhatRateLimitLimitReports is spec 012's header
// row through a real decision: with the bucket middleware in front of
// the mux, the figure on a response is the one the authorizer named for
// that subject, not the node's ORIGO_REQUESTS_PER_MINUTE. Reporting the
// node's figure to a subject on an override is what let spec 021's
// rate_limited case send 2401 requests against a budget of 6000 in the
// v0.1.2 release run.
func TestAuthorizerRateIsWhatTheRateLimitHeadersReport(t *testing.T) {
	h := newHarness(t, withLimits(limits.Options{}), withBucketed())
	o := seedOps(t, h, repoA)

	// A subject the authorizer names no figure for reads the node's
	// figure. dave rather than alice, because the guard caches a
	// decision per subject, action and repository, and alice's read is
	// the one that must carry the rate below.
	node := strconv.Itoa(limits.RequestsPerMinute)
	h.as(auth.Principal{Subject: "dave"})
	for range 2 {
		status, _, header := h.doHeader("GET", "/v1/repos/"+o.id, "")
		if status != 200 || header.Get(contract.HeaderRateLimit) != node {
			t.Fatalf("without an override: %d, %s %q", status, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
		}
	}
	h.as(auth.Principal{Subject: "alice"})

	// The authorizer names alice a rate. The handler records it after
	// the middleware has answered, so the response that carries the
	// decision still reports the node's figure and the next one reports
	// the override; spec 012 records that one-response window.
	h.authz.Allow(authorizer.Rule{Subject: "alice", RequestsPerMinute: 6000})
	status, _, header := h.doHeader("GET", "/v1/repos/"+o.id, "")
	if status != 200 || header.Get(contract.HeaderRateLimit) != node {
		t.Fatalf("the response that carries the decision: %d, %s %q", status, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
	}
	if status, _, header = h.doHeader("GET", "/v1/repos/"+o.id, ""); status != 200 || header.Get(contract.HeaderRateLimit) != "6000" {
		t.Fatalf("after the decision: %d, %s %q", status, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
	}
	// RateLimit-Remaining is measured against the figure beside it, so
	// what is left is near the override's depth and not near the node's.
	left, err := strconv.Atoi(header.Get(contract.HeaderRateRemaining))
	if err != nil || left <= limits.RequestsPerMinute || left >= 6000 {
		t.Fatalf("%s is %q against a limit of 6000", contract.HeaderRateRemaining, header.Get(contract.HeaderRateRemaining))
	}
	// It falls as the subject spends.
	if _, _, header = h.doHeader("GET", "/v1/repos/"+o.id, ""); header.Get(contract.HeaderRateRemaining) != strconv.Itoa(left-1) {
		t.Fatalf("%s went %d then %q", contract.HeaderRateRemaining, left, header.Get(contract.HeaderRateRemaining))
	}
	if h.limits.SubjectRate("alice") != 6000 {
		t.Fatalf("alice is bucketed at %d", h.limits.SubjectRate("alice"))
	}

	// dave still reads the node's figure with alice's override beside
	// it: the header is the subject's, not the table's last answer.
	h.as(auth.Principal{Subject: "dave"})
	if status, _, header = h.doHeader("GET", "/v1/repos/"+o.id, ""); status != 200 || header.Get(contract.HeaderRateLimit) != node {
		t.Fatalf("dave beside the override: %d, %s %q", status, contract.HeaderRateLimit, header.Get(contract.HeaderRateLimit))
	}
	h.as(auth.Principal{Subject: "alice"})
}

// TestOperationsOnAnEmptyRepositoryAndAnActor covers the first commit
// of a repository with no history, the actor trailer, and the refusal
// of from: null on a repository that already has one.
func TestOperationsOnAnEmptyRepositoryAndAnActor(t *testing.T) {
	h := newHarness(t)
	h.as(auth.Principal{Subject: "alice", Actor: "svc"})
	empty := newID(7)
	if status, out := h.do("POST", "/v1/repos", `{"id":"`+empty+`","owner":"acme","slug":"empty"}`); status != 201 {
		t.Fatalf("create: %d %v", status, out)
	}
	body := `{"branch":"main","create_branch":true,"from":null,"expected_head":null,` + author +
		`,"message":"First","changes":[{"path":"README.md","content":"` + b64("# first\n") + `"}]}`
	status, out := h.do("POST", "/v1/repos/"+empty+"/commits", body)
	if status != 201 {
		t.Fatalf("first commit: %d %v", status, out)
	}
	r := h.get("/v1/repos/" + empty + "/commits/" + out["commit"].(string))
	first := r.json()
	if len(first["parents"].([]any)) != 0 {
		t.Fatalf("the first commit has parents: %v", first["parents"])
	}
	trailers, _ := first["trailers"].([]any)
	if len(trailers) != 2 || trailers[1].(map[string]any)["key"] != "Origo-Actor" || trailers[1].(map[string]any)["value"] != "svc" {
		t.Fatalf("trailers: %v", trailers)
	}
	// The same request on a repository that has history is refused.
	o := seedOps(t, h, repoA)
	status, out = o.post("commits", `{"branch":"other","create_branch":true,"from":null,"expected_head":null,`+author+`,"message":"m","changes":[{"path":"a","content":"`+b64("a")+`"}]}`)
	if status != 400 || code(out) != contract.CodeInvalid || details(out)["field"] != "from" {
		t.Fatalf("from null with history: %d %v", status, out)
	}
	h.as(auth.Principal{Subject: "alice"})
}

// TestOperationsRefuseAPackPastThePushLimit holds the single-push bound
// of spec 012 on an operation's own pack.
func TestOperationsRefuseAPackPastThePushLimit(t *testing.T) {
	h := newHarness(t, withLimits(limits.Options{MaxPushBytes: 1}))
	o := seedOps(t, h, repoA)
	status, out := o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"a","content":"`+b64("a")+`"}]}`)
	if status != 413 || code(out) != contract.CodeOverQuota || details(out)["limit"] != limits.LimitPush {
		t.Fatalf("push limit: %d %v", status, out)
	}
}

// TestOperationsSurviveGitFailures: a failed step before the commit is
// 503 and writes nothing, and a branch the copy could not move after
// the entry landed still answers 201, because the log is the truth and
// the next currency check reconciles the reference.
func TestOperationsSurviveGitFailures(t *testing.T) {
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()))
	o := seedOps(t, h, repoA)
	body := func(head, path string) string {
		return `{"branch":"main","expected_head":"` + head + `",` + author + `,"message":"m","changes":[{"path":"` + path + `","content":"` + b64("x") + `"}]}`
	}
	// The copy is materialized first, so the failures below are the
	// operation's own subprocesses.
	status, out := o.post("commits", body(o.main, "warm.txt"))
	if status != 201 {
		t.Fatalf("warm: %d %v", status, out)
	}
	head := out["commit"].(string)

	for _, step := range []string{"write-tree", "pack-objects", "index-pack"} {
		spy.failOn(step)
		status, out := o.post("commits", body(head, step+".txt"))
		if status != 503 || code(out) != contract.CodeStorageUnavailable {
			t.Fatalf("%s: %d %v", step, status, out)
		}
		ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
		if ix.Refs["refs/heads/main"] != head {
			t.Fatalf("%s: the branch moved", step)
		}
	}
	spy.failOn("update-ref")
	status, out = o.post("commits", body(head, "moved.txt"))
	if status != 201 {
		t.Fatalf("update-ref: %d %v", status, out)
	}
	spy.failOn("none")
	// The log holds the commit and the next read of the repository sees
	// the reference, reconciled from the index.
	ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
	if ix.Refs["refs/heads/main"] != out["commit"] {
		t.Fatalf("the entry did not move the reference: %v", ix.Refs["refs/heads/main"])
	}
	if r := h.get("/v1/repos/" + o.id + "/refs?prefix=refs/heads/main"); !strings.Contains(string(r.body), out["commit"].(string)) {
		t.Fatalf("the copy was not reconciled: %s", r.body)
	}
}

// TestRevertOfAMergeTakesTheMainline covers the mainline of a merge
// commit and a dry run of a merge.
func TestRevertOfAMergeTakesTheMainline(t *testing.T) {
	h := newHarness(t)
	o := seedOps(t, h, repoA)
	// A branch with a merge commit on it, pushed as a client would.
	gittest.Run(t, o.src.Dir, nil, "checkout", "-q", "-b", "mline", o.main)
	o.src.Commit("mm.txt", "mm\n", "On mline")
	merged := o.src.Merge("feature", "Merge feature into mline")
	if err := pushRef(h, o, "refs/heads/mline", wal.ZeroSHA, merged); err != nil {
		t.Fatal(err)
	}
	status, out := o.post("revert", `{"branch":"mline","expected_head":"`+merged+`","commits":["`+merged+`"],"mainline":1,`+author+`}`)
	if status != 201 {
		t.Fatalf("revert of a merge: %d %v", status, out)
	}
	// The revert undid the second parent's file.
	r := h.get("/v1/repos/" + o.id + "/tree/-?ref=refs/heads/mline")
	if strings.Contains(string(r.body), "src/b.txt") {
		t.Fatalf("the merge was not reverted: %s", r.body)
	}

	// A dry run of a merge answers without committing.
	seq := func() uint64 {
		ix, _, _ := h.log.Newest(context.Background(), o.id, 0, false)
		return ix.Seq
	}
	before := seq()
	status, out = o.post("merge", `{"branch":"main","expected_head":"`+o.main+`","source":"feature","strategy":"merge_commit","dry_run":true,`+author+`}`)
	if status != 200 || out["committed"] != false || out["entry_seq"] != nil {
		t.Fatalf("dry merge: %d %v", status, out)
	}
	if seq() != before {
		t.Fatal("a dry merge committed an entry")
	}
	o.src.Checkout("main")
}

// TestOperationsOnADeletedRepository answers 404 before any work.
func TestOperationsOnADeletedRepository(t *testing.T) {
	h := newHarness(t)
	o := seedOps(t, h, repoA)
	if status, out := h.do("DELETE", "/v1/repos/"+o.id, ""); status != 202 {
		t.Fatalf("delete: %d %v", status, out)
	}
	if status, out := o.post("commits", `{"branch":"main","expected_head":"`+o.main+`",`+author+`,"message":"m","changes":[{"path":"a","content":"`+b64("a")+`"}]}`); status != 404 {
		t.Fatalf("deleted: %d %v", status, out)
	}
}

// TestOperationErrorsCarryTheirText holds the developer sentence of the
// two failures the operations raise inside themselves.
func TestOperationErrorsCarryTheirText(t *testing.T) {
	if got := (&changeError{index: 2, reason: ReasonPath}).Error(); !strings.Contains(got, ReasonPath) {
		t.Errorf("changeError: %q", got)
	}
	if got := (&mergeConflict{commit: "abc", paths: []string{"a", "b"}}).Error(); !strings.Contains(got, "a, b") {
		t.Errorf("mergeConflict: %q", got)
	}
}

// TestPicksNameNoMergeBaseOption holds the fix for the defect the stack
// found: git merge-tree learned --merge-base after the runtime image's
// git, so a cherry-pick and a revert express the base by re-parenting
// each side onto it and pass no such option. The sweep reads every
// invocation the operation made.
func TestPicksNameNoMergeBaseOption(t *testing.T) {
	spy := newSpyGit(t)
	h := newHarness(t, withGit(spy.bin()))
	o := seedOps(t, h, repoA)
	o.src.Checkout("feature")
	picked := o.src.Commit("src/p.txt", "picked\n", "Pick me")
	if err := pushRef(h, o, "refs/heads/feature", o.feature, picked); err != nil {
		t.Fatal(err)
	}
	o.src.Checkout("main")
	// The copy is materialized first, so the sweep sees the
	// operation's own subprocesses alone.
	if status, out := h.do("GET", "/v1/repos/"+o.id+"/refs", ""); status != 200 {
		t.Fatalf("warm: %d %v", status, out)
	}
	spy.reset()
	status, out := o.post("cherry-pick", `{"branch":"main","expected_head":"`+o.main+`","commits":["`+picked+`"],`+author+`}`)
	if status != 201 {
		t.Fatalf("cherry-pick: %d %v", status, out)
	}
	merges := 0
	for _, call := range spy.calls() {
		if !strings.Contains(call, "merge-tree") {
			continue
		}
		merges++
		if strings.Contains(call, "--merge-base") {
			t.Fatalf("the pick asked git for an option its runtime may not have: %s", call)
		}
	}
	if merges == 0 {
		t.Fatal("the pick ran no merge-tree")
	}
}
