// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package issuer is the stub OIDC issuer of spec 013: a discovery
// document, a JWKS, and a control API that mints any token, so a test
// produces the token for each row of spec 007's verification table by
// setting one field wrong. Spec 007 builds it because its own criteria
// need it; spec 013's binary runs it beside the other stubs.
//
// It serves plain HTTP. A node that reaches it on a host other than a
// loopback address lists it in ORIGO_OIDC_INSECURE_ISSUERS.
package issuer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Claims is a mint request: every field is optional and the default of
// each is a value the verifier accepts, so a test sets exactly the one it
// wants refused. Exp, Nbf, and Iat are Unix seconds; zero means the
// default (one hour ahead, absent, now). Kid names a key of the set, or
// any string for a token no key set holds. Alg is written into the
// header as given; a value other than the key's algorithm is signed with
// the key all the same, which is how a test produces an unsupported alg.
type Claims struct {
	Sub string     `json:"sub,omitempty"`
	Act string     `json:"act,omitempty"`
	Aud StringList `json:"aud,omitempty"`
	Exp int64      `json:"exp,omitempty"`
	Nbf int64      `json:"nbf,omitempty"`
	Iat int64      `json:"iat,omitempty"`
	Kid string     `json:"kid,omitempty"`
	Alg string     `json:"alg,omitempty"`
}

// StringList decodes a JSON string or a list of strings, the two forms
// of aud.
type StringList []string

// UnmarshalJSON accepts both forms.
func (s *StringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// Defaults of a mint request.
const (
	DefaultSubject  = "dev"
	DefaultAudience = "origo"
	DefaultLifetime = time.Hour
)

// Option configures a Server.
type Option func(*Server)

// WithIssuer fixes the issuer URL the discovery document and every token
// carry. A stub behind a Service is reached at one URL by the node and
// another by the runner; the issuer is whichever the node is configured
// with. The default is the server's own address.
func WithIssuer(url string) Option {
	return func(s *Server) { s.issuer = strings.TrimRight(url, "/") }
}

// WithKey uses the given ES256 key instead of generating one.
func WithKey(key *ecdsa.PrivateKey) Option {
	return func(s *Server) { s.keys = []signingKey{newECKey(key)} }
}

// WithRS256 generates an RS256 key instead of an ES256 one, so a test
// covers both algorithms the verifier accepts.
func WithRS256() Option {
	return func(s *Server) { s.rs256 = true }
}

// WithClock substitutes the clock the defaults are taken from.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// Server is one stub issuer.
type Server struct {
	mu     sync.Mutex
	issuer string
	keys   []signingKey
	rs256  bool
	now    func() time.Time
	hung   chan struct{}
	closed chan struct{}
	srv    *httptest.Server
	mux    *http.ServeMux
}

type signingKey struct {
	kid string
	alg string
	ec  *ecdsa.PrivateKey
	rsa *rsa.PrivateKey
}

// New starts a stub issuer for the test and stops it with the test.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := NewHandler(opts...)
	s.srv = httptest.NewServer(s.mux)
	if s.issuer == "" {
		s.issuer = s.srv.URL
	}
	t.Cleanup(s.Close)
	return s
}

// NewHandler builds a stub without a listener, for a binary that serves
// Handler itself. WithIssuer is required then, because the stub cannot
// know its own address.
func NewHandler(opts ...Option) *Server {
	s := &Server{now: time.Now, closed: make(chan struct{}), mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	if len(s.keys) == 0 {
		s.keys = []signingKey{s.generate()}
	}
	s.mux.HandleFunc("GET /.well-known/openid-configuration", s.discovery)
	s.mux.HandleFunc("GET /jwks", s.jwks)
	s.mux.HandleFunc("POST /mint", s.mint)
	s.mux.HandleFunc("POST /rotate", func(w http.ResponseWriter, _ *http.Request) { s.Rotate(); w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("POST /hang", func(w http.ResponseWriter, _ *http.Request) { s.Hang(); w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("POST /resume", func(w http.ResponseWriter, _ *http.Request) { s.Resume(); w.WriteHeader(http.StatusNoContent) })
	return s
}

// Handler serves the discovery, the JWKS, and the control API.
func (s *Server) Handler() http.Handler { return s.mux }

// Close stops the listener and releases every hung request.
func (s *Server) Close() {
	s.mu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	s.mu.Unlock()
	if s.srv != nil {
		s.srv.Close()
	}
}

// URL is the issuer: the iss claim of every token and the value a node
// lists in ORIGO_OIDC_ISSUERS.
func (s *Server) URL() string { return s.issuer }

// KID is the kid of the current signing key.
func (s *Server) KID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[len(s.keys)-1].kid
}

// Rotate adds a key and drops the oldest, so a token signed before the
// call names a kid the key set no longer holds.
func (s *Server) Rotate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, s.generate())
	s.keys = s.keys[1:]
}

// Hang makes discovery and the JWKS never answer until Resume or Close,
// the issuer_unavailable case of spec 007. Minting keeps working.
func (s *Server) Hang() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hung == nil {
		s.hung = make(chan struct{})
	}
}

// Resume lets discovery and the JWKS answer again and releases the
// requests Hang held.
func (s *Server) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hung != nil {
		close(s.hung)
		s.hung = nil
	}
}

// Mint signs a token with the claims, defaults applied.
func (s *Server) Mint(c Claims) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key := s.keys[len(s.keys)-1]
	if c.Sub == "" {
		c.Sub = DefaultSubject
	}
	if len(c.Aud) == 0 {
		c.Aud = StringList{DefaultAudience}
	}
	if c.Iat == 0 {
		c.Iat = now.Unix()
	}
	if c.Exp == 0 {
		c.Exp = now.Add(DefaultLifetime).Unix()
	}
	if c.Kid == "" {
		c.Kid = key.kid
	}
	if c.Alg == "" {
		c.Alg = key.alg
	}
	claims := map[string]any{"iss": s.issuer, "sub": c.Sub, "aud": []string(c.Aud), "exp": c.Exp, "iat": c.Iat}
	if c.Act != "" {
		claims["act"] = c.Act
	}
	if c.Nbf != 0 {
		claims["nbf"] = c.Nbf
	}
	header, err := json.Marshal(map[string]string{"alg": c.Alg, "kid": c.Kid, "typ": "JWT"})
	if err != nil {
		panic(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return signing + "." + base64.RawURLEncoding.EncodeToString(key.sign([]byte(signing)))
}

func (s *Server) wait(w http.ResponseWriter) bool {
	s.mu.Lock()
	hung := s.hung
	s.mu.Unlock()
	if hung == nil {
		return true
	}
	select {
	case <-hung:
		return true
	case <-s.closed:
		w.WriteHeader(http.StatusServiceUnavailable)
		return false
	}
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	if !s.wait(w) {
		return
	}
	writeJSON(w, map[string]any{
		"issuer": s.issuer, "jwks_uri": s.issuer + "/jwks",
		"response_types_supported": []string{"id_token"}, "subject_types_supported": []string{"public"},
		"id_token_signing_alg_values_supported": []string{"ES256", "RS256"},
	})
}

func (s *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	if !s.wait(w) {
		return
	}
	s.mu.Lock()
	keys := make([]map[string]string, 0, len(s.keys))
	for _, k := range s.keys {
		keys = append(keys, k.jwk())
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"keys": keys})
}

func (s *Server) mint(w http.ResponseWriter, r *http.Request) {
	var c Claims
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"token": s.Mint(c)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) generate() signingKey {
	if s.rs256 {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		return signingKey{kid: kid(&key.PublicKey), alg: "RS256", rsa: key}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return newECKey(key)
}

func newECKey(key *ecdsa.PrivateKey) signingKey {
	return signingKey{kid: kid(&key.PublicKey), alg: "ES256", ec: key}
}

// kid is spec 007's rule: the first 16 hex characters of the SHA-256 of
// the public key's DER encoding.
func kid(pub any) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:16]
}

func (k signingKey) sign(data []byte) []byte {
	digest := sha256.Sum256(data)
	if k.rsa != nil {
		sig, err := rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, digest[:])
		if err != nil {
			panic(err)
		}
		return sig
	}
	r, s, err := ecdsa.Sign(rand.Reader, k.ec, digest[:])
	if err != nil {
		panic(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return sig
}

func (k signingKey) jwk() map[string]string {
	if k.rsa != nil {
		return map[string]string{
			"kty": "RSA", "kid": k.kid, "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(k.rsa.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.rsa.E)).Bytes()),
		}
	}
	// The uncompressed point: 0x04, then x and y of 32 bytes each.
	point, err := k.ec.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return map[string]string{
		"kty": "EC", "crv": "P-256", "kid": k.kid, "alg": "ES256", "use": "sig",
		"x": base64.RawURLEncoding.EncodeToString(point[1:33]), "y": base64.RawURLEncoding.EncodeToString(point[33:65]),
	}
}
