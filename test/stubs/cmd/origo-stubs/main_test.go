// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/stubs/sink"
	"github.com/latere-ai/origo/test/stubs/source"
)

// lockedBuffer is a bytes.Buffer run writes to from its goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var listening = regexp.MustCompile(`(?m)^([a-z ]+) listening on (\S+)$`)

// start runs the binary with the arguments in the background and returns
// the address of each listener by name once every wanted name has
// printed its line, and a stop that ends the run and returns its exit
// code.
func start(t *testing.T, args []string, want ...string) (map[string]string, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	done := make(chan int, 1)
	go func() { done <- run(ctx, args, stdout, stderr) }()
	stop := func() int {
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("run did not return")
			return -1
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		addrs := map[string]string{}
		for _, m := range listening.FindAllStringSubmatch(stdout.String(), -1) {
			addrs[m[1]] = m[2]
		}
		complete := true
		for _, name := range want {
			if addrs[name] == "" {
				complete = false
			}
		}
		if complete {
			return addrs, stop
		}
		select {
		case code := <-done:
			t.Fatalf("run exited %d before every listener was up\n%s", code, stderr.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listeners not up: %s\n%s", stdout.String(), stderr.String())
	return nil, nil
}

func do(t *testing.T, client *http.Client, method, url, bearer, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

var loopback = []string{
	"-issuer-listen", "127.0.0.1:0", "-authorizer-listen", "127.0.0.1:0", "-sink-listen", "127.0.0.1:0",
	"-source-listen", "127.0.0.1:0", "-slowproxy-listen", "127.0.0.1:0", "-slowproxy-data", "127.0.0.1:0",
}

func TestRunsEveryStubFromFlags(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("minio")) }))
	defer target.Close()
	args := append(loopback, "-authorizer-token", "t", "-source-token", "s", "-issuer-url", "http://issuer.example/",
		"-allow", "dev, ops", "-secret", "k", "-slowproxy-target", strings.TrimPrefix(target.URL, "http://"))
	addrs, stop := start(t, args, "issuer", "authorizer", "sink", "source", "slowproxy", "slowproxy data")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the test trusts its own stub

	// The issuer carries the issuer URL, not its listen address.
	_, raw := do(t, client, "GET", "http://"+addrs["issuer"]+"/.well-known/openid-configuration", "", "")
	var disc map[string]any
	_ = json.Unmarshal(raw, &disc)
	if disc["issuer"] != "http://issuer.example" {
		t.Fatalf("discovery: %s", raw)
	}
	if status, _ := do(t, client, "POST", "http://"+addrs["issuer"]+"/mint", "", `{"sub":"dev"}`); status != 200 {
		t.Fatalf("mint: %d", status)
	}
	// The authorizer requires the bearer and allows the listed subjects.
	if status, _ := do(t, client, "POST", "http://"+addrs["authorizer"], "wrong", `{"subject":"dev","action":"read"}`); status != 401 {
		t.Fatalf("authorizer bearer: %d", status)
	}
	for sub, want := range map[string]string{"dev": `"allow":true`, "ops": `"allow":true`, "eve": `"allow":false`} {
		if _, raw := do(t, client, "POST", "http://"+addrs["authorizer"], "t", `{"subject":"`+sub+`","action":"read"}`); !strings.Contains(string(raw), want) {
			t.Fatalf("authorizer %s: %s", sub, raw)
		}
	}
	// The sink verifies with the secret.
	body := `{"id":"e1","kind":"push","repo":"r"}`
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://"+addrs["sink"], strings.NewReader(body))
	req.Header.Set(sink.HeaderSignature, sink.Sign("k", []byte(body)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("sink: %d", resp.StatusCode)
	}
	if status, raw := do(t, client, "GET", "http://"+addrs["sink"]+"/deliveries?kind=push", "", ""); status != 200 || !strings.Contains(string(raw), `"e1"`) {
		t.Fatalf("deliveries: %d %s", status, raw)
	}
	// The source serves TLS with a certificate for the stack's names and
	// requires its own bearer.
	status, ca := do(t, client, "GET", "https://"+addrs["source"]+"/ca.pem", "", "")
	if status != 200 || !bytes.Contains(ca, []byte("BEGIN CERTIFICATE")) {
		t.Fatalf("ca.pem: %d", status)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: source.SANs[0], MinVersion: tls.VersionTLS12}}}
	if status, _ := do(t, trusting, "GET", "https://"+addrs["source"]+"/fixture.git/info/refs?service=git-upload-pack", "", ""); status != 401 {
		t.Fatalf("source without the bearer: %d", status)
	}
	if status, raw := do(t, trusting, "GET", "https://"+addrs["source"]+"/fixture.git/info/refs?service=git-upload-pack", "s", ""); status != 200 || !bytes.Contains(raw, []byte("refs/heads/main")) {
		t.Fatalf("source with the bearer: %d %s", status, raw)
	}
	// The slow proxy reports its target on the control endpoint and
	// forwards bytes on the data one.
	if status, raw := do(t, client, "GET", "http://"+addrs["slowproxy"]+"/", "", ""); status != 200 || !strings.Contains(string(raw), `"delay":"0s"`) {
		t.Fatalf("slowproxy control: %d %s", status, raw)
	}
	if status, raw := do(t, client, "GET", "http://"+addrs["slowproxy data"]+"/", "", ""); status != 200 || string(raw) != "minio" {
		t.Fatalf("slowproxy data: %d %s", status, raw)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestSourceAndProxyStartOnlyWhenAsked(t *testing.T) {
	addrs, stop := start(t, append(loopback, "-authorizer-token", "t"), "issuer", "authorizer", "sink")
	if addrs["source"] != "" || addrs["slowproxy"] != "" {
		t.Fatalf("started without a token or a target: %v", addrs)
	}
	// The default issuer URL is the listen address.
	_, raw := do(t, http.DefaultClient, "GET", "http://"+addrs["issuer"]+"/.well-known/openid-configuration", "", "")
	if !strings.Contains(string(raw), `"issuer":"http://127.0.0.1:`) {
		t.Fatalf("default issuer: %s", raw)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestKeyFailAndCAFiles(t *testing.T) {
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(key)
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	// A CA the way up.sh makes one, taken from a source stub of the test.
	src := source.New(t)
	caFile, caKeyFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if err := os.WriteFile(caFile, src.CA(), 0o600); err != nil {
		t.Fatal(err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caKeyDER, _ := x509.MarshalECPrivateKey(caKey)
	if err := os.WriteFile(caKeyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	// The CA's key is not the one that signed the certificate, which the
	// source refuses at start: exit 1.
	var stderr bytes.Buffer
	if code := run(context.Background(), append(loopback, "-authorizer-token", "t", "-source-token", "s", "-ca", caFile, "-ca-key", caKeyFile), io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "source") {
		t.Fatalf("mismatched CA: %d %s", code, stderr.String())
	}
	// A matching pair: the served chain verifies against the CA file.
	ownCA, ownKey := selfSignedCA(t)
	if err := os.WriteFile(caFile, ownCA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKeyFile, ownKey, 0o600); err != nil {
		t.Fatal(err)
	}
	addrs, stop := start(t, append(loopback, "-authorizer-token", "t", "-source-token", "s", "-ca", caFile, "-ca-key", caKeyFile, "-key", keyFile, "-fail", "503", "-hang"), "issuer", "authorizer", "source")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ownCA)
	conn, err := tls.Dial("tcp", addrs["source"], &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("the served certificate is not signed by the CA file: %v", err)
	}
	conn.Close()
	_, raw := do(t, http.DefaultClient, "GET", "http://"+addrs["issuer"]+"/jwks", "", "")
	if !strings.Contains(string(raw), `"kid"`) {
		t.Fatalf("jwks: %s", raw)
	}
	// -hang wins over -fail: the authorizer never answers.
	client := &http.Client{Timeout: 200 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://"+addrs["authorizer"], strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer t")
	if _, err := client.Do(req); err == nil {
		t.Fatal("a hung authorizer answered")
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	addrs, stop = start(t, append(loopback, "-authorizer-token", "t", "-fail", "503"), "authorizer")
	if status, _ := do(t, http.DefaultClient, "POST", "http://"+addrs["authorizer"], "t", `{}`); status != 503 {
		t.Fatalf("-fail: %d", status)
	}
	stop()
}

// selfSignedCA is a CA in the form up.sh writes with openssl.
func selfSignedCA(t *testing.T) (cert, key []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: bigOne(), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestUsageAndStartFailures(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		text string
	}{
		{"bad flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"help", []string{"-h"}, 2, "Usage"},
		{"no token", loopback, 2, "-authorizer-token is required"},
		{"ca alone", append(loopback, "-authorizer-token", "t", "-ca", "x"), 2, "go together"},
		{"key unreadable", append(loopback, "-authorizer-token", "t", "-key", filepath.Join(t.TempDir(), "nope")), 1, "-key"},
		{"key malformed", append(loopback, "-authorizer-token", "t", "-key", writeFile(t, "nope")), 1, "-key"},
		{"ca unreadable", append(loopback, "-authorizer-token", "t", "-source-token", "s", "-ca", filepath.Join(t.TempDir(), "nope"), "-ca-key", writeFile(t, "x")), 1, "-ca"},
		{"ca key unreadable", append(loopback, "-authorizer-token", "t", "-source-token", "s", "-ca", writeFile(t, "x"), "-ca-key", filepath.Join(t.TempDir(), "nope")), 1, "-ca-key"},
		{"ca malformed", append(loopback, "-authorizer-token", "t", "-source-token", "s", "-ca", writeFile(t, "x"), "-ca-key", writeFile(t, "y")), 1, "source"},
	}
	for _, c := range cases {
		var stderr bytes.Buffer
		if code := run(context.Background(), c.args, io.Discard, &stderr); code != c.code || !strings.Contains(stderr.String(), c.text) {
			t.Errorf("%s: exit %d, want %d; stderr %q", c.name, code, c.code, stderr.String())
		}
	}
	// A listen address that is taken fails the start, for a stub and for
	// the proxy's data listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().String()
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-issuer-listen", taken, "-authorizer-listen", "127.0.0.1:0", "-sink-listen", "127.0.0.1:0", "-authorizer-token", "t"}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "issuer") {
		t.Fatalf("taken issuer port: %d %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run(context.Background(), append(loopback[:6], "-slowproxy-listen", "127.0.0.1:0", "-slowproxy-data", taken, "-slowproxy-target", "127.0.0.1:1", "-authorizer-token", "t"), io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "slowproxy data") {
		t.Fatalf("taken data port: %d %s", code, stderr.String())
	}
}

func TestProxyEndsWithAnUnreachableTargetAndOnStop(t *testing.T) {
	addrs, stop := start(t, append(loopback, "-authorizer-token", "t", "-slowproxy-target", "127.0.0.1:1"), "slowproxy data")
	conn, err := net.Dial("tcp", addrs["slowproxy data"])
	if err != nil {
		t.Fatal(err)
	}
	// The upstream refuses, so the proxy closes the client side.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("read from a proxy with no upstream")
	}
	conn.Close()
	stop()
	// A forwarded connection open at stop is closed by the proxy.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c) }()
		}
	}()
	addrs, stop = start(t, append(loopback, "-authorizer-token", "t", "-slowproxy-target", target.Addr().String()), "slowproxy data")
	conn, err = net.Dial("tcp", addrs["slowproxy data"])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through the proxy: %q %v", buf, err)
	}
	stop()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("the connection outlived the proxy")
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func bigOne() *big.Int { return big.NewInt(1) }
