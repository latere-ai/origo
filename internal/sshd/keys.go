// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"latere.ai/x/pkg/cache"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/metrics"
)

// The values of spec 024's key resolution contract. They are spec 007's
// figures for the authorizer, deliberately: an operator reasons about
// one set of numbers for both endpoints Origo asks before it serves a
// request.
const (
	// KeysTimeout bounds one call, the retry included.
	KeysTimeout = 5 * time.Second
	// DefaultTTL is how long an answer that names a subject is cached
	// when it names no ttl, and MaxTTL the cap on one it does name.
	DefaultTTL = auth.DefaultTTL
	MaxTTL     = auth.MaxTTL
	// NotFoundTTL is how long {"found": false} is held, so a revocation
	// is not kept alive by a negative answer.
	NotFoundTTL = auth.DenyTTL
	// CacheEntries is the most fingerprints the node holds answers for.
	CacheEntries = auth.CacheEntries
	// maxAnswerBytes bounds the body read from the endpoint.
	maxAnswerBytes = 64 << 10
	keysAttempts   = 2
)

// Answer is what the operator's endpoint said about one public key.
type Answer struct {
	// Found is whether the key names a subject that may authenticate
	// now. A revoked, expired, or unknown key is false, and nothing says
	// which: the client reads Permission denied (publickey) either way.
	Found bool
	// Subject is the effective subject, the same string the operator's
	// OIDC issuer puts in sub for the same person, so one identity
	// crosses both transports.
	Subject string
	// KeyID is the store's own id for the key. Origo logs it and sends
	// it nowhere.
	KeyID string
	// TTL is how long the answer may be cached.
	TTL time.Duration
}

// Resolver asks the operator's key resolution endpoint which subject an
// offered public key belongs to, and caches what it said.
//
// Origo stores no public key. The endpoint is the whole store contract:
// always 200, one subject per fingerprint installation-wide, and a
// refusal when it cannot answer. Authentication fails closed, which is
// spec 007's rule for the authorizer and is the same here.
type Resolver struct {
	url     string
	token   string
	http    *http.Client
	timeout time.Duration
	now     func() time.Time
	seconds *pkgmetrics.Histogram
	cache   *cache.TTLCache[string, cachedAnswer]
}

type cachedAnswer struct {
	answer Answer
	until  time.Time
}

// ResolverOptions configures a Resolver.
type ResolverOptions struct {
	// URL is ORIGO_SSH_KEYS_URL and Token ORIGO_SSH_KEYS_TOKEN.
	URL   string
	Token string
	// HTTP sends the calls. Required; the node passes its instrumented
	// client, the one the authorizer's calls go through.
	HTTP *http.Client
	// Timeout bounds one call. KeysTimeout when zero.
	Timeout time.Duration
	// Metrics receives origo_ssh_keys_seconds{result}.
	Metrics *metrics.Set
	// Now is the clock the cache runs on. The wall clock by default.
	Now func() time.Time
}

// NewResolver builds the client. It sends nothing.
func NewResolver(o ResolverOptions) (*Resolver, error) {
	if o.HTTP == nil {
		return nil, errors.New("sshd: the key resolver needs an HTTP client")
	}
	if o.URL == "" {
		return nil, errors.New("sshd: the key resolver needs a URL")
	}
	r := &Resolver{url: o.URL, token: o.Token, http: o.HTTP, timeout: o.Timeout, now: o.Now}
	if r.timeout == 0 {
		r.timeout = KeysTimeout
	}
	if r.now == nil {
		r.now = time.Now
	}
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	r.seconds = set.SSHKeys
	r.cache = cache.New[string, cachedAnswer](MaxTTL, cache.WithMaxSize[string, cachedAnswer](CacheEntries), cache.WithClock[string, cachedAnswer](r.now))
	return r, nil
}

// Request is the body of one resolve call, the shape any operator
// implements.
type Request struct {
	Fingerprint string `json:"fingerprint"`
	Type        string `json:"type"`
	PublicKey   string `json:"public_key"`
}

// RequestFor is what Origo sends for an offered key: the OpenSSH
// SHA-256 fingerprint, the algorithm name, and the key in
// authorized_keys form with no options and no comment, so a store that
// keeps keys as text matches on it without re-encoding.
func RequestFor(key ssh.PublicKey) Request {
	return Request{
		Fingerprint: ssh.FingerprintSHA256(key),
		Type:        key.Type(),
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
	}
}

// Resolve answers from the cache or asks the endpoint. It returns an
// error only when no answer could be had, which authentication treats
// as a refusal and never as an allow.
func (r *Resolver) Resolve(ctx context.Context, key ssh.PublicKey) (Answer, error) {
	req := RequestFor(key)
	now := r.now()
	if e, ok := r.cache.Get(req.Fingerprint); ok && now.Before(e.until) {
		return e.answer, nil
	}
	start := time.Now()
	answer, err := r.call(ctx, req)
	result := "found"
	switch {
	case err != nil:
		result = "error"
	case !answer.Found:
		result = "not_found"
	}
	r.seconds.Observe(map[string]string{"result": result}, time.Since(start).Seconds())
	if err != nil {
		return Answer{}, err
	}
	ttl := NotFoundTTL
	if answer.Found {
		ttl = answer.TTL
	}
	r.cache.Set(req.Fingerprint, cachedAnswer{answer: answer, until: now.Add(ttl)})
	return answer, nil
}

// CacheLen reports the cache's size, for a test.
func (r *Resolver) CacheLen() int { return r.cache.Len() }

// call makes the request under the timeout, retrying once when the
// connection failed before a response line arrived. The rule is spec
// 007's for the authorizer, shared rather than restated.
func (r *Resolver) call(ctx context.Context, req Request) (Answer, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Answer{}, &auth.Unavailable{URL: r.url, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var last error
	for range keysAttempts {
		answer, err := r.once(ctx, body)
		if err == nil {
			return answer, nil
		}
		last = err
		if !auth.Retryable(err) {
			break
		}
	}
	return Answer{}, last
}

func (r *Resolver) once(ctx context.Context, body []byte) (Answer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return Answer{}, &auth.Unavailable{URL: r.url, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return Answer{}, &auth.Unavailable{URL: r.url, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Answer{}, &auth.Unavailable{URL: r.url, Status: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return Answer{}, &auth.Unavailable{URL: r.url, Status: resp.StatusCode, Err: err}
	}
	var answer struct {
		Found   *bool  `json:"found"`
		Subject string `json:"subject"`
		KeyID   string `json:"key_id"`
		TTL     *int   `json:"ttl"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Found == nil {
		if err == nil {
			err = errors.New("no found field")
		}
		return Answer{}, &auth.Unavailable{URL: r.url, Status: resp.StatusCode, Err: fmt.Errorf("body: %w", err)}
	}
	// A found answer with no subject names nobody, which is not an
	// identity to run a push under.
	if *answer.Found && answer.Subject == "" {
		return Answer{}, &auth.Unavailable{URL: r.url, Status: resp.StatusCode, Err: errors.New("body: found with no subject")}
	}
	out := Answer{Found: *answer.Found, Subject: answer.Subject, KeyID: answer.KeyID, TTL: DefaultTTL}
	if answer.TTL != nil && *answer.TTL > 0 {
		out.TTL = min(time.Duration(*answer.TTL)*time.Second, MaxTTL)
	}
	return out, nil
}
