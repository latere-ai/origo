// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/source"
)

// countingListener accepts on a loopback port and counts connections,
// so a test asserts that a refusal opened none.
type countingListener struct {
	net.Listener
	conns atomic.Int64
}

func newCountingListener(t *testing.T) *countingListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &countingListener{Listener: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.conns.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return c
}

func (c *countingListener) port() string {
	_, port, _ := net.SplitHostPort(c.Addr().String())
	return port
}

// fakeResolver answers each host from a table; a slice of answers is
// consumed one per lookup, the last one repeating.
type fakeResolver struct {
	answers map[string][]string
	calls   map[string]int
}

func (f *fakeResolver) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	all, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	i := min(f.calls[host], len(all)-1)
	f.calls[host]++
	var out []netip.Addr
	for s := range strings.SplitSeq(all[i], ",") {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

// recordingDial records what the dialer asked to connect to and refuses
// every connection, so an address that must not be dialed is seen and
// nothing is reached.
type recordingDial struct{ addrs []string }

func (r *recordingDial) dial(_ context.Context, _, address string) (net.Conn, error) {
	r.addrs = append(r.addrs, address)
	return nil, errors.New("recording dialer")
}

// egressError fails the test unless err is a refusal whose details map
// to reason "egress", the shape a handler answers as 400 invalid_request.
func egressError(t *testing.T, err error) *EgressError {
	t.Helper()
	ee, ok := errors.AsType[*EgressError](err)
	if !ok {
		t.Fatalf("not an egress refusal: %v", err)
	}
	d := ee.Details()
	if d["reason"] != "egress" || d["host"] == "" || d["cause"] == "" {
		t.Fatalf("details %v", d)
	}
	rec := httptest.NewRecorder()
	contract.Write(rec, http.StatusBadRequest, contract.CodeInvalid, d)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"reason":"egress"`) {
		t.Fatalf("rendered %d %s", rec.Code, rec.Body)
	}
	return ee
}

// TestEgressDialerHonoursTheAllowList: a host not on the list, or one
// that resolves to a loopback, private, link-local, or unique-local
// address, or into ORIGO_CLUSTER_CIDRS though a wildcard lists it, is
// refused with the egress error and opens no connection; with the list
// unset every host is refused.
func TestEgressDialerHonoursTheAllowList(t *testing.T) {
	ln := newCountingListener(t)
	res := &fakeResolver{answers: map[string][]string{
		"loop.example.com":    {"127.0.0.1"},
		"rfc1918.example.com": {"10.0.0.1"},
		"meta.example.com":    {"169.254.169.254"},
		"ula.example.com":     {"fd00::1"},
		"svc.example.com":     {"10.96.0.42"},
		"pod.example.com":     {"10.244.1.7"},
		"mapped.example.com":  {"::ffff:10.0.0.1"},
		"public.example.com":  {"93.184.216.34"},
		"unlisted.example":    {"93.184.216.34"},
	}}
	dial := &recordingDial{}
	e := NewEgress(EgressOptions{Allow: []string{"*.example.com"}, ClusterCIDRs: []netip.Prefix{netip.MustParsePrefix("10.96.0.0/16"), netip.MustParsePrefix("10.244.0.0/16")}})
	e.resolve, e.dial = res.lookup, dial.dial
	ctx := context.Background()
	for host, cause := range map[string]string{
		"loop.example.com":    "loopback address",
		"rfc1918.example.com": "private address",
		"meta.example.com":    "link-local address",
		"ula.example.com":     "private address",
		"svc.example.com":     "not the host's pinned address",
		"pod.example.com":     "not the host's pinned address",
		"mapped.example.com":  "private address",
		"unlisted.example":    "not on ORIGO_EGRESS_ALLOW",
		"example.com":         "not on ORIGO_EGRESS_ALLOW",
		"nowhere.example.com": "resolve: no such host",
	} {
		_, err := e.DialContext(ctx, "tcp", net.JoinHostPort(host, ln.port()))
		if ee := egressError(t, err); !strings.Contains(ee.Cause, cause) {
			t.Errorf("%s: cause %q, want %q", host, ee.Cause, cause)
		}
	}
	if len(dial.addrs) != 0 || ln.conns.Load() != 0 {
		t.Fatalf("a refusal dialed: %v, %d connections", dial.addrs, ln.conns.Load())
	}
	// A public address on a listed host is dialed, by IP, with the port.
	if _, err := e.DialContext(ctx, "tcp", "Public.example.com.:443"); err == nil || !slices.Equal(dial.addrs, []string{"93.184.216.34:443"}) {
		t.Fatalf("public: %v, dialed %v", err, dial.addrs)
	}
	if _, err := e.DialContext(ctx, "tcp", "public.example.com"); err == nil {
		t.Fatal("an address without a port was accepted")
	}
	// The list unset refuses every host, the public one included.
	unset := NewEgress(EgressOptions{})
	unset.resolve, unset.dial = res.lookup, dial.dial
	if _, err := unset.DialContext(ctx, "tcp", "public.example.com:443"); !strings.Contains(egressError(t, err).Cause, "not on ORIGO_EGRESS_ALLOW") {
		t.Fatalf("unset list: %v", err)
	}
	// A refused address among admitted ones is skipped and the dial
	// goes to the admitted one; a name with no address is refused.
	res.answers["mixed.example.com"] = []string{"127.0.0.1,93.184.216.34"}
	res.answers["empty.example.com"] = []string{""}
	dial.addrs = nil
	if _, err := e.DialContext(ctx, "tcp", "mixed.example.com:443"); err == nil || !slices.Equal(dial.addrs, []string{"93.184.216.34:443"}) {
		t.Fatalf("mixed: %v, dialed %v", err, dial.addrs)
	}
	res.answers["empty.example.com"] = nil
	e.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "empty.example.com" {
			return nil, nil
		}
		return res.lookup(ctx, host)
	}
	if _, err := e.Resolve(ctx, "empty.example.com"); !strings.Contains(egressError(t, err).Cause, "no address") {
		t.Fatalf("empty: %v", err)
	}
}

// TestEgressDialerAdmitsOnlyThePinnedClusterAddress: a host listed as
// host=10.96.0.42 that resolves to 10.96.0.42, inside the cluster
// ranges, is dialed; the same host resolving to 10.96.0.43 or to
// 127.0.0.1 is refused; an exact host without a pin that resolves into
// the cluster ranges is refused; and the pin admits its address even
// where a well-known range also contains it.
func TestEgressDialerAdmitsOnlyThePinnedClusterAddress(t *testing.T) {
	ln := newCountingListener(t)
	res := &fakeResolver{answers: map[string][]string{
		"origo-stubs.origo.svc": {"10.96.0.42", "10.96.0.43", "127.0.0.1", "10.0.0.9"},
		"plain.origo.svc":       {"10.96.0.42"},
	}}
	dial := &recordingDial{}
	e := NewEgress(EgressOptions{
		Allow:        []string{"origo-stubs.origo.svc", "plain.origo.svc"},
		Pinned:       map[string]netip.Addr{"Origo-Stubs.origo.svc": netip.MustParseAddr("::ffff:10.96.0.42")},
		ClusterCIDRs: []netip.Prefix{netip.MustParsePrefix("10.96.0.0/16"), netip.MustParsePrefix("10.244.0.0/16"), netip.MustParsePrefix("10.0.0.0/8")},
	})
	e.resolve, e.dial = res.lookup, dial.dial
	ctx := context.Background()
	_, err := e.DialContext(ctx, "tcp", "origo-stubs.origo.svc:8443")
	if err == nil || !slices.Equal(dial.addrs, []string{"10.96.0.42:8443"}) {
		t.Fatalf("pinned address: %v, dialed %v", err, dial.addrs)
	}
	dial.addrs = nil
	for _, want := range []string{"10.96.0.43", "127.0.0.1", "10.0.0.9"} {
		_, err := e.DialContext(ctx, "tcp", "origo-stubs.origo.svc:8443")
		if ee := egressError(t, err); ee.Address != want {
			t.Errorf("refused %q, want %s", ee.Address, want)
		}
	}
	if _, err := e.DialContext(ctx, "tcp", net.JoinHostPort("plain.origo.svc", ln.port())); !strings.Contains(egressError(t, err).Cause, "not the host's pinned address") {
		t.Fatalf("unpinned exact host: %v", err)
	}
	if len(dial.addrs) != 0 || ln.conns.Load() != 0 {
		t.Fatalf("a refusal dialed: %v, %d connections", dial.addrs, ln.conns.Load())
	}
}

// TestEgressDialerAllowLoopbackIsATestSeam: with AllowLoopback set
// through the handler's constructor a listed host on 127.0.0.1 is
// dialed and an unlisted one is still refused; without it the same host
// is refused and the listener sees no connection.
func TestEgressDialerAllowLoopbackIsATestSeam(t *testing.T) {
	ln := newCountingListener(t)
	res := &fakeResolver{answers: map[string][]string{"localhost": {"127.0.0.1"}, "other.local": {"127.0.0.1"}}}
	base := NewEgress(EgressOptions{Allow: []string{"localhost"}})
	base.resolve = res.lookup
	ctx := context.Background()
	if _, err := base.DialContext(ctx, "tcp", "localhost:"+ln.port()); !strings.Contains(egressError(t, err).Cause, "loopback") || ln.conns.Load() != 0 {
		t.Fatalf("without the seam: %v, %d connections", err, ln.conns.Load())
	}
	h := newHarness(t, withEgress(base, true))
	e := h.handler.Egress()
	if e == base || !e.allowLoopback || base.allowLoopback {
		t.Fatal("the seam changed the shared dialer")
	}
	conn, err := e.DialContext(ctx, "tcp", "localhost:"+ln.port())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, err := e.DialContext(ctx, "tcp", "other.local:"+ln.port()); !strings.Contains(egressError(t, err).Cause, "not on ORIGO_EGRESS_ALLOW") {
		t.Fatalf("unlisted host with the seam: %v", err)
	}
	// A handler built without an Egress refuses everything.
	if _, err := newHarness(t).handler.Egress().Resolve(ctx, "localhost"); err == nil {
		t.Fatal("the default dialer admitted a host")
	}
}

// TestEgressDialerPinsTheResolvedAddress: a host whose name resolves to
// a public address on the check and to 127.0.0.1 on the next lookup is
// dialed at the first address and never reaches the loopback listener.
func TestEgressDialerPinsTheResolvedAddress(t *testing.T) {
	ln := newCountingListener(t)
	res := &fakeResolver{answers: map[string][]string{"rebind.example.com": {"93.184.216.34", "127.0.0.1"}}}
	dial := &recordingDial{}
	e := NewEgress(EgressOptions{Allow: []string{"rebind.example.com"}})
	e.resolve, e.dial = res.lookup, dial.dial
	ctx := context.Background()
	if _, err := e.DialContext(ctx, "tcp", "rebind.example.com:"+ln.port()); err == nil || err.Error() != "recording dialer" {
		t.Fatalf("first dial: %v", err)
	}
	if want := []string{"93.184.216.34:" + ln.port()}; !slices.Equal(dial.addrs, want) {
		t.Fatalf("dialed %v, want %v", dial.addrs, want)
	}
	// The next lookup answers loopback, which the rules refuse.
	if _, err := e.DialContext(ctx, "tcp", "rebind.example.com:"+ln.port()); !strings.Contains(egressError(t, err).Cause, "loopback") {
		t.Fatalf("rebound: %v", err)
	}
	if res.calls["rebind.example.com"] != 2 || len(dial.addrs) != 1 || ln.conns.Load() != 0 {
		t.Fatalf("lookups %d, dials %v, connections %d", res.calls["rebind.example.com"], dial.addrs, ln.conns.Load())
	}
}

// proxyClient sends one request through the proxy the way git does: an
// absolute URL with the credential of the proxy URL as Basic
// Proxy-Authorization.
func proxyClient(t *testing.T, p *Proxy) *http.Client {
	t.Helper()
	u, err := url.Parse(p.URL())
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

// TestEgressProxyFollowsRedirectsAndRefusesConnect: a source that
// answers 302 to a second listener is followed by the proxy through the
// dialer, a redirect to a refused address is refused with the egress
// error and opens no connection, a CONNECT answers 405 and opens no
// connection, a request without the credential answers 407, and a git
// ls-remote through the proxy at an http:// URL sends no CONNECT,
// asserted by the proxy's request log.
func TestEgressProxyFollowsRedirectsAndRefusesConnect(t *testing.T) {
	var secondHits atomic.Int64
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.Header().Set("X-Hop", "second")
		_, _ = io.WriteString(w, "hello from "+r.URL.Path+" "+r.Method)
	}))
	t.Cleanup(second.Close)
	refusedLn := newCountingListener(t)
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop":
			http.Redirect(w, r, second.URL+"/landed", http.StatusFound)
		case "/post":
			http.Redirect(w, r, second.URL+"/after-post", http.StatusSeeOther)
		case "/refused":
			http.Redirect(w, r, "https://meta.example.com:"+refusedLn.port()+"/", http.StatusFound)
		case "/plain":
			http.Redirect(w, r, "http://"+r.Host+"/x", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, first(r)+"/loop", http.StatusFound)
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(first.Close)
	roots := x509.NewCertPool()
	roots.AddCert(first.Certificate())
	roots.AddCert(second.Certificate())
	res := &fakeResolver{answers: map[string][]string{"127.0.0.1": {"127.0.0.1"}, "meta.example.com": {"169.254.169.254"}}}
	e := NewEgress(EgressOptions{Allow: []string{"127.0.0.1", "meta.example.com"}, Roots: roots}).withLoopback()
	e.resolve = res.lookup
	p, err := e.StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	client := proxyClient(t, p)
	via := func(u string) string { return "http://" + strings.TrimPrefix(u, "https://") }

	resp, err := client.Get(via(first.URL) + "/hop")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from /landed GET" || resp.Header.Get("X-Hop") != "second" || secondHits.Load() != 1 {
		t.Fatalf("redirect: %d %q %v, second hits %d", resp.StatusCode, body, resp.Header, secondHits.Load())
	}
	// A 303 on a POST is followed with a GET.
	resp, err = client.Post(via(first.URL)+"/post", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from /after-post GET" {
		t.Fatalf("303: %d %q", resp.StatusCode, body)
	}
	// A body reaches the source once, unchanged.
	resp, err = client.Post(via(first.URL)+"/echo", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "payload" {
		t.Fatalf("echo: %d %q", resp.StatusCode, body)
	}
	// A redirect to a refused address is refused: 403 to git, the
	// refusal recorded, the listener never reached.
	resp, err = client.Get(via(first.URL) + "/refused")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 || p.Refusal() == nil || refusedLn.conns.Load() != 0 {
		t.Fatalf("refused hop: %d, refusal %v, %d connections", resp.StatusCode, p.Refusal(), refusedLn.conns.Load())
	}
	if ee := egressError(t, p.Refusal()); ee.Host != "meta.example.com" || ee.Address != "169.254.169.254" {
		t.Fatalf("refusal %+v", ee)
	}
	// A redirect to a plain http URL is refused too, and the first
	// refusal is the one kept.
	resp, err = client.Get(via(first.URL) + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 || p.Refusal().Host != "meta.example.com" {
		t.Fatalf("plain hop: %d, refusal %v", resp.StatusCode, p.Refusal())
	}
	// More than ten hops is a bad gateway.
	resp, err = client.Get(via(first.URL) + "/loop")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("redirect loop: %d", resp.StatusCode)
	}
	// CONNECT is refused and opens nothing; a request without the
	// credential is 407; a relative request is 400.
	raw, err := net.Dial("tcp", p.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	target := "127.0.0.1:" + refusedLn.port()
	for _, req := range []string{
		"CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: Basic " + basic(p.credential) + "\r\n\r\n",
		"GET " + via(first.URL) + "/hop HTTP/1.1\r\nHost: " + first.Listener.Addr().String() + "\r\n\r\n",
		"GET /hop HTTP/1.1\r\nHost: " + first.Listener.Addr().String() + "\r\nProxy-Authorization: Basic " + basic(p.credential) + "\r\n\r\n",
	} {
		if _, err := io.WriteString(raw, req); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case strings.HasPrefix(req, "CONNECT") && resp.StatusCode != 405:
			t.Fatalf("CONNECT: %d", resp.StatusCode)
		case strings.Contains(req, "Proxy-Authorization") && strings.HasPrefix(req, "GET /") && resp.StatusCode != 400:
			t.Fatalf("relative request: %d", resp.StatusCode)
		case !strings.Contains(req, "Proxy-Authorization") && (resp.StatusCode != 407 || resp.Header.Get("Proxy-Authenticate") == ""):
			t.Fatalf("without the credential: %d %v", resp.StatusCode, resp.Header)
		}
	}
	if refusedLn.conns.Load() != 0 {
		t.Fatal("CONNECT opened a connection")
	}
	// git ls-remote through the proxy at an http:// URL of the source
	// stub of spec 013: the proxy terminates the stub's TLS, the bearer
	// travels in the environment, and the log shows GET and no CONNECT.
	stub := source.New(t)
	stubRoots := x509.NewCertPool()
	stubRoots.AppendCertsFromPEM(stub.CA())
	ge := NewEgress(EgressOptions{Allow: []string{"localhost"}, Roots: stubRoots}).withLoopback()
	gp, err := ge.StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gp.Close)
	stubURL, _ := url.Parse(stub.URL())
	stubURL.Host = "localhost:" + stubURL.Port()
	stubURL.Path = "/fixture.git"
	rewritten, env := gp.GitConfig(stubURL, stub.Token())
	if !strings.HasPrefix(rewritten, "http://localhost:") || env[0] != "GIT_CONFIG_COUNT=2" || !slices.Contains(env, "GIT_CONFIG_KEY_1=http.proxy") || !slices.Contains(env, "GIT_CONFIG_VALUE_1="+gp.URL()) {
		t.Fatalf("git config %q %q", rewritten, env)
	}
	if _, env := gp.GitConfig(stubURL, ""); env[0] != "GIT_CONFIG_COUNT=1" || env[1] != "GIT_CONFIG_KEY_0=http.proxy" {
		t.Fatalf("git config without a token: %q", env)
	}
	cmd := exec.CommandContext(context.Background(), "git", "-c", "transfer.fsckObjects=true", "ls-remote", "--end-of-options", rewritten)
	cmd.Env = append(gittest.Env(t.TempDir()), env...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "refs/heads/main") {
		t.Fatalf("git ls-remote: %v\n%s", err, out)
	}
	for _, r := range gp.Requests() {
		if r.Method == http.MethodConnect || !strings.HasPrefix(r.URL, "http://localhost:") {
			t.Fatalf("git sent %+v", r)
		}
	}
	if reqs := stub.Requests(); len(reqs) == 0 || !reqs[0].Bearer || gp.Refusal() != nil {
		t.Fatalf("stub saw %+v, refusal %v", reqs, gp.Refusal())
	}
	for _, s := range slices.Concat(cmd.Args, cmd.Env) {
		if strings.Contains(s, "https://") {
			t.Fatalf("the source's https URL reached git: %q", s)
		}
	}
}

// first returns the request's own origin as an https URL, for a redirect
// back to the same listener.
func first(r *http.Request) string { return "https://" + r.Host }

// basic is the Basic credential curl sends for the user of a proxy URL
// with no password.
func basic(user string) string { return base64.StdEncoding.EncodeToString([]byte(user + ":")) }

// moduleRoot is the checkout, resolved from this file with
// runtime.Caller and never from the working directory, so the gate that
// runs the suite from an empty directory finds the module.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// TestAllowLoopbackIsSetOnlyByTests is spec 016's criterion for the
// seam: the only writes of the field AllowLoopback in the module, a
// composite literal key or an assignment, are in _test.go files, so no
// deployment can admit a loopback source.
func TestAllowLoopbackIsSetOnlyByTests(t *testing.T) {
	root := moduleRoot(t)
	var writes []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "out" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && id.Name == "AllowLoopback" {
					writes = append(writes, fset.Position(x.Pos()).String())
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "AllowLoopback" {
						writes = append(writes, fset.Position(x.Pos()).String())
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Fatalf("AllowLoopback is written outside _test.go files:\n%s", strings.Join(writes, "\n"))
	}
	// The walk sees a write when there is one: this file's own harness
	// option writes the field.
	own := 0
	f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "api", "api_test.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "AllowLoopback" {
				own++
			}
		}
		return true
	})
	if own == 0 {
		t.Fatal("the walk found no write in the harness that sets the seam")
	}
}
