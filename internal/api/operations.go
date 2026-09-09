// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/wal"
)

// The server-side git operations of spec 020: a commit built from a
// description of the change, a merge, a cherry-pick, and a revert, each
// run on the node's warm copy and committed through the log as a push
// entry, so one operation is one entry, one commit, and one push event.
// A consumer that changes many repositories makes a request instead of
// a clone.

// The bounds of spec 020's Limits table.
const (
	// MaxChanges is how many changes one commits request carries.
	MaxChanges = 1000
	// MaxContentBytes is the decoded content of one file; larger
	// content is uploaded through LFS or pushed.
	MaxContentBytes = 10 << 20
	// MaxOperationBytes is the request body of every operation. Spec
	// 012's 64 KiB JSON limit does not apply to these routes.
	MaxOperationBytes = 64 << 20
	// MaxPickCommits is how many commits one cherry-pick or revert
	// applies.
	MaxPickCommits = 100
	// MaxMessageBytes is the longest commit message.
	MaxMessageBytes = 64 << 10
	// OperationsPerMinute is the token bucket per repository per node,
	// the way spec 012's bucket is one per subject.
	OperationsPerMinute = 60
	// MergeBudget bounds a merge, a cherry-pick, and a revert; commits
	// runs under the read budget of spec 009.
	MergeBudget = 5 * time.Minute
)

// The names one operation is known by: the route, the value of the push
// option origo.operation, and the operation field of the push event
// (spec 008).
const (
	OpCommits     = "commits"
	OpMerge       = "merge"
	OpCherryPick  = "cherry-pick"
	OpRevert      = "revert"
	opCommitsPath = "/v1/repos/{id}/commits"
)

// The merge strategies of spec 020's Operations table.
const (
	StrategyFastForwardOnly       = "fast_forward_only"
	StrategyMergeCommit           = "merge_commit"
	StrategyFastForwardIfPossible = "fast_forward_if_possible"
)

// The values of details.reason on invalid_change, spec 020's closed
// set. Everything else a request gets wrong is invalid_request with
// details.field.
const (
	ReasonPath     = "path"
	ReasonMode     = "mode"
	ReasonContent  = "content"
	ReasonTooMany  = "too_many"
	ReasonTooLarge = "too_large"
)

// The file modes a change may carry.
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
)

// registerOperations mounts the four routes. Each is a POST beside a
// read of spec 009 under the same path, which the method separates.
func (h *Handler) registerOperations(mux *http.ServeMux) {
	mux.HandleFunc("POST "+opCommitsPath, h.opCommits)
	mux.HandleFunc("POST /v1/repos/{id}/merge", h.opMerge)
	mux.HandleFunc("POST /v1/repos/{id}/cherry-pick", h.opCherryPick)
	mux.HandleFunc("POST /v1/repos/{id}/revert", h.opRevert)
}

// identityRequest is the author of the commit an operation writes.
type identityRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// common is the shape every operation shares (spec 020's Common shape).
type common struct {
	Branch       string           `json:"branch"`
	ExpectedHead *string          `json:"expected_head"`
	Author       *identityRequest `json:"author"`
	Message      string           `json:"message"`
	DryRun       bool             `json:"dry_run"`
}

// change is one file of a commits request.
type change struct {
	Path       string  `json:"path"`
	Content    *string `json:"content"`
	ContentRef string  `json:"content_ref"`
	Delete     bool    `json:"delete"`
	Mode       string  `json:"mode"`
}

type commitsRequest struct {
	common
	// CreateBranch and From are the commits operation's own: the branch
	// is created at From, whose tree the first commit starts from and
	// whose commit is its parent. From is absent, null, or a name.
	CreateBranch bool            `json:"create_branch"`
	From         json.RawMessage `json:"from"`
	Changes      []change        `json:"changes"`
}

type mergeRequest struct {
	common
	Source   string `json:"source"`
	Strategy string `json:"strategy"`
}

type pickRequest struct {
	common
	Commits  []string `json:"commits"`
	Mainline int      `json:"mainline"`
}

// operationResult is the answer of every operation: 201 with the entry
// the commit produced, or 200 with committed false for a dry run.
type operationResult struct {
	Commit    string  `json:"commit"`
	Branch    string  `json:"branch"`
	EntrySeq  *uint64 `json:"entry_seq"`
	Tree      string  `json:"tree"`
	Committed *bool   `json:"committed,omitempty"`
}

// invalidChange answers a change the request got wrong: the index of
// the change and one reason of the closed set.
func invalidChange(w http.ResponseWriter, index int, reason string) {
	contract.Write(w, http.StatusBadRequest, contract.CodeInvalidChange, map[string]any{
		"index": index, "reason": reason,
	})
}

// changeError is a refusal a change earned, carried out of the work so
// the caller answers it after the subprocesses are done with.
type changeError struct {
	index  int
	reason string
}

func (e *changeError) Error() string { return "api: change " + e.reason }

// nonFastForward is spec 003's 409 with the details spec 020 fixes: the
// reference, the commit the caller expected there, and the one that is
// there. expected is null for a create_branch on a branch that exists.
func nonFastForward(w http.ResponseWriter, ref string, expected *string, actual string) {
	details := map[string]any{"ref": ref, "expected": nil, "actual": actual}
	if expected != nil {
		details["expected"] = *expected
	}
	contract.Write(w, http.StatusConflict, contract.CodeNonFastForward, details)
}

// decodeOperation reads a request body of at most 64 MiB. A body past
// the bound is 400 invalid_request, the answer spec 020's Limits table
// gives it, and so is a body that does not parse.
func (h *Handler) decodeOperation(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength > MaxOperationBytes {
		invalid(w, "body: larger than the 64 MiB request limit", "")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxOperationBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			invalid(w, "body: larger than the 64 MiB request limit", "")
			return false
		}
		invalid(w, "body: "+err.Error(), "")
		return false
	}
	return true
}

// committer is Origo at the host of ORIGO_PUBLIC_URL, the identity every
// server-side commit is committed by whoever authored it.
func (h *Handler) committer() (name, email string) {
	host := "localhost"
	if u, err := url.Parse(h.signer.Issuer()); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return "Origo", "origo@" + host
}

// opBranch normalizes the branch a request names: a full refs/heads/
// name or a short one, refused when it is not a valid reference name or
// would be read by git as an option.
func opBranch(name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "-") {
		return "", false
	}
	full := name
	if !strings.HasPrefix(full, "refs/heads/") {
		full = "refs/heads/" + full
	}
	if !wal.ValidRefName(full) {
		return "", false
	}
	return full, true
}

// validSHA reports whether a value is an object id as the request may
// carry one: hexadecimal of a whole SHA-1 or SHA-256 name.
func validSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validRevision is the rule for a name a request hands to git as a
// revision: no leading dash, no option, and short enough to be a name.
func validRevision(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") && len(s) <= 512 && !strings.ContainsAny(s, "\x00\n")
}

// validAuthor is spec 020's rule: both fields present and an email with
// one @.
func validAuthor(a *identityRequest) bool {
	if a == nil || a.Name == "" || a.Email == "" {
		return false
	}
	if strings.ContainsAny(a.Name+a.Email, "\x00\n<>") {
		return false
	}
	return strings.Count(a.Email, "@") == 1
}

// validate checks the fields every operation shares. It answers the
// full branch name and whether the handler goes on.
func (h *Handler) validateCommon(w http.ResponseWriter, c *common) (string, bool) {
	branch, ok := opBranch(c.Branch)
	if !ok {
		invalid(w, "branch must be refs/heads/<name> or a short branch name", "branch")
		return "", false
	}
	if !validAuthor(c.Author) {
		invalid(w, "author needs a name and an email with one @", "author")
		return "", false
	}
	if len(c.Message) > MaxMessageBytes {
		invalid(w, "message is at most 64 KiB", "message")
		return "", false
	}
	if c.ExpectedHead != nil && !validSHA(*c.ExpectedHead) {
		invalid(w, "expected_head must be a whole object id", "expected_head")
		return "", false
	}
	return branch, true
}

// begin is the prologue of every operation: the body is read, the
// write is authorized on the id the path names, the repository's own
// state is checked, and the repository's bucket takes a token. It
// answers the operation with the decision behind it.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request, name string, budget time.Duration, body any) (*operation, bool) {
	if !h.decodeOperation(w, r, body) {
		return nil, false
	}
	m, ix, d, ok := h.loadDecision(w, r, auth.ActionWrite, false)
	if !ok {
		return nil, false
	}
	// A frozen repository accepts reads and refuses every write (spec
	// 019), an operation like a push.
	if m.FrozenAt != nil {
		contract.Write(w, http.StatusForbidden, contract.CodeRepoFrozen, map[string]any{"frozen_at": m.FrozenAt})
		return nil, false
	}
	if allowed, retry := h.operations.Allow(m.ID); !allowed {
		h.limits.Refused(limits.LimitRepository)
		h.logger.WarnContext(r.Context(), "repository rate limited", "repo", m.ID, "path", r.URL.Path,
			"retry_after_ms", retry.Milliseconds())
		limits.WriteRateLimited(w, limits.LimitRepository, retry)
		return nil, false
	}
	return &operation{h: h, w: w, r: r, name: name, budget: budget, id: m.ID, base: ix, quota: d.QuotaBytes}, true
}

// opCommits answers POST /v1/repos/{id}/commits.
func (h *Handler) opCommits(w http.ResponseWriter, r *http.Request) {
	var req commitsRequest
	op, ok := h.begin(w, r, OpCommits, h.readTimeout, &req)
	if !ok {
		return
	}
	branch, ok := h.validateCommon(w, &req.common)
	if !ok {
		return
	}
	// create_branch and from are one field: neither alone is a request.
	hasFrom := len(req.From) > 0
	if req.CreateBranch != hasFrom {
		field := "from"
		if hasFrom {
			field = "create_branch"
		}
		invalid(w, "create_branch and from are given together", field)
		return
	}
	var from string
	if hasFrom && string(req.From) != "null" {
		if err := json.Unmarshal(req.From, &from); err != nil || !validRevision(from) {
			invalid(w, "from is a commit id or a branch name in this repository", "from")
			return
		}
	}
	switch {
	case req.CreateBranch && req.ExpectedHead != nil:
		invalid(w, "expected_head is null when create_branch is true", "expected_head")
		return
	case !req.CreateBranch && req.ExpectedHead == nil:
		invalid(w, "expected_head names the commit the branch is at", "expected_head")
		return
	case len(req.Changes) == 0:
		invalid(w, "changes holds 1 to 1000 changes", "changes")
		return
	case len(req.Changes) > MaxChanges:
		invalidChange(w, MaxChanges, ReasonTooMany)
		return
	case req.Message == "":
		invalid(w, "message is the commit message", "message")
		return
	}
	// Every change is checked before a subprocess starts, so a request
	// with a path the read rules refuse costs no git at all.
	decoded := make([][]byte, len(req.Changes))
	for i, c := range req.Changes {
		content, reason := checkChange(c)
		if reason != "" {
			invalidChange(w, i, reason)
			return
		}
		decoded[i] = content
	}
	op.run(w, r, branch, &req.common, func(o *operation) (string, error) {
		return o.commits(&req, from, decoded)
	})
}

// checkChange holds one change to spec 020's rules and answers its
// decoded content, or the reason it is refused.
func checkChange(c change) ([]byte, string) {
	if c.Path == "" || !ValidPath(c.Path) {
		return nil, ReasonPath
	}
	forms := 0
	if c.Content != nil {
		forms++
	}
	if c.ContentRef != "" {
		forms++
	}
	if c.Delete {
		forms++
	}
	if forms != 1 {
		return nil, ReasonContent
	}
	if c.Delete {
		if c.Mode != "" {
			return nil, ReasonMode
		}
		return nil, ""
	}
	switch c.Mode {
	case "", ModeFile, ModeExecutable, ModeSymlink:
	default:
		return nil, ReasonMode
	}
	if c.ContentRef != "" {
		if !validSHA(c.ContentRef) {
			return nil, ReasonContent
		}
		return nil, ""
	}
	content, err := base64.StdEncoding.DecodeString(*c.Content)
	if err != nil {
		return nil, ReasonContent
	}
	if len(content) > MaxContentBytes {
		return nil, ReasonTooLarge
	}
	// A symbolic link's content is the target it names, held to the
	// path rules of spec 009 like the path itself.
	if c.Mode == ModeSymlink {
		target := string(content)
		if target == "" || !ValidPath(target) {
			return nil, ReasonPath
		}
	}
	return content, ""
}

// opMerge answers POST /v1/repos/{id}/merge.
func (h *Handler) opMerge(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	op, ok := h.begin(w, r, OpMerge, MergeBudget, &req)
	if !ok {
		return
	}
	branch, ok := h.validateCommon(w, &req.common)
	if !ok {
		return
	}
	if req.Strategy == "" {
		req.Strategy = StrategyFastForwardIfPossible
	}
	switch {
	case !validRevision(req.Source):
		invalid(w, "source is a branch name or a commit id", "source")
		return
	case req.Strategy != StrategyFastForwardOnly && req.Strategy != StrategyMergeCommit && req.Strategy != StrategyFastForwardIfPossible:
		invalid(w, "strategy is fast_forward_only, merge_commit, or fast_forward_if_possible", "strategy")
		return
	case req.ExpectedHead == nil:
		invalid(w, "expected_head names the commit the branch is at", "expected_head")
		return
	}
	op.run(w, r, branch, &req.common, func(o *operation) (string, error) { return o.merge(&req) })
}

// opCherryPick answers POST /v1/repos/{id}/cherry-pick.
func (h *Handler) opCherryPick(w http.ResponseWriter, r *http.Request) {
	h.opPick(w, r, OpCherryPick)
}

// opRevert answers POST /v1/repos/{id}/revert.
func (h *Handler) opRevert(w http.ResponseWriter, r *http.Request) {
	h.opPick(w, r, OpRevert)
}

// opPick serves the cherry-pick and the revert, which differ only in
// which side of the picked commit is applied.
func (h *Handler) opPick(w http.ResponseWriter, r *http.Request, name string) {
	var req pickRequest
	op, ok := h.begin(w, r, name, MergeBudget, &req)
	if !ok {
		return
	}
	branch, ok := h.validateCommon(w, &req.common)
	if !ok {
		return
	}
	switch {
	case len(req.Commits) == 0:
		invalid(w, "commits holds 1 to 100 commit ids", "commits")
		return
	case len(req.Commits) > MaxPickCommits:
		invalidChange(w, MaxPickCommits, ReasonTooMany)
		return
	case req.Mainline < 0:
		invalid(w, "mainline is the 1-based parent of a merge commit", "mainline")
		return
	case req.ExpectedHead == nil:
		invalid(w, "expected_head names the commit the branch is at", "expected_head")
		return
	}
	for _, c := range req.Commits {
		if !validSHA(c) {
			invalid(w, "commits holds whole object ids", "commits")
			return
		}
	}
	op.run(w, r, branch, &req.common, func(o *operation) (string, error) { return o.pick(&req, name) })
}

// writeResult renders the answer of an operation.
func writeResult(w http.ResponseWriter, res operationResult, dryRun bool) {
	if dryRun {
		no := false
		res.Committed = &no
		httpjson.Write(w, http.StatusOK, res)
		return
	}
	httpjson.Write(w, http.StatusCreated, res)
}
