// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package source is the stub git source of spec 013: git http-backend
// behind a TLS listener that requires a bearer, serving the fixture
// repository /fixture.git unpacked at start from the bundle the package
// embeds and any repository a test adds, for the import of spec 019 and
// the verify of spec 014. The certificate is signed by a CA generated at
// start or given with WithCA, served at /ca.pem, so a client trusts the
// stub through the file.
package source

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

//go:generate go test -tags generate -run TestGenerateFixture -count=1 .

// fixtureBundle is the fixture repository: FixtureCommits commits on
// main, produced by TestGenerateFixture under go generate and checked in.
//
//go:embed testdata/fixture.bundle
var fixtureBundle []byte

// FixtureCommits is how many commits /fixture.git holds at start.
const FixtureCommits = 5000

// DefaultToken is the bearer the stub requires unless WithToken sets
// another; the kind overlay's test-source component uses it.
const DefaultToken = "stub-source-token"

// SANs are the names the serving certificate carries: the stubs'
// Service inside the cluster and the host port outside it.
var SANs = []string{"origo-stubs.origo.svc", "localhost"}

// Request is one git request the stub saw: its path and whether it
// carried the bearer, never the bearer's value.
type Request struct {
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Bearer bool      `json:"bearer"`
	At     time.Time `json:"at"`
}

// Option configures a Server.
type Option func(*Server)

// WithToken sets the bearer the git endpoints require.
func WithToken(token string) Option {
	return func(s *Server) { s.token = token }
}

// WithCA uses the given CA certificate and key, both PEM, to sign the
// serving certificate instead of generating a CA.
func WithCA(cert, key []byte) Option {
	return func(s *Server) { s.caPEM, s.caKeyPEM = cert, key }
}

// WithGit sets the git binary, "git" by default.
func WithGit(bin string) Option {
	return func(s *Server) { s.git = bin }
}

// Server is one stub source.
type Server struct {
	mu       sync.Mutex
	root     string
	token    string
	git      string
	caPEM    []byte
	caKeyPEM []byte
	tlsCfg   *tls.Config
	requests []Request
	late     int
	mux      *http.ServeMux
	backend  http.Handler
	srv      *httptest.Server
}

// New starts a stub source for the test over TLS, its repositories under
// a temporary directory, and stops it with the test.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s, err := NewHandler(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewUnstartedServer(s.mux)
	s.srv.TLS = s.TLSConfig()
	s.srv.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// NewHandler builds a stub without a listener, its repositories under
// root, for a binary that serves Handler under TLSConfig itself. The
// fixture is unpacked into root at once.
func NewHandler(root string, opts ...Option) (*Server, error) {
	s := &Server{root: root, token: DefaultToken, git: "git", mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	git, err := exec.LookPath(s.git)
	if err != nil {
		return nil, err
	}
	s.git = git
	if err := s.setupTLS(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if err := s.AddRepo("fixture", fixtureBundle); err != nil {
		return nil, fmt.Errorf("fixture: %w", err)
	}
	s.backend = &cgi.Handler{
		Path: s.git, Args: []string{"http-backend"}, Dir: root,
		Env: append(gitEnv(root), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1"),
	}
	s.mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write(s.caPEM)
	})
	s.mux.HandleFunc("POST /commit", s.postCommit)
	s.mux.HandleFunc("POST /repos", s.postRepos)
	s.mux.HandleFunc("GET /requests", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Requests()) })
	s.mux.HandleFunc("DELETE /requests", func(w http.ResponseWriter, _ *http.Request) { s.ClearRequests(); w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("/", s.serveGit)
	return s, nil
}

// Handler serves the git endpoints and the control API.
func (s *Server) Handler() http.Handler { return s.mux }

// TLSConfig is the server configuration carrying the certificate the CA
// signed, for a listener of one's own.
func (s *Server) TLSConfig() *tls.Config { return s.tlsCfg.Clone() }

// Close stops the listener.
func (s *Server) Close() {
	if s.srv != nil {
		s.srv.Close()
	}
}

// URL is the base URL; /fixture.git under it is the fixture.
func (s *Server) URL() string { return s.srv.URL }

// Token is the bearer the git endpoints require.
func (s *Server) Token() string { return s.token }

// CA is the PEM certificate of the CA that signed the serving
// certificate, what /ca.pem serves.
func (s *Server) CA() []byte { return slices.Clone(s.caPEM) }

// Requests lists every git request seen, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ClearRequests empties the list.
func (s *Server) ClearRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// AddRepo unpacks a bundle as the bare repository <name>.git.
func (s *Server) AddRepo(name string, bundle []byte) error {
	if !repoName.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid repository name %q", name)
	}
	dir := filepath.Join(s.root, name+".git")
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("repository %s exists", name)
	}
	tmp, err := os.CreateTemp(s.root, ".bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bundle); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := s.run(s.root, nil, "clone", "-q", "--bare", tmp.Name(), dir); err != nil {
		return err
	}
	if _, err := s.run(dir, nil, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	return nil
}

// Commit adds one commit on the branch of the repository, the late
// write of spec 014's cut-over test, and returns its id. The branch
// must exist.
func (s *Server) Commit(repo, branch string) (string, error) {
	if !repoName.MatchString(repo) {
		return "", fmt.Errorf("invalid repository name %q", repo)
	}
	dir := filepath.Join(s.root, repo+".git")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("no repository %s", repo)
	}
	s.mu.Lock()
	s.late++
	n := s.late
	s.mu.Unlock()
	tip, err := s.run(dir, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("no branch %s on %s", branch, repo)
	}
	blob, err := s.run(dir, fmt.Appendf(nil, "late write %d\n", n), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	index := filepath.Join(s.root, ".index-"+strconv.Itoa(n))
	defer os.Remove(index)
	indexEnv := "GIT_INDEX_FILE=" + index
	if _, err := s.run(dir, nil, "-c", "core.bare=true", "read-tree", tip, "--", indexEnv); err != nil {
		return "", err
	}
	if _, err := s.run(dir, nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+",late-writes.txt", "--", indexEnv); err != nil {
		return "", err
	}
	tree, err := s.run(dir, nil, "write-tree", "--", indexEnv)
	if err != nil {
		return "", err
	}
	commit, err := s.run(dir, nil, "commit-tree", tree, "-p", tip, "-m", "late write "+strconv.Itoa(n))
	if err != nil {
		return "", err
	}
	if _, err := s.run(dir, nil, "update-ref", "refs/heads/"+branch, commit, tip); err != nil {
		return "", err
	}
	return commit, nil
}

// run runs git in dir. An argument after "--" that starts with
// GIT_INDEX_FILE= is an environment entry, which is how Commit gives a
// plumbing command its own index.
func (s *Server) run(dir string, stdin []byte, args ...string) (string, error) {
	env := gitEnv(s.root)
	if i := slices.Index(args, "--"); i >= 0 && i+1 < len(args) && strings.HasPrefix(args[i+1], "GIT_INDEX_FILE=") {
		env = append(env, args[i+1:]...)
		args = args[:i]
	}
	cmd := exec.CommandContext(context.Background(), s.git, args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// gitEnv is the environment every git invocation runs with: no user or
// system configuration, no prompts, a fixed identity.
func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Origo Stub Source", "GIT_AUTHOR_EMAIL=source@example.com",
		"GIT_COMMITTER_NAME=Origo Stub Source", "GIT_COMMITTER_EMAIL=source@example.com",
		"LC_ALL=C",
	}
}

var gitPath = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9._-]*)\.git(/.*)?$`)

// serveGit is every path under /<name>.git/: the bearer is required,
// the request is recorded, and git http-backend answers.
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request) {
	m := gitPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	bearer := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(raw)), []byte(s.token)) == 1
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Bearer: bearer, At: time.Now()})
	s.mu.Unlock()
	if !bearer {
		w.Header().Set("WWW-Authenticate", `Bearer realm="origo-stubs"`)
		http.Error(w, "bearer required", http.StatusUnauthorized)
		return
	}
	if _, err := os.Stat(filepath.Join(s.root, m[1]+".git")); err != nil {
		http.NotFound(w, r)
		return
	}
	s.backend.ServeHTTP(w, r)
}

func (s *Server) postCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Branch == "" {
		body.Branch = "main"
	}
	commit, err := s.Commit(body.Repo, body.Branch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"commit": commit})
}

func (s *Server) postRepos(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Bundle string `json:"bundle"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bundle, err := base64.StdEncoding.DecodeString(body.Bundle)
	if err != nil {
		http.Error(w, "bundle must be base64: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.AddRepo(body.Name, bundle); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// setupTLS reads or generates the CA and signs the serving certificate
// with it.
func (s *Server) setupTLS() error {
	var caCert *x509.Certificate
	var caKey *ecdsa.PrivateKey
	if s.caPEM != nil || s.caKeyPEM != nil {
		block, _ := pem.Decode(s.caPEM)
		if block == nil {
			return errors.New("ca: no PEM certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("ca: %w", err)
		}
		keyBlock, _ := pem.Decode(s.caKeyPEM)
		if keyBlock == nil {
			return errors.New("ca key: no PEM key")
		}
		key, err := parseECKey(keyBlock)
		if err != nil {
			return fmt.Errorf("ca key: %w", err)
		}
		caCert, caKey = cert, key
	} else {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "origo-stubs CA"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
			DNSNames: SANs,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			return err
		}
		caCert, caKey = tmpl, key
		caCert.Raw = der
		s.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return err
		}
		s.caKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: SANs[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: SANs, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("sign the serving certificate: %w", err)
	}
	s.tlsCfg = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der, caCert.Raw}, PrivateKey: leafKey}},
		MinVersion:   tls.VersionTLS12,
	}
	return nil
}

// parseECKey reads an EC key in the SEC 1 form openssl ecparam writes or
// the PKCS #8 form.
func parseECKey(block *pem.Block) (*ecdsa.PrivateKey, error) {
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an EC key")
	}
	return ec, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
