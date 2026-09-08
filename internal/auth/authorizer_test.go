// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"

	"github.com/latere-ai/origo/test/stubs/authorizer"
)

const (
	repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

// handlerTransport serves an http.Handler in-process, with no socket, and
// counts the attempts.
type handlerTransport struct {
	h        http.Handler
	attempts atomic.Int64
}

func (t *handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// countingTransport counts the attempts of a real transport.
type countingTransport struct {
	next     http.RoundTripper
	attempts atomic.Int64
}

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	return t.next.RoundTrip(r)
}

func newClient(t *testing.T, url, token string, transport http.RoundTripper, clk *clock, reg *pkgmetrics.Registry) *Client {
	t.Helper()
	c, err := NewClient(ClientOptions{URL: url, Token: token, HTTP: &http.Client{Transport: transport}, Timeout: 500 * time.Millisecond, Metrics: metrics.Register(reg), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func request(subject, actor, id string, action Action) Request {
	return Request{Subject: subject, Actor: actor, Repo: RepoRef{ID: id}, Action: action}
}

func TestAuthorizerAnswersAndCaches(t *testing.T) {
	if AuthorizerTimeout != 5*time.Second || DefaultTTL != 60*time.Second || MaxTTL != 600*time.Second || DenyTTL != 5*time.Second || DefaultReplicas != 1 || DefaultQuotaBytes != 53687091200 {
		t.Fatal("the authorizer values are not spec 007's")
	}
	clk := newClock()
	stub := authorizer.New(t)
	reg := pkgmetrics.NewRegistry()
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, reg)
	ctx := context.Background()
	stub.Allow(authorizer.Rule{Subject: "alice", Repo: repoA, Action: "read", TTL: 30, Replicas: 3, QuotaBytes: 1024})
	stub.Allow(authorizer.Rule{Subject: "alice", Repo: repoA, Action: "write", TTL: 9000})
	stub.Deny(authorizer.Rule{Subject: "eve"}, "not welcome")

	d, err := c.Authorize(ctx, request("alice", "", repoA, ActionRead))
	if err != nil || !d.Allow || d.TTL != 30*time.Second || d.Replicas != 3 || d.QuotaBytes != 1024 {
		t.Fatalf("allow with figures: %+v, %v", d, err)
	}
	d, err = c.Authorize(ctx, request("alice", "", repoA, ActionWrite))
	if err != nil || !d.Allow || d.TTL != MaxTTL {
		t.Fatalf("ttl capped: %+v, %v", d, err)
	}
	d, err = c.Authorize(ctx, request("bob", "", repoA, ActionAdmin))
	if err != nil || !d.Allow || d.TTL != DefaultTTL || d.Replicas != DefaultReplicas || d.QuotaBytes != DefaultQuotaBytes {
		t.Fatalf("defaults: %+v, %v", d, err)
	}
	d, err = c.Authorize(ctx, request("eve", "", repoA, ActionRead))
	if err != nil || d.Allow || d.Reason != "not welcome" {
		t.Fatalf("deny: %+v, %v", d, err)
	}
	if got := stub.Requests(); len(got) != 4 || got[0].Subject != "alice" || got[0].Repo.ID != repoA || got[0].Action != "read" {
		t.Fatalf("requests: %+v", got)
	}
	// Cached: the same four answer without a call, until each ttl.
	for _, r := range []Request{request("alice", "", repoA, ActionRead), request("bob", "", repoA, ActionAdmin), request("eve", "", repoA, ActionRead)} {
		if _, err := c.Authorize(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(stub.Requests()) != 4 {
		t.Fatalf("cached answers called the authorizer: %d", len(stub.Requests()))
	}
	clk.Advance(DenyTTL + time.Second)
	if _, err := c.Authorize(ctx, request("eve", "", repoA, ActionRead)); err != nil || len(stub.Requests()) != 5 {
		t.Fatalf("deny cached past 5 seconds: %v, %d", err, len(stub.Requests()))
	}
	if _, err := c.Authorize(ctx, request("alice", "", repoA, ActionRead)); err != nil || len(stub.Requests()) != 5 {
		t.Fatal("a 30 second allow expired early")
	}
	clk.Advance(30 * time.Second)
	if _, err := c.Authorize(ctx, request("alice", "", repoA, ActionRead)); err != nil || len(stub.Requests()) != 6 {
		t.Fatal("a 30 second allow outlived its ttl")
	}
	// An unresolved name is never cached.
	byName := Request{Subject: "alice", Repo: RepoRef{Owner: "acme", Slug: "app"}, Action: ActionRead}
	for range 2 {
		if _, err := c.Authorize(ctx, byName); err != nil {
			t.Fatal(err)
		}
	}
	if len(stub.Requests()) != 8 || c.CacheLen() != 4 {
		t.Fatalf("unresolved name cached: %d requests, %d entries", len(stub.Requests()), c.CacheLen())
	}
	hist := reg.Histogram("origo_authorizer_seconds", "", nil)
	if hist.Count(map[string]string{"result": "allow"}) != 6 || hist.Count(map[string]string{"result": "deny"}) != 2 || hist.Count(map[string]string{"result": "error"}) != 0 {
		t.Fatalf("metric: allow %d deny %d", hist.Count(map[string]string{"result": "allow"}), hist.Count(map[string]string{"result": "deny"}))
	}
	// The bearer travels.
	wrong := newClient(t, stub.URL(), "wrong", &http.Transport{}, clk, nil)
	if _, err := wrong.Authorize(ctx, request("x", "", repoB, ActionRead)); err == nil {
		t.Fatal("a wrong bearer was answered")
	}
	if _, err := NewClient(ClientOptions{URL: "x"}); err == nil {
		t.Fatal("a client without HTTP")
	}
	if _, err := NewClient(ClientOptions{HTTP: &http.Client{Transport: &http.Transport{}}}); err == nil {
		t.Fatal("a client without a URL")
	}
}

func TestAuthorizerOutageDeniesAndRecovers(t *testing.T) {
	clk := newClock()
	stub := authorizer.New(t)
	reg := pkgmetrics.NewRegistry()
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, reg)
	ctx := context.Background()
	unavailable := func(err error) *Unavailable {
		t.Helper()
		var u *Unavailable
		if !errors.As(err, &u) {
			t.Fatalf("not an Unavailable: %v", err)
		}
		return u
	}
	// A 500 is sent one request and not retried.
	stub.Fail(500)
	u := unavailable(func() error { _, err := c.Authorize(ctx, request("alice", "", repoA, ActionRead)); return err }())
	if u.Status != 500 || u.URL != stub.URL() || len(stub.Requests()) != 1 || !strings.Contains(u.Error(), "500") {
		t.Fatalf("500: %+v, %d requests", u, len(stub.Requests()))
	}
	// A body that does not parse, and one without allow, are not retried.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) }))
	t.Cleanup(bad.Close)
	badT := &countingTransport{next: &http.Transport{}}
	if u := unavailable(func() error {
		_, err := newClient(t, bad.URL, "t", badT, clk, nil).Authorize(ctx, request("a", "", repoA, ActionRead))
		return err
	}()); u.Status != 200 || u.Err == nil || badT.attempts.Load() != 1 {
		t.Fatalf("bad body: %+v, %d attempts", u, badT.attempts.Load())
	}
	noAllow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ttl":1}`)) }))
	t.Cleanup(noAllow.Close)
	if u := unavailable(func() error {
		_, err := newClient(t, noAllow.URL, "t", &http.Transport{}, clk, nil).Authorize(ctx, request("a", "", repoA, ActionRead))
		return err
	}()); !strings.Contains(u.Error(), "no allow field") {
		t.Fatalf("no allow: %+v", u)
	}
	// A refused connection is two attempts, then unavailable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedURL := "http://" + ln.Addr().String()
	_ = ln.Close()
	refusedT := &countingTransport{next: &http.Transport{}}
	refused := newClient(t, refusedURL, "t", refusedT, clk, reg)
	if u := unavailable(func() error { _, err := refused.Authorize(ctx, request("a", "", repoA, ActionRead)); return err }()); u.Err == nil || u.Status != 0 || refusedT.attempts.Load() != 2 {
		t.Fatalf("refused: %+v, %d attempts", u, refusedT.attempts.Load())
	}
	// A connection the server closes before any response line is two
	// attempts as well. Which error the transport returns depends on
	// what lands first, the FIN at the read loop's peek, the reset at
	// the write, or the EOF at the read of the response: every form is
	// retryable, so the count does not depend on the race.
	closing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closing.Close() })
	go func() {
		for {
			conn, err := closing.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	closingT := &countingTransport{next: &http.Transport{}}
	if u := unavailable(func() error {
		_, err := newClient(t, "http://"+closing.Addr().String(), "t", closingT, clk, nil).Authorize(ctx, request("a", "", repoA, ActionRead))
		return err
	}()); u.Err == nil || closingT.attempts.Load() != 2 {
		t.Fatalf("closed: %+v, %d attempts", u, closingT.attempts.Load())
	}
	// An authorizer that never answers is unavailable within the
	// timeout, once: a timeout after the request was sent is not retried.
	stub.Resume()
	stub.Hang()
	hungT := &countingTransport{next: &http.Transport{}}
	hung := newClient(t, stub.URL(), stub.Token(), hungT, clk, reg)
	start := time.Now()
	u = unavailable(func() error { _, err := hung.Authorize(ctx, request("alice", "", repoA, ActionRead)); return err }())
	if elapsed := time.Since(start); elapsed > 2*time.Second || !errors.Is(u.Err, context.DeadlineExceeded) || hungT.attempts.Load() != 1 {
		t.Fatalf("hung: %v after %v, %d attempts", u, elapsed, hungT.attempts.Load())
	}
	// The first request after it recovers is served, no restart.
	stub.Resume()
	if d, err := hung.Authorize(ctx, request("alice", "", repoA, ActionRead)); err != nil || !d.Allow {
		t.Fatalf("after recovery: %+v, %v", d, err)
	}
	hist := reg.Histogram("origo_authorizer_seconds", "", nil)
	if hist.Count(map[string]string{"result": "error"}) != 3 || hist.Count(map[string]string{"result": "allow"}) != 1 {
		t.Fatalf("metric: error %d allow %d", hist.Count(map[string]string{"result": "error"}), hist.Count(map[string]string{"result": "allow"}))
	}
	// A cancelled context is not retried either.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cancelT := &countingTransport{next: &http.Transport{}}
	if _, err := newClient(t, stub.URL(), stub.Token(), cancelT, clk, nil).Authorize(cancelled, request("a", "", repoB, ActionRead)); err == nil || cancelT.attempts.Load() != 1 {
		t.Fatalf("cancelled: %v, %d attempts", err, cancelT.attempts.Load())
	}
	if retryable(errors.New("plain")) || retryable(&Unavailable{Status: 500}) || (&Unavailable{URL: "u"}).Error() == "" {
		t.Fatal("retryable classification")
	}
}

// closeReportingConn is a dialed connection that reports its Close and
// accepts a Write after it, the way a kernel accepts bytes for a peer
// that sent FIN and no RST yet: the case net/http documents for its
// closed idle connection error.
type closeReportingConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeReportingConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *closeReportingConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return len(p), nil
	default:
		return c.Conn.Write(p)
	}
}

// keepAliveThenCloseServer serves one request on its first connection,
// keeps the connection open until closeFirst is closed, then closes it.
// Every later connection is served until the client closes it.
func keepAliveThenCloseServer(t *testing.T, closeFirst <-chan struct{}) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	serve := func(conn net.Conn, first bool) {
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			const body = `{"allow":true}`
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			if first {
				<-closeFirst
				return
			}
		}
	}
	go func() {
		for n := 0; ; n++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn, n == 0)
		}
	}()
	return "http://" + ln.Addr().String()
}

// TestClosedIdleConnectionIsRetried reproduces the CI flake of run
// 34202697213: the authorizer closes a kept-alive connection, and the
// transport's read loop sees the FIN before the next request is
// registered on the connection, so net/http returns its closed idle
// connection error, which a POST is never retried on inside the
// transport. The call failed before any response byte, so the client
// retries once.
//
// The order is forced: the trace's GotConn runs after the idle
// connection is handed to the request and before the request is
// registered on it, so the test closes the server side there and waits
// until the read loop has closed the client side.
func TestClosedIdleConnectionIsRetried(t *testing.T) {
	closeFirst := make(chan struct{})
	url := keepAliveThenCloseServer(t, closeFirst)
	var (
		mu   sync.Mutex
		dial *closeReportingConn
	)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		c := &closeReportingConn{Conn: conn, closed: make(chan struct{})}
		mu.Lock()
		dial = c
		mu.Unlock()
		return c, nil
	}}
	counting := &countingTransport{next: transport}
	c := newClient(t, url, "t", counting, newClock(), nil)
	var reused atomic.Int64
	var closeOnce sync.Once
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if !info.Reused {
			return
		}
		reused.Add(1)
		closeOnce.Do(func() { close(closeFirst) })
		mu.Lock()
		conn := dial
		mu.Unlock()
		select {
		case <-conn.closed:
		case <-time.After(5 * time.Second):
			t.Error("the transport did not close the idle connection the server closed")
		}
	}})
	if d, err := c.Authorize(ctx, request("alice", "", repoA, ActionRead)); err != nil || !d.Allow || counting.attempts.Load() != 1 {
		t.Fatalf("first call: %+v, %v, %d attempts", d, err, counting.attempts.Load())
	}
	// A second subject, so the answer is not the cache's.
	d, err := c.Authorize(ctx, request("bob", "", repoA, ActionRead))
	if reused.Load() != 1 {
		t.Fatalf("the second call did not reuse the connection: %d", reused.Load())
	}
	if err != nil || !d.Allow || counting.attempts.Load() != 3 {
		t.Fatalf("closed idle connection: %+v, %v, %d attempts", d, err, counting.attempts.Load())
	}
	// The classification, on the error as http.Client.Do shapes it.
	shaped := &Unavailable{URL: url, Err: fmt.Errorf("Post %q: %w", url, errors.New(serverClosedIdle))}
	if !retryable(shaped) || retryable(&Unavailable{URL: url, Err: errors.New("http: server closed idle connection early")}) {
		t.Fatal("closed idle classification")
	}
}

func TestCachesAreBounded(t *testing.T) {
	if CacheEntries != 65536 {
		t.Fatal("the cache bound is not spec 007's")
	}
	clk := newClock()
	stub := authorizer.New(t)
	transport := &handlerTransport{h: stub.Handler()}
	c := newClient(t, "http://authorizer.test/", stub.Token(), transport, clk, nil)
	ctx := context.Background()
	first := request("subject-0", "", repoA, ActionRead)
	for i := range CacheEntries + 1 {
		if _, err := c.Authorize(ctx, request(fmt.Sprintf("subject-%d", i), "", repoA, ActionRead)); err != nil {
			t.Fatal(err)
		}
	}
	if c.CacheLen() != CacheEntries || transport.attempts.Load() != CacheEntries+1 {
		t.Fatalf("%d entries after %d calls", c.CacheLen(), transport.attempts.Load())
	}
	// The first allow was evicted: asking again is a call; the newest
	// was kept.
	if _, err := c.Authorize(ctx, first); err != nil || transport.attempts.Load() != CacheEntries+2 {
		t.Fatalf("first entry still cached: %d calls", transport.attempts.Load())
	}
	if _, err := c.Authorize(ctx, request(fmt.Sprintf("subject-%d", CacheEntries), "", repoA, ActionRead)); err != nil || transport.attempts.Load() != CacheEntries+2 {
		t.Fatalf("newest entry evicted: %d calls", transport.attempts.Load())
	}

	// The verified-token cache holds the same bound. Verify remembers
	// every token it accepts under the SHA-256 of its bytes; the entries
	// are written the way Verify writes them, then one real token lands.
	key := newKey(t)
	v := newVerifier(t, clk, key)
	signer := NewSigner(key, localIssuer, clk.Now)
	until := clk.Now().Add(time.Hour)
	var firstKey [32]byte
	for i := range CacheEntries {
		k := tokenKey(fmt.Sprintf("token-%d", i))
		if i == 0 {
			firstKey = k
		}
		v.cache.Set(k, cached{principal: Principal{Subject: "x"}, until: until})
	}
	tok, _, err := signer.Mint(Principal{Subject: "last"}, repoA, ScopeRead, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.cache.Get(firstKey); v.CacheLen() != CacheEntries || ok {
		t.Fatalf("%d token entries, first present %v", v.CacheLen(), ok)
	}
	if _, ok := v.cache.Get(tokenKey(tok)); !ok {
		t.Fatal("the verified token was not remembered")
	}
}

func TestCacheKeyIncludesActor(t *testing.T) {
	clk := newClock()
	stub := authorizer.New(t)
	c := newClient(t, stub.URL(), stub.Token(), &http.Transport{}, clk, nil)
	ctx := context.Background()
	for _, actor := range []string{"svc-1", "svc-2", "svc-1", "svc-2"} {
		if _, err := c.Authorize(ctx, request("alice", actor, repoA, ActionWrite)); err != nil {
			t.Fatal(err)
		}
	}
	reqs := stub.Requests()
	if len(reqs) != 2 || reqs[0].Actor != "svc-1" || reqs[1].Actor != "svc-2" || c.CacheLen() != 2 {
		t.Fatalf("%d calls, %d entries: %+v", len(reqs), c.CacheLen(), reqs)
	}
	// The other three components each key the cache as well.
	for _, r := range []Request{request("bob", "svc-1", repoA, ActionWrite), request("alice", "svc-1", repoB, ActionWrite), request("alice", "svc-1", repoA, ActionRead)} {
		if _, err := c.Authorize(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(stub.Requests()) != 5 || c.CacheLen() != 5 {
		t.Fatalf("%d calls, %d entries", len(stub.Requests()), c.CacheLen())
	}
}
