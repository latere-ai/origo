// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// The bounds of the read API (spec 009).
const (
	// DefaultReadTimeout is the budget of the git subprocesses of one
	// read request; a request that runs out of it is 504 operation_timeout.
	DefaultReadTimeout = 30 * time.Second
	// MaxRefs is the cap of the refs list; past it Origo-Truncated is true.
	MaxRefs = 10000
	// DefaultCommitLimit and MaxCommitLimit bound a page of commits.
	DefaultCommitLimit = 50
	MaxCommitLimit     = 200
	// TreePageSize is the number of entries of one tree page.
	TreePageSize = 5000
	// MaxCompareBytes is where a diff is cut, at a file boundary.
	MaxCompareBytes = 1 << 20
	// MaxBlobBytes is the largest blob, or Range of one, served whole.
	MaxBlobBytes = 50 << 20
)

// The headers of the read API.
const (
	// HeaderCommit carries the object id the request's {sha} resolved
	// to, a tag peeled to what it points at.
	HeaderCommit = "Origo-Commit"
	// HeaderTruncated is true when refs hit its cap or compare was cut.
	HeaderTruncated = "Origo-Truncated"
)

// shortNameRe is the grammar of a {sha} path segment: a full object id
// or a short name without a slash. A full reference name travels in a
// query parameter with the segment set to the placeholder.
var shortNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// placeholder is the segment value that says the name is in the query.
const placeholder = "-"

// registerRead mounts the read routes.
func (h *Handler) registerRead(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/repos/{id}/refs", h.refs)
	mux.HandleFunc("GET /v1/repos/{id}/commits", h.commits)
	mux.HandleFunc("GET /v1/repos/{id}/commits/{sha}", h.commit)
	mux.HandleFunc("GET /v1/repos/{id}/compare/{range}", h.compare)
	mux.HandleFunc("GET /v1/repos/{id}/tree/{sha}", h.tree)
	mux.HandleFunc("GET /v1/repos/{id}/blob/{sha}", h.blob)
	mux.HandleFunc("GET /v1/repos/{id}/archive/{file}", h.archive)
}

// readError is a refusal decided before or during a read: the status,
// the code, and the details of the envelope.
type readError struct {
	status  int
	code    string
	details map[string]any
}

func (e *readError) Error() string { return fmt.Sprintf("%d %s %v", e.status, e.code, e.details) }

func invalidRead(reason string) *readError {
	return &readError{status: http.StatusBadRequest, code: contract.CodeInvalid, details: map[string]any{"reason": reason}}
}

func refNotFound(ref string) *readError {
	return &readError{status: http.StatusNotFound, code: contract.CodeRefNotFound, details: map[string]any{"ref": ref}}
}

// readRequest is one read request after the prologue: the repository
// under its read lock, the deadline every subprocess runs under, and
// the name of the operation for a timeout's details.
type readRequest struct {
	h     *Handler
	w     http.ResponseWriter
	id    string
	repo  *repo.Repo
	op    string
	wrote bool
}

// value reads a query parameter under the option rule: a value that
// starts with - would be read by git as an option, so it is refused
// before any subprocess starts.
func value(r *http.Request, name string) (string, error) {
	v := r.URL.Query().Get(name)
	if strings.HasPrefix(v, "-") {
		return "", invalidRead("option: " + name + " must not start with -")
	}
	return v, nil
}

// target is the name an endpoint resolves: the path segment, or the
// query parameter when the segment is the placeholder. A parameter
// given beside a real segment is refused, and so is a segment outside
// the short-name grammar.
func target(r *http.Request, segment, param string) (string, error) {
	q := r.URL.Query()
	if segment == placeholder {
		if !q.Has(param) || q.Get(param) == "" {
			return "", invalidRead("ref: the segment - takes the name from ?" + param + "=")
		}
		return value(r, param)
	}
	if q.Has(param) {
		return "", invalidRead("ref: ?" + param + "= is only read with the segment -")
	}
	if strings.HasPrefix(segment, "-") {
		return "", invalidRead("option: the segment must not start with -")
	}
	if !shortNameRe.MatchString(segment) {
		return "", invalidRead("ref: the segment is a full object id or a short name without a slash; a full name goes in ?" + param + "= with the segment -")
	}
	return segment, nil
}

// pathValue reads ?path= under the option rule and the path rules.
func pathValue(r *http.Request) (string, error) {
	p, err := value(r, "path")
	if err != nil {
		return "", err
	}
	if !ValidPath(p) {
		return "", invalidRead("path: not a valid repository path")
	}
	return p, nil
}

// open is the prologue of every read: authorize the read on the id the
// path names, validate what the request carries before any subprocess
// can start, open the repository under a read lock current with the
// log, answer 304 to a revalidation without running git, and take one
// slot of the node's subprocess semaphore (spec 012). It reports
// whether the handler goes on; the release runs when the request is
// done. The handler then bounds its subprocesses with budget, so a
// catch-up of the copy is not charged to the read.
//
// One slot covers the whole read, not one per subprocess: a read runs
// rev-parse and then its operation in sequence under one budget (spec
// 009), and taking the slot again between them would refuse a request
// that had already begun.
func (h *Handler) open(w http.ResponseWriter, r *http.Request, op string, validate func() error) (*readRequest, func(), bool) {
	id := r.PathValue("id")
	if _, ok := h.admit(w, r, id, auth.ActionRead); !ok {
		return nil, nil, false
	}
	if err := validate(); err != nil {
		writeReadError(w, err)
		return nil, nil, false
	}
	l, err := h.cache.Lease(r.Context(), id, false)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrDeleted):
			// A purged repository answers 410 gone from its tombstone
			// (spec 019); anything else the log does not hold is 404.
			h.notFoundOrGone(w, r, id)
		default:
			h.storageError(w, r, err)
		}
		return nil, nil, false
	}
	rp, release := l.Repo, l.Release
	// Served from the copy without a currency check while the read
	// breaker is open (spec 015): Origo-Stale carries the whole seconds
	// since the last check that answered, on every read endpoint alike.
	if l.Stale {
		w.Header().Set(contract.HeaderStale, strconv.Itoa(int(l.StaleFor/time.Second)))
	}
	etag := `"` + strconv.FormatUint(rp.Seq, 10) + `"`
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		release()
		w.WriteHeader(http.StatusNotModified)
		return nil, nil, false
	}
	slot, ok := h.limits.Slot(w, r)
	if !ok {
		release()
		return nil, nil, false
	}
	return &readRequest{h: h, w: w, id: id, repo: rp, op: op}, func() { slot(); release() }, true
}

// etagMatches reports whether an If-None-Match header names the tag.
func etagMatches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// writeReadError renders a refusal, or a *readError's envelope.
func writeReadError(w http.ResponseWriter, err error) {
	if re, ok := errors.AsType[*readError](err); ok {
		contract.Write(w, re.status, re.code, re.details)
		return
	}
	contract.Write(w, http.StatusInternalServerError, contract.CodeStorageUnavailable, map[string]any{"error": err.Error()})
}

// fail answers a failed subprocess: 504 operation_timeout when the
// request's budget ran out, 503 otherwise. A response already begun is
// cut short instead, which the client sees as a truncated body.
func (rr *readRequest) fail(ctx context.Context, err error) {
	if rr.wrote {
		rr.h.logger.WarnContext(ctx, "read cut short", "repo", rr.id, "operation", rr.op, "error", err)
		return
	}
	if re, ok := errors.AsType[*readError](err); ok {
		contract.Write(rr.w, re.status, re.code, re.details)
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		contract.Write(rr.w, http.StatusGatewayTimeout, contract.CodeOperationTimeout, map[string]any{
			"operation": rr.op, "budget_seconds": int(rr.h.readTimeout / time.Second),
		})
		return
	}
	rr.h.logger.ErrorContext(ctx, "read failed", "repo", rr.id, "operation", rr.op, "error", err)
	rr.h.retryAfter(rr.w, err)
	contract.Write(rr.w, http.StatusServiceUnavailable, contract.CodeStorageUnavailable, wal.ErrorDetails(err))
}

// gitCommand builds a git subprocess against the repository under the
// request's deadline: --no-pager, the environment of spec 016, and the
// whole process group killed when the context ends.
func (rr *readRequest) gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := rr.h.cache.Git().Command(ctx, rr.repo.Dir, append([]string{"--no-pager"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}

// run runs a subprocess to completion and returns its stdout.
func (rr *readRequest) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := rr.gitCommand(ctx, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &repo.Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

// resolve turns a name from the request into an object id with git's
// own rules, peeled as the operation needs; an unresolvable name is
// 404 ref_not_found with the name in details.
func (rr *readRequest) resolve(ctx context.Context, name, peel string) (string, error) {
	out, err := rr.run(ctx, nil, "rev-parse", "--verify", "-q", "--end-of-options", name+peel)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return "", refNotFound(name)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// stream is a running subprocess whose stdout the handler reads.
type stream struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr bytes.Buffer
	cancel context.CancelFunc
	args   []string
}

// start begins a subprocess with its stdout as a pipe. The caller
// reads it and calls finish, which ends the subprocess if it is still
// running, as a page or a cut leaves it.
func (rr *readRequest) start(ctx context.Context, stdin io.Reader, args ...string) (*stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := rr.gitCommand(ctx, args...)
	cmd.Stdin = stdin
	s := &stream{cmd: cmd, cancel: cancel, args: args}
	cmd.Stderr = &s.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	s.out = out
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

// finish waits for the subprocess. With done true the reader took what
// it needed and the subprocess is ended first; with done false a
// non-zero exit is the error.
func (s *stream) finish(done bool) error {
	if done {
		s.cancel()
	}
	err := s.cmd.Wait()
	s.cancel()
	if err != nil && !done {
		return &repo.Error{Args: s.args, Err: err, Stderr: strings.TrimSpace(s.stderr.String())}
	}
	return nil
}

// refs answers GET /v1/repos/{id}/refs.
func (h *Handler) refs(w http.ResponseWriter, r *http.Request) {
	var prefix string
	rr, release, ok := h.open(w, r, "refs", func() (err error) {
		prefix, err = value(r, "prefix")
		return err
	})
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	if prefix == "" {
		prefix = "refs/"
	}
	// git matches a pattern as a whole name or a directory, so the
	// directory of the prefix is listed and the prefix filters it here,
	// the stream ending once the page past the cap is seen.
	dir := prefix[:strings.LastIndex(prefix, "/")+1]
	args := []string{"for-each-ref", "--format=%(refname)%00%(objectname)%00%(*objectname)"}
	if dir != "" {
		args = append(args, "--end-of-options", dir)
	}
	s, err := rr.start(ctx, nil, args...)
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	type ref struct {
		Name   string  `json:"name"`
		SHA    string  `json:"sha"`
		Peeled *string `json:"peeled"`
	}
	refs := []ref{}
	sc := bufio.NewScanner(s.out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() && len(refs) <= MaxRefs {
		f := strings.SplitN(sc.Text(), "\x00", 3)
		if len(f) != 3 || !strings.HasPrefix(f[0], prefix) {
			continue
		}
		e := ref{Name: f[0], SHA: f[1]}
		if f[2] != "" {
			e.Peeled = &f[2]
		}
		refs = append(refs, e)
	}
	if err := sc.Err(); err != nil {
		_ = s.finish(true)
		rr.fail(ctx, err)
		return
	}
	truncated := len(refs) > MaxRefs
	if err := s.finish(truncated); err != nil {
		rr.fail(ctx, err)
		return
	}
	if truncated {
		refs = refs[:MaxRefs]
		w.Header().Set(HeaderTruncated, "true")
	}
	httpjson.Write(w, http.StatusOK, refs)
}

// commitJSON is one commit of the log endpoints.
type commitJSON struct {
	SHA       string    `json:"sha"`
	Parents   []string  `json:"parents"`
	Author    identity  `json:"author"`
	Committer identity  `json:"committer"`
	Message   string    `json:"message"`
	Trailers  []trailer `json:"trailers"`
	Stats     *stats    `json:"stats,omitempty"`
}

type identity struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	At    time.Time `json:"at"`
}

type trailer struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type stats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// commitFormat is the pretty format both log endpoints read: NUL
// separated fields, the trailers parsed by git the way
// interpret-trailers --parse does (only the trailer lines, unfolded).
const commitFormat = "%H%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%B%x00%(trailers:only,unfold)%x00"

const commitFields = 10

// parseCommit builds a commit from the ten fields of commitFormat.
func parseCommit(f []string) (commitJSON, error) {
	c := commitJSON{SHA: f[0], Parents: strings.Fields(f[1]), Message: f[8], Trailers: []trailer{}}
	if c.Parents == nil {
		c.Parents = []string{}
	}
	var err error
	if c.Author, err = parseIdentity(f[2], f[3], f[4]); err != nil {
		return c, err
	}
	if c.Committer, err = parseIdentity(f[5], f[6], f[7]); err != nil {
		return c, err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(f[9], "\n"), "\n") {
		if key, v, ok := strings.Cut(line, ":"); ok {
			c.Trailers = append(c.Trailers, trailer{Key: strings.TrimSpace(key), Value: strings.TrimSpace(v)})
		}
	}
	return c, nil
}

func parseIdentity(name, email, at string) (identity, error) {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return identity{}, fmt.Errorf("commit date %q: %w", at, err)
	}
	return identity{Name: name, Email: email, At: t}, nil
}

// commitScanner reads commit records from a rev-list --no-commit-header
// stream: ten NUL terminated fields, then the newline git adds.
type commitScanner struct {
	sc *bufio.Scanner
}

func newCommitScanner(r io.Reader) *commitScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	return &commitScanner{sc: sc}
}

// next returns the next commit, or false at the end of the stream.
func (s *commitScanner) next() (commitJSON, bool, error) {
	fields := make([]string, 0, commitFields)
	for len(fields) < commitFields && s.sc.Scan() {
		f := s.sc.Text()
		if len(fields) == 0 {
			f = strings.TrimPrefix(f, "\n")
		}
		fields = append(fields, f)
	}
	if len(fields) == 0 || len(fields) == 1 && fields[0] == "" {
		return commitJSON{}, false, s.sc.Err()
	}
	if len(fields) < commitFields {
		return commitJSON{}, false, fmt.Errorf("rev-list record with %d fields", len(fields))
	}
	c, err := parseCommit(fields)
	return c, err == nil, err
}

// commitsQuery is what GET /v1/repos/{id}/commits carries.
type commitsQuery struct {
	ref, path, cursor string
	since, until      string
	limit             int
}

func parseCommitsQuery(r *http.Request) (commitsQuery, error) {
	var q commitsQuery
	var err error
	if q.ref, err = value(r, "ref"); err != nil {
		return q, err
	}
	if q.ref == "" {
		q.ref = "HEAD"
	}
	if q.path, err = pathValue(r); err != nil {
		return q, err
	}
	if q.cursor, err = value(r, "cursor"); err != nil {
		return q, err
	}
	for _, p := range []struct {
		name string
		dst  *string
	}{{"since", &q.since}, {"until", &q.until}} {
		v := r.URL.Query().Get(p.name)
		if v == "" {
			continue
		}
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			return q, invalidRead(p.name + ": not an RFC 3339 time")
		}
		*p.dst = t.UTC().Format(time.RFC3339)
	}
	q.limit = DefaultCommitLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > MaxCommitLimit {
			return q, invalidRead("limit: an integer from 1 to " + strconv.Itoa(MaxCommitLimit))
		}
		q.limit = n
	}
	return q, nil
}

// commits answers GET /v1/repos/{id}/commits: a page of the walk from
// the reference, newest first, exact whatever the graph because the
// walk starts at the reference every time and the cursor is where the
// previous page ended.
func (h *Handler) commits(w http.ResponseWriter, r *http.Request) {
	var q commitsQuery
	rr, release, ok := h.open(w, r, "commits", func() (err error) {
		q, err = parseCommitsQuery(r)
		return err
	})
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	oid, err := rr.resolve(ctx, q.ref, "^{commit}")
	if err != nil {
		if re, ok := errors.AsType[*readError](err); ok && re.code == contract.CodeRefNotFound && !hasHistory(rr.repo) {
			// No commit yet: the reference cannot exist, and the log
			// is empty rather than missing.
			httpjson.Write(w, http.StatusOK, map[string]any{"commits": []commitJSON{}, "next_cursor": nil})
			return
		}
		rr.fail(ctx, err)
		return
	}
	args := []string{"rev-list", "--no-commit-header", "--format=" + commitFormat}
	if q.since != "" {
		args = append(args, "--since="+q.since)
	}
	if q.until != "" {
		args = append(args, "--until="+q.until)
	}
	args = append(args, "--end-of-options", oid)
	if q.path != "" {
		args = append(args, "--", q.path)
	}
	s, err := rr.start(ctx, nil, args...)
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	page := []commitJSON{}
	var next *string
	skipping := q.cursor != ""
	sc := newCommitScanner(s.out)
	for {
		c, ok, err := sc.next()
		if err != nil {
			_ = s.finish(true)
			rr.fail(ctx, err)
			return
		}
		if !ok {
			break
		}
		if skipping {
			skipping = c.SHA != q.cursor
			continue
		}
		if len(page) == q.limit {
			last := page[len(page)-1].SHA
			next = &last
			break
		}
		page = append(page, c)
	}
	if err := s.finish(next != nil); err != nil {
		rr.fail(ctx, err)
		return
	}
	if skipping {
		rr.fail(ctx, invalidRead("cursor: not a commit of this walk"))
		return
	}
	w.Header().Set(HeaderCommit, oid)
	httpjson.Write(w, http.StatusOK, map[string]any{"commits": page, "next_cursor": next})
}

// hasHistory reports whether the repository has any reference besides
// HEAD, which is what a commit needs to be reachable.
func hasHistory(rp *repo.Repo) bool {
	for name := range rp.Index.Refs {
		if name != "HEAD" {
			return true
		}
	}
	return false
}

// commit answers GET /v1/repos/{id}/commits/{sha}: the commit with the
// numstat of its change.
func (h *Handler) commit(w http.ResponseWriter, r *http.Request) {
	var name string
	rr, release, ok := h.open(w, r, "commits", func() (err error) {
		name, err = target(r, r.PathValue("sha"), "ref")
		return err
	})
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	oid, err := rr.resolve(ctx, name, "^{commit}")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	// Renames are detected as compare detects them, and a merge's stats
	// are against its first parent whatever git's default for show.
	out, err := rr.run(ctx, nil, "show", "--numstat", "-M", "--diff-merges=first-parent", "--format="+commitFormat, "--end-of-options", oid)
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	fields := strings.SplitN(string(out), "\x00", commitFields+1)
	if len(fields) != commitFields+1 {
		rr.fail(ctx, fmt.Errorf("show: %d fields", len(fields)))
		return
	}
	c, err := parseCommit(fields[:commitFields])
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	c.Stats = &stats{}
	for line := range strings.SplitSeq(fields[commitFields], "\n") {
		add, rest, ok := strings.Cut(line, "\t")
		del, _, ok2 := strings.Cut(rest, "\t")
		if !ok || !ok2 {
			continue
		}
		c.Stats.Files++
		// A binary file counts as a file with 0 lines.
		if a, err := strconv.Atoi(add); err == nil {
			c.Stats.Additions += a
		}
		if d, err := strconv.Atoi(del); err == nil {
			c.Stats.Deletions += d
		}
	}
	w.Header().Set(HeaderCommit, oid)
	httpjson.Write(w, http.StatusOK, c)
}

// compare answers GET /v1/repos/{id}/compare/{base}...{head}: the diff
// as text, cut at a file boundary past 1 MiB.
func (h *Handler) compare(w http.ResponseWriter, r *http.Request) {
	var base, head, path string
	rr, release, ok := h.open(w, r, "compare", func() (err error) {
		segs := strings.Split(r.PathValue("range"), "...")
		if len(segs) != 2 {
			return invalidRead("ref: the segment is {base}...{head}")
		}
		if base, err = target(r, segs[0], "base"); err != nil {
			return err
		}
		if head, err = target(r, segs[1], "head"); err != nil {
			return err
		}
		path, err = pathValue(r)
		return err
	})
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	// Tags are peeled to what they point at; a commit or a tree stays.
	baseOID, err := rr.resolve(ctx, base, "^{}")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	headOID, err := rr.resolve(ctx, head, "^{}")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	args := []string{"diff", "-M", "--no-color", "--end-of-options", baseOID, headOID}
	if path != "" {
		args = append(args, "--", path)
	}
	s, err := rr.start(ctx, nil, args...)
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	buf, err := io.ReadAll(io.LimitReader(s.out, MaxCompareBytes+1))
	if err != nil {
		_ = s.finish(true)
		rr.fail(ctx, err)
		return
	}
	truncated := len(buf) > MaxCompareBytes
	if err := s.finish(truncated); err != nil {
		rr.fail(ctx, err)
		return
	}
	if truncated {
		buf = cutAtFileBoundary(buf[:MaxCompareBytes])
		w.Header().Set(HeaderTruncated, "true")
	}
	w.Header().Set(HeaderCommit, headOID)
	w.Header().Set("Content-Type", "text/x-diff")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// cutAtFileBoundary drops the last, partial file of a diff: everything
// from the last "diff --git" header on.
func cutAtFileBoundary(buf []byte) []byte {
	const header = "\ndiff --git "
	if bytes.HasPrefix(buf, []byte(header[1:])) && !bytes.Contains(buf[1:], []byte(header)) {
		return nil
	}
	if i := bytes.LastIndex(buf, []byte(header)); i >= 0 {
		return buf[:i+1]
	}
	return nil
}

// treeEntry is one entry of a tree page.
type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size"`
}

// tree answers GET /v1/repos/{id}/tree/{sha}: a page of the entries
// of a tree, the cursor the last path of the previous page.
func (h *Handler) tree(w http.ResponseWriter, r *http.Request) {
	var name, path, cursor string
	var recursive bool
	rr, release, ok := h.open(w, r, "tree", func() (err error) {
		if name, err = target(r, r.PathValue("sha"), "ref"); err != nil {
			return err
		}
		if path, err = pathValue(r); err != nil {
			return err
		}
		if cursor, err = value(r, "cursor"); err != nil {
			return err
		}
		switch r.URL.Query().Get("recursive") {
		case "", "0", "false":
		case "1", "true":
			recursive = true
		default:
			return invalidRead("recursive: 0 or 1")
		}
		return nil
	})
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	oid, err := rr.resolve(ctx, name, "^{}")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	args := []string{"ls-tree", "-l", "-z"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, "--end-of-options", oid)
	if path != "" {
		// A trailing slash lists the directory's entries rather than
		// the directory itself.
		args = append(args, "--", path+"/")
	}
	s, err := rr.start(ctx, nil, args...)
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	entries := []treeEntry{}
	var next *string
	skipping := cursor != ""
	sc := bufio.NewScanner(s.out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		e, ok := parseTreeEntry(sc.Text())
		if !ok {
			continue
		}
		if skipping {
			skipping = e.Path != cursor
			continue
		}
		if len(entries) == TreePageSize {
			last := entries[len(entries)-1].Path
			next = &last
			break
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		_ = s.finish(true)
		rr.fail(ctx, err)
		return
	}
	if err := s.finish(next != nil); err != nil {
		rr.fail(ctx, err)
		return
	}
	if skipping {
		rr.fail(ctx, invalidRead("cursor: not a path of this listing"))
		return
	}
	w.Header().Set(HeaderCommit, oid)
	httpjson.Write(w, http.StatusOK, map[string]any{"entries": entries, "next_cursor": next})
}

// parseTreeEntry reads one ls-tree -l record: mode, type, id, and the
// size right-aligned, a tab, then the path.
func parseTreeEntry(rec string) (treeEntry, bool) {
	meta, path, ok := strings.Cut(rec, "\t")
	f := strings.Fields(meta)
	if !ok || len(f) != 4 {
		return treeEntry{}, false
	}
	e := treeEntry{Path: path, Mode: f[0], Type: f[1], SHA: f[2]}
	if n, err := strconv.ParseInt(f[3], 10, 64); err == nil {
		e.Size = &n
	}
	return e, true
}

// blob answers GET /v1/repos/{id}/blob/{sha}: the raw bytes, a Range
// of them, or 413 for a blob over the bound asked for whole.
func (h *Handler) blob(w http.ResponseWriter, r *http.Request) {
	var name string
	rr, release, ok := h.open(w, r, "blob", func() (err error) {
		name, err = target(r, r.PathValue("sha"), "ref")
		return err
	})
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	oid, err := rr.resolve(ctx, name, "^{blob}")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	s, err := rr.start(ctx, strings.NewReader(oid+"\n"), "cat-file", "--batch")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	body := bufio.NewReaderSize(s.out, 64<<10)
	line, err := body.ReadString('\n')
	if err != nil {
		_ = s.finish(true)
		rr.fail(ctx, fmt.Errorf("cat-file: %w", err))
		return
	}
	f := strings.Fields(line)
	if len(f) != 3 || f[1] != "blob" {
		_ = s.finish(true)
		rr.fail(ctx, fmt.Errorf("cat-file: %q", strings.TrimSpace(line)))
		return
	}
	size, _ := strconv.ParseInt(f[2], 10, 64)
	start, end, status, err := blobRange(r.Header.Get("Range"), size)
	if err != nil {
		_ = s.finish(true)
		if re, ok := errors.AsType[*readError](err); ok && re.status == http.StatusRequestedRangeNotSatisfiable {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		}
		rr.fail(ctx, err)
		return
	}
	if end-start+1 > MaxBlobBytes {
		_ = s.finish(true)
		rr.fail(ctx, &readError{status: http.StatusRequestEntityTooLarge, code: contract.CodeBlobTooLarge, details: map[string]any{"size": size, "max": MaxBlobBytes}})
		return
	}
	// The content type is sniffed over the first 512 bytes of the blob
	// whatever the Range.
	head, _ := body.Peek(int(min(size, 512)))
	w.Header().Set("Content-Type", http.DetectContentType(head))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set(HeaderCommit, oid)
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	if start > 0 {
		if _, err := io.CopyN(io.Discard, body, start); err != nil {
			_ = s.finish(true)
			rr.fail(ctx, err)
			return
		}
	}
	w.WriteHeader(status)
	rr.wrote = true
	if _, err := io.CopyN(w, body, end-start+1); err != nil {
		_ = s.finish(true)
		rr.fail(ctx, err)
		return
	}
	_ = s.finish(true)
}

// blobRange interprets a Range header over a blob of size bytes: the
// first and last byte served and the status. Without a header the
// whole blob is served with 200. A header outside the single-range
// form is refused, and a range past the end is 416.
func blobRange(header string, size int64) (start, end int64, status int, err error) {
	if header == "" {
		return 0, size - 1, http.StatusOK, nil
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	first, last, dash := strings.Cut(spec, "-")
	if !ok || !dash || strings.Contains(spec, ",") {
		return 0, 0, 0, invalidRead("range: one bytes=<first>-<last> range")
	}
	switch {
	case first == "" && last != "":
		// The last n bytes.
		n, perr := strconv.ParseInt(last, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, 0, invalidRead("range: one bytes=<first>-<last> range")
		}
		start = max(size-n, 0)
		end = size - 1
	case first != "":
		var perr error
		if start, perr = strconv.ParseInt(first, 10, 64); perr != nil || start < 0 {
			return 0, 0, 0, invalidRead("range: one bytes=<first>-<last> range")
		}
		end = size - 1
		if last != "" {
			if end, perr = strconv.ParseInt(last, 10, 64); perr != nil || end < start {
				return 0, 0, 0, invalidRead("range: one bytes=<first>-<last> range")
			}
			end = min(end, size-1)
		}
	default:
		return 0, 0, 0, invalidRead("range: one bytes=<first>-<last> range")
	}
	if start >= size {
		return 0, 0, 0, &readError{status: http.StatusRequestedRangeNotSatisfiable, code: contract.CodeInvalid, details: map[string]any{"reason": "range: past the end of the blob", "size": size}}
	}
	return start, end, http.StatusPartialContent, nil
}

// archive answers GET /v1/repos/{id}/archive/{sha}.tar.gz: git's own
// tar.gz of the commit's tree, streamed as git produces it.
func (h *Handler) archive(w http.ResponseWriter, r *http.Request) {
	var name string
	rr, release, ok := h.open(w, r, "archive", func() (err error) {
		file, ok := strings.CutSuffix(r.PathValue("file"), ".tar.gz")
		if !ok {
			return invalidRead("archive: the format is .tar.gz")
		}
		name, err = target(r, file, "ref")
		return err
	})
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	oid, err := rr.resolve(ctx, name, "^{commit}")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	m, err := h.log.ReadMeta(ctx, rr.id)
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	prefix := m.Slug + "-" + oid[:7]
	s, err := rr.start(ctx, nil, "archive", "--format=tar.gz", "--prefix="+prefix+"/", "--end-of-options", oid)
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	for {
		n, rerr := s.out.Read(buf)
		if n > 0 {
			if !rr.wrote {
				w.Header().Set(HeaderCommit, oid)
				w.Header().Set("Content-Type", "application/gzip")
				w.Header().Set("Content-Disposition", `attachment; filename="`+prefix+`.tar.gz"`)
				w.WriteHeader(http.StatusOK)
				rr.wrote = true
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				_ = s.finish(true)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := s.finish(false); err != nil {
		rr.fail(ctx, err)
	}
}
