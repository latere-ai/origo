// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/origoclient"
)

// The byte defaults of spec 025: the smallest answer that answers the
// question. Each is one flag away from more, and the flag is named on stderr
// where the answer was cut. Zero always means every one of them.
const (
	defaultRepos    = 200
	defaultRefs     = 100
	defaultEntries  = 200
	defaultLines    = 800
	defaultCatBytes = 32768
	defaultCommits  = 20
	defaultPatch    = 32768
)

// emit writes a value as the contract's own JSON. A paged answer is one merged
// object in the route's own envelope, never a second shape invented here, so
// `origo log -n 1 --json | jq -r '.commits[0].sha'` reads the same field
// whether the command made one call or ten.
func (s *session) emit(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.out.line("%s", raw)
	return nil
}

// runRepos lists the repositories the credential may see (spec 026).
func runRepos(ctx context.Context, s *session, args []string) error {
	fs := s.flags("repos")
	n := fs.Int("n", defaultRepos, "rows, 0 for every one")
	asJSON := fs.Bool("json", false, "print the contract's own JSON")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "repos takes no argument"}
	}
	page := origoclient.DirectoryPage{Repos: []origoclient.Repository{}}
	opt := origoclient.DirectoryOptions{}
	cut := false
	for {
		got, err := s.client.Directory(ctx, opt)
		if err != nil {
			return directoryHint(err)
		}
		s.out.stale(got.Meta)
		page.Repos = append(page.Repos, got.Repos...)
		page.NextCursor = got.NextCursor
		if *n > 0 && len(page.Repos) >= *n {
			page.Repos = page.Repos[:*n]
			cut = got.NextCursor != nil
			break
		}
		if got.NextCursor == nil {
			break
		}
		opt.Cursor = *got.NextCursor
	}
	if *asJSON {
		return s.emit(page)
	}
	for _, r := range page.Repos {
		s.out.line("%s  %s/%s", r.ID, r.Owner, r.Slug)
	}
	if cut {
		s.out.notice("%s", truncation(fmt.Sprintf("%d rows, and the directory has more", len(page.Repos)), "add -n 0 for every one"))
	}
	return nil
}

// directoryHint says what a refused listing means for a caller, because both
// of its refusals are facts about the credential or the installation rather
// than mistakes in the request, and neither sentence says what to do next.
//
// A repository-bound token cannot list at all. Its decision was made at
// minting, for one repository, so `internal/auth` refuses the list action on
// its scope before the authorizer is asked (guard.go, ReasonScope). That is
// the credential the skill recommends for an agent, so `origo repos` is the
// one command it does not have, and the line says so rather than leaving a
// caller to read a bare permission error.
func directoryHint(err error) error {
	ref, ok := origoclient.AsRefusal(err)
	if !ok {
		return err
	}
	switch {
	case ref.Code == contract.CodeDirectoryUnsupported:
		ref.Message += " Name one with -repo or ORIGO_REPO."
	case ref.Code == contract.CodeForbidden && render(ref.Details["action"]) == "list":
		ref.Message += " A repository-bound token names one repository and cannot list; name it with -repo or ORIGO_REPO, or use a token from the issuer."
	}
	return ref
}

// runInfo is one call. The counts and the recent history are `origo refs` and
// `origo log`, so a caller pays for the three it wants and not for four.
func runInfo(ctx context.Context, s *session, args []string) error {
	fs := s.flags("info")
	named := s.repoFlag(fs)
	asJSON := fs.Bool("json", false, "print the contract's own JSON")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "info takes no argument"}
	}
	id, err := s.repoID(ctx, *named)
	if err != nil {
		return err
	}
	repo, err := s.client.Repository(ctx, id)
	if err != nil {
		return err
	}
	if *asJSON {
		return s.emit(repo)
	}
	s.out.line("repo     %s", repo.ID)
	s.out.line("name     %s/%s", repo.Owner, repo.Slug)
	s.out.line("branch   %s", repo.DefaultBranch)
	// The whole head, which is the one place a full id is worth its bytes:
	// it is what -expect takes.
	s.out.line("head     %s", orDash(repo.Head))
	s.out.line("size     %d", repo.SizeBytes)
	s.out.line("updated  %s", stamp(repo.UpdatedAt))
	if repo.PushedAt != nil {
		s.out.line("pushed   %s", stamp(*repo.PushedAt))
	}
	if repo.FrozenAt != nil {
		s.out.line("frozen   %s", stamp(*repo.FrozenAt))
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runRefs lists branches or tags.
func runRefs(ctx context.Context, s *session, args []string) error {
	fs := s.flags("refs")
	named := s.repoFlag(fs)
	tags := fs.Bool("tags", false, "tags instead of branches")
	all := fs.Bool("all", false, "every reference")
	prefix := fs.String("prefix", "", "a string prefix of the whole name")
	n := fs.Int("n", defaultRefs, "references, 0 for every one")
	asJSON := fs.Bool("json", false, "print the contract's own JSON")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "refs takes no argument"}
	}
	id, err := s.repoID(ctx, *named)
	if err != nil {
		return err
	}
	want := *prefix
	if want == "" {
		switch {
		case *all:
			want = "refs/"
		case *tags:
			want = "refs/tags/"
		default:
			want = "refs/heads/"
		}
	}
	list, err := s.client.Refs(ctx, id, want)
	if err != nil {
		return err
	}
	s.out.stale(list.Meta)
	shown := list.Refs
	cut := false
	if *n > 0 && len(shown) > *n {
		shown, cut = shown[:*n], true
	}
	if *asJSON {
		return s.emit(shown)
	}
	for _, r := range shown {
		s.out.line("%s %s", r.Name, short(r.SHA))
	}
	if cut {
		s.out.notice("%s", truncation(fmt.Sprintf("%d of %d references", len(shown), len(list.Refs)), "add -n 0 for every one"))
	}
	if list.Meta.Truncated {
		// The route has no cursor, so naming a flag here would promise a
		// call that does not exist. The wall is reported as the wall it is.
		s.out.notice("%s", wall(fmt.Sprintf("the installation stopped at %d references, which is its cap, and the rest cannot be asked for", origoclient.MaxRefs)))
	}
	return nil
}

// runLs lists a tree, paging transparently. -r walks the whole tree, which is
// what `origo ls -r -n 0 | grep -i handler` needs.
func runLs(ctx context.Context, s *session, args []string) error {
	fs := s.flags("ls")
	named := s.repoFlag(fs)
	ref := fs.String("ref", "HEAD", "the revision")
	recursive := fs.Bool("r", false, "every entry of the tree, not one directory")
	n := fs.Int("n", defaultEntries, "entries, 0 for every one")
	asJSON := fs.Bool("json", false, "print the contract's own JSON")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "ls takes at most one path"}
	}
	if fs.NArg() > 1 {
		return misuse("ls takes at most one path")
	}
	id, err := s.repoID(ctx, *named)
	if err != nil {
		return err
	}
	opt := origoclient.TreeOptions{Ref: *ref, Path: fs.Arg(0), Recursive: *recursive}
	merged := origoclient.TreePage{Entries: []origoclient.TreeEntry{}}
	cut := false
	for {
		page, err := s.client.Tree(ctx, id, opt)
		if err != nil {
			return err
		}
		s.out.stale(page.Meta)
		merged.Entries = append(merged.Entries, page.Entries...)
		merged.NextCursor = page.NextCursor
		if *n > 0 && len(merged.Entries) >= *n {
			merged.Entries = merged.Entries[:*n]
			cut = true
			break
		}
		if page.NextCursor == nil {
			break
		}
		opt.Cursor = *page.NextCursor
	}
	if *asJSON {
		return s.emit(merged)
	}
	for _, e := range merged.Entries {
		s.out.line("%s", entryLine(e.Path, e.Mode, e.Type, e.Size))
	}
	if cut {
		more := "add -n 0 for every one"
		if !*recursive {
			more = "add -n 0 for every one, and -r to walk the whole tree"
		}
		s.out.notice("%s", truncation(fmt.Sprintf("%d entries", len(merged.Entries)), more))
	}
	return nil
}

// runCat prints a file's text, windowed. The header goes to stderr so a
// redirect writes the file and nothing else.
func runCat(ctx context.Context, s *session, args []string) error {
	fs := s.flags("cat")
	named := s.repoFlag(fs)
	ref := fs.String("ref", "HEAD", "the revision")
	n := fs.Int("n", defaultLines, "lines, 0 for every one")
	offset := fs.Int("offset", 0, "the first line, counted from 0")
	maxBytes := fs.Int("max-bytes", defaultCatBytes, "bytes of text, 0 for every one")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "cat takes one path"}
	}
	if fs.NArg() != 1 {
		return misuse("cat takes one path")
	}
	if *offset < 0 {
		return misuse("-offset counts from 0")
	}
	id, err := s.repoID(ctx, *named)
	if err != nil {
		return err
	}
	entry, err := s.client.File(ctx, id, *ref, fs.Arg(0))
	if err != nil {
		return err
	}
	if entry.Type != "blob" {
		return misuse("%s is a %s, not a file; list it with origo ls", entry.Path, entry.Type)
	}
	blob, err := s.client.Blob(ctx, id, entry.SHA, window(entry.Size, *maxBytes))
	if err != nil {
		return err
	}
	s.out.stale(blob.Meta)
	if binaryHead(head(blob.Bytes, 8192)) {
		// The bytes are named rather than printed: a binary file in a
		// context window is the failure this whole design exists to avoid.
		s.out.notice("%s is binary, %s bytes, object %s; it is not printed", entry.Path, size(entry.Size), short(entry.SHA))
		return nil
	}
	lines := *n
	if lines == 0 {
		lines = int(^uint(0) >> 1)
	}
	cap := *maxBytes
	if cap == 0 {
		cap = len(blob.Bytes) + 1
	}
	text, shown, taken, total := lineWindow(string(blob.Bytes), *offset, lines, cap)
	s.out.notice("%s@%s  %s bytes  %d lines", entry.Path, short(blob.Meta.Commit), size(entry.Size), total)
	s.out.write([]byte(text))
	if *offset+shown < total {
		s.out.notice("%s", truncation(
			fmt.Sprintf("lines %d-%d of %d, %d bytes", *offset, *offset+shown-1, total, taken),
			fmt.Sprintf("add -offset %d", *offset+shown)))
	}
	return nil
}

// window decides whether a blob needs a Range. The node measures the
// requested length against its 50 MiB bound, so knowing the size in advance is
// what keeps blob_too_large unreachable. A window of the byte cap is enough:
// no more text than that is ever printed.
func window(fileSize *int64, maxBytes int) *origoclient.ByteRange {
	if fileSize == nil {
		return nil
	}
	want := int64(maxBytes)
	if want <= 0 || want > origoclient.MaxBlobBytes {
		want = origoclient.MaxBlobBytes
	}
	if *fileSize <= want {
		return nil
	}
	return &origoclient.ByteRange{First: 0, Last: want - 1}
}

func head(b []byte, n int) []byte { return b[:min(len(b), n)] }

func size(n *int64) string {
	if n == nil {
		return "-"
	}
	return strconv.FormatInt(*n, 10)
}

// runLog prints history, paging past the route's 200 per page.
func runLog(ctx context.Context, s *session, args []string) error {
	fs := s.flags("log")
	named := s.repoFlag(fs)
	ref := fs.String("ref", "", "the revision, HEAD by default")
	p := fs.String("path", "", "only commits touching this path")
	since := fs.String("since", "", "RFC 3339")
	until := fs.String("until", "", "RFC 3339")
	n := fs.Int("n", defaultCommits, "commits, 0 for every one")
	asJSON := fs.Bool("json", false, "print the contract's own JSON")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "log takes no argument"}
	}
	id, err := s.repoID(ctx, *named)
	if err != nil {
		return err
	}
	opt := origoclient.CommitOptions{Ref: *ref, Path: *p, Since: *since, Until: *until}
	merged := origoclient.CommitPage{Commits: []origoclient.Commit{}}
	cut := false
	for {
		opt.Limit = origoclient.MaxCommitLimit
		if *n > 0 {
			opt.Limit = min(*n-len(merged.Commits), origoclient.MaxCommitLimit)
		}
		page, err := s.client.Commits(ctx, id, opt)
		if err != nil {
			return err
		}
		s.out.stale(page.Meta)
		merged.Commits = append(merged.Commits, page.Commits...)
		merged.NextCursor = page.NextCursor
		if *n > 0 && len(merged.Commits) >= *n {
			merged.Commits = merged.Commits[:*n]
			cut = page.NextCursor != nil
			break
		}
		if page.NextCursor == nil {
			break
		}
		opt.Cursor = *page.NextCursor
	}
	if *asJSON {
		return s.emit(merged)
	}
	for _, c := range merged.Commits {
		s.out.line("%s %s %s %s", short(c.SHA), day(c.Author.At), c.Author.Name, subject(c.Message))
	}
	if cut {
		s.out.notice("%s", truncation(fmt.Sprintf("%d commits, and the history goes on", len(merged.Commits)), "raise -n, or -n 0 for every one"))
	}
	return nil
}

// runShow prints one commit: the metadata, the message, the trailers, the
// totals and one stat line per file. The patch is one flag away.
func runShow(ctx context.Context, s *session, args []string) error {
	fs := s.flags("show")
	named := s.repoFlag(fs)
	patch := fs.Bool("p", false, "the patch as well as the stat")
	only := fs.String("path", "", "narrow to one path")
	maxBytes := fs.Int("max-bytes", defaultPatch, "bytes of patch, 0 for every one")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "show takes one commit"}
	}
	if fs.NArg() != 1 {
		return misuse("show takes one commit")
	}
	id, err := s.repoID(ctx, *named)
	if err != nil {
		return err
	}
	c, err := s.client.Commit(ctx, id, fs.Arg(0))
	if err != nil {
		return err
	}
	s.out.line("commit    %s", c.SHA)
	s.out.line("author    %s <%s>  %s", c.Author.Name, c.Author.Email, stamp(c.Author.At))
	s.out.line("committer %s <%s>  %s", c.Committer.Name, c.Committer.Email, stamp(c.Committer.At))
	if len(c.Parents) > 0 {
		shorts := make([]string, 0, len(c.Parents))
		for _, p := range c.Parents {
			shorts = append(shorts, short(p))
		}
		s.out.line("parents   %s", strings.Join(shorts, " "))
	}
	s.out.line("%s", "")
	for _, l := range indent(c.Message) {
		s.out.line("%s", l)
	}
	for _, t := range c.Trailers {
		s.out.line("%s: %s", t.Key, t.Value)
	}
	s.out.line("%s", "")
	if c.Stats != nil {
		s.out.line("%s  +%d -%d", plural(c.Stats.Files, "file"), c.Stats.Additions, c.Stats.Deletions)
	}
	if len(c.Parents) == 0 {
		// The per-file lines come from a comparison, and a root commit has
		// no base for one. The totals above are the whole answer.
		s.out.notice("a root commit has no comparison, so there are no per-file lines")
		return nil
	}
	// A merge is compared against its first parent, which is what the
	// single-commit route's own totals are computed against.
	if len(c.Parents) > 1 {
		s.out.notice("a merge is compared against its first parent, %s", short(c.Parents[0]))
	}
	return s.comparison(ctx, id, c.Parents[0], c.SHA, *only, *patch, *maxBytes, false)
}

// runDiff compares two revisions, stat first.
func runDiff(ctx context.Context, s *session, args []string) error {
	fs := s.flags("diff")
	named := s.repoFlag(fs)
	patch := fs.Bool("p", false, "the patch as well as the stat")
	only := fs.String("path", "", "narrow to one path, repeatable as a comma separated list")
	maxBytes := fs.Int("max-bytes", defaultPatch, "bytes of patch, 0 for every one")
	if err := fs.Parse(args); err != nil {
		return &usageError{text: "diff takes a base and a head"}
	}
	if fs.NArg() != 2 {
		return misuse("diff takes a base and a head")
	}
	id, err := s.repoID(ctx, *named)
	if err != nil {
		return err
	}
	return s.comparison(ctx, id, fs.Arg(0), fs.Arg(1), *only, *patch, *maxBytes, true)
}

// comparison is the stat-first answer both show and diff give, and the largest
// saving in the set: the patch is parsed into per-file counts here, on this
// machine, so a megabyte on the wire becomes one line per file in a reader's
// context.
//
// A path narrows the fetch itself, one call per path, so the bytes are never
// fetched at all. Where Origo cut the comparison, the notice says Origo cut
// it and names the last file seen; the summary is not presented as complete.
func (s *session) comparison(ctx context.Context, id, base, head, only string, patch bool, maxBytes int, totals bool) error {
	paths := []string{""}
	if only != "" {
		paths = strings.Split(only, ",")
	}
	var all []fileStat
	var body []byte
	truncated := false
	for _, p := range paths {
		cmp, err := s.client.Compare(ctx, id, base, head, strings.TrimSpace(p))
		if err != nil {
			return err
		}
		s.out.stale(cmp.Meta)
		truncated = truncated || cmp.Meta.Truncated
		all = append(all, parseDiff(cmp.Diff)...)
		body = append(body, cmp.Diff...)
	}
	lines := statLines(all)
	if !totals {
		lines = lines[:len(lines)-1] // show printed its own totals from the route
	}
	for _, l := range lines {
		s.out.line("%s", l)
	}
	if truncated {
		last := "no file"
		if len(all) > 0 {
			last = all[len(all)-1].Path
		}
		s.out.notice("%s", wall("the installation cut the comparison at its 1 MiB cap; the files above end at "+last+" and the rest are not counted"))
	}
	if !patch {
		if len(all) > 0 && only == "" {
			s.out.notice("stat only; add -p for the patch, or -path <file> for one file's")
		}
		return nil
	}
	cut, wasCut := cutPatch(body, patchCap(maxBytes, len(body)))
	s.out.write(cut)
	if wasCut {
		s.out.notice("%s", truncation(fmt.Sprintf("%d of %d patch bytes, cut at a file boundary", len(cut), len(body)), "raise -max-bytes, or narrow with -path"))
	}
	return nil
}

func patchCap(maxBytes, have int) int {
	if maxBytes <= 0 {
		return have
	}
	return maxBytes
}

// refusalLine is spec 003's envelope as one line: the code, the sentence, and
// that row's details. Nothing else reaches a reader, and never the body.
func refusalLine(r *origoclient.Refusal) string {
	var b strings.Builder
	b.WriteString(r.Code)
	b.WriteString(": ")
	b.WriteString(r.Message)
	if waitCodes[r.Code] {
		if r.RetryAfter != "" {
			b.WriteString(" retry_after=" + r.RetryAfter)
		} else if v := render(r.Details["retry_after"]); v != "" {
			b.WriteString(" retry_after=" + v)
		}
		if r.Code == contract.CodeRateLimited && r.RateLimit != "" {
			b.WriteString(" rate_limit=" + r.RateLimit)
		}
		return b.String()
	}
	for _, k := range detailFields[r.Code] {
		if v := render(r.Details[k]); v != "" {
			b.WriteString(" " + k + "=" + v)
		}
	}
	if r.Code == contract.CodeUnauthenticated && render(r.Details["reason"]) == "expired" {
		b.WriteString("; mint a fresh token and set ORIGO_TOKEN to it")
	}
	return b.String()
}

// detailFields is spec 025's error table: per code, the details that tell a
// caller what to do next. A code absent here adds nothing beyond its sentence,
// which is what repo_not_found, repo_frozen and gone want: all three are
// terminal.
var detailFields = map[string][]string{
	contract.CodeNonFastForward:       {"expected", "actual"},
	contract.CodeMergeConflict:        {"commit", "paths"},
	contract.CodeInvalidChange:        {"index", "reason"},
	contract.CodeOverQuota:            {"limit", "bytes", "max"},
	contract.CodeRefNotFound:          {"ref"},
	contract.CodeUnauthenticated:      {"reason"},
	contract.CodeForbidden:            {"action", "reason"},
	contract.CodeInvalid:              {"reason", "field"},
	contract.CodeBlobTooLarge:         {"size", "max"},
	contract.CodeDirectoryUnsupported: {"reason"},
	contract.CodeOperationTimeout:     {"operation", "budget_seconds"},
}

// waitCodes are the refusals whose line names the wait rather than a detail of
// the request.
var waitCodes = map[string]bool{
	contract.CodeRateLimited:           true,
	contract.CodeStorageUnavailable:    true,
	contract.CodeRepositoryUnavailable: true,
	contract.CodeAuthorizerUnavailable: true,
}

// render prints one details value in the register a line takes: a string as
// itself, a number without a decimal point it does not need, a list joined by
// commas in a stable order, and anything else as the JSON it arrived as.
func render(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, render(e))
		}
		sort.Strings(parts)
		return strings.Join(parts, ",")
	default:
		// A details value of a shape no line has a register for. It came
		// off the wire, so it can be anything, and a value that will not
		// marshal is named rather than dropped: a caller reading an empty
		// field would think the installation sent none.
		raw, err := json.Marshal(t)
		if err != nil {
			return "<unrenderable>"
		}
		return string(raw)
	}
}
