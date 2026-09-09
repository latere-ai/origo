// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
)

// The rows of spec 020's Operations table, each on its success path
// against a repository the case pushes, with the two codes the spec
// defines and the non_fast_forward of a stale expected_head.

func cases020() []testCase {
	return []testCase{
		{name: "commits", run: case020Commits},
		{name: "merge", run: case020Merge},
		{name: "cherry-pick", run: case020CherryPick},
		{name: "revert", run: case020Revert},
	}
}

// author is the identity every operation names.
const author = `"author":{"name":"Conformance","email":"conformance@example.com"}`

// operation posts one operation and answers the response.
func (s *session) operation(t *testing.T, id, name, body string) response {
	t.Helper()
	return s.call(t, "POST", "/v1/repos/"+id+"/"+name, body)
}

// expectOperation asserts a 201 with the response fields and answers
// the commit.
func expectOperation(t *testing.T, r response, branch string) string {
	t.Helper()
	expectStatus(t, r, http.StatusCreated)
	commit := str(r.json["commit"])
	failIf(t, len(commit) != 40 || r.json["branch"] != branch || r.json["entry_seq"] == nil || len(str(r.json["tree"])) != 40, "operation response: %s", r.body)
	return commit
}

// operationRepo is a repository with one pushed commit on main and a
// clone of it.
func (s *session) operationRepo(t *testing.T, label string) (id, head, work string) {
	t.Helper()
	id = s.create(t, label)
	work = clone(t, s.repoURL(id))
	head = commitFile(t, work, "a.txt", []byte("one\n"), "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return id, head, work
}

func case020Commits(t *testing.T, s *session) {
	id, head, work := s.operationRepo(t, "commits")
	content := base64.StdEncoding.EncodeToString([]byte("made by a request\n"))
	body := fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"add b","changes":[{"path":"b.txt","content":%q,"mode":"100644"}]}`, head, author, content)
	// A dry run answers the result and commits nothing.
	r := s.operation(t, id, "commits", `{"dry_run":true,`+body[1:])
	expectStatus(t, r, http.StatusOK)
	failIf(t, r.json["committed"] != false || r.json["entry_seq"] != nil, "dry run: %s", r.body)
	commit := expectOperation(t, s.operation(t, id, "commits", body), "refs/heads/main")
	mustGit(t, work, "fetch", "-q", "origin")
	failIf(t, mustGit(t, work, "rev-parse", "origin/main") != commit, "main did not move to the commit")
	failIf(t, mustGit(t, work, "show", "origin/main:b.txt") != "made by a request", "the file's content")
	failIf(t, mustGit(t, work, "rev-parse", "origin/main^") != head, "the commit's parent")
	if d, ok := s.expectEvent(t, id, "push", 2); ok {
		failIf(t, event(t, d)["operation"] != "commits", "the event's operation: %s", d.Body)
	}
	// A stale expected_head is non_fast_forward; an empty path is
	// invalid_change with its index and reason.
	d := expectError(t, s.operation(t, id, "commits", body), http.StatusConflict, contract.CodeNonFastForward)
	failIf(t, d["ref"] != "refs/heads/main" || d["expected"] != head || d["actual"] != commit, "stale details: %v", d)
	bad := fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"bad","changes":[{"path":"","content":%q,"mode":"100644"}]}`, commit, author, content)
	d = expectError(t, s.operation(t, id, "commits", bad), http.StatusBadRequest, contract.CodeInvalidChange)
	failIf(t, d["index"] != float64(0) || d["reason"] != "path", "invalid_change details: %v", d)
}

// topic pushes a branch topic with one commit on top of head that
// changes the file, and answers its commit.
func topic(t *testing.T, work, name, file, content string) string {
	t.Helper()
	mustGit(t, work, "checkout", "-q", "-b", name)
	c := commitFile(t, work, file, []byte(content), name)
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/"+name)
	mustGit(t, work, "checkout", "-q", "main")
	return c
}

func case020Merge(t *testing.T, s *session) {
	id, head, work := s.operationRepo(t, "merge")
	topicHead := topic(t, work, "topic", "t.txt", "topic\n")
	body := fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"merge topic","source":"topic","strategy":"merge_commit"}`, head, author)
	commit := expectOperation(t, s.operation(t, id, "merge", body), "refs/heads/main")
	r := s.call(t, "GET", "/v1/repos/"+id+"/commits/"+commit, "")
	expectStatus(t, r, http.StatusOK)
	parents, _ := r.json["parents"].([]any)
	failIf(t, len(parents) != 2 || parents[0] != head || parents[1] != topicHead, "the merge commit's parents: %v", parents)
	// A fast-forward moves the branch without a commit.
	ff := topic(t, work, "ff", "f.txt", "ff\n")
	mustGit(t, work, "fetch", "-q", "origin")
	mustGit(t, work, "reset", "-q", "--hard", "origin/main")
	mustGit(t, work, "checkout", "-q", "-B", "ff2", "origin/main")
	ff2 := commitFile(t, work, "g.txt", []byte("g\n"), "ff2")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/ff2")
	_ = ff
	r = s.operation(t, id, "merge", fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"ff","source":"ff2","strategy":"fast_forward_only"}`, commit, author))
	failIf(t, expectOperation(t, r, "refs/heads/main") != ff2, "fast-forward did not answer the source's commit: %s", r.body)
	// A conflict is 409 merge_conflict naming the paths and leaves the
	// branch where it was.
	mustGit(t, work, "fetch", "-q", "origin")
	mustGit(t, work, "checkout", "-q", "-B", "clash", "origin/main")
	commitFile(t, work, "a.txt", []byte("clash\n"), "clash")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/clash")
	mustGit(t, work, "checkout", "-q", "-B", "main", "origin/main")
	mainHead := commitFile(t, work, "a.txt", []byte("main\n"), "main side")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	d := expectError(t, s.operation(t, id, "merge", fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"clash","source":"clash","strategy":"merge_commit"}`, mainHead, author)), http.StatusConflict, contract.CodeMergeConflict)
	paths, _ := d["paths"].([]any)
	failIf(t, len(paths) != 1 || paths[0] != "a.txt", "conflict details: %v", d)
	mustGit(t, work, "fetch", "-q", "origin")
	failIf(t, mustGit(t, work, "rev-parse", "origin/main") != mainHead, "the conflicting merge moved main")
}

func case020CherryPick(t *testing.T, s *session) {
	id, head, work := s.operationRepo(t, "cherry-pick")
	picked := topic(t, work, "topic", "p.txt", "picked\n")
	body := fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"pick","commits":[%q]}`, head, author, picked)
	commit := expectOperation(t, s.operation(t, id, "cherry-pick", body), "refs/heads/main")
	mustGit(t, work, "fetch", "-q", "origin")
	failIf(t, mustGit(t, work, "rev-parse", "origin/main") != commit || mustGit(t, work, "rev-parse", "origin/main^") != head, "main after the pick")
	failIf(t, mustGit(t, work, "show", "origin/main:p.txt") != "picked", "the picked file")
	// 101 commits are too many.
	many := strings.Repeat(fmt.Sprintf("%q,", picked), 100) + fmt.Sprintf("%q", picked)
	d := expectError(t, s.operation(t, id, "cherry-pick", fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"many","commits":[%s]}`, commit, author, many)), http.StatusBadRequest, contract.CodeInvalidChange)
	failIf(t, d["reason"] != "too_many", "too many picks: %v", d)
}

func case020Revert(t *testing.T, s *session) {
	id, head, work := s.operationRepo(t, "revert")
	second := commitFile(t, work, "r.txt", []byte("revert me\n"), "second")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	_ = head
	body := fmt.Sprintf(`{"branch":"main","expected_head":%q,%s,"message":"revert second","commits":[%q]}`, second, author, second)
	commit := expectOperation(t, s.operation(t, id, "revert", body), "refs/heads/main")
	mustGit(t, work, "fetch", "-q", "origin")
	failIf(t, mustGit(t, work, "rev-parse", "origin/main") != commit, "main after the revert")
	if _, err := git(t, work, "cat-file", "-e", "origin/main:r.txt"); err == nil {
		t.Fatal("the reverted file is still there")
	}
	failIf(t, mustGit(t, work, "show", "origin/main:a.txt") != "one", "the other file")
}
