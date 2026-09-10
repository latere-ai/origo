// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package sshd is spec 024: git over SSH, beside the smart HTTP of
// internal/httpgit and in front of the same write path.
//
// It is a third listener of the node, opt-in through ORIGO_SSH_ADDR. It
// terminates the SSH transport, resolves the offered public key to a
// subject through an endpoint the operator runs, parses the one exec
// request, and hands the two git services to internal/httpgit, which
// spools the body, runs the subprocess, takes the hook's transaction,
// and commits the entry. A push over SSH is the same log entry as a
// push over HTTP.
//
// Three things are this package's alone. It stores no key: the operator's
// endpoint is the whole store contract. It carries no delegation: a
// public key signs nothing but the session, so the actor of every
// SSH-originated authorizer call and of every SSH entry is empty. And
// its command surface is a maintained list of two entries, so a shell, a
// subsystem, a second session, a forwarded port, and an agent socket are
// refused before anything is opened.
//
// golang.org/x/crypto/ssh is confined to this package (spec 001,
// invariant 7).
package sshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/httpgit"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// Before authentication a connection is cheap and bounded, because
// nothing about the caller is known yet.
const (
	// HandshakeTimeout covers the transport handshake and authentication
	// together. A client that completes the handshake and sends nothing
	// is closed when it passes.
	HandshakeTimeout = 30 * time.Second
	// MaxAuthTries is how many public keys one connection may offer.
	MaxAuthTries = 3
	// ServerVersion names the software and not the installation: a
	// banner that names the installation tells an unauthenticated caller
	// where it is.
	ServerVersion = "SSH-2.0-origo"
)

// ClientKeyAlgorithms are the signature algorithms a client may
// authenticate with (spec 024). ssh-dss and SHA-1 RSA signatures are
// absent, so they are refused in the negotiation rather than in a check
// after the fact, and certificate algorithms are absent because
// certificate authentication is a spec of its own.
var ClientKeyAlgorithms = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA256,
	ssh.KeyAlgoRSASHA512,
}

// Options configures a Server.
type Options struct {
	// HostKeys is the ordered set of ORIGO_SSH_HOST_KEYS. Required.
	HostKeys *HostKeys
	// Keys resolves an offered public key to a subject. Required.
	Keys *Resolver
	// Guard decides every operation, before the repository is looked up
	// (spec 007). Required.
	Guard *auth.Guard
	// Cache leases the repository the service runs against. Required.
	Cache *repo.Cache
	// Git runs the two services on the session. Required.
	Git *httpgit.Handler
	// Limits holds the per-subject bucket and the subprocess semaphore
	// (spec 012); a nil one enforces nothing, which is what a test of
	// another rule wants.
	Limits  *limits.Limits
	Metrics *metrics.Set
	Logger  *slog.Logger
	// Handshake bounds the transport handshake and authentication.
	// HandshakeTimeout when zero; a test lowers it.
	Handshake time.Duration
}

// Server is the node's SSH listener.
type Server struct {
	cfg       *ssh.ServerConfig
	hostKeys  *HostKeys
	keys      *Resolver
	guard     *auth.Guard
	cache     *repo.Cache
	git       *httpgit.Handler
	limits    *limits.Limits
	logger    *slog.Logger
	handshake time.Duration

	sessions *pkgmetrics.Counter
	auths    *pkgmetrics.Counter

	conns sync.WaitGroup
	mu    sync.Mutex
	live  map[*connState]struct{}
}

// connState is one connection during a drain. A push is as long as its
// pack and is never cut short, so a drain closes the connections that
// are running nothing and lets the rest end by themselves; a connection
// whose operation finishes after the drain began is closed then.
//
// It is registered before the handshake, not after: a connection whose
// key resolution is waiting on the operator's endpoint is doing no I/O
// on the socket, so the handshake deadline cannot fire and only a close
// ends it. What it closes is the socket, because that is what exists
// before there is an SSH connection over it.
type connState struct {
	conn net.Conn

	mu       sync.Mutex
	busy     int
	draining bool
}

func (c *connState) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy++
}

func (c *connState) end() {
	c.mu.Lock()
	c.busy--
	close := c.draining && c.busy == 0
	c.mu.Unlock()
	if close {
		_ = c.conn.Close()
	}
}

func (c *connState) drain() {
	c.mu.Lock()
	c.draining = true
	idle := c.busy == 0
	c.mu.Unlock()
	if idle {
		_ = c.conn.Close()
	}
}

// track and untrack keep the set of connections a drain walks.
func (s *Server) track(c *connState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[c] = struct{}{}
}

func (s *Server) untrack(c *connState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, c)
}

func (s *Server) drain() {
	s.mu.Lock()
	states := make([]*connState, 0, len(s.live))
	for c := range s.live {
		states = append(states, c)
	}
	s.mu.Unlock()
	for _, c := range states {
		c.drain()
	}
}

// New builds the server. It binds nothing.
func New(o Options) (*Server, error) {
	switch {
	case o.HostKeys == nil || len(o.HostKeys.All()) == 0:
		return nil, errors.New("sshd: the listener needs a host key")
	case o.Keys == nil:
		return nil, errors.New("sshd: the listener needs a key resolver")
	case o.Guard == nil:
		return nil, errors.New("sshd: the listener needs a guard")
	case o.Cache == nil || o.Git == nil:
		return nil, errors.New("sshd: the listener needs the repository cache and the git handler")
	}
	s := &Server{
		hostKeys: o.HostKeys, keys: o.Keys, guard: o.Guard, cache: o.Cache,
		git: o.Git, limits: o.Limits, logger: o.Logger, handshake: o.Handshake,
		live: map[*connState]struct{}{},
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.handshake <= 0 {
		s.handshake = HandshakeTimeout
	}
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	s.sessions, s.auths = set.SSHSessions, set.SSHAuth
	s.cfg = &ssh.ServerConfig{
		ServerVersion:           ServerVersion,
		MaxAuthTries:            MaxAuthTries,
		PublicKeyAuthAlgorithms: ClientKeyAlgorithms,
		PublicKeyCallback:       s.authenticate,
	}
	for _, signer := range o.HostKeys.Presented() {
		s.cfg.AddHostKey(signer)
	}
	return s, nil
}

// Serve accepts connections until ctx ends or the listener fails, then
// waits for the connections in flight. A git push is as long as its
// pack, so nothing is cut short here; the node's grace period bounds
// the wait.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.drain()
			s.conns.Wait()
			if ctx.Err() != nil {
				// The listener was closed by the drain, which is the
				// stop this loop ends on and not a failure.
				return nil //nolint:nilerr // a closed listener during a drain is the stop
			}
			return fmt.Errorf("ssh listener: %w", err)
		}
		s.conns.Go(func() { s.handle(ctx, conn) })
	}
}

// authenticate resolves an offered public key to a subject. It decides
// identity and nothing else: what the subject may do is the authorizer's
// answer once the exec names a service and a repository.
//
// The SSH user name is not read. `git` is a convention and any name is
// accepted, because reading it would create a second identity input that
// contradicts the fingerprint the key store resolved.
func (s *Server) authenticate(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	ctx, cancel := context.WithTimeout(context.Background(), KeysTimeout)
	defer cancel()
	answer, err := s.keys.Resolve(ctx, key)
	if err != nil {
		result := "resolver_error"
		if errors.Is(err, context.DeadlineExceeded) {
			result = "timeout"
		}
		s.auths.Inc(map[string]string{"result": result})
		s.logger.ErrorContext(ctx, "key resolver unavailable", "remote", conn.RemoteAddr().String(), "error", err)
		return nil, errKeyRefused
	}
	if !answer.Found {
		s.auths.Inc(map[string]string{"result": "unknown_key"})
		return nil, errKeyRefused
	}
	s.auths.Inc(map[string]string{"result": "ok"})
	return &ssh.Permissions{Extensions: map[string]string{
		extSubject: answer.Subject,
		extKeyID:   answer.KeyID,
	}}, nil
}

// The subject and the store's key id travel from authentication to the
// session on the connection's permissions, which is the one value the
// library carries between them.
const (
	extSubject = "origo-subject"
	extKeyID   = "origo-key-id"
)

// errKeyRefused is every authentication refusal. Unknown, revoked,
// expired, and belonging-to-someone-else look alike on the wire: the
// client reads Permission denied (publickey) whichever it was.
var errKeyRefused = errors.New("permission denied")

// handle runs one connection: the bounded handshake, the host key
// announcement, the global requests, and the one session.
func (s *Server) handle(ctx context.Context, nc net.Conn) {
	defer func() { _ = nc.Close() }()
	state := &connState{conn: nc}
	s.track(state)
	defer s.untrack(state)
	// The deadline covers the handshake and authentication together,
	// before anything about the caller is known. It is cleared once the
	// connection is authenticated, because a clone is as long as the
	// client's link makes it.
	_ = nc.SetDeadline(time.Now().Add(s.handshake))
	conn, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		s.logger.DebugContext(ctx, "ssh handshake failed", "remote", nc.RemoteAddr().String(), "error", err)
		return
	}
	defer func() { _ = conn.Close() }()
	_ = nc.SetDeadline(time.Time{})
	subject := conn.Permissions.Extensions[extSubject]
	s.logger.InfoContext(ctx, "ssh session", "remote", nc.RemoteAddr().String(), "subject", subject, "key_id", conn.Permissions.Extensions[extKeyID])

	// Every configured host key reaches the client here, so a client
	// with UpdateHostKeys writes the ones it does not hold into
	// known_hosts by itself and a rotation is an overlap.
	if _, _, err := conn.SendRequest(hostKeysRequest, false, s.hostKeys.announcement()); err != nil {
		s.logger.DebugContext(ctx, "host keys not announced", "error", err)
	}
	go s.globalRequests(ctx, conn, reqs)
	s.channels(ctx, chans, subject, state)
}

// globalRequests answers the connection's global requests. Only the
// host key proof is answered; every forwarding request is refused with
// no listener opened, which is where tcpip-forward,
// cancel-tcpip-forward, and streamlocal-forward@openssh.com end.
func (s *Server) globalRequests(ctx context.Context, conn ssh.Conn, reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type != hostKeysProve {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		payload, err := s.hostKeys.prove(conn.SessionID(), req.Payload)
		if err != nil {
			s.logger.DebugContext(ctx, "host key proof refused", "error", err)
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		if req.WantReply {
			_ = req.Reply(true, payload)
		}
	}
}

// channels opens the one session a connection carries. A channel of any
// other type is refused with no channel opened, and so is a second
// session: one connection carries one operation.
func (s *Server) channels(ctx context.Context, chans <-chan ssh.NewChannel, subject string, state *connState) {
	opened := false
	var sessions sync.WaitGroup
	defer sessions.Wait()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only a session channel is served")
			continue
		}
		if opened {
			_ = nc.Reject(ssh.Prohibited, "one connection carries one operation")
			continue
		}
		opened = true
		ch, reqs, err := nc.Accept()
		if err != nil {
			s.logger.DebugContext(ctx, "session not accepted", "error", err)
			return
		}
		state.start()
		sessions.Go(func() {
			defer state.end()
			s.session(ctx, ch, reqs, subject)
		})
	}
}

// requestsAnsweredNo are the session requests that are answered and do
// nothing: a pty, an environment variable, X11, an agent socket, a
// signal, and a window change. No environment variable a client sends
// ever reaches a subprocess (spec 016), so the protocol version is the
// one git falls back to when the server accepts none.
var requestsAnsweredNo = map[string]bool{
	"pty-req": true, "env": true, "x11-req": true,
	"auth-agent-req@openssh.com": true, "signal": true, "window-change": true,
}

// session runs the one operation of a session channel. The command
// surface is the maintained list of decision 7: two services, and every
// entry not on it is refused with the code's line on stderr and exit
// status 1, before a subprocess is started, the repository is read, or
// the authorizer is asked.
func (s *Server) session(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, subject string) {
	// The channel is closed when the operation ends, not when the client
	// stops sending requests: a git client waits for the server to close
	// the session before it reports the exit status, so a session that
	// stayed open until the client closed it would hold both sides.
	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }
	go func() {
		<-done
		_ = ch.Close()
	}()

	var op sync.WaitGroup
	started := false
	for req := range reqs {
		switch {
		case req.Type == "exec" && !started:
			started = true
			line, err := execCommand(req.Payload)
			if err != nil {
				_ = req.Reply(false, nil)
				s.refuse(ch, refuseInvalid, 0)
				finish()
				return
			}
			_ = req.Reply(true, nil)
			op.Go(func() {
				s.run(ctx, ch, subject, line)
				finish()
			})
		case req.Type == "exec":
			// A second exec on one session: refused without disturbing
			// the operation the first one started.
			_ = req.Reply(false, nil)
			_, _ = fmt.Fprintln(ch.Stderr(), refuseInvalid.Line())
		case requestsAnsweredNo[req.Type]:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		default:
			// shell, subsystem, and anything else: nothing is opened.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			if !started {
				s.refuse(ch, refuseInvalid, 0)
				finish()
				return
			}
		}
	}
	// The client closed the session. An operation in flight is waited
	// for: a push that reached the log is committed whatever the client
	// did afterwards.
	op.Wait()
	finish()
}

// execCommand reads the exec request's payload, one SSH string, and
// bounds it before it is parsed.
func execCommand(payload []byte) (string, error) {
	parts, err := readStrings(payload)
	if err != nil || len(parts) != 1 || len(parts[0]) > MaxPathBytes {
		return "", errCommand
	}
	return string(parts[0]), nil
}

// run serves one exec: the command, the path, the authorizer, the
// lease, and the service. Every refusal before the service is a code on
// stderr and exit status 1.
func (s *Server) run(ctx context.Context, ch ssh.Channel, subject, line string) {
	service, arg, err := ParseCommand(line)
	if err != nil {
		s.refuse(ch, refuseInvalid, 0)
		return
	}
	action, label := auth.ActionRead, "upload-pack"
	if service == ServiceReceivePack {
		action, label = auth.ActionWrite, "receive-pack"
	}
	result := "refused"
	defer func() { s.sessions.Inc(map[string]string{"service": label, "result": result}) }()

	// One session costs one token of the subject's bucket (spec 012): an
	// SSH clone is one connection where an HTTP clone is two requests.
	if ok, retry := s.limits.Take(ctx, subject); !ok {
		s.refuse(ch, refuseRateLimited, retry)
		return
	}
	ref, err := ParsePath(arg)
	if err != nil {
		s.refuse(ch, refuseInvalid, 0)
		return
	}
	// Delegation does not exist over SSH: a public key signs nothing but
	// the session, so the actor is empty on every call and on every
	// entry an SSH push commits.
	principal := auth.Principal{Subject: subject}
	ctx = auth.WithPrincipal(ctx, principal)

	// The name form resolves to an id before the authorizer is asked and
	// the id form sends the id, which is spec 007's authorization before
	// lookup: the call happens before any read of meta or the index, so a
	// denied caller cannot tell a repository apart from one that does not
	// exist.
	if ref.ID == "" {
		id, err := s.cache.Log().Resolve(ctx, ref.Owner, ref.Slug)
		if err != nil && !errors.Is(err, wal.ErrNotFound) {
			s.refuse(ch, storageCode(err), 0)
			return
		}
		ref.ID = id
	}
	decision, err := s.guard.Decide(ctx, principal, ref, action)
	if err != nil {
		if denied, ok := errors.AsType[*auth.Denied](err); ok {
			s.logger.InfoContext(ctx, "ssh refused", "repo", ref.ID, "action", action, "subject", subject, "reason", denied.Reason)
			s.refuse(ch, refuseForbidden, 0)
			return
		}
		s.logger.ErrorContext(ctx, "authorizer unavailable", "repo", ref.ID, "subject", subject, "error", err)
		s.refuse(ch, refuseNoAuthz, 0)
		return
	}
	if ref.ID == "" {
		s.refuse(ch, refuseNotFound, 0)
		return
	}
	s.limits.SetSubjectRate(subject, decision.RequestsPerMinute)

	lease, err := s.cache.Lease(ctx, ref.ID, action == auth.ActionWrite)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrDeleted):
			s.refuse(ch, refuseNotFound, 0)
		default:
			s.logger.ErrorContext(ctx, "repository unavailable", "repo", ref.ID, "error", err)
			s.refuse(ch, storageCode(err), 0)
		}
		return
	}
	defer lease.Release()
	if lease.Stale {
		// A push needs a current index object as its base, so a stale
		// lease refuses one; a read is served and says how old it is.
		// The stderr line is not the header's equivalent: a git client
		// cannot act on it, so a consumer that must not read stale uses
		// HTTPS.
		if action == auth.ActionWrite {
			s.refuse(ch, refuseStorage, 0)
			return
		}
		_, _ = fmt.Fprintf(ch.Stderr(), "%s: %d\n", contract.HeaderStale, int(lease.StaleFor/time.Second))
	}

	stream := httpgit.Stream{In: ch, Out: ch, Err: ch.Stderr()}
	var refusal contract.Refusal
	if action == auth.ActionWrite {
		refusal, err = s.git.ReceivePack(ctx, ref.ID, lease.Repo, decision.QuotaBytes, stream)
	} else {
		refusal, err = s.git.UploadPack(ctx, ref.ID, lease.Repo, stream)
	}
	switch {
	case refusal.Code != "":
		s.refuse(ch, refusal, 0)
	case err != nil:
		result = "error"
		exit(ch, 1)
	default:
		result = "ok"
		exit(ch, 0)
	}
}

// storageCode maps a failure of the log to the code the client reads: a
// repository the log names objects for that are missing or fail their
// digest is unavailable until an operator restores it, and every other
// storage failure is temporary (spec 015).
func storageCode(err error) contract.Refusal {
	if _, ok := errors.AsType[*wal.IntegrityError](err); ok {
		return refuseBroken
	}
	return refuseStorage
}

// The refusals this listener answers with, each built where its code is
// chosen, so the code table's test reads one call site per code (spec
// 021). The status is the code's own row; SSH sends none, and naming it
// here keeps the two surfaces on one table.
var (
	refuseInvalid     = contract.Refuse(http.StatusBadRequest, contract.CodeInvalid, nil)
	refuseForbidden   = contract.Refuse(http.StatusForbidden, contract.CodeForbidden, nil)
	refuseNoAuthz     = contract.Refuse(http.StatusServiceUnavailable, contract.CodeAuthorizerUnavailable, nil)
	refuseNotFound    = contract.Refuse(http.StatusNotFound, contract.CodeRepoNotFound, nil)
	refuseRateLimited = contract.Refuse(http.StatusTooManyRequests, contract.CodeRateLimited, nil)
	refuseStorage     = contract.Refuse(http.StatusServiceUnavailable, contract.CodeStorageUnavailable, nil)
	refuseBroken      = contract.Refuse(http.StatusServiceUnavailable, contract.CodeRepositoryUnavailable, nil)
)

// refuse writes a refusal to the session's stderr and ends it with exit
// status 1. The first line is contract.Line of the code, byte for byte
// the form spec 021 fixed for the sideband, so a person reads the same
// sentence they would read in a JSON envelope. A refusal that has a wait
// carries it on a line of its own, because SSH has no header to put it
// on and the code's line is not rewritten to hold it.
func (s *Server) refuse(ch ssh.Channel, refusal contract.Refusal, retry time.Duration) {
	_, _ = fmt.Fprintln(ch.Stderr(), refusal.Line())
	if retry > 0 {
		_, _ = fmt.Fprintln(ch.Stderr(), "retry after "+strconv.Itoa(limits.RetryAfterSeconds(retry))+" seconds")
	}
	exit(ch, 1)
}

// exit sends the exit status and ends the session's output.
func exit(ch ssh.Channel, status uint32) {
	_ = ch.CloseWrite()
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}

// Fingerprints is every host key's SHA-256 fingerprint, for the node's
// start-up line.
func (s *Server) Fingerprints() string { return strings.Join(s.hostKeys.Fingerprints(), ",") }
