// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// The mechanics of spec 020: git plumbing on the warm copy under the
// write lock, in a temporary index and without a worktree, then a pack
// and a one-update transaction committed through the log exactly as a
// push is. No operation runs user code: no hooks, no filters, no
// smudge, no submodule fetch.

// operation is one server-side operation in flight.
type operation struct {
	h      *Handler
	w      http.ResponseWriter
	r      *http.Request
	name   string
	budget time.Duration
	id     string
	// base is the index the log held when the request was authorized;
	// the lease's own index is the commit's base.
	base *wal.Index

	// quota is the authorizer's quota_bytes for this write (spec 012).
	quota int64

	ctx    context.Context
	repo   *repo.Repo
	work   string
	index  string
	object string
	branch string
	// haves are the commits the log already holds that the new tip
	// descends from, so the entry's pack carries the new objects alone.
	haves  []string
	dryRun bool
	author identityRequest
	// subject and actor are the effective identity of the request,
	// read once: they name the entry's writer, the commit's trailers,
	// and the log line.
	subject, actor string
}

// mergeConflict is a merge, cherry-pick, or revert git could not apply.
type mergeConflict struct {
	commit string
	paths  []string
}

func (e *mergeConflict) Error() string {
	return "api: merge conflict in " + strings.Join(e.paths, ", ")
}

// run is the body of every operation: the write lock on the repository,
// the branch state the request declared, one subprocess slot, a
// temporary directory for the index and a dry run's objects, the
// operation itself, and the commit.
func (o *operation) run(w http.ResponseWriter, r *http.Request, branch string, c *common, build func(*operation) (string, error)) {
	h := o.h
	o.w, o.r, o.branch, o.dryRun, o.author = w, r, branch, c.DryRun, *c.Author
	o.subject, o.actor = auth.Subject(r.Context()), auth.Actor(r.Context())
	expectedHead := c.ExpectedHead
	ctx, cancel := context.WithTimeout(r.Context(), o.budget)
	defer cancel()
	o.ctx = ctx
	l, err := h.cache.Lease(ctx, o.id, true)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrDeleted):
			h.notFoundOrGone(o.w, o.r, o.id)
		default:
			h.storageError(o.w, o.r, err)
		}
		return
	}
	defer l.Release()
	o.repo = l.Repo
	o.base = l.Index
	// The branch the request declared, against the copy the currency
	// check just brought up to date: a caller that read the branch
	// before someone else moved it is refused here rather than after
	// the work.
	head, present := o.refs()[o.branch]
	switch {
	case expectedHead == nil && present:
		nonFastForward(o.w, o.branch, nil, head)
		return
	case expectedHead == nil:
	case !present:
		writeReadError(o.w, refNotFound(o.branch))
		return
	case head != *expectedHead:
		nonFastForward(o.w, o.branch, expectedHead, head)
		return
	}
	slot, ok := h.limits.Slot(o.w, o.r)
	if !ok {
		return
	}
	defer slot()
	if err := o.workspace(); err != nil {
		h.storageError(o.w, o.r, err)
		return
	}
	defer o.cleanup()
	tip, err := build(o)
	if err != nil {
		o.fail(err)
		return
	}
	o.finish(tip, expectedHead)
}

// refs is the reference map the copy holds, empty for a repository with
// no index yet.
func (o *operation) refs() map[string]string {
	if o.base == nil {
		return map[string]string{}
	}
	return o.base.Refs
}

// workspace makes the temporary directory the operation works in: the
// index every plumbing command shares and, for a dry run, the object
// directory its objects go into instead of the repository's. The
// directory sits directly under spool/ and is removed after the
// response, so a loop of dry runs leaves nothing behind.
func (o *operation) workspace() error {
	if err := os.MkdirAll(o.h.cache.SpoolDir(), 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(o.h.cache.SpoolDir(), "op-")
	if err != nil {
		return err
	}
	o.work = dir
	o.index = filepath.Join(dir, "index")
	if o.dryRun {
		o.object = filepath.Join(dir, "objects")
		for _, sub := range []string{"", "pack", "info"} {
			if err := os.MkdirAll(filepath.Join(o.object, sub), 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *operation) cleanup() {
	if o.work != "" {
		_ = os.RemoveAll(o.work)
	}
}

// command builds one plumbing subprocess against the copy under the
// operation's budget: the temporary index every step shares and, for a
// dry run, the temporary object directory with the repository's objects
// as an alternate, so nothing a dry run writes enters the repository.
func (o *operation) command(extra []string, args ...string) *exec.Cmd {
	cmd := o.h.cache.Git().Command(o.ctx, o.repo.Dir, append([]string{"--no-pager"}, args...)...)
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+o.index)
	if o.object != "" {
		cmd.Env = append(cmd.Env,
			"GIT_OBJECT_DIRECTORY="+o.object,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(o.repo.Dir, "objects"))
	}
	cmd.Env = append(cmd.Env, extra...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}

// run runs a subprocess to completion and answers its stdout.
func (o *operation) runGit(cmd *exec.Cmd, stdin io.Reader, args []string) ([]byte, error) {
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &repo.Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

// git runs one plumbing command against the copy.
func (o *operation) git(stdin io.Reader, args ...string) ([]byte, error) {
	return o.runGit(o.command(nil, args...), stdin, args)
}

// index runs one git update-index. The command insists on a work tree
// whatever it is asked to do, and the copy is bare, so it is given the
// operation's own empty directory: nothing is read from it and the
// index is the only thing the call changes.
func (o *operation) updateIndex(args ...string) error {
	_, err := o.runGit(o.command([]string{"GIT_WORK_TREE=" + o.work}, args...), nil, args)
	return err
}

// out is git with the output trimmed.
func (o *operation) out(stdin io.Reader, args ...string) (string, error) {
	b, err := o.git(stdin, args...)
	return strings.TrimSpace(string(b)), err
}

// resolve turns a name the request carried into an object id of the
// kind the operation needs, 404 ref_not_found when git cannot.
func (o *operation) resolve(name, peel string) (string, error) {
	sha, err := o.out(nil, "rev-parse", "--verify", "-q", "--end-of-options", name+peel)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && o.ctx.Err() == nil {
			return "", refNotFound(name)
		}
		return "", err
	}
	return sha, nil
}

// commitTree writes the commit object: the author from the request, the
// committer Origo at the host of ORIGO_PUBLIC_URL, and the effective
// subject and actor in the trailers spec 020 names.
func (o *operation) commitTree(tree, message string, parents ...string) (string, error) {
	name, email := o.h.committer()
	at := o.h.now().UTC().Format(time.RFC3339)
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-")
	cmd := o.h.cache.Git().Command(o.ctx, o.repo.Dir, append([]string{"--no-pager"}, args...)...)
	cmd.Env = append(cmd.Env,
		"GIT_INDEX_FILE="+o.index,
		"GIT_AUTHOR_NAME="+o.author.Name, "GIT_AUTHOR_EMAIL="+o.author.Email, "GIT_AUTHOR_DATE="+at,
		"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email, "GIT_COMMITTER_DATE="+at)
	if o.object != "" {
		cmd.Env = append(cmd.Env,
			"GIT_OBJECT_DIRECTORY="+o.object,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(o.repo.Dir, "objects"))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stdin = strings.NewReader(o.message(message))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", &repo.Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// message is the commit message with the trailers that record who the
// operation was made for.
func (o *operation) message(body string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n\nOrigo-Subject: " + o.subject + "\n")
	if o.actor != "" {
		b.WriteString("Origo-Actor: " + o.actor + "\n")
	}
	return b.String()
}

// commits builds one commit from the request's changes: the parent's
// tree read into the temporary index, one plumbing call per change, a
// tree, and a commit.
func (o *operation) commits(req *commitsRequest, from string, decoded [][]byte) (string, error) {
	parent := ""
	switch {
	case req.CreateBranch && from != "":
		sha, err := o.resolve(from, "^{commit}")
		if err != nil {
			return "", err
		}
		parent = sha
	case req.CreateBranch:
		// from: null, the first commit of a repository with no history.
		if len(o.namedRefs()) > 0 {
			return "", &readError{status: http.StatusBadRequest, code: contract.CodeInvalid,
				details: map[string]any{"reason": "from is null only for a repository with no commit", "field": "from"}}
		}
	default:
		parent = *req.ExpectedHead
	}
	if parent == "" {
		if _, err := o.git(nil, "read-tree", "--empty"); err != nil {
			return "", err
		}
	} else {
		o.haves = append(o.haves, parent)
		if _, err := o.git(nil, "read-tree", "--end-of-options", parent); err != nil {
			return "", err
		}
	}
	for i, c := range req.Changes {
		if err := o.apply(i, c, decoded[i]); err != nil {
			return "", err
		}
	}
	tree, err := o.out(nil, "write-tree")
	if err != nil {
		return "", err
	}
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	return o.commitTree(tree, req.Message, parents...)
}

// namedRefs is every reference the index holds beside HEAD.
func (o *operation) namedRefs() []string {
	var out []string
	for name := range o.refs() {
		if name != "HEAD" {
			out = append(out, name)
		}
	}
	return out
}

// apply writes one change into the temporary index. A change git
// refuses at its path or its content is the caller's, not the node's,
// so it answers invalid_change with the change's own index.
func (o *operation) apply(i int, c change, content []byte) error {
	if c.Delete {
		if err := o.updateIndex("update-index", "--force-remove", "--", c.Path); err != nil {
			if o.ctx.Err() != nil {
				return err
			}
			return &changeError{index: i, reason: ReasonPath}
		}
		return nil
	}
	mode := c.Mode
	if mode == "" {
		mode = ModeFile
	}
	blob := c.ContentRef
	if blob == "" {
		sha, err := o.out(bytes.NewReader(content), "hash-object", "-w", "-t", "blob", "--stdin")
		if err != nil {
			return err
		}
		blob = sha
	} else if _, err := o.resolve(blob, "^{blob}"); err != nil {
		if o.ctx.Err() != nil {
			return err
		}
		return &changeError{index: i, reason: ReasonContent}
	}
	if err := o.updateIndex("update-index", "--add", "--cacheinfo", mode+","+blob+","+c.Path); err != nil {
		if o.ctx.Err() != nil {
			return err
		}
		return &changeError{index: i, reason: ReasonPath}
	}
	return nil
}

// merge is spec 020's merge: a fast-forward when the strategy allows
// one and the branch is behind the source, a two-parent commit
// otherwise.
func (o *operation) merge(req *mergeRequest) (string, error) {
	head := *req.ExpectedHead
	source, err := o.resolve(req.Source, "^{commit}")
	if err != nil {
		return "", err
	}
	o.haves = append(o.haves, head, source)
	forward, err := o.ancestor(head, source)
	if err != nil {
		return "", err
	}
	switch {
	case forward && req.Strategy != StrategyMergeCommit:
		// The source is already in the log, so the entry carries no
		// pack: the branch moves and nothing new is written.
		return source, nil
	case !forward && req.Strategy == StrategyFastForwardOnly:
		return "", &readError{status: http.StatusConflict, code: contract.CodeNonFastForward,
			details: map[string]any{"ref": o.branch, "expected": source, "actual": head}}
	}
	tree, err := o.mergeTree(source, "", head, source)
	if err != nil {
		return "", err
	}
	message := req.Message
	if message == "" {
		message = "Merge " + req.Source + " into " + req.Branch
	}
	return o.commitTree(tree, message, head, source)
}

// ancestor reports whether old is an ancestor of new, git's own
// fast-forward test.
func (o *operation) ancestor(old, next string) (bool, error) {
	_, err := o.git(nil, "merge-base", "--is-ancestor", "--end-of-options", old, next)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && o.ctx.Err() == nil {
		return false, nil
	}
	return false, err
}

// mergeTree runs git merge-tree --write-tree, which merges without a
// worktree and reports conflicts as data. The conflicted paths are the
// records of the first section of the -z output, which ends at the
// empty record before the informational messages.
func (o *operation) mergeTree(commit, base, ours, theirs string) (string, error) {
	args := []string{"merge-tree", "--write-tree", "-z", "--name-only"}
	if base != "" {
		args = append(args, "--merge-base="+base)
	}
	args = append(args, "--end-of-options", ours, theirs)
	out, err := o.git(nil, args...)
	if err == nil {
		tree, _, _ := strings.Cut(string(out), "\x00")
		return tree, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(out) == 0 || o.ctx.Err() != nil {
		return "", err
	}
	fields := strings.Split(string(out), "\x00")
	var paths []string
	for _, f := range fields[1:] {
		if f == "" {
			break
		}
		if !contains(paths, f) {
			paths = append(paths, f)
		}
	}
	return "", &mergeConflict{commit: commit, paths: paths}
}

func contains(list []string, v string) bool {
	return slices.Contains(list, v)
}

// pick applies the request's commits in order onto the branch, one
// commit each, all in one entry: a cherry-pick applies the change the
// commit made, a revert the change it undid. A conflict on any of them
// commits nothing and names the commit.
func (o *operation) pick(req *pickRequest, name string) (string, error) {
	head := *req.ExpectedHead
	o.haves = append(o.haves, head)
	onto := head
	for _, want := range req.Commits {
		commit, err := o.resolve(want, "^{commit}")
		if err != nil {
			return "", err
		}
		parent, err := o.parent(commit, req.Mainline)
		if err != nil {
			return "", err
		}
		// The commit's own change is the difference between its parent
		// and itself; a revert is the same difference the other way
		// round, which is the same three-way merge with the sides
		// swapped.
		base, theirs := parent, commit
		if name == OpRevert {
			base, theirs = commit, parent
		}
		tree, err := o.mergeTree(commit, base, onto, theirs)
		if err != nil {
			return "", err
		}
		message := req.Message
		if message == "" {
			message, err = o.pickMessage(commit, name)
			if err != nil {
				return "", err
			}
		}
		onto, err = o.commitTree(tree, message, onto)
		if err != nil {
			return "", err
		}
	}
	return onto, nil
}

// parent is the side of a picked commit the change is measured
// against: its one parent, or the mainline the request named for a
// merge commit.
func (o *operation) parent(commit string, mainline int) (string, error) {
	out, err := o.out(nil, "rev-list", "--parents", "-n", "1", "--end-of-options", commit)
	if err != nil {
		return "", err
	}
	parents := strings.Fields(out)[1:]
	field := map[string]any{"reason": "", "field": "mainline"}
	switch {
	case len(parents) == 0:
		field["reason"] = "a root commit cannot be picked or reverted"
		field["field"] = "commits"
	case len(parents) == 1 && mainline != 0:
		field["reason"] = "mainline is only for a merge commit"
	case len(parents) > 1 && mainline == 0:
		field["reason"] = "mainline names which parent of the merge commit the change is measured against"
	case mainline > len(parents):
		field["reason"] = "mainline is past the commit's parents"
	default:
		if mainline > 0 {
			return parents[mainline-1], nil
		}
		return parents[0], nil
	}
	return "", &readError{status: http.StatusBadRequest, code: contract.CodeInvalid, details: field}
}

// pickMessage is the message of a commit a pick writes when the request
// named none: the picked commit's own for a cherry-pick, git's own
// revert message for a revert.
func (o *operation) pickMessage(commit, name string) (string, error) {
	body, err := o.out(nil, "log", "-1", "--format=%B", "--end-of-options", commit)
	if err != nil {
		return "", err
	}
	if name == OpCherryPick {
		return body, nil
	}
	subject, _, _ := strings.Cut(body, "\n")
	return "Revert \"" + subject + "\"\n\nThis reverts commit " + commit + ".", nil
}

// finish packs what the operation wrote, checks it the way
// receive.fsckObjects checks a push, and commits it through the log as
// a push entry. A dry run answers before any of that.
func (o *operation) finish(tip string, expectedHead *string) {
	tree, err := o.out(nil, "rev-parse", "--verify", "-q", "--end-of-options", tip+"^{tree}")
	if err != nil {
		o.fail(err)
		return
	}
	res := operationResult{Commit: tip, Branch: o.branch, Tree: tree}
	if o.dryRun {
		writeResult(o.w, res, true)
		return
	}
	pack, err := o.pack(tip)
	if err != nil {
		o.fail(err)
		return
	}
	if !o.withinLimits(pack.Size) {
		return
	}
	old := wal.ZeroSHA
	if expectedHead != nil {
		old = *expectedHead
	}
	refs := []wal.RefUpdate{{Ref: o.branch, Old: old, New: tip}}
	entry := wal.Entry{
		Kind: wal.KindPush, Subject: o.subject, Actor: o.actor,
		Refs: refs, Pack: pack, PushOptions: []string{"origo.operation=" + o.name},
	}
	committed, err := o.h.log.Commit(o.ctx, o.id, o.base, entry, func(ctx context.Context, ix *wal.Index) error {
		return o.h.cache.Apply(ctx, o.repo, ix)
	})
	if err != nil {
		o.fail(err)
		return
	}
	// The entry is the source of truth from here: the branch is moved
	// in the copy first, so a failure to move it leaves the copy behind
	// the log and the next currency check reconciles the reference,
	// then the sequence is recorded.
	if _, err := o.git(nil, "update-ref", "--end-of-options", o.branch, tip, old); err != nil {
		o.h.logger.ErrorContext(o.ctx, "branch not moved in the local copy", "repo", o.id, "ref", o.branch, "error", err)
	} else if err := o.h.cache.Advance(o.repo, committed.Index); err != nil {
		o.h.logger.ErrorContext(o.ctx, "local sequence not advanced", "repo", o.id, "error", err)
	}
	o.h.logger.InfoContext(o.ctx, "server-side operation", "repo", o.id, "operation", o.name,
		"seq", committed.Index.Seq, "ref", o.branch, "commit", tip, "pack_bytes", pack.Size,
		"subject", o.subject, "actor", o.actor)
	_ = o.h.events.Enqueue(o.ctx, o.id, events.Entry{Header: committed.Header, Refs: refs})
	seq := committed.Index.Seq
	res.EntrySeq = &seq
	writeResult(o.w, res, false)
}

// pack is the entry's packfile: what is reachable from the new tip and
// from none of the commits the log already holds, which is what a push
// of the same change would send. A tip the log already holds (a
// fast-forward merge) has no pack at all.
func (o *operation) pack(tip string) (wal.Body, error) {
	if contains(o.haves, tip) {
		return wal.Body{}, nil
	}
	var revs strings.Builder
	revs.WriteString(tip + "\n")
	for _, h := range o.haves {
		revs.WriteString("^" + h + "\n")
	}
	out, err := o.git(strings.NewReader(revs.String()), "pack-objects", "--revs", "--stdout", "-q")
	if err != nil {
		return wal.Body{}, err
	}
	path := filepath.Join(o.work, "entry.pack")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return wal.Body{}, err
	}
	// The checks receive.fsckObjects makes on the way in, made here
	// because no receive-pack ran: the pack indexes strictly and the
	// new commit is connected.
	if _, err := o.git(nil, "index-pack", "--strict", path); err != nil {
		return wal.Body{}, err
	}
	if _, err := o.git(nil, "fsck", "--connectivity-only", "--no-progress", "--end-of-options", tip); err != nil {
		return wal.Body{}, err
	}
	return wal.FileBody(path)
}

// withinLimits holds the pack to the two bounds of spec 012 a push is
// held to: the single-push size and the repository's quota. It reports
// whether the commit goes on and writes the 413 when it does not.
func (o *operation) withinLimits(size int64) bool {
	if max := o.h.limits.MaxPush(); size > max {
		o.overQuota(limits.LimitPush, size, max)
		return false
	}
	var held int64
	if o.base != nil {
		held = o.base.SizeBytes
	}
	q, err := o.h.limits.Measure(o.ctx, o.id, held, size, o.quota)
	if err != nil {
		o.h.storageError(o.w, o.r, err)
		return false
	}
	if q.Over() {
		o.overQuota(limits.LimitRepository, q.Bytes, q.Max)
		return false
	}
	return true
}

func (o *operation) overQuota(limit string, size, max int64) {
	o.h.logger.InfoContext(o.ctx, "operation refused", "repo", o.id, "operation", o.name,
		"limit", limit, "bytes", size, "max", max, "subject", o.subject)
	contract.Write(o.w, http.StatusRequestEntityTooLarge, contract.CodeOverQuota, map[string]any{
		"limit": limit, "bytes": size, "max": max,
	})
}

// fail renders what stopped the operation. Nothing is committed by any
// of these, so every one of them leaves the repository as it was.
func (o *operation) fail(err error) {
	if errors.Is(o.ctx.Err(), context.DeadlineExceeded) {
		contract.Write(o.w, http.StatusGatewayTimeout, contract.CodeOperationTimeout, map[string]any{
			"operation": o.name, "budget_seconds": int(o.budget / time.Second),
		})
		return
	}
	if ce, ok := errors.AsType[*changeError](err); ok {
		invalidChange(o.w, ce.index, ce.reason)
		return
	}
	if mc, ok := errors.AsType[*mergeConflict](err); ok {
		paths := mc.paths
		if paths == nil {
			paths = []string{}
		}
		contract.Write(o.w, http.StatusConflict, contract.CodeMergeConflict, map[string]any{
			"commit": mc.commit, "paths": paths,
		})
		return
	}
	if re, ok := errors.AsType[*readError](err); ok {
		contract.Write(o.w, re.status, re.code, re.details)
		return
	}
	if conflict, ok := errors.AsType[*wal.ConflictError](err); ok {
		expected := conflict.Expected
		nonFastForward(o.w, conflict.Ref, &expected, conflict.Actual)
		return
	}
	o.h.logger.ErrorContext(o.ctx, "server-side operation failed", "repo", o.id, "operation", o.name, "error", err)
	o.h.retryAfter(o.w, err)
	contract.Write(o.w, http.StatusServiceUnavailable, contract.CodeStorageUnavailable, wal.ErrorDetails(err))
}
