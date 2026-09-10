// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origoclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"
)

// The budgets of spec 020. The commit route runs under the read budget of 30
// seconds; merge, cherry-pick and revert run under MergeBudget, 300. The
// client waits a little longer than the node so a timeout is reported by the
// node, which knows the operation it was running, rather than by the wire.
const (
	CommitTimeout    = 60 * time.Second
	OperationTimeout = 330 * time.Second
)

// The merge strategies spec 020 accepts.
const (
	FastForwardOnly       = "fast_forward_only"
	MergeCommit           = "merge_commit"
	FastForwardIfPossible = "fast_forward_if_possible"
)

// Author is spec 020's author object. The node refuses an email with no @,
// and a front end is expected to refuse it earlier still.
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Common is the half of every operation body that the four routes share. It is
// embedded, so its fields are flat at the top level of the request.
type Common struct {
	Branch       string  `json:"branch"`
	ExpectedHead *string `json:"expected_head"`
	Author       *Author `json:"author"`
	Message      string  `json:"message,omitempty"`
	DryRun       bool    `json:"dry_run,omitempty"`
}

// Change is one file of a commit. Exactly one of Content, ContentRef and
// Delete is set. Content is the file's bytes; this package base64 encodes it,
// because encoding is the wire's problem and not a caller's.
type Change struct {
	Path       string
	Content    []byte
	ContentRef string
	Delete     bool
	Mode       string
}

// MarshalJSON writes the change the way spec 020 takes it.
//
// The encoding is hand-written for one reason: `omitempty` on a byte slice
// drops an empty one, and an empty file is a real change whose content is
// zero bytes. A struct tag would have turned it into a change with no
// content, no content_ref and no delete, which the node refuses as
// invalid_change. Presence, not length, is what decides the key.
func (c Change) MarshalJSON() ([]byte, error) {
	out := map[string]any{"path": c.Path}
	switch {
	case c.Delete:
		out["delete"] = true
	case c.ContentRef != "":
		out["content_ref"] = c.ContentRef
	case c.Content != nil:
		out["content"] = base64.StdEncoding.EncodeToString(c.Content)
	}
	if c.Mode != "" {
		out["mode"] = c.Mode
	}
	return json.Marshal(out)
}

// CommitRequest is POST /v1/repos/{id}/commits.
type CommitRequest struct {
	Common
	CreateBranch bool     `json:"create_branch,omitempty"`
	From         string   `json:"from,omitempty"`
	Changes      []Change `json:"changes"`
}

// MergeRequest is POST /v1/repos/{id}/merge.
type MergeRequest struct {
	Common
	Source   string `json:"source"`
	Strategy string `json:"strategy,omitempty"`
}

// PickRequest is POST /v1/repos/{id}/cherry-pick and POST .../revert.
type PickRequest struct {
	Common
	Commits  []string `json:"commits"`
	Mainline int      `json:"mainline,omitempty"`
}

// Receipt is the answer of all four operations.
//
// Committed is a three-state field on the wire and is kept as one here. A real
// write answers 201 with Committed absent, never true; a dry run answers 200
// with Committed false and EntrySeq null. Reading it as a plain boolean would
// call a real commit uncommitted, so it is a pointer and a front end reads
// present-and-false as the dry run it is.
type Receipt struct {
	Commit    string  `json:"commit"`
	Branch    string  `json:"branch"`
	EntrySeq  *uint64 `json:"entry_seq"`
	Tree      string  `json:"tree"`
	Committed *bool   `json:"committed,omitempty"`
	// Status is the HTTP status the node answered: 201 for a write that
	// landed, 200 for a dry run.
	Status int `json:"-"`
}

// Landed reports whether the write was applied, which is what the node's own
// two answers mean taken together.
func (r Receipt) Landed() bool { return r.Committed == nil || *r.Committed }

// CreateCommit appends one commit built from a set of changes.
func (c *Client) CreateCommit(ctx context.Context, id string, req CommitRequest) (Receipt, error) {
	return c.operate(ctx, repoPath(id, "/commits"), req, CommitTimeout)
}

// Merge merges a source into a branch.
func (c *Client) Merge(ctx context.Context, id string, req MergeRequest) (Receipt, error) {
	return c.operate(ctx, repoPath(id, "/merge"), req, OperationTimeout)
}

// CherryPick replays commits onto a branch.
func (c *Client) CherryPick(ctx context.Context, id string, req PickRequest) (Receipt, error) {
	return c.operate(ctx, repoPath(id, "/cherry-pick"), req, OperationTimeout)
}

// Revert replays commits backwards onto a branch.
func (c *Client) Revert(ctx context.Context, id string, req PickRequest) (Receipt, error) {
	return c.operate(ctx, repoPath(id, "/revert"), req, OperationTimeout)
}

// operate is the one shape all four routes share.
func (c *Client) operate(ctx context.Context, path string, body any, budget time.Duration) (Receipt, error) {
	var out Receipt
	r, err := c.post(ctx, path, body, budget)
	if err != nil {
		return out, err
	}
	if err := decode(r, &out); err != nil {
		return out, err
	}
	out.Status = r.status
	return out, nil
}
