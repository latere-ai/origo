// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package source_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/source"
)

// gitClient runs the client git against the stub: the CA file trusted
// through GIT_SSL_CAINFO and the bearer as an extra header when token is
// not empty.
func gitClient(t *testing.T, s *source.Server, token string, args ...string) (string, error) {
	t.Helper()
	home := t.TempDir()
	ca := filepath.Join(home, "ca.pem")
	if err := os.WriteFile(ca, s.CA(), 0o600); err != nil {
		t.Fatal(err)
	}
	full := []string{"-c", "http.sslCAInfo=" + ca}
	if token != "" {
		full = append(full, "-c", "http.extraHeader=Authorization: Bearer "+token)
	}
	return gittest.Try(home, nil, append(full, args...)...)
}

func control(t *testing.T, s *source.Server, method, path, body string) (int, []byte) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(s.CA())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	req, _ := http.NewRequestWithContext(context.Background(), method, s.URL()+path, strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestSourceServesTheFixtureOverTLS is spec 013's criterion for the
// source: the fixture is cloned over TLS with the bearer and refused
// without it, Commit adds one commit the next ls-remote shows, Requests
// lists the clone without the bearer's value, and the bundle unpacks
// to FixtureCommits commits.
func TestSourceServesTheFixtureOverTLS(t *testing.T) {
	s := source.New(t)
	if s.Token() != source.DefaultToken || !strings.HasPrefix(s.URL(), "https://") {
		t.Fatalf("token %q url %q", s.Token(), s.URL())
	}
	work := filepath.Join(t.TempDir(), "clone")
	if out, err := gitClient(t, s, s.Token(), "clone", "-q", s.URL()+"/fixture.git", work); err != nil {
		t.Fatalf("clone with the bearer: %v\n%s", err, out)
	}
	if got := gittest.Run(t, work, nil, "rev-list", "--count", "HEAD"); got != "5000" {
		t.Fatalf("%s commits, want 5000", got)
	}
	// git answers a 401 by asking for credentials, which the hermetic
	// environment refuses.
	if out, err := gitClient(t, s, "", "clone", "-q", s.URL()+"/fixture.git", filepath.Join(t.TempDir(), "x")); err == nil || !strings.Contains(err.Error(), "Username") {
		t.Fatalf("clone without the bearer: %v\n%s", err, out)
	}
	if out, err := gitClient(t, s, "wrong", "ls-remote", s.URL()+"/fixture.git"); err == nil {
		t.Fatalf("wrong bearer: %s", out)
	}
	before, _ := gitClient(t, s, s.Token(), "ls-remote", s.URL()+"/fixture.git", "refs/heads/main")
	commit, err := s.Commit("fixture", "main")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := gitClient(t, s, s.Token(), "ls-remote", s.URL()+"/fixture.git", "refs/heads/main")
	if !strings.HasPrefix(after, commit) || after == before {
		t.Fatalf("ls-remote after Commit: %q, commit %s", after, commit)
	}
	gittest.Run(t, work, nil, "-c", "http.extraHeader=Authorization: Bearer "+s.Token(), "-c", "http.sslVerify=false", "pull", "-q")
	if got := gittest.Run(t, work, nil, "rev-list", "--count", "HEAD"); got != "5001" {
		t.Fatalf("%s commits after the late write", got)
	}
	if _, err := s.Commit("fixture", "nope"); err == nil || !strings.Contains(err.Error(), "no branch") {
		t.Fatalf("Commit on a missing branch: %v", err)
	}
	if _, err := s.Commit("nope", "main"); err == nil {
		t.Fatal("Commit on a missing repository")
	}
	if _, err := s.Commit("../x", "main"); err == nil {
		t.Fatal("Commit on an invalid name")
	}
	// Every git request is listed with whether it carried the bearer and
	// never with the bearer's value.
	reqs := s.Requests()
	raw, _ := json.Marshal(reqs)
	if strings.Contains(string(raw), s.Token()) {
		t.Fatal("the request list carries the bearer")
	}
	var withBearer, without int
	for _, r := range reqs {
		if !strings.HasPrefix(r.Path, "/fixture.git/") {
			t.Fatalf("path %q", r.Path)
		}
		if r.Bearer {
			withBearer++
		} else {
			without++
		}
	}
	if withBearer == 0 || without < 2 {
		t.Fatalf("requests: %d with the bearer, %d without", withBearer, without)
	}
	status, listed := control(t, s, "GET", "/requests", "")
	var decoded []source.Request
	if err := json.Unmarshal(listed, &decoded); err != nil || status != 200 || len(decoded) != len(reqs) {
		t.Fatalf("GET /requests: %d %v %d", status, err, len(decoded))
	}
	if status, _ := control(t, s, "DELETE", "/requests", ""); status != 204 || len(s.Requests()) != 0 {
		t.Fatal("DELETE /requests")
	}
}

func TestControlAPIAddsRepositoriesAndCommits(t *testing.T) {
	s := source.New(t)
	if status, raw := control(t, s, "GET", "/ca.pem", ""); status != 200 || string(raw) != string(s.CA()) {
		t.Fatalf("GET /ca.pem: %d", status)
	}
	// A repository of the test's own, as a bundle over the control API.
	src := gittest.NewSource(t)
	src.Commit("a.txt", "one", "first")
	bundle := filepath.Join(t.TempDir(), "r.bundle")
	gittest.Run(t, src.Dir, nil, "bundle", "create", "-q", bundle, "refs/heads/main")
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"name": "other", "bundle": base64.StdEncoding.EncodeToString(raw)})
	if status, out := control(t, s, "POST", "/repos", string(body)); status != 201 {
		t.Fatalf("POST /repos: %d %s", status, out)
	}
	if status, _ := control(t, s, "POST", "/repos", string(body)); status != 400 {
		t.Fatal("a second POST of the same name")
	}
	if status, _ := control(t, s, "POST", "/repos", `{"name":"bad name","bundle":""}`); status != 400 {
		t.Fatal("an invalid name")
	}
	if status, _ := control(t, s, "POST", "/repos", `{"name":"b","bundle":"%%%"}`); status != 400 {
		t.Fatal("a bundle that is not base64")
	}
	if status, _ := control(t, s, "POST", "/repos", `{"name":"b","bundle":"bm90IGEgYnVuZGxl"}`); status != 400 {
		t.Fatal("bytes that are not a bundle")
	}
	if status, _ := control(t, s, "POST", "/repos", `nope`); status != 400 {
		t.Fatal("malformed body")
	}
	if err := s.AddRepo("other", raw); err == nil {
		t.Fatal("AddRepo of an existing name")
	}
	work := filepath.Join(t.TempDir(), "other")
	if out, err := gitClient(t, s, s.Token(), "clone", "-q", s.URL()+"/other.git", work); err != nil {
		t.Fatalf("clone the added repository: %v\n%s", err, out)
	}
	if got := gittest.Run(t, work, nil, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("%s commits", got)
	}
	status, out := control(t, s, "POST", "/commit", `{"repo":"other"}`)
	var minted struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(out, &minted); err != nil || status != 200 || len(minted.Commit) != 40 {
		t.Fatalf("POST /commit: %d %s", status, out)
	}
	if got, _ := gitClient(t, s, s.Token(), "ls-remote", s.URL()+"/other.git", "refs/heads/main"); !strings.HasPrefix(got, minted.Commit) {
		t.Fatalf("ls-remote after POST /commit: %q", got)
	}
	if status, _ := control(t, s, "POST", "/commit", `{"repo":"other","branch":"nope"}`); status != 400 {
		t.Fatal("POST /commit on a missing branch")
	}
	if status, _ := control(t, s, "POST", "/commit", `nope`); status != 400 {
		t.Fatal("malformed POST /commit")
	}
	// Paths outside the git and control routes.
	if status, _ := control(t, s, "GET", "/nope", ""); status != 404 {
		t.Fatal("unknown path")
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", s.URL()+"/missing.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the test trusts its own stub
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing repository: %d", resp.StatusCode)
	}
}

// newCA generates a CA the way up.sh does with openssl: a self-signed
// certificate and its key, the key in the SEC 1 form when pkcs8 is false
// and in the PKCS #8 form otherwise.
func newCA(t *testing.T, pkcs8 bool) (cert, key []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	var keyDER []byte
	typ := "EC PRIVATE KEY"
	if pkcs8 {
		keyDER, err = x509.MarshalPKCS8PrivateKey(priv)
		typ = "PRIVATE KEY"
	} else {
		keyDER, err = x509.MarshalECPrivateKey(priv)
	}
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: keyDER})
}

func TestOptionsAndErrors(t *testing.T) {
	// A CA of one's own, in the SEC 1 form openssl ecparam writes or in
	// PKCS #8, signs the serving certificate, and the stub serves the CA
	// as given.
	for _, pkcs8 := range []bool{false, true} {
		cert, key := newCA(t, pkcs8)
		s := source.New(t, source.WithToken("t"), source.WithCA(cert, key))
		if s.Token() != "t" || string(s.CA()) != string(cert) {
			t.Fatal("WithToken or WithCA")
		}
		if out, err := gitClient(t, s, "t", "ls-remote", s.URL()+"/fixture.git"); err != nil {
			t.Fatalf("clone under the given CA (pkcs8 %v): %v\n%s", pkcs8, err, out)
		}
	}
	first := source.New(t)
	if _, err := source.NewHandler(t.TempDir(), source.WithCA([]byte("nope"), nil)); err == nil {
		t.Fatal("a CA that is not PEM")
	}
	if _, err := source.NewHandler(t.TempDir(), source.WithCA(first.CA(), []byte("nope"))); err == nil {
		t.Fatal("a key that is not PEM")
	}
	rsaKey, _ := x509.MarshalPKCS8PrivateKey(mustRSA(t))
	if _, err := source.NewHandler(t.TempDir(), source.WithCA(first.CA(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaKey}))); err == nil {
		t.Fatal("a key that is not EC")
	}
	if _, err := source.NewHandler(t.TempDir(), source.WithCA(first.CA(), []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"))); err == nil {
		t.Fatal("a key that does not parse")
	}
	cert, _ := newCA(t, false)
	_, otherKey := newCA(t, false)
	if _, err := source.NewHandler(t.TempDir(), source.WithCA(cert, otherKey)); err == nil {
		t.Fatal("a key that is not the certificate's")
	}
	if _, err := source.NewHandler(t.TempDir(), source.WithCA([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), nil)); err == nil {
		t.Fatal("a certificate that does not parse")
	}
	if _, err := source.NewHandler(t.TempDir(), source.WithGit(filepath.Join(t.TempDir(), "nogit"))); err == nil {
		t.Fatal("a git that does not exist")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.NewHandler(filepath.Join(file, "x")); err == nil {
		t.Fatal("a root under a file")
	}
	h, err := source.NewHandler(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if h.Handler() == nil || h.TLSConfig() == nil || len(h.TLSConfig().Certificates) != 1 {
		t.Fatal("handler")
	}
	h.Close()
}

// mustRSA returns an RSA key, the one kind parseECKey refuses.
func mustRSA(t *testing.T) any {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
