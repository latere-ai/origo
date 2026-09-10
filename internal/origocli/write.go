// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/latere-ai/origo/internal/origoclient"
)

// writeFlags are the arguments every write command shares. `-expect` is
// required on all four and the command never fills it in: a write is a
// statement about a branch the caller has read, and a branch that moved is
// non_fast_forward with the actual head rather than a clobbered one.
//
// There is no -author. The author is ORIGO_AUTHOR, so a model writing a
// command line cannot choose who a commit is attributed to.
type writeFlags struct {
	repo   *string
	branch *string
	expect *string
	dryRun *bool
	msg    *string
}

func (s *session) writeFlagsOn(fs *flag.FlagSet, message string) *writeFlags {
	return &writeFlags{
		repo:   s.repoFlag(fs),
		branch: fs.String("branch", "", "the branch, the repository's default by default"),
		expect: fs.String("expect", "", "the commit the branch is at now; a short id is expanded first"),
		dryRun: fs.Bool("dry-run", false, "check the plan without landing it"),
		msg:    fs.String("m", "", message),
	}
}

// common builds the half of an operation body the four routes share, after
// resolving the repository, the branch and the expected head.
//
// A short -expect is expanded through GET /v1/repos/{id}/commits/{sha} before
// the write is sent, so a short id that no longer resolves is ref_not_found
// with nothing written, and one of a commit the branch has moved off still
// produces non_fast_forward with the actual head.
func (s *session) common(ctx context.Context, w *writeFlags, needExpect bool) (string, origoclient.Common, error) {
	var out origoclient.Common
	id, err := s.repoID(ctx, *w.repo)
	if err != nil {
		return "", out, err
	}
	branch := *w.branch
	if branch == "" {
		repo, err := s.client.Repository(ctx, id)
		if err != nil {
			return "", out, err
		}
		branch = repo.DefaultBranch
	}
	author, err := parseAuthor(s.cfg.author)
	if err != nil {
		return "", out, err
	}
	out = origoclient.Common{Branch: branch, Author: &author, Message: *w.msg, DryRun: *w.dryRun}
	switch {
	case *w.expect != "":
		head := *w.expect
		if len(head) < 40 {
			if head, err = s.client.Expand(ctx, id, *w.expect); err != nil {
				return "", out, err
			}
		}
		out.ExpectedHead = &head
	case needExpect:
		return "", out, misuse("-expect names the commit the branch is at; read it with origo log -n 1 --json")
	}
	return id, out, nil
}

// receipt is what a write prints: the commit, the branch, the log sequence,
// and whether it landed.
//
// It reads Committed as the three-state field the wire carries. A real write
// answers 201 with the key absent, never true, and only a dry run carries
// committed: false, so reading it as a plain boolean would call a landed
// commit uncommitted.
func (s *session) receipt(r origoclient.Receipt) {
	state := "committed"
	if !r.Landed() {
		state = "dry-run"
	}
	seq := "-"
	if r.EntrySeq != nil {
		seq = fmt.Sprint(*r.EntrySeq)
	}
	s.out.line("%s %s entry %s %s", r.Commit, r.Branch, seq, state)
}

// runCommit builds one commit from files the caller named.
func runCommit(ctx context.Context, s *session, args []string) error {
	fs := s.flags("commit")
	w := s.writeFlagsOn(fs, "the commit message")
	var deletes, files stringList
	fs.Var(&deletes, "delete", "a repository path to remove, repeatable")
	fs.Var(&files, "file", "<repository path>=<local path>, repeatable")
	create := fs.Bool("create", false, "create the branch")
	from := fs.String("from", "", "the revision a created branch starts at")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "commit takes the paths to commit"}
	}
	if *w.msg == "" {
		return misuse("-m is the commit message")
	}
	changes, err := gather(fs.Args(), files, deletes)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return misuse("name at least one path to commit, or -delete one")
	}
	// A created branch is the one case with no expected head, because there
	// is no commit on a branch that does not exist yet.
	id, common, err := s.common(ctx, w, !*create)
	if err != nil {
		return err
	}
	req := origoclient.CommitRequest{Common: common, CreateBranch: *create, From: *from, Changes: changes}
	got, err := s.client.CreateCommit(ctx, id, req)
	if err != nil {
		return err
	}
	s.receipt(got)
	return nil
}

// gather turns the caller's arguments into the changes of one commit.
//
// A positional argument is a repository path read from the local file at the
// same path; -file maps a repository path to a different local one. Nothing
// else is opened: the command reads the files a caller named and writes none.
func gather(positional []string, files, deletes stringList) ([]origoclient.Change, error) {
	var out []origoclient.Change
	add := func(repoPath, localPath string) error {
		if repoPath == "" {
			return misuse("a change needs a repository path")
		}
		b, mode, err := readLocal(localPath)
		if err != nil {
			return err
		}
		out = append(out, origoclient.Change{Path: repoPath, Content: b, Mode: mode})
		return nil
	}
	for _, p := range positional {
		if err := add(p, p); err != nil {
			return nil, err
		}
	}
	for _, pair := range files {
		repoPath, localPath, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, misuse("-file is <repository path>=<local path>, not %q", pair)
		}
		if err := add(repoPath, localPath); err != nil {
			return nil, err
		}
	}
	for _, p := range deletes {
		out = append(out, origoclient.Change{Path: p, Delete: true})
	}
	return out, nil
}

// readLocal reads one file the caller named, and refuses any path that leaves
// the working directory.
//
// The bound is here because it is the one this command owns: the repository
// path is checked by the node, but the local read is not checked by anyone
// else. Without it, `origo commit ../../etc/passwd` would put a file from
// outside the tree into a repository, which is a write no caller asked for.
func readLocal(name string) ([]byte, string, error) {
	if name == "" {
		return nil, "", misuse("a change needs a local path")
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", misuse("%s is outside the working directory; origo reads only files under it", name)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, "", misuse("%s cannot be read: %v", name, err)
	}
	if info.IsDir() {
		return nil, "", misuse("%s is a directory; name the files", name)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", misuse("%s cannot be read: %v", name, err)
	}
	mode := "100644"
	if info.Mode().Perm()&0o111 != 0 {
		mode = "100755"
	}
	return b, mode, nil
}

// runMerge merges a source into a branch.
func runMerge(ctx context.Context, s *session, args []string) error {
	fs := s.flags("merge")
	w := s.writeFlagsOn(fs, "the merge commit message")
	strategy := fs.String("strategy", "", "fast_forward_only, merge_commit, or fast_forward_if_possible")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "merge takes one source"}
	}
	if fs.NArg() != 1 {
		return misuse("merge takes one source, a branch or a commit")
	}
	switch *strategy {
	case "", origoclient.FastForwardOnly, origoclient.MergeCommit, origoclient.FastForwardIfPossible:
	default:
		return misuse("-strategy is fast_forward_only, merge_commit, or fast_forward_if_possible")
	}
	id, common, err := s.common(ctx, w, true)
	if err != nil {
		return err
	}
	got, err := s.client.Merge(ctx, id, origoclient.MergeRequest{Common: common, Source: fs.Arg(0), Strategy: *strategy})
	if err != nil {
		return err
	}
	s.receipt(got)
	return nil
}

func runCherryPick(ctx context.Context, s *session, args []string) error {
	return replay(ctx, s, args, "cherry-pick")
}

func runRevert(ctx context.Context, s *session, args []string) error {
	return replay(ctx, s, args, "revert")
}

// replay is cherry-pick and revert, which take the same arguments and differ
// only in direction. They are two commands rather than one with a mode,
// because a command name costs nothing a caller holds between turns.
func replay(ctx context.Context, s *session, args []string, name string) error {
	fs := s.flags(name)
	w := s.writeFlagsOn(fs, "the message, where the operation makes one")
	mainline := fs.Int("mainline", 0, "the 1-based parent of a merge commit")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: name + " takes at least one commit"}
	}
	if fs.NArg() == 0 {
		return misuse("%s takes at least one commit", name)
	}
	if *mainline < 0 {
		return misuse("-mainline is the 1-based parent of a merge commit")
	}
	id, common, err := s.common(ctx, w, true)
	if err != nil {
		return err
	}
	req := origoclient.PickRequest{Common: common, Commits: fs.Args(), Mainline: *mainline}
	call := s.client.CherryPick
	if name == "revert" {
		call = s.client.Revert
	}
	got, err := call(ctx, id, req)
	if err != nil {
		return err
	}
	s.receipt(got)
	return nil
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}
