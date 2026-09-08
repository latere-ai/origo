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
	"latere.ai/x/pkg/metrics"
)

// Action is what a request wants to do with a repository.
type Action string

// The three actions of spec 007.
const (
	ActionRead  Action = "read"
	ActionWrite Action = "write"
	ActionAdmin Action = "admin"
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

// Decision is the authorizer's answer with the defaults applied.
type Decision struct {
	Allow      bool
	Reason     string
	TTL        time.Duration
	Replicas   int
	QuotaBytes int64
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
	AuthorizerTimeout  = 5 * time.Second
	DefaultTTL         = 60 * time.Second
	MaxTTL             = 600 * time.Second
	DenyTTL            = 5 * time.Second
	DefaultReplicas    = 1
	DefaultQuotaBytes  = 53687091200
	maxDecisionBytes   = 64 << 10
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
	Metrics *metrics.Registry
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
	seconds *metrics.Histogram
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
	reg := o.Metrics
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	c.seconds = reg.Histogram("origo_authorizer_seconds", "authorizer calls by result", metrics.DefaultDurationBuckets)
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

// CacheLen reports the cache's size, for tests.
func (c *Client) CacheLen() int { return c.cache.Len() }

// call makes the request under the timeout, retrying once when the
// connection failed before a response line arrived.
func (c *Client) call(ctx context.Context, req Request) (Decision, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, &Unavailable{URL: c.url, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var last error
	for range authorizerAttempts {
		d, err := c.once(ctx, body)
		if err == nil {
			return d, nil
		}
		last = err
		if !retryable(err) {
			break
		}
	}
	return Decision{}, last
}

func (c *Client) once(ctx context.Context, body []byte) (Decision, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Decision{}, &Unavailable{URL: c.url, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Decision{}, &Unavailable{URL: c.url, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Decision{}, &Unavailable{URL: c.url, Status: resp.StatusCode}
	}
	var answer struct {
		Allow      *bool  `json:"allow"`
		Reason     string `json:"reason"`
		TTL        *int   `json:"ttl"`
		Replicas   *int   `json:"replicas"`
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDecisionBytes))
	if err != nil {
		return Decision{}, &Unavailable{URL: c.url, Status: resp.StatusCode, Err: err}
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Allow == nil {
		if err == nil {
			err = errors.New("no allow field")
		}
		return Decision{}, &Unavailable{URL: c.url, Status: resp.StatusCode, Err: fmt.Errorf("body: %w", err)}
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
	return d, nil
}

// retryable reports whether the call failed before a response line
// arrived: a refused or reset connection, a dial timeout, or a
// connection closed without a response. A timeout after the request
// was sent, a non-200, and a body that does not parse are not.
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
		errors.Is(u.Err, io.EOF) || errors.Is(u.Err, io.ErrUnexpectedEOF)
}
