// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"latere.ai/x/pkg/cache"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"
)

// Action is what a request wants to do with a repository.
type Action string

// The three actions of spec 007.
const (
	ActionRead  Action = "read"
	ActionWrite Action = "write"
	ActionAdmin Action = "admin"
	// ActionList is spec 026's fourth action: which repositories may
	// this subject see. It names no repository, so it travels in a body
	// of its own and never through Request.
	ActionList Action = "list"
)

// RepoRef is the repository a request names, as the authorizer receives
// it: the id from the path, or the id a name resolved to with the owner
// and slug, or the owner and slug alone when the name did not resolve.
type RepoRef struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Slug  string `json:"slug"`
}

// Request is the body of one authorizer call.
type Request struct {
	Subject string  `json:"subject"`
	Actor   string  `json:"actor"`
	Repo    RepoRef `json:"repo"`
	Action  Action  `json:"action"`
}

// ListRequest is the body of one directory call (spec 026). It carries
// no repo object, which is what tells an endpoint that no repository is
// named and what distinguishes the question from the three actions that
// name one.
type ListRequest struct {
	Subject string `json:"subject"`
	Actor   string `json:"actor"`
	Action  Action `json:"action"`
	Cursor  string `json:"cursor,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// DirectoryEntry is one repository of a directory page, as the
// authorizer names it.
type DirectoryEntry struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Slug  string `json:"slug"`
}

// Directory is what a list call answered. Exactly one of the three
// answers of spec 026 is represented: a page (Supported and Allowed),
// a deny (Supported, not Allowed, with Reason), or an authorizer with
// no directory (not Supported). Anything else was an *Unavailable and
// no Directory was produced.
type Directory struct {
	// Supported is false when the authorizer answered
	// {"directory": false}: it has not learned the question, which is
	// not an outage and not a deny.
	Supported bool
	// Allowed is false when the authorizer answered {"allow": false}.
	Allowed bool
	Reason  string
	// Repos is the page, in the authorizer's order. An empty page is a
	// valid answer.
	Repos []DirectoryEntry
	// NextCursor is the authorizer's, passed through unread; empty on
	// the last page.
	NextCursor string
}

// Lister is an Authorizer that also answers spec 026's directory
// question. An authorizer client that does not implement it is an
// installation with no directory, which is what the guard answers.
type Lister interface {
	List(ctx context.Context, req ListRequest) (Directory, error)
}

// Decision is the authorizer's answer with the defaults applied.
type Decision struct {
	Allow      bool
	Reason     string
	TTL        time.Duration
	Replicas   int
	QuotaBytes int64
	// RequestsPerMinute is the rate this subject alone is bucketed at
	// (spec 012's limit, read by spec 020's operations). It carries no
	// default: absent from the answer means the node's own
	// ORIGO_REQUESTS_PER_MINUTE, so zero here is that and not a figure.
	RequestsPerMinute int
}

// Authorizer decides one request. A deny is a Decision, not an error;
// an error is an *Unavailable and never an allow.
type Authorizer interface {
	Authorize(ctx context.Context, req Request) (Decision, error)
}

// Unavailable is a call that produced no decision: the authorizer timed
// out, refused the connection after the retry, answered another status
// than 200, or sent a body that does not parse. Fail closed.
type Unavailable struct {
	URL    string
	Status int
	Err    error
}

func (u *Unavailable) Error() string {
	if u.Err != nil {
		return "auth: authorizer unavailable: " + u.Err.Error()
	}
	return fmt.Sprintf("auth: authorizer answered %d", u.Status)
}

func (u *Unavailable) Unwrap() error { return u.Err }

// Values of spec 007's authorizer section.
const (
	AuthorizerTimeout = 5 * time.Second
	DefaultTTL        = 60 * time.Second
	MaxTTL            = 600 * time.Second
	DenyTTL           = 5 * time.Second
	DefaultReplicas   = 1
	DefaultQuotaBytes = 53687091200
	maxDecisionBytes  = 64 << 10
	// maxDirectoryBytes bounds a directory page: 200 entries of an id,
	// an owner, and a slug fit in well under this.
	maxDirectoryBytes = 1 << 20
	// DefaultListLimit and MaxListLimit bound one directory page, the
	// figures spec 009 uses for a page of commits.
	DefaultListLimit   = 50
	MaxListLimit       = 200
	authorizerAttempts = 2
)

// ClientOptions configures a Client.
type ClientOptions struct {
	// URL is ORIGO_AUTHORIZER_URL and Token ORIGO_AUTHORIZER_TOKEN.
	URL   string
	Token string
	// HTTP sends the calls. Required; the node passes its instrumented
	// client.
	HTTP *http.Client
	// Timeout bounds one call, the retry included. AuthorizerTimeout
	// when zero.
	Timeout time.Duration
	// Metrics receives origo_authorizer_seconds{result}.
	Metrics *metrics.Set
	// Now is the clock the cache runs on.
	Now func() time.Time
}

// Client is the authorizer client: one call per decision, one retry
// when the connection failed before a response line arrived, and a
// cache per (subject, actor, repo id, action).
type Client struct {
	url     string
	token   string
	http    *http.Client
	timeout time.Duration
	now     func() time.Time
	seconds *pkgmetrics.Histogram
	cache   *cache.TTLCache[cacheKey, cachedDecision]
}

type cacheKey struct {
	subject, actor, repo string
	action               Action
}

type cachedDecision struct {
	decision Decision
	until    time.Time
}

// NewClient builds the client. It sends nothing.
func NewClient(o ClientOptions) (*Client, error) {
	if o.HTTP == nil {
		return nil, errors.New("auth: the authorizer client needs an HTTP client")
	}
	if o.URL == "" {
		return nil, errors.New("auth: the authorizer client needs a URL")
	}
	c := &Client{url: o.URL, token: o.Token, http: o.HTTP, timeout: o.Timeout, now: o.Now}
	if c.timeout == 0 {
		c.timeout = AuthorizerTimeout
	}
	if c.now == nil {
		c.now = time.Now
	}
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	c.seconds = set.AuthorizerSeconds
	c.cache = cache.New[cacheKey, cachedDecision](MaxTTL, cache.WithMaxSize[cacheKey, cachedDecision](CacheEntries), cache.WithClock[cacheKey, cachedDecision](c.now))
	return c, nil
}

// Authorize answers from the cache or asks the authorizer. An answer for
// an unresolved name (empty id) is never cached.
func (c *Client) Authorize(ctx context.Context, req Request) (Decision, error) {
	key := cacheKey{req.Subject, req.Actor, req.Repo.ID, req.Action}
	now := c.now()
	if req.Repo.ID != "" {
		if e, ok := c.cache.Get(key); ok && now.Before(e.until) {
			return e.decision, nil
		}
	}
	start := time.Now()
	d, err := c.call(ctx, req)
	result := "allow"
	switch {
	case err != nil:
		result = "error"
	case !d.Allow:
		result = "deny"
	}
	c.seconds.Observe(map[string]string{"result": result}, time.Since(start).Seconds())
	if err != nil {
		return Decision{}, err
	}
	if req.Repo.ID != "" {
		ttl := DenyTTL
		if d.Allow {
			ttl = d.TTL
		}
		c.cache.Set(key, cachedDecision{decision: d, until: now.Add(ttl)})
	}
	return d, nil
}

// List asks the authorizer which repositories the subject may see
// (spec 026). The answer is never cached: it is a page rather than a
// verdict, it varies by cursor, and its shape carries no ttl.
//
// The metric records a page as an allow and both refusals, a deny and an
// authorizer with no directory, as a deny, so the label set of spec 011
// is unchanged: neither refusal is an error, and only an *Unavailable is.
func (c *Client) List(ctx context.Context, req ListRequest) (Directory, error) {
	req.Action = ActionList
	if req.Limit <= 0 {
		req.Limit = DefaultListLimit
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Directory{}, &Unavailable{URL: c.url, Err: err}
	}
	start := time.Now()
	raw, status, err := c.post(ctx, body, maxDirectoryBytes)
	var dir Directory
	if err == nil {
		dir, err = c.directory(raw, status)
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

// directory parses the three answers of spec 026 apart. They are
// disjoint by key, so one field tells them apart; a body carrying none
// of the three is no answer at all and fails closed.
func (c *Client) directory(raw []byte, status int) (Directory, error) {
	var answer struct {
		Repos      *[]DirectoryEntry `json:"repos"`
		NextCursor string            `json:"next_cursor"`
		Allow      *bool             `json:"allow"`
		Reason     string            `json:"reason"`
		Directory  *bool             `json:"directory"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return Directory{}, &Unavailable{URL: c.url, Status: status, Err: fmt.Errorf("body: %w", err)}
	}
	switch {
	case answer.Directory != nil && !*answer.Directory:
		return Directory{}, nil
	case answer.Repos != nil:
		return Directory{Supported: true, Allowed: true, Repos: *answer.Repos, NextCursor: answer.NextCursor}, nil
	case answer.Allow != nil && !*answer.Allow:
		return Directory{Supported: true, Reason: answer.Reason}, nil
	}
	return Directory{}, &Unavailable{URL: c.url, Status: status, Err: errors.New("body: no repos, allow, or directory field")}
}

// CacheLen reports the cache's size, for tests.
func (c *Client) CacheLen() int { return c.cache.Len() }

// call makes the request under the timeout and parses the decision.
func (c *Client) call(ctx context.Context, req Request) (Decision, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, &Unavailable{URL: c.url, Err: err}
	}
	raw, status, err := c.post(ctx, body, maxDecisionBytes)
	if err != nil {
		return Decision{}, err
	}
	return c.decision(raw, status)
}

// post sends one body under the timeout, retrying once when the
// connection failed before a response line arrived, and returns the
// bytes of a 200. Every other outcome is an *Unavailable: there is no
// fail-open on either shape this client parses.
func (c *Client) post(ctx context.Context, body []byte, limit int64) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var last error
	for range authorizerAttempts {
		raw, status, err := c.once(ctx, body, limit)
		if err == nil {
			return raw, status, nil
		}
		last = err
		if !retryable(err) {
			break
		}
	}
	return nil, 0, last
}

func (c *Client) once(ctx context.Context, body []byte, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, &Unavailable{URL: c.url, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &Unavailable{URL: c.url, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, &Unavailable{URL: c.url, Status: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, &Unavailable{URL: c.url, Status: resp.StatusCode, Err: err}
	}
	return raw, resp.StatusCode, nil
}

// decision parses the answer to a read, write, or admin call. The allow
// field stays mandatory here: spec 026 added a second answer shape and
// gave it a parser of its own precisely so this one keeps failing closed
// on a body that does not carry a verdict.
func (c *Client) decision(raw []byte, status int) (Decision, error) {
	var answer struct {
		Allow             *bool  `json:"allow"`
		Reason            string `json:"reason"`
		TTL               *int   `json:"ttl"`
		Replicas          *int   `json:"replicas"`
		QuotaBytes        *int64 `json:"quota_bytes"`
		RequestsPerMinute *int   `json:"requests_per_minute"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Allow == nil {
		if err == nil {
			err = errors.New("no allow field")
		}
		return Decision{}, &Unavailable{URL: c.url, Status: status, Err: fmt.Errorf("body: %w", err)}
	}
	d := Decision{Allow: *answer.Allow, Reason: answer.Reason, TTL: DefaultTTL, Replicas: DefaultReplicas, QuotaBytes: DefaultQuotaBytes}
	if answer.TTL != nil && *answer.TTL > 0 {
		d.TTL = min(time.Duration(*answer.TTL)*time.Second, MaxTTL)
	}
	if answer.Replicas != nil && *answer.Replicas > 0 {
		d.Replicas = *answer.Replicas
	}
	if answer.QuotaBytes != nil && *answer.QuotaBytes > 0 {
		d.QuotaBytes = *answer.QuotaBytes
	}
	if answer.RequestsPerMinute != nil && *answer.RequestsPerMinute > 0 {
		d.RequestsPerMinute = *answer.RequestsPerMinute
	}
	return d, nil
}

// serverClosedIdle is the text of net/http's errServerClosedIdle: the
// transport's read loop saw the peer's FIN while no response was
// expected on the connection, that is before the request was
// registered on it. net/http returns the sentinel undecorated when the
// connection was fresh, or when it was reused and the request is not
// replayable (a POST), and both apply here. The sentinel is unexported
// and a plain errors.New, so no type or wrapped value reaches the
// caller; the string, compared exactly, is the only handle.
const serverClosedIdle = "http: server closed idle connection"

// retryable reports whether the call failed before a response line
// arrived: a refused or reset connection, a dial timeout, or a
// connection closed without a response (an EOF read, or the closed
// idle connection of net/http). A timeout after the request was sent,
// a non-200, and a body that does not parse are not.
func retryable(err error) bool {
	var u *Unavailable
	if !errors.As(err, &u) || u.Err == nil || u.Status != 0 {
		return false
	}
	var op *net.OpError
	if errors.As(u.Err, &op) && op.Op == "dial" {
		return true
	}
	if errors.Is(u.Err, context.DeadlineExceeded) || errors.Is(u.Err, context.Canceled) {
		return false
	}
	return errors.Is(u.Err, syscall.ECONNRESET) || errors.Is(u.Err, syscall.ECONNREFUSED) ||
		errors.Is(u.Err, io.EOF) || errors.Is(u.Err, io.ErrUnexpectedEOF) || isServerClosedIdle(u.Err)
}

// isServerClosedIdle reports whether err, or an error it wraps, is
// net/http's closed idle connection. The sentinel arrives inside the
// *url.Error of http.Client.Do.
func isServerClosedIdle(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e.Error() == serverClosedIdle {
			return true
		}
	}
	return false
}

// Retryable reports whether a call to an operator's endpoint failed
// before a response line arrived, which is the one class of failure
// worth a second attempt. It is exported so the key resolver of spec
// 024, which calls a second endpoint of the operator's under the same
// rule, shares this one rather than restating it.
func Retryable(err error) bool { return retryable(err) }
