// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"latere.ai/x/pkg/authz"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"
)

// Action is what a request wants to do with a repository, in the shared
// contract's vocabulary (Origo spec 028).
type Action string

// The four actions Origo names in an envelope.
const (
	ActionRead  Action = "repo.read"
	ActionWrite Action = "repo.write"
	ActionAdmin Action = "repo.admin"
	// ActionList is spec 026's fourth action: which repositories may this
	// subject see. It names no repository, so its resource carries the
	// kind and no id.
	ActionList Action = "repo.list"
)

// ResourceKind is the kind every repository envelope names.
const ResourceKind = "Repository"

// RepoRef is the repository a request names, as the node resolves it
// before it builds the envelope: the id from the path, or the id a name
// resolved to with the owner and slug, or the owner and slug alone when
// the name did not resolve. It is the node's own value and never travels
// as contract 1's repo object; the guard renders it into the envelope's
// resource (Origo spec 028).
type RepoRef struct {
	ID    string
	Owner string
	Slug  string
}

// resource renders the ref as the envelope's resource of kind Repository.
func (r RepoRef) resource() authz.Resource {
	fields := map[string]any{}
	if r.Owner != "" {
		fields["owner"] = r.Owner
	}
	if r.Slug != "" {
		fields["slug"] = r.Slug
	}
	return authz.NewResource(ResourceKind, r.ID, fields)
}

// Unavailable is a call that produced no decision. It is the shared
// package's type: the key resolver of spec 024 constructs it too.
type Unavailable = authz.Unavailable

// Retryable reports whether a call to an operator's endpoint failed
// before a response line arrived, the one class worth a second attempt.
// The key resolver of spec 024 shares this one.
func Retryable(err error) bool { return authz.Retryable(err) }

// DirectoryEntry is one repository of a directory page, as the
// authorizer names it.
type DirectoryEntry struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Slug  string `json:"slug"`
}

// Directory is what a list call answered. Exactly one of the three
// answers of spec 026 is represented: a page (Supported and Allowed), a
// deny (Supported, not Allowed, with Reason), or an authorizer with no
// directory (not Supported). Anything else was an *Unavailable and no
// Directory was produced.
type Directory struct {
	Supported  bool
	Allowed    bool
	Reason     string
	Repos      []DirectoryEntry
	NextCursor string
}

// Lister is an Authorizer that also answers spec 026's directory
// question. An authorizer that does not implement it is an installation
// with no directory, which is what the guard answers.
type Lister interface {
	List(ctx context.Context, req authz.Request) (Directory, error)
}

// Decision is the authorizer's answer with the defaults applied, the
// figures decoded out of the contract-2 limits object (Origo spec 028).
type Decision struct {
	Allow      bool
	Reason     string
	TTL        time.Duration
	Replicas   int
	QuotaBytes int64
	// RequestsPerMinute carries no default: absent from the answer means
	// the node's own ORIGO_REQUESTS_PER_MINUTE, so zero here is that and
	// not a figure.
	RequestsPerMinute int
}

// Authorizer decides one request. A deny is a Decision, not an error; an
// error is an *Unavailable and never an allow. The request is the shared
// envelope; the answer is Origo's Decision with its figures decoded.
type Authorizer interface {
	Authorize(ctx context.Context, req authz.Request) (Decision, error)
}

// Values of spec 007's authorizer section, drawn from the shared
// contract so the two cannot drift.
const (
	AuthorizerTimeout = authz.Timeout
	DefaultTTL        = authz.DefaultTTL
	MaxTTL            = authz.MaxTTL
	DenyTTL           = authz.DenyTTL
	DefaultReplicas   = 1
	DefaultQuotaBytes = 53687091200
	// DefaultListLimit and MaxListLimit bound one directory page.
	DefaultListLimit = 50
	MaxListLimit     = 200
	// maxDirectoryBytes bounds a directory page.
	maxDirectoryBytes = 1 << 20
)

// figures is the contract-2 limits object: the three figures spec 007
// names, under limits rather than at the top level.
type figures struct {
	Replicas          *int   `json:"replicas"`
	QuotaBytes        *int64 `json:"quota_bytes"`
	RequestsPerMinute *int   `json:"requests_per_minute"`
}

// ClientOptions configures a Client.
type ClientOptions struct {
	// URL is ORIGO_AUTHORIZER_URL and Token ORIGO_AUTHORIZER_TOKEN.
	URL   string
	Token string
	// HTTP sends the calls. Required; the node passes its instrumented
	// client.
	HTTP *http.Client
	// Timeout bounds one call, the retry included. AuthorizerTimeout when
	// zero.
	Timeout time.Duration
	// Metrics receives origo_authorizer_seconds{result}.
	Metrics *metrics.Set
	// Now is the clock the cache runs on.
	Now func() time.Time
}

// Client is the authorizer client. It is the shared package's client
// (its cache, its retry, its failure rules) with Origo's figures decoded
// out of the answer's limits object.
type Client struct {
	inner   *authz.Client
	seconds *pkgmetrics.Histogram
}

// NewClient builds the client. It sends nothing.
func NewClient(o ClientOptions) (*Client, error) {
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	seconds := set.AuthorizerSeconds
	inner, err := authz.NewClient(authz.Options{
		URL: o.URL, Token: o.Token, HTTP: o.HTTP, Timeout: o.Timeout, Now: o.Now,
		Observe: func(result string, s float64) { seconds.Observe(map[string]string{"result": result}, s) },
	})
	if err != nil {
		return nil, err
	}
	return &Client{inner: inner, seconds: seconds}, nil
}

// Authorize asks the authorizer through the shared client and decodes the
// figures out of the answer.
func (c *Client) Authorize(ctx context.Context, req authz.Request) (Decision, error) {
	d, err := c.inner.Authorize(ctx, req)
	if err != nil {
		return Decision{}, err
	}
	return c.decision(d)
}

// decision applies spec 007's defaults and reads the figures out of the
// contract-2 limits object: this is the one seam where limits becomes
// Origo's three figures (Origo spec 028).
func (c *Client) decision(d authz.Decision) (Decision, error) {
	out := Decision{Allow: d.Allow, Reason: d.Reason, TTL: d.TTL, Replicas: DefaultReplicas, QuotaBytes: DefaultQuotaBytes}
	var f figures
	if err := d.DecodeLimits(&f); err != nil {
		return Decision{}, &Unavailable{URL: c.inner.URL(), Status: http.StatusOK, Err: fmt.Errorf("limits: %w", err)}
	}
	if f.Replicas != nil && *f.Replicas > 0 {
		out.Replicas = *f.Replicas
	}
	if f.QuotaBytes != nil && *f.QuotaBytes > 0 {
		out.QuotaBytes = *f.QuotaBytes
	}
	if f.RequestsPerMinute != nil && *f.RequestsPerMinute > 0 {
		out.RequestsPerMinute = *f.RequestsPerMinute
	}
	return out, nil
}

// List asks the authorizer which repositories the subject may see (spec
// 026). The answer is never cached: it is a page rather than a verdict,
// it varies by cursor, and its shape carries no ttl. The metric records
// a page as an allow and both refusals as a deny; only an *Unavailable is
// an error.
func (c *Client) List(ctx context.Context, req authz.Request) (Directory, error) {
	start := time.Now()
	raw, err := c.inner.Ask(ctx, req)
	var dir Directory
	if err == nil {
		dir, err = c.directory(raw)
	}
	result := "allow"
	switch {
	case err != nil:
		result = "error"
	case !dir.Supported || !dir.Allowed:
		result = "deny"
	}
	c.seconds.Observe(map[string]string{"result": result}, time.Since(start).Seconds())
	if err != nil {
		return Directory{}, err
	}
	return dir, nil
}

// directory parses the three answers of spec 026 apart. They are disjoint
// by key, so one field tells them apart; a body carrying none of the
// three is no answer at all and fails closed.
func (c *Client) directory(raw []byte) (Directory, error) {
	if len(raw) > maxDirectoryBytes {
		return Directory{}, &Unavailable{URL: c.inner.URL(), Status: http.StatusOK, Err: errors.New("body: over the directory bound")}
	}
	var answer struct {
		Repos      *[]DirectoryEntry `json:"repos"`
		NextCursor string            `json:"next_cursor"`
		Allow      *bool             `json:"allow"`
		Reason     string            `json:"reason"`
		Directory  *bool             `json:"directory"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return Directory{}, &Unavailable{URL: c.inner.URL(), Status: http.StatusOK, Err: fmt.Errorf("body: %w", err)}
	}
	switch {
	case answer.Directory != nil && !*answer.Directory:
		return Directory{}, nil
	case answer.Repos != nil:
		return Directory{Supported: true, Allowed: true, Repos: *answer.Repos, NextCursor: answer.NextCursor}, nil
	case answer.Allow != nil && !*answer.Allow:
		return Directory{Supported: true, Reason: answer.Reason}, nil
	}
	return Directory{}, &Unavailable{URL: c.inner.URL(), Status: http.StatusOK, Err: errors.New("body: no repos, allow, or directory field")}
}

// Check sends the probe through the shared package and reports an
// authorizer that answered nothing or allowed it (Origo spec 028). It
// runs against the inner client, which is the shared contract's client,
// so origod check exercises the same path a decision takes.
func (c *Client) Check(ctx context.Context) error {
	return authz.Check(ctx, c.inner, string(ActionRead), ResourceKind)
}

// CacheLen reports the decision cache's size, for tests.
func (c *Client) CacheLen() int { return c.inner.CacheLen() }
