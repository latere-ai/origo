// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command origo-stubs runs the test stubs of spec 013 from flags: the
// issuer, the authorizer, the sink, the source when -source-token is
// set, and the slow proxy of spec 015 when -slowproxy-target is set.
// `make dev` runs it on the loopback interface beside MinIO and the kind
// overlay runs it as a pod; each listen flag defaults to the stub's
// fixed port in the stack on every interface.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/issuer"
	"github.com/latere-ai/origo/test/stubs/sink"
	"github.com/latere-ai/origo/test/stubs/source"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// options is the flag table of spec 013.
type options struct {
	issuerListen, authorizerListen, sinkListen, sourceListen string
	slowproxyListen, slowproxyData, slowproxyTarget          string
	issuerURL, authorizerToken, sourceToken                  string
	allow, secret, key, ca, caKey                            string
	fail                                                     int
	hang                                                     bool
}

// run parses the flags, starts every stub, prints one line per listener
// on stdout, and serves until ctx ends. It returns the exit code: 0 on a
// clean stop, 1 when a stub cannot start, 2 on a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "origo-stubs:", err)
		}
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	stubs, err := build(o)
	if err != nil {
		fmt.Fprintln(stderr, "origo-stubs:", err)
		return 1
	}
	defer stubs.close()
	if err := stubs.serve(ctx, stdout, logger); err != nil {
		fmt.Fprintln(stderr, "origo-stubs:", err)
		return 1
	}
	return 0
}

// parse reads the flag table of spec 013. A flag error is reported on
// stderr by the flag set itself and returned as is.
func parse(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("origo-stubs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.issuerListen, "issuer-listen", "0.0.0.0:8081", "the issuer's listen address")
	fs.StringVar(&o.authorizerListen, "authorizer-listen", "0.0.0.0:8082", "the authorizer's listen address")
	fs.StringVar(&o.sinkListen, "sink-listen", "0.0.0.0:8083", "the sink's listen address")
	fs.StringVar(&o.sourceListen, "source-listen", "0.0.0.0:8443", "the source's TLS listen address; the source starts only with -source-token")
	fs.StringVar(&o.slowproxyListen, "slowproxy-listen", "0.0.0.0:8085", "the slow proxy's control listen address; the proxy starts only with -slowproxy-target")
	fs.StringVar(&o.slowproxyData, "slowproxy-data", "0.0.0.0:8086", "the slow proxy's data listen address, what ORIGO_S3_ENDPOINT points at")
	fs.StringVar(&o.slowproxyTarget, "slowproxy-target", "", "the host:port the slow proxy forwards to, MinIO's Service in the stack")
	fs.StringVar(&o.issuerURL, "issuer-url", "", "the iss of every token and the issuer of the discovery document; http://<issuer-listen> by default")
	fs.StringVar(&o.authorizerToken, "authorizer-token", "", "the bearer the authorizer requires (required)")
	fs.StringVar(&o.sourceToken, "source-token", "", "the bearer the source requires; unset runs no source")
	fs.StringVar(&o.allow, "allow", "*", "the subjects the authorizer allows when no rule matches, comma separated, * for all")
	fs.StringVar(&o.secret, "secret", sink.DefaultSecret, "the HMAC key the sink verifies Origo-Signature with")
	fs.StringVar(&o.key, "key", "", "a PEM file holding the issuer's ES256 key; generated when unset")
	fs.IntVar(&o.fail, "fail", 0, "make the authorizer answer this status from the start")
	fs.BoolVar(&o.hang, "hang", false, "make the authorizer never answer from the start")
	fs.StringVar(&o.ca, "ca", "", "a PEM file holding the CA certificate the source's certificate is signed by; generated when unset")
	fs.StringVar(&o.caKey, "ca-key", "", "a PEM file holding the CA's key")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.authorizerToken == "" {
		return o, errors.New("-authorizer-token is required")
	}
	if (o.ca == "") != (o.caKey == "") {
		return o, errors.New("-ca and -ca-key go together")
	}
	if o.issuerURL == "" {
		o.issuerURL = "http://" + o.issuerListen
	}
	return o, nil
}

// listener is one stub's name, address, and handler, served over TLS
// when tls is set (the source).
type listener struct {
	name    string
	addr    string
	handler http.Handler
	tls     bool
}

// stubs is everything run starts.
type stubs struct {
	issuer     *issuer.Server
	authz      *authorizer.Server
	sink       *sink.Server
	source     *source.Server
	sourceRoot string
	proxy      *slowproxy
	listeners  []listener
}

// build constructs every stub from the options without listening.
func build(o options) (*stubs, error) {
	s := &stubs{}
	issuerOpts := []issuer.Option{issuer.WithIssuer(o.issuerURL)}
	if o.key != "" {
		key, err := readKey(o.key)
		if err != nil {
			return nil, fmt.Errorf("-key: %w", err)
		}
		issuerOpts = append(issuerOpts, issuer.WithKey(key))
	}
	s.issuer = issuer.NewHandler(issuerOpts...)
	s.authz = authorizer.NewHandler(authorizer.WithToken(o.authorizerToken), authorizer.WithAllow(splitList(o.allow)...))
	if o.fail != 0 {
		s.authz.Fail(o.fail)
	}
	if o.hang {
		s.authz.Hang()
	}
	s.sink = sink.NewHandler(sink.WithSecret(o.secret))
	s.listeners = []listener{
		{name: "issuer", addr: o.issuerListen, handler: s.issuer.Handler()},
		{name: "authorizer", addr: o.authorizerListen, handler: s.authz.Handler()},
		{name: "sink", addr: o.sinkListen, handler: s.sink.Handler()},
	}
	if o.sourceToken != "" {
		sourceOpts := []source.Option{source.WithToken(o.sourceToken)}
		if o.ca != "" {
			cert, err := os.ReadFile(o.ca)
			if err != nil {
				return nil, fmt.Errorf("-ca: %w", err)
			}
			key, err := os.ReadFile(o.caKey)
			if err != nil {
				return nil, fmt.Errorf("-ca-key: %w", err)
			}
			sourceOpts = append(sourceOpts, source.WithCA(cert, key))
		}
		root, err := os.MkdirTemp("", "origo-stubs-source-")
		if err != nil {
			return nil, err
		}
		s.sourceRoot = root
		src, err := source.NewHandler(root, sourceOpts...)
		if err != nil {
			s.close()
			return nil, fmt.Errorf("source: %w", err)
		}
		s.source = src
		s.listeners = append(s.listeners, listener{name: "source", addr: o.sourceListen, handler: src.Handler(), tls: true})
	}
	if o.slowproxyTarget != "" {
		s.proxy = newSlowproxy(o.slowproxyTarget)
		s.proxy.dataAddr = o.slowproxyData
		s.listeners = append(s.listeners, listener{name: "slowproxy", addr: o.slowproxyListen, handler: s.proxy.control()})
	}
	return s, nil
}

// close releases what build made.
func (s *stubs) close() {
	if s.issuer != nil {
		s.issuer.Close()
	}
	if s.authz != nil {
		s.authz.Close()
	}
	if s.sourceRoot != "" {
		_ = os.RemoveAll(s.sourceRoot)
	}
}

// serve binds every listener, prints "<name> listening on <address>"
// for each, and serves until ctx ends or a listener fails.
func (s *stubs) serve(ctx context.Context, stdout io.Writer, logger *slog.Logger) error {
	var lc net.ListenConfig
	var servers []*http.Server
	var lns []net.Listener
	failed := make(chan error, len(s.listeners)+1)
	for _, l := range s.listeners {
		ln, err := lc.Listen(ctx, "tcp", l.addr)
		if err != nil {
			for _, open := range lns {
				_ = open.Close()
			}
			return fmt.Errorf("%s: %w", l.name, err)
		}
		if l.tls {
			ln = tls.NewListener(ln, s.source.TLSConfig())
		}
		lns = append(lns, ln)
		srv := &http.Server{Handler: l.handler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn)}
		servers = append(servers, srv)
		fmt.Fprintf(stdout, "%s listening on %s\n", l.name, ln.Addr())
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("%s: %w", l.name, err)
			}
		}()
	}
	var proxyLn net.Listener
	if s.proxy != nil {
		ln, err := lc.Listen(ctx, "tcp", s.proxy.dataAddr)
		if err != nil {
			for _, open := range lns {
				_ = open.Close()
			}
			return fmt.Errorf("slowproxy data: %w", err)
		}
		proxyLn = ln
		fmt.Fprintf(stdout, "slowproxy data listening on %s\n", ln.Addr())
		go func() {
			if err := s.proxy.serve(ln); err != nil {
				failed <- fmt.Errorf("slowproxy data: %w", err)
			}
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	if proxyLn != nil {
		_ = proxyLn.Close()
		s.proxy.close()
	}
	return err
}

// readKey reads an ES256 key from a PEM file, the form ORIGO_TOKEN_KEY
// takes.
func readKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return auth.ParseKey(string(raw))
}

func splitList(raw string) []string {
	var out []string
	for s := range strings.SplitSeq(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
