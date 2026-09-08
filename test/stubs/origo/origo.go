// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package origo is the contract stub of spec 013: the node's own
// handlers of internal/httpgit and internal/api, served in-process over
// the S3 adapter against an in-process bucket of
// latere.ai/x/pkg/s3/s3test, a temporary cache directory, and the stub
// issuer, authorizer, and sink, so a consumer runs its integration tests
// against the whole contract with nothing but `go test`. The bucket is a
// real endpoint rather than the in-memory store so a presigned URL
// resolves, and its Fail and the adapter's Delete are what spec 021's
// Fault uses.
package origo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/health"
	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/s3/s3test"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/httpgit"
	"github.com/latere-ai/origo/internal/lfs"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/version"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/issuer"
	"github.com/latere-ai/origo/test/stubs/sink"
)

// Bucket is the name of the in-process bucket.
const Bucket = "origo-test"

// Server is one in-process Origo with its stubs.
type Server struct {
	srv    *httptest.Server
	bucket *s3test.Server
	store  *wal.S3
	log    *wal.Log
	issuer *issuer.Server
	authz  *authorizer.Server
	sink   *sink.Server
	cancel context.CancelFunc
}

// New starts the stub for the test and stops it with the test.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		bucket: s3test.New(t, Bucket),
		issuer: issuer.New(t),
		authz:  authorizer.New(t),
		sink:   sink.New(t),
	}
	logger := slog.New(slog.DiscardHandler)
	reg := metrics.NewRegistry()
	store, err := wal.NewS3(wal.S3Options{
		Endpoint: s.bucket.URL(), Region: s3test.Region, Bucket: Bucket, Key: s3test.Key, Secret: s3test.Secret, PathStyle: true,
		Client: &http.Client{Transport: &http.Transport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	s.log = wal.New(wal.Options{Store: store, Prefix: config.Prefix, Metrics: reg, Logger: logger})
	cache, err := repo.New(repo.Options{Dir: t.TempDir(), Log: s.log, Logger: logger, Metrics: reg})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// The listener first, so the public URL the signer and the verifier
	// carry is known before the handler is built.
	s.srv = httptest.NewUnstartedServer(nil)
	url := "http://" + s.srv.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{}}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{
		Issuers: []string{s.issuer.URL()}, LocalIssuer: url, LocalKey: &key.PublicKey, Client: client, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	authzClient, err := auth.NewClient(auth.ClientOptions{URL: s.authz.URL(), Token: s.authz.Token(), HTTP: client, Metrics: reg})
	if err != nil {
		t.Fatal(err)
	}
	guard := auth.NewGuard(authzClient, logger)
	signer := auth.NewSigner(key, url, nil)

	// The application surface as cmd/origod wires it: smart HTTP and the
	// repository API behind the verifier, the three unauthenticated
	// paths in front, and the contract version on every response.
	app := http.NewServeMux()
	httpgit.New(httpgit.Options{Cache: cache, Logger: logger, Metrics: reg, Guard: guard}).Register(app)
	api.New(api.Options{Cache: cache, Logger: logger, Guard: guard, Signer: signer}).Register(app)
	presigner, err := lfs.NewPresigner(lfs.PresignerOptions{
		Endpoint: s.bucket.URL(), Region: s3test.Region, Bucket: Bucket, Key: s3test.Key, Secret: s3test.Secret, PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lfs.New(lfs.Options{Log: s.log, Guard: guard, Presigner: presigner, Logger: logger}).Register(app)
	app.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, map[string]any{"reason": "no such route"})
	})
	probes := health.Handler(health.Options{
		Ready:   health.Checks(health.Check{Name: "storage", Run: s.log.Ping}),
		Version: version.Version, Commit: version.Commit, BuildTime: version.Date,
	})
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", probes)
	mux.Handle("GET /version", probes)
	mux.Handle("GET /.well-known/jwks.json", signer.JWKS())
	mux.Handle("/", verifier.Middleware(app))
	s.srv.Config.Handler = contract.Middleware(mux)
	s.srv.Start()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	done := make(chan struct{})
	go func() { defer close(done); _ = verifier.Run(ctx) }()
	t.Cleanup(func() {
		s.srv.Close()
		cancel()
		<-done
	})
	return s
}

// URL is the base URL, the value a consumer's tests target and the iss
// of every repository-bound token.
func (s *Server) URL() string { return s.srv.URL }

// Token mints a token the stub accepts for the subject, acting for act
// when it is not empty.
func (s *Server) Token(sub, act string) string {
	return s.issuer.Mint(issuer.Claims{Sub: sub, Act: act})
}

// Issuer is the stub issuer the node trusts.
func (s *Server) Issuer() *issuer.Server { return s.issuer }

// Authorizer is the stub authorizer every request is decided by.
func (s *Server) Authorizer() *authorizer.Server { return s.authz }

// Sink is the stub sink; the node delivers to it from spec 008 on.
func (s *Server) Sink() *sink.Server { return s.sink }

// Bucket is the in-process bucket: its Fail is the storage fault of
// spec 021.
func (s *Server) Bucket() *s3test.Server { return s.bucket }

// Store is the adapter over the bucket: its Delete is the object fault
// of spec 021.
func (s *Server) Store() *wal.S3 { return s.store }

// Log is the write-ahead log over the store, for a test that reads what
// a push committed.
func (s *Server) Log() *wal.Log { return s.log }
