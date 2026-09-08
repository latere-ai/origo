// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/hostmatch"

	"github.com/latere-ai/origo/internal/config"
)

// The egress rules of spec 016 for a server-side fetch, the import of
// spec 019 and the verify of spec 014. A source is a host the caller
// names, so it is hostile until the allow-list admits it, and even then
// the fetch runs through a dialer that resolves the host once, refuses
// every address in a refused range, and dials one of the remaining
// addresses by IP, so a name that rebinds between the check and the
// connection cannot redirect it. Git cannot be given such a dialer, so
// the fetch runs through Proxy, a forward proxy on a loopback port that
// applies it to every hop.

// EgressOptions builds an Egress from the configuration of spec 002.
type EgressOptions struct {
	// Allow is ORIGO_EGRESS_ALLOW, normalized: exact hosts and *.
	// wildcards; empty refuses every host.
	Allow []string
	// Pinned maps an exact host of Allow to the one address inside
	// ClusterCIDRs it may resolve to.
	Pinned map[string]netip.Addr
	// ClusterCIDRs is ORIGO_CLUSTER_CIDRS, refused beside the well-known
	// ranges except for a host's pinned address.
	ClusterCIDRs []netip.Prefix
	// Roots is what the proxy verifies a source's certificate against;
	// nil is the system roots.
	Roots *x509.CertPool
}

// Egress is the pinned dialer.
type Egress struct {
	matcher hostmatch.Matcher
	pinned  map[string]netip.Addr
	cluster []netip.Prefix
	roots   *x509.CertPool
	// allowLoopback admits loopback addresses for a listed host. It has
	// no configuration variable and no exported setter: Options.AllowLoopback
	// of the handler's constructor is the one way to set it, which a unit
	// test of import or verify does to fetch from an in-process source.
	allowLoopback bool
	// resolve and dial are seams for the tests; the defaults are the
	// system resolver and a dialer with a connect timeout.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
}

// egressDialTimeout bounds one connection attempt to a source.
const egressDialTimeout = 10 * time.Second

// NewEgress builds the dialer.
func NewEgress(o EgressOptions) *Egress {
	pinned := map[string]netip.Addr{}
	for host, addr := range o.Pinned {
		pinned[config.NormalizeHost(host)] = addr.Unmap()
	}
	return &Egress{
		matcher: hostmatch.New(o.Allow, config.NormalizeHost),
		pinned:  pinned,
		cluster: o.ClusterCIDRs,
		roots:   o.Roots,
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: (&net.Dialer{Timeout: egressDialTimeout}).DialContext,
	}
}

// withLoopback is the copy the handler builds when Options.AllowLoopback
// is set.
func (e *Egress) withLoopback() *Egress {
	c := *e
	c.allowLoopback = true
	return &c
}

// EgressError is a refused destination. A handler maps it to 400
// invalid_request with the details of Details.
type EgressError struct {
	Host    string
	Address string
	Cause   string
}

func (e *EgressError) Error() string {
	if e.Address != "" {
		return fmt.Sprintf("egress: %s (%s): %s", e.Host, e.Address, e.Cause)
	}
	return fmt.Sprintf("egress: %s: %s", e.Host, e.Cause)
}

// Details is the developer register of the refusal: reason "egress",
// the host, the address when one was refused, and the cause.
func (e *EgressError) Details() map[string]any {
	d := map[string]any{"reason": "egress", "host": e.Host, "cause": e.Cause}
	if e.Address != "" {
		d["address"] = e.Address
	}
	return d
}

// Resolve checks host against the list, resolves it once, and returns
// the addresses the rules admit, in the resolver's order. It opens no
// connection. An empty result is an *EgressError.
func (e *Egress) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = config.NormalizeHost(host)
	if host == "" || !e.matcher.Matches(host) {
		return nil, &EgressError{Host: host, Cause: "host is not on ORIGO_EGRESS_ALLOW"}
	}
	addrs, err := e.resolve(ctx, host)
	if err != nil {
		return nil, &EgressError{Host: host, Cause: "resolve: " + err.Error()}
	}
	pinned, hasPin := e.pinned[host]
	var admitted []netip.Addr
	var refused *EgressError
	for _, a := range addrs {
		a = a.Unmap()
		if cause := e.refuses(a, pinned, hasPin); cause != "" {
			if refused == nil {
				refused = &EgressError{Host: host, Address: a.String(), Cause: cause}
			}
			continue
		}
		admitted = append(admitted, a)
	}
	if len(admitted) == 0 {
		if refused == nil {
			refused = &EgressError{Host: host, Cause: "the name resolves to no address"}
		}
		return nil, refused
	}
	return admitted, nil
}

// refuses names the reason an address is refused, or "" when the rules
// admit it. Loopback, link-local, and unspecified addresses are refused
// whatever the pin, loopback excepted under allowLoopback; a cluster
// address is admitted only when it is the host's pinned address; the
// private ranges of RFC 1918 and RFC 4193 are refused unless the
// address is the host's pinned cluster address.
func (e *Egress) refuses(a, pinned netip.Addr, hasPin bool) string {
	switch {
	case a.IsLoopback():
		if e.allowLoopback {
			return ""
		}
		return "loopback address"
	case a.IsUnspecified():
		return "unspecified address"
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast():
		return "link-local address"
	}
	inCluster := false
	for _, p := range e.cluster {
		if p.Contains(a) {
			inCluster = true
			break
		}
	}
	if inCluster {
		if hasPin && a == pinned {
			return ""
		}
		return "address is in ORIGO_CLUSTER_CIDRS and is not the host's pinned address"
	}
	if a.IsPrivate() {
		return "private address"
	}
	return ""
}

// DialContext resolves the host of address through the rules and dials
// the first admitted address by IP; the host name never reaches the
// operating system's connect path. A refusal is an *EgressError and
// opens no connection.
func (e *Egress) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &EgressError{Host: address, Cause: "address has no port"}
	}
	addrs, err := e.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, a := range addrs {
		conn, err := e.dial(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// transport is what the proxy reaches a source through: the pinned
// dialer, TLS verified against the roots, no proxy of its own, no
// automatic redirect, and no transparent compression, so a source's
// response reaches git as the source sent it.
func (e *Egress) transport() *http.Transport {
	return &http.Transport{
		DialContext:           e.DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: e.roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   egressDialTimeout,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   4,
	}
}

// The proxy's bounds: how many redirects one request follows and how
// large a request body it holds, since a redirected request is sent
// again. Git's requests to a source are the negotiation of a fetch,
// which is bounded by the reference count, never a pack.
const (
	proxyMaxHops      = 10
	proxyMaxBodyBytes = 64 << 20
)

// ProxyRequest is one request the proxy saw, for a test that asserts
// on what git sent.
type ProxyRequest struct {
	Method string
	URL    string
}

// Proxy is the forward proxy of one import or verify: a loopback
// listener git is pointed at through http.proxy, which answers only a
// request carrying its credential, refuses CONNECT, dials every hop of
// a request through the dialer over TLS, and follows redirects itself
// so every hop is checked.
type Proxy struct {
	egress     *Egress
	listener   net.Listener
	server     *http.Server
	client     *http.Client
	credential string

	mu       sync.Mutex
	requests []ProxyRequest
	refusal  *EgressError
}

// StartProxy starts a proxy on a loopback port with a fresh credential.
// The caller closes it when git exits.
func (e *Egress) StartProxy() (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		_ = ln.Close()
		return nil, err
	}
	p := &Proxy{
		egress:     e,
		listener:   ln,
		credential: hex.EncodeToString(raw[:]),
		client:     &http.Client{Transport: e.transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(ln) }()
	return p, nil
}

// URL is the http.proxy value git is given: the credential as the user
// of the loopback address, so the credential travels in the proxy URL
// and nowhere else. The fixed password is there because git prompts
// for one when a proxy URL carries a user alone, and a prompt under
// GIT_TERMINAL_PROMPT=0 ends the fetch; the proxy reads the user only.
func (p *Proxy) URL() string {
	return "http://" + p.credential + ":egress@" + p.listener.Addr().String()
}

// Close stops the listener and every connection.
func (p *Proxy) Close() {
	_ = p.server.Close()
	p.client.CloseIdleConnections()
}

// Requests lists what the proxy saw, in order.
func (p *Proxy) Requests() []ProxyRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProxyRequest(nil), p.requests...)
}

// Refusal is the first destination the rules refused, nil when none
// was; a handler answers it as 400 invalid_request after git exits.
func (p *Proxy) Refusal() *EgressError {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refusal
}

// GitConfig is the environment of spec 016's egress row: the source
// with its scheme rewritten to http://, which is the URL git is given,
// and GIT_CONFIG_COUNT with the extraheader carrying the source bearer
// and http.proxy carrying this proxy, so neither is on the command
// line. Without a token the count is 1 and no header is set.
func (p *Proxy) GitConfig(source *url.URL, token string) (rewritten string, env []string) {
	u := *source
	u.Scheme = "http"
	u.User = nil
	rewritten = u.String()
	n := 0
	if token != "" {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=http.%s.extraheader", n, rewritten), fmt.Sprintf("GIT_CONFIG_VALUE_%d=Authorization: Bearer %s", n, token))
		n++
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=http.proxy", n), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, p.URL()))
	n++
	return rewritten, append([]string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", n)}, env...)
}

// hopByHop are the headers that belong to one connection and are never
// forwarded, the Proxy-* pair included.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// authorized reports whether the request carries the proxy's credential
// as the user of a Basic Proxy-Authorization, which is how curl sends
// the user of an http.proxy URL.
func (p *Proxy) authorized(r *http.Request) bool {
	scheme, value, ok := strings.Cut(r.Header.Get("Proxy-Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	user, _, _ := strings.Cut(string(raw), ":")
	return user == p.credential
}

func (p *Proxy) record(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, ProxyRequest{Method: r.Method, URL: r.URL.String()})
}

func (p *Proxy) refuse(w http.ResponseWriter, err *EgressError) {
	p.mu.Lock()
	if p.refusal == nil {
		p.refusal = err
	}
	p.mu.Unlock()
	http.Error(w, err.Error(), http.StatusForbidden)
}

// ServeHTTP handles one request from git.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.record(r)
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT is not served: the proxy dials every hop itself", http.StatusMethodNotAllowed)
		return
	}
	if !p.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="origo-egress"`)
		http.Error(w, "proxy credential required", http.StatusProxyAuthRequired)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "the proxy serves absolute URLs only", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, proxyMaxBodyBytes+1))
	if err != nil {
		http.Error(w, "request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > proxyMaxBodyBytes {
		http.Error(w, "request body exceeds the proxy's bound", http.StatusRequestEntityTooLarge)
		return
	}
	target := *r.URL
	target.Scheme = "https"
	method := r.Method
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(r.Context(), method, target.String(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "hop: "+err.Error(), http.StatusBadGateway)
			return
		}
		copyHeaders(req.Header, r.Header)
		req.Header.Del("Host")
		resp, err := p.client.Do(req)
		if err != nil {
			if ee, ok := errors.AsType[*EgressError](err); ok {
				p.refuse(w, ee)
				return
			}
			http.Error(w, "source: "+err.Error(), http.StatusBadGateway)
			return
		}
		location := resp.Header.Get("Location")
		if !isRedirect(resp.StatusCode) || location == "" {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			_ = resp.Body.Close()
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if hop == proxyMaxHops {
			http.Error(w, fmt.Sprintf("source: more than %d redirects", proxyMaxHops), http.StatusBadGateway)
			return
		}
		next, err := target.Parse(location)
		if err != nil {
			http.Error(w, "source: redirect: "+err.Error(), http.StatusBadGateway)
			return
		}
		if next.Scheme != "https" {
			p.refuse(w, &EgressError{Host: config.NormalizeHost(next.Hostname()), Cause: "redirect to a URL that is not https"})
			return
		}
		// A 303, or a 301 or 302 answering a POST, is followed with a
		// GET and no body, the way a browser and git's own client do.
		if resp.StatusCode == http.StatusSeeOther || ((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) && method == http.MethodPost) {
			method, body = http.MethodGet, nil
		}
		target = *next
	}
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}
