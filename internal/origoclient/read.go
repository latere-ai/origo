// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origoclient

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/latere-ai/origo/internal/contract"
)

// The bounds of the read API this package must know to page and to window
// (spec 009). They are the node's, not this package's, and nothing here
// enforces a limit the node does not.
const (
	// MaxRefs is where the refs list stops. It is a wall, not a page: there
	// is no cursor on that route, so past it the rest is unreachable.
	MaxRefs = 10000
	// MaxCommitLimit is the largest page of commits.
	MaxCommitLimit = 200
	// TreePageSize is the number of entries of one tree page.
	TreePageSize = 5000
	// MaxBlobBytes is the largest blob, or Range of one, served at once. The
	// node measures the requested length against it, not the blob's size, so
	// a Range of at most this reads any blob.
	MaxBlobBytes = 50 << 20
)

// Repository is spec 003's representation, the body of GET /v1/repos/{id} and
// of the name mode of GET /v1/repos. Every key is always present; the four
// pointers are null until their operation ran.
type Repository struct {
	ID            string     `json:"id"`
	Owner         string     `json:"owner"`
	Slug          string     `json:"slug"`
	DefaultBranch string     `json:"default_branch"`
	SizeBytes     int64      `json:"size_bytes"`
	Head          string     `json:"head"`
	UpdatedAt     time.Time  `json:"updated_at"`
	PushedAt      *time.Time `json:"pushed_at"`
	FrozenAt      *time.Time `json:"frozen_at"`
	VerifiedAt    *time.Time `json:"verified_at"`
	VerifiedEqual *bool      `json:"verified_equal"`
}

// Ref is one entry of the refs array.
type Ref struct {
	Name   string  `json:"name"`
	SHA    string  `json:"sha"`
	Peeled *string `json:"peeled"`
}

// Identity is an author or a committer.
type Identity struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	At    time.Time `json:"at"`
}

// Trailer is one parsed trailer of a commit message.
type Trailer struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Stats is the totals of a commit. Only GET /v1/repos/{id}/commits/{sha}
// carries it; the list route omits the key.
type Stats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Commit is one commit as both commit routes render it.
type Commit struct {
	SHA       string    `json:"sha"`
	Parents   []string  `json:"parents"`
	Author    Identity  `json:"author"`
	Committer Identity  `json:"committer"`
	Message   string    `json:"message"`
	Trailers  []Trailer `json:"trailers"`
	Stats     *Stats    `json:"stats,omitempty"`
}

// TreeEntry is one entry of a tree listing. Size is null for a tree.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size"`
}

// DirectoryPage is one page of GET /v1/repos in its directory mode. A page can
// be shorter than the limit, or empty, with NextCursor still set: the node
// drops ids the authorizer named that the log no longer holds.
type DirectoryPage struct {
	Repos      []Repository `json:"repos"`
	NextCursor *string      `json:"next_cursor"`
	Meta       Meta         `json:"-"`
}

// RefList is the whole refs answer. Truncated means the 10 000 wall was hit
// and the rest cannot be asked for.
type RefList struct {
	Refs []Ref
	Meta Meta
}

// CommitPage is one page of history. NextCursor is the sha of the last commit
// of this page and the next request restarts the walk and discards up to and
// including it, so a caller pages by handing back what it was given.
type CommitPage struct {
	Commits    []Commit `json:"commits"`
	NextCursor *string  `json:"next_cursor"`
	Meta       Meta     `json:"-"`
}

// TreePage is one page of a tree listing, cursored by the last path served.
type TreePage struct {
	Entries    []TreeEntry `json:"entries"`
	NextCursor *string     `json:"next_cursor"`
	Meta       Meta        `json:"-"`
}

// Comparison is the raw unified diff of GET /v1/repos/{id}/compare. Truncated
// means the node cut it at a file boundary at 1 MiB, which can leave an empty
// Diff when one file's patch alone is past the cap.
type Comparison struct {
	Diff []byte
	Meta Meta
}

// Blob is the bytes of a blob, or of the Range that was asked for.
type Blob struct {
	Bytes []byte
	Meta  Meta
}

// ByteRange is a half-open window in the sense HTTP means it: both ends are
// inclusive byte offsets.
type ByteRange struct{ First, Last int64 }

// DirectoryOptions pages GET /v1/repos. Cursor is the node's own, opaque.
type DirectoryOptions struct {
	Cursor string
	Limit  int
}

// CommitOptions is the query of the commits route.
type CommitOptions struct {
	Ref    string
	Path   string
	Since  string
	Until  string
	Cursor string
	Limit  int
}

// TreeOptions is the query of the tree route.
type TreeOptions struct {
	Ref       string
	Path      string
	Cursor    string
	Recursive bool
}

func repoPath(id, rest string) string { return "/v1/repos/" + url.PathEscape(id) + rest }

// ref adds the name of a reference the way the read API takes it beside a
// placeholder segment: never in the segment, always in the query.
func withRef(q url.Values, param, name string) url.Values {
	if q == nil {
		q = url.Values{}
	}
	q.Set(param, name)
	return q
}

// Directory reads one page of the repositories the subject may see. An
// installation whose authorizer has no directory answers a Refusal with code
// directory_unsupported, which is a fact about the installation and not an
// error in the request.
func (c *Client) Directory(ctx context.Context, opt DirectoryOptions) (DirectoryPage, error) {
	q := url.Values{}
	if opt.Cursor != "" {
		q.Set("cursor", opt.Cursor)
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	var page DirectoryPage
	r, err := c.get(ctx, "/v1/repos", q)
	if err != nil {
		return page, err
	}
	if err := decode(r, &page); err != nil {
		return page, err
	}
	page.Meta = r.meta()
	return page, nil
}

// Resolve reads the name mode of GET /v1/repos. A deny answers forbidden
// whether or not the name exists, so a refused caller learns nothing.
func (c *Client) Resolve(ctx context.Context, owner, slug string) (Repository, error) {
	q := url.Values{"owner": {owner}, "slug": {slug}}
	var repo Repository
	r, err := c.get(ctx, "/v1/repos", q)
	if err != nil {
		return repo, err
	}
	err = decode(r, &repo)
	return repo, err
}

// Repository reads one repository by id.
func (c *Client) Repository(ctx context.Context, id string) (Repository, error) {
	var repo Repository
	r, err := c.get(ctx, repoPath(id, ""), nil)
	if err != nil {
		return repo, err
	}
	err = decode(r, &repo)
	return repo, err
}

// Refs reads the reference list under a prefix. The route answers a bare JSON
// array with no envelope and no cursor.
func (c *Client) Refs(ctx context.Context, id, prefix string) (RefList, error) {
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var out RefList
	r, err := c.get(ctx, repoPath(id, "/refs"), q)
	if err != nil {
		return out, err
	}
	if err := decode(r, &out.Refs); err != nil {
		return out, err
	}
	out.Meta = r.meta()
	return out, nil
}

// Commits reads one page of history. The route takes its ref in the query
// already, so no placeholder is needed here.
func (c *Client) Commits(ctx context.Context, id string, opt CommitOptions) (CommitPage, error) {
	q := url.Values{}
	if opt.Ref != "" {
		q.Set("ref", opt.Ref)
	}
	for k, v := range map[string]string{"path": opt.Path, "since": opt.Since, "until": opt.Until, "cursor": opt.Cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	var page CommitPage
	r, err := c.get(ctx, repoPath(id, "/commits"), q)
	if err != nil {
		return page, err
	}
	if err := decode(r, &page); err != nil {
		return page, err
	}
	page.Meta = r.meta()
	return page, nil
}

// Commit reads one commit, with its totals. rev is any name or object id and
// always travels in the query beside the placeholder segment.
func (c *Client) Commit(ctx context.Context, id, rev string) (Commit, error) {
	var out Commit
	r, err := c.get(ctx, repoPath(id, "/commits/"+Placeholder), withRef(nil, "ref", rev))
	if err != nil {
		return out, err
	}
	err = decode(r, &out)
	return out, err
}

// Expand turns a short object id into the whole one, so a write names the
// commit it means. A short id that no longer resolves is ref_not_found and
// nothing is written.
func (c *Client) Expand(ctx context.Context, id, rev string) (string, error) {
	commit, err := c.Commit(ctx, id, rev)
	if err != nil {
		return "", err
	}
	return commit.SHA, nil
}

// Compare reads the unified diff between two revisions. Both sides travel in
// the query beside the placeholder range, so a branch named feature/x works.
func (c *Client) Compare(ctx context.Context, id, base, head, p string) (Comparison, error) {
	q := url.Values{"base": {base}, "head": {head}}
	if p != "" {
		q.Set("path", p)
	}
	var out Comparison
	r, err := c.get(ctx, repoPath(id, "/compare/"+Placeholder+"..."+Placeholder), q)
	if err != nil {
		return out, err
	}
	out.Diff, out.Meta = r.body, r.meta()
	return out, nil
}

// Tree reads one page of a tree listing. The node appends a trailing slash to
// path, so this lists the entries of a directory and never a file.
func (c *Client) Tree(ctx context.Context, id string, opt TreeOptions) (TreePage, error) {
	q := withRef(nil, "ref", opt.Ref)
	if opt.Path != "" {
		q.Set("path", opt.Path)
	}
	if opt.Cursor != "" {
		q.Set("cursor", opt.Cursor)
	}
	if opt.Recursive {
		q.Set("recursive", "1")
	}
	var page TreePage
	r, err := c.get(ctx, repoPath(id, "/tree/"+Placeholder), q)
	if err != nil {
		return page, err
	}
	if err := decode(r, &page); err != nil {
		return page, err
	}
	page.Meta = r.meta()
	return page, nil
}

// File resolves one path at one revision to its tree entry, which carries the
// object id a blob read takes and the size that decides whether it needs a
// Range.
//
// It is one listing of the path's parent directory with a match on the
// basename, and no descent: ?path= lists a directory, so a file path matches
// nothing, ever. A directory past one page is paged until the basename is
// seen. A path with no such entry is ref_not_found, and no blob is read.
func (c *Client) File(ctx context.Context, id, ref, p string) (TreeEntry, error) {
	dir, base := path.Split(strings.TrimPrefix(p, "./"))
	dir = strings.TrimSuffix(dir, "/")
	if base == "" {
		return TreeEntry{}, notFound(p)
	}
	opt := TreeOptions{Ref: ref, Path: dir}
	for {
		page, err := c.Tree(ctx, id, opt)
		if err != nil {
			return TreeEntry{}, err
		}
		for _, e := range page.Entries {
			if path.Base(e.Path) == base && e.Path == joinPath(dir, base) {
				return e, nil
			}
		}
		if page.NextCursor == nil {
			return TreeEntry{}, notFound(p)
		}
		opt.Cursor = *page.NextCursor
	}
}

func joinPath(dir, base string) string {
	if dir == "" {
		return base
	}
	return dir + "/" + base
}

func notFound(p string) *Refusal {
	return &Refusal{
		Status:  http.StatusNotFound,
		Code:    contract.CodeRefNotFound,
		Message: contract.Sentence(contract.CodeRefNotFound),
		Details: map[string]any{"ref": p},
	}
}

// Blob reads a blob by its object id, whole or over one byte range. The node
// measures the requested length against its 50 MiB bound rather than the
// blob's size, so a caller that knows the size never provokes blob_too_large.
func (c *Client) Blob(ctx context.Context, id, sha string, window *ByteRange) (Blob, error) {
	p := repoPath(id, "/blob/"+Placeholder)
	q := withRef(nil, "ref", sha)
	var (
		r   *reply
		err error
	)
	if window == nil {
		r, err = c.get(ctx, p, q)
	} else {
		r, err = c.getRange(ctx, p, q, window.First, window.Last)
	}
	if err != nil {
		return Blob{}, err
	}
	return Blob{Bytes: r.body, Meta: r.meta()}, nil
}
