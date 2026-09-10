// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package origoclient speaks Origo's contract 1 and formats nothing.
//
// It is the layer every client of this repository sits on: the routes, the
// query grammar, the paging, the Range arithmetic, the parent-directory path
// resolution, the short-id expansion, the short Retry-After wait, and spec
// 003's refusal envelope decoded into a typed [Refusal]. It writes no line, it
// holds no default a caller did not ask for, it reads no environment variable,
// and it never touches os.Stdout. A front end decides what a reader sees; this
// package decides what the wire carries.
//
// Two rules of the read API bind every method here and are easy to get wrong,
// so they are enforced in one place rather than at each call site. A reference
// name with a slash cannot travel in a {sha} path segment, so every request
// sends the placeholder segment "-" with the name in ?ref=, ?base= or ?head=.
// And ?path= on the tree route appends a trailing slash server-side, so it
// lists a directory and never a file; [Client.File] resolves a file by listing
// its parent directory and matching the basename.
//
// There is deliberately no method that mints a credential and none that
// reaches an administration route. POST /v1/repos/{id}/tokens is an admin
// action (spec 007), and a client an agent runs in a loop must hold no code
// path that widens its own access.
package origoclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/latere-ai/origo/internal/contract"
)

// The two read-API headers a client reads. internal/api owns them and a
// client binary cannot import a node package without dragging git and the
// write-ahead log behind it, so they are named here and the conformance
// suite is what holds the two in step.
const (
	HeaderCommit    = "Origo-Commit"
	HeaderTruncated = "Origo-Truncated"
)

// Placeholder is the {sha} path segment that says the name is in the query.
// Every request this package makes uses it, so a branch named feature/x is
// served rather than refused (internal/api/read.go, shortNameRe).
const Placeholder = "-"

// ShortWaitLimit is the longest Retry-After this package sleeps through
// rather than returning. A caller's next turn costs more than five seconds
// of waiting; a minute of it does not.
const ShortWaitLimit = 5 * time.Second

// Config is what a Client needs. URL and Token are required.
type Config struct {
	// URL is the installation, with no trailing slash.
	URL string
	// Token is the bearer sent on every request. It is never logged and
	// never appears in an error this package returns.
	Token string
	// HTTP is the transport; nil takes http.DefaultClient.
	HTTP *http.Client
	// Timeout bounds one request; zero takes DefaultTimeout.
	Timeout time.Duration
	// Sleep is the short Retry-After wait, replaceable in a test.
	Sleep func(time.Duration)
}

// DefaultTimeout bounds one call to a read route, whose own budget is 30
// seconds (spec 009).
const DefaultTimeout = 60 * time.Second

// Client is every call this repository's clients make on contract 1.
type Client struct {
	url     string
	token   string
	hc      *http.Client
	timeout time.Duration
	sleep   func(time.Duration)
}

// New builds a Client. It performs no request and reaches no network.
func New(cfg Config) *Client {
	c := &Client{
		url:     strings.TrimRight(cfg.URL, "/"),
		token:   cfg.Token,
		hc:      cfg.HTTP,
		timeout: cfg.Timeout,
		sleep:   cfg.Sleep,
	}
	if c.hc == nil {
		c.hc = &http.Client{Transport: transport()}
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	if c.sleep == nil {
		c.sleep = time.Sleep
	}
	return c
}

// transport is the standard transport, cloned so this client's connection
// settings are its own rather than the process-global ones anything else in
// the program can mutate.
//
// It carries no OpenTelemetry transport, and that is a decision rather than an
// omission. `origo` is a command a person or an agent runs on a laptop (spec
// 025): nothing upstream of it holds a trace to continue, no exporter is
// configured in that process, and otelhttp would pull go.opentelemetry.io into
// a binary whose depcheck row states it reaches no OpenTelemetry package. The
// node's own outbound calls are instrumented and unaffected.
func transport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// Refusal is spec 003's error envelope, decoded. Details is the developer
// half of the envelope, handed over as the wire carried it: rendering one of
// its values is a front end's decision, not this package's.
type Refusal struct {
	Status     int
	Code       string
	Message    string
	Details    map[string]any
	RetryAfter string
	RateLimit  string
}

// Error is the developer register: the status and the code, and nothing a
// reader would be shown. A front end reads the fields.
func (r *Refusal) Error() string { return strconv.Itoa(r.Status) + " " + r.Code }

// Detail reads one details field without deciding how it prints.
func (r *Refusal) Detail(key string) (any, bool) {
	v, ok := r.Details[key]
	return v, ok && v != nil
}

// AsRefusal is errors.As for the one error type this package defines.
func AsRefusal(err error) (*Refusal, bool) {
	var ref *Refusal
	ok := errors.As(err, &ref)
	return ref, ok
}

// Unreachable is a transport failure: the installation did not answer, or its
// answer could not be read. It never carries the URL, which holds no
// credential but does name an installation.
type Unreachable struct {
	Op  string
	Err error
}

func (u *Unreachable) Error() string { return u.Op + ": " + u.Err.Error() }
func (u *Unreachable) Unwrap() error { return u.Err }

// Meta is what every read response carries beside its body: the object id the
// request resolved to, whether Origo cut the body itself, and the seconds
// since the last currency check when it answered from a copy it did not check
// (spec 015).
type Meta struct {
	Commit    string
	Truncated bool
	Stale     string
}

// reply is one answer, the body already read.
type reply struct {
	status int
	body   []byte
	header http.Header
}

func (r *reply) meta() Meta {
	return Meta{
		Commit:    r.header.Get(HeaderCommit),
		Truncated: r.header.Get(HeaderTruncated) == "true",
		Stale:     r.header.Get(contract.HeaderStale),
	}
}

// get calls a read route.
func (c *Client) get(ctx context.Context, path string, q url.Values) (*reply, error) {
	return c.do(ctx, http.MethodGet, target(path, q), nil, nil, c.timeout)
}

// getRange calls a read route for a window of a blob.
func (c *Client) getRange(ctx context.Context, path string, q url.Values, first, last int64) (*reply, error) {
	h := http.Header{"Range": []string{fmt.Sprintf("bytes=%d-%d", first, last)}}
	return c.do(ctx, http.MethodGet, target(path, q), nil, h, c.timeout)
}

// post calls an operation route under its own budget.
func (c *Client) post(ctx context.Context, path string, body any, budget time.Duration) (*reply, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, &Unreachable{Op: "encode", Err: err}
	}
	h := http.Header{"Content-Type": []string{"application/json"}}
	return c.do(ctx, http.MethodPost, path, raw, h, budget)
}

// target joins a path and its query.
func target(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// do makes one call, waits out a short Retry-After once, and turns any status
// at or past 400 into a Refusal.
func (c *Client) do(ctx context.Context, method, path string, body []byte, extra http.Header, budget time.Duration) (*reply, error) {
	r, err := c.once(ctx, method, path, body, extra, budget)
	if err != nil {
		return nil, err
	}
	if wait, ok := shortWait(r); ok {
		// A wait a caller's next turn is dearer than: slept here, so the
		// model pays nothing for it. Once, never twice.
		c.sleep(wait)
		if r, err = c.once(ctx, method, path, body, extra, budget); err != nil {
			return nil, err
		}
	}
	if r.status >= 400 {
		return nil, refusalOf(r)
	}
	return r, nil
}

// once makes exactly one request.
func (c *Client) once(ctx context.Context, method, path string, body []byte, extra http.Header, budget time.Duration) (*reply, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rdr)
	if err != nil {
		return nil, &Unreachable{Op: "request", Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Refusal{
				Status:  http.StatusGatewayTimeout,
				Code:    contract.CodeOperationTimeout,
				Message: contract.Sentence(contract.CodeOperationTimeout),
				Details: map[string]any{
					"operation":      operationOf(path),
					"budget_seconds": float64(budget / time.Second),
				},
			}
		}
		return nil, &Unreachable{Op: "call", Err: bare(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &Unreachable{Op: "read", Err: bare(err)}
	}
	return &reply{status: resp.StatusCode, body: raw, header: resp.Header}, nil
}

// bare strips the URL a net/http error wraps: it carries no credential, but it
// does name an installation an operator may not wish repeated.
func bare(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// operationOf names the route of a timeout in one word, the way the node's own
// operation_timeout details do.
func operationOf(path string) string {
	p, _, _ := strings.Cut(path, "?")
	seg := strings.Split(strings.Trim(p, "/"), "/")
	last := seg[len(seg)-1]
	if strings.HasPrefix(last, Placeholder) && len(seg) > 1 {
		return seg[len(seg)-2]
	}
	return last
}

// shortWait reports the wait of a refusal worth sleeping through: a 429 or a
// 503 whose Retry-After is at most ShortWaitLimit.
func shortWait(r *reply) (time.Duration, bool) {
	if r.status != http.StatusTooManyRequests && r.status != http.StatusServiceUnavailable {
		return 0, false
	}
	n, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || n <= 0 {
		return 0, false
	}
	if d := time.Duration(n) * time.Second; d <= ShortWaitLimit {
		return d, true
	}
	return 0, false
}

// envelope is spec 003's refusal body as httpjson writes it.
type envelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// refusalOf decodes one refusal. A body that is not the envelope, which is
// what a proxy in front of the installation answers, still produces a Refusal
// rather than a nil error, because a caller must never read a failed response
// as a served one.
func refusalOf(r *reply) *Refusal {
	var e envelope
	_ = json.Unmarshal(r.body, &e)
	ref := &Refusal{
		Status:     r.status,
		Code:       e.Error.Code,
		Message:    e.Error.Message,
		Details:    e.Error.Details,
		RetryAfter: r.header.Get("Retry-After"),
		RateLimit:  r.header.Get(contract.HeaderRateLimit),
	}
	if ref.Code == "" {
		ref.Code = "unexpected_status"
		ref.Message = "The installation answered " + strconv.Itoa(r.status) + "."
	}
	if ref.Details == nil {
		ref.Details = map[string]any{}
	}
	return ref
}

// decode reads a served body into v.
func decode(r *reply, v any) error {
	if err := json.Unmarshal(r.body, v); err != nil {
		return &Unreachable{Op: "decode", Err: err}
	}
	return nil
}
