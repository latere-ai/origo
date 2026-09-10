// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package origo is a client of Origo's read API.
//
// It holds no git knowledge: it sends the reader's own token to an Origo
// installation and returns what came back. It never runs git, never reads
// object storage, never resolves a reference, and never decides who may see
// what. Every call is a GET of a path Origo's specs 003 and 009 publish, and
// the fields below are the fields those documents name.
//
// A call carries an Authorization header when the caller has a token and no
// Authorization header at all when it does not, so the same code path serves
// a signed-in reader and, on an installation that grows an anonymous read,
// a signed-out one.
package origo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/otel"
)

// Client talks to one Origo installation.
type Client struct {
	base *url.URL
	hc   *http.Client
}

// New returns a client for the installation at base.
func New(base *url.URL, hc *http.Client) *Client {
	if hc == nil {
		// The traced client of latere.ai/x/pkg/otel, so a call to the
		// installation shows up as one span of the request that made it.
		hc = otel.HTTPClient()
	}
	return &Client{base: base, hc: hc}
}

// Meta is what a read response says about itself beside its body.
type Meta struct {
	// Commit is Origo-Commit, the object id the request resolved to.
	Commit string
	// Truncated is Origo-Truncated: the server cut the body.
	Truncated bool
	// Stale is Origo-Stale: the age of the last currency check. A reader
	// looking at a commit log must be told; StaleSet says whether the
	// header was there at all, because zero seconds is a value.
	Stale    time.Duration
	StaleSet bool
}

// Repo is the repository representation of spec 003.
type Repo struct {
	ID            string     `json:"id"`
	Owner         string     `json:"owner"`
	Slug          string     `json:"slug"`
	DefaultBranch string     `json:"default_branch"`
	Head          string     `json:"head"`
	SizeBytes     int64      `json:"size_bytes"`
	UpdatedAt     *time.Time `json:"updated_at"`
	PushedAt      *time.Time `json:"pushed_at"`
	FrozenAt      *time.Time `json:"frozen_at"`
}

// Ref is one branch or tag. Peeled is a tag's target, empty otherwise.
type Ref struct {
	Name   string `json:"name"`
	SHA    string `json:"sha"`
	Peeled string `json:"peeled"`
}

// Short is the reference name without its refs/heads/ or refs/tags/ prefix.
func (r Ref) Short() string {
	for _, p := range []string{"refs/heads/", "refs/tags/", "refs/"} {
		if s, ok := strings.CutPrefix(r.Name, p); ok {
			return s
		}
	}
	return r.Name
}

// Target is the object a reference points at: a tag's peeled target when it
// has one, its own object otherwise.
func (r Ref) Target() string {
	if r.Peeled != "" {
		return r.Peeled
	}
	return r.SHA
}

// Person is an author or a committer.
type Person struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	At    time.Time `json:"at"`
}

// Trailer is one message trailer, as git's own parser read it.
type Trailer struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Stats is a commit's file and line counts.
type Stats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Commit is one commit.
type Commit struct {
	SHA       string    `json:"sha"`
	Parents   []string  `json:"parents"`
	Author    Person    `json:"author"`
	Committer Person    `json:"committer"`
	Message   string    `json:"message"`
	Trailers  []Trailer `json:"trailers"`
	Stats     *Stats    `json:"stats"`
}

// Subject is the first line of the message.
func (c Commit) Subject() string {
	s, _, _ := strings.Cut(c.Message, "\n")
	return strings.TrimSpace(s)
}

// Body is the message after its subject line, trimmed.
func (c Commit) Body() string {
	_, rest, ok := strings.Cut(c.Message, "\n")
	if !ok {
		return ""
	}
	return strings.Trim(rest, "\n")
}

// Short is the abbreviated object id a person reads.
func Short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Entry is one tree entry.
type Entry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// Name is the entry's last path segment.
func (e Entry) Name() string {
	if i := strings.LastIndex(e.Path, "/"); i >= 0 {
		return e.Path[i+1:]
	}
	return e.Path
}

// IsDir reports whether the entry is a directory.
func (e Entry) IsDir() bool { return e.Type == "tree" }

// Page is one page of a paged read.
type Page[T any] struct {
	Items []T
	Next  string
	Meta  Meta
}

// Repo reads the repository representation.
func (c *Client) Repo(ctx context.Context, tok, id string) (Repo, Meta, error) {
	var out Repo
	meta, err := c.getJSON(ctx, tok, "/v1/repos/"+url.PathEscape(id), nil, &out)
	return out, meta, err
}

// Refs lists the references under a prefix. The prefix is a string prefix of
// the full name, as spec 009 defines it.
func (c *Client) Refs(ctx context.Context, tok, id, prefix string) ([]Ref, Meta, error) {
	var out struct {
		Refs []Ref `json:"refs"`
	}
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	meta, err := c.getJSON(ctx, tok, "/v1/repos/"+url.PathEscape(id)+"/refs", q, &out.Refs)
	return out.Refs, meta, err
}

// CommitsOptions selects a page of the commit log.
type CommitsOptions struct {
	Ref    string
	Path   string
	Limit  int
	Cursor string
}

// Commits reads one page of the commit log, newest first.
func (c *Client) Commits(ctx context.Context, tok, id string, o CommitsOptions) (Page[Commit], error) {
	var out struct {
		Commits    []Commit `json:"commits"`
		NextCursor string   `json:"next_cursor"`
	}
	q := url.Values{}
	setIf(q, "ref", o.Ref)
	setIf(q, "path", o.Path)
	setIf(q, "cursor", o.Cursor)
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	meta, err := c.getJSON(ctx, tok, "/v1/repos/"+url.PathEscape(id)+"/commits", q, &out)
	return Page[Commit]{Items: out.Commits, Next: out.NextCursor, Meta: meta}, err
}

// Commit reads one commit with its stats.
func (c *Client) Commit(ctx context.Context, tok, id, sha string) (Commit, Meta, error) {
	var out Commit
	meta, err := c.getJSON(ctx, tok, "/v1/repos/"+url.PathEscape(id)+"/commits/"+url.PathEscape(sha), nil, &out)
	return out, meta, err
}

// Compare reads the unified diff between two revisions, optionally limited to
// one path. The body is the diff text Origo produced; this package does not
// read it.
func (c *Client) Compare(ctx context.Context, tok, id, base, head, path string) ([]byte, Meta, error) {
	q := url.Values{}
	setIf(q, "path", path)
	// The base...head form is one path segment with two literal dots
	// triplets in it, so it is escaped as a whole and not per side.
	p := "/v1/repos/" + url.PathEscape(id) + "/compare/" + url.PathEscape(base+"..."+head)
	resp, meta, err := c.get(ctx, tok, p, q)
	if err != nil {
		return nil, meta, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiffBytes))
	if err != nil {
		return nil, meta, fmt.Errorf("read compare: %w", err)
	}
	return body, meta, nil
}

// maxDiffBytes bounds what this service will hold in memory from one compare.
// Origo cuts a diff at 1 MiB (spec 009); the extra byte is the guard that
// makes reading past the cap impossible rather than unlikely.
const maxDiffBytes = 1<<20 + 1

// TreeOptions selects a page of a tree.
type TreeOptions struct {
	Path   string
	Cursor string
}

// Tree reads one page of a tree at a revision. It never asks for a recursive
// listing: one screen is one call whatever the repository holds.
func (c *Client) Tree(ctx context.Context, tok, id, rev string, o TreeOptions) (Page[Entry], error) {
	var out struct {
		Entries    []Entry `json:"entries"`
		NextCursor string  `json:"next_cursor"`
	}
	q := url.Values{}
	setIf(q, "path", o.Path)
	setIf(q, "cursor", o.Cursor)
	meta, err := c.getJSON(ctx, tok, "/v1/repos/"+url.PathEscape(id)+"/tree/"+url.PathEscape(rev), q, &out)
	return Page[Entry]{Items: out.Entries, Next: out.NextCursor, Meta: meta}, err
}

// Blob is the body of one blob, with the type Origo detected.
type Blob struct {
	Body        []byte
	ContentType string
	// Partial reports that the request carried a Range and the server
	// answered with part of the file.
	Partial bool
	Meta    Meta
}

// BlobRange reads at most limit bytes of a blob. The cap is enforced by the
// request, with a Range header, so the bytes past it are never sent. A limit
// of zero asks for the whole blob.
func (c *Client) BlobRange(ctx context.Context, tok, id, sha string, limit int64) (Blob, error) {
	var hdr http.Header
	if limit > 0 {
		hdr = http.Header{"Range": []string{fmt.Sprintf("bytes=0-%d", limit-1)}}
	}
	resp, meta, err := c.do(ctx, tok, "/v1/repos/"+url.PathEscape(id)+"/blob/"+url.PathEscape(sha), nil, hdr)
	if err != nil {
		return Blob{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	read := limit
	if read <= 0 {
		read = maxBlobBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, read))
	if err != nil {
		return Blob{}, fmt.Errorf("read blob: %w", err)
	}
	return Blob{
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Partial:     resp.StatusCode == http.StatusPartialContent,
		Meta:        meta,
	}, nil
}

// maxBlobBytes bounds an unbounded blob read. The file screen never asks for
// one; the guard exists so a future caller cannot.
const maxBlobBytes = 1 << 20

// BlobStream opens a blob for copying. The caller closes the response body.
// The raw screen streams it through, so a large file never sits in memory.
func (c *Client) BlobStream(ctx context.Context, tok, id, sha string) (*http.Response, error) {
	resp, _, err := c.do(ctx, tok, "/v1/repos/"+url.PathEscape(id)+"/blob/"+url.PathEscape(sha), nil, nil)
	return resp, err
}

// ErrNoDirectory is what List returns from an installation that cannot
// enumerate repositories: the collection read of /v1/repos is absent, or it
// answered 501 because the authorizer has no directory. It is not a failure,
// and the home screen degrades to the name form on it.
var ErrNoDirectory = fmt.Errorf("origo: this installation lists no repositories")

// List reads one page of the repositories the subject may see.
//
// The collection route does not exist in Origo today (spec 023 states the
// shape it needs). An installation without it answers a 400, 404, or 501,
// each of which is ErrNoDirectory here, and the caller degrades.
func (c *Client) List(ctx context.Context, tok, cursor string, limit int) (Page[Repo], error) {
	var out struct {
		Repos      []Repo `json:"repos"`
		NextCursor string `json:"next_cursor"`
	}
	q := url.Values{}
	setIf(q, "cursor", cursor)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	meta, err := c.getJSON(ctx, tok, "/v1/repos", q, &out)
	var apiErr *Error
	if As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusNotImplemented, http.StatusNotFound, http.StatusBadRequest, http.StatusMethodNotAllowed:
			return Page[Repo]{}, ErrNoDirectory
		}
	}
	return Page[Repo]{Items: out.Repos, Next: out.NextCursor, Meta: meta}, err
}

func (c *Client) getJSON(ctx context.Context, tok, path string, q url.Values, out any) (Meta, error) {
	resp, meta, err := c.get(ctx, tok, path, q)
	if err != nil {
		return meta, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(out); err != nil {
		return meta, fmt.Errorf("decode %s: %w", path, err)
	}
	return meta, nil
}

// maxJSONBytes bounds a JSON body. The largest page spec 009 serves is 5 000
// tree entries or 10 000 references; 16 MiB holds either with room to spare
// and refuses a body no Origo would send.
const maxJSONBytes = 16 << 20

func (c *Client) get(ctx context.Context, tok, path string, q url.Values) (*http.Response, Meta, error) {
	return c.do(ctx, tok, path, q, nil)
}

func (c *Client) do(ctx context.Context, tok, path string, q url.Values, hdr http.Header) (*http.Response, Meta, error) {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("build request: %w", err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// The one branch on "is there a token", and it is on the header alone:
	// nothing below reads it, so an installation that answers a tokenless
	// read renders through the same path as a signed-in one.
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, Meta{}, &Error{Status: 0, Code: "unreachable", cause: err}
	}
	meta := readMeta(resp.Header)
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		return nil, meta, readError(resp)
	}
	return resp, meta, nil
}

func readMeta(h http.Header) Meta {
	m := Meta{
		Commit:    h.Get("Origo-Commit"),
		Truncated: strings.EqualFold(h.Get("Origo-Truncated"), "true"),
	}
	if v := h.Get("Origo-Stale"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			m.Stale, m.StaleSet = time.Duration(secs)*time.Second, true
		}
	}
	return m
}

func setIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}
