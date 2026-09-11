// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/tracing"
	"github.com/latere-ai/origo/internal/wal"
)

// The two git services on a stream, and nothing else (spec 024).
// UploadPack and ReceivePack run one subprocess each on the repository
// the caller already leased.
//
// Smart HTTP and SSH differ in how bytes arrive and in nothing else.
// Over HTTP a fetch is a sequence of `--stateless-rpc` requests, each
// carrying one round of the negotiation in its body; over a stream the
// same negotiation runs in one subprocess over one connection, which is
// git's own protocol and not something a transport can translate. So
// the framing is per transport and everything below it, the guard the
// caller asked, the limits table, the repository cache, the spooled
// body, the subprocess, the hook, and the log commit, is this package's
// and is shared: an SSH push writes the same entry, under the same
// create-if-absent, and produces the same push event.

// Stream is one git service's connection to a client: the client's
// bytes, the service's bytes, and the out-of-band channel a refusal
// travels on. Over SSH they are the session channel, the session
// channel, and its stderr.
type Stream struct {
	In  io.Reader
	Out io.Writer
	// Err carries git's own diagnostics to the client, the way an SSH
	// server passes a service's stderr through. A refusal of Origo's is
	// written by the caller from the code returned, so the one sentence
	// of the code table is never rebuilt here.
	Err io.Writer
}

func (s Stream) errWriter() io.Writer {
	if s.Err == nil {
		return io.Discard
	}
	return s.Err
}

// UploadPack serves a fetch or clone over a stream. It returns the
// refusal the caller renders, the zero Refusal when git ran, and an
// error only when the node failed at something the caller must log.
func (h *Handler) UploadPack(ctx context.Context, id string, rp *repo.Repo, s Stream) (contract.Refusal, error) {
	release, _, ok := h.limits.Acquire(ctx)
	if !ok {
		return refuseRateLimited, nil
	}
	defer release()
	cmd, cancel := h.streamCommand(ctx, rp, "upload-pack", rp.Dir)
	defer cancel()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.In, s.Out, s.errWriter()
	if err := cmd.Run(); err != nil {
		h.logger.WarnContext(ctx, "upload-pack ended with an error", "repo", id, "transport", "stream", "error", err)
		return contract.Refusal{}, err
	}
	h.fetches.Inc(nil)
	return contract.Refusal{}, nil
}

// ReceivePack serves a push over a stream: the client's bytes are
// spooled as they pass to git, the pre-receive hook hands the
// transaction over, the entry is committed, and only then does git
// update its own references. It is receivePack's ordering with the
// stream's framing, so a push over SSH is the same log entry as a push
// over HTTP.
//
// It returns the refusal that never reached git, or the zero Refusal
// when git ran; a push git or the log refused travels to the client in
// the sideband as the hook's verdict, which is where a git client reads
// a refusal.
func (h *Handler) ReceivePack(ctx context.Context, id string, rp *repo.Repo, quota int64, s Stream) (contract.Refusal, error) {
	// The repository's own state (spec 019) and the write breaker (spec
	// 015) are read before git starts, so a frozen repository and an
	// unreachable bucket refuse the push before the client uploads a
	// pack.
	m, err := h.log.ReadMeta(ctx, id)
	switch {
	case errors.Is(err, wal.ErrNotFound):
	case err != nil:
		h.logger.ErrorContext(ctx, "repository unavailable", "repo", id, "transport", "stream", "error", err)
		return refuseStorage, nil
	default:
		if refusal, refused := stateRefusal(m); refused {
			h.logger.InfoContext(ctx, "push refused", "repo", id, "code", refusal.Code, "subject", auth.Subject(ctx))
			return refusal, nil
		}
	}
	if bs := h.log.Breakers(); bs != nil && !bs.Admits(wal.ClassWrite) {
		return refuseStorage, nil
	}
	if err := installHook(rp.Dir); err != nil {
		return refuseStorage, err
	}
	ch, err := newHookChannel(h.cache.SpoolDir())
	if err != nil {
		return refuseStorage, err
	}
	defer ch.close()
	release, _, ok := h.limits.Acquire(ctx)
	if !ok {
		return refuseRateLimited, nil
	}
	defer release()

	spool, err := os.CreateTemp(h.cache.SpoolDir(), "receive-*.body")
	if err != nil {
		return refuseStorage, err
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()

	cmd, cancel := h.streamCommand(ctx, rp, "receive-pack", rp.Dir)
	defer cancel()
	cmd.Env = append(cmd.Env, "ORIGO_HOOK_DIR="+ch.dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return refuseStorage, err
	}
	cmd.Stdout, cmd.Stderr = s.Out, s.errWriter()
	if err := cmd.Start(); err != nil {
		return refuseStorage, err
	}
	t := newSpoolTee(spool, stdin, s.In, h.limits.MaxPush())
	go t.run()
	refusal := h.receiveStream(ctx, id, rp, quota, t, ch, cmd.Wait, spool.Name())
	<-t.stopped
	return refusal, nil
}

// The refusals a stream answers before git runs, each built where its
// code is chosen, so the code table's test reads one call site per code
// (spec 021).
var (
	refuseStorage     = contract.Refuse(http.StatusServiceUnavailable, contract.CodeStorageUnavailable, nil)
	refuseRateLimited = contract.Refuse(http.StatusTooManyRequests, contract.CodeRateLimited, nil)
	refuseOverQuota   = contract.Refuse(http.StatusRequestEntityTooLarge, contract.CodeOverQuota, nil)
	refuseInvalid     = contract.Refuse(http.StatusBadRequest, contract.CodeInvalid, nil)
)

// receiveStream is the half of ReceivePack that runs while git does:
// it waits for the hook or for git's exit, decides the verdict, and
// commits. It mirrors receivePack's select, with the spool measured at
// the moment the hook fired rather than at the end of a request body.
func (h *Handler) receiveStream(ctx context.Context, id string, rp *repo.Repo, quota int64, t *spoolTee, ch *hookChannel, wait func() error, spoolPath string) contract.Refusal {
	started := time.Now()
	_, endReceive := tracing.Start(ctx, "receive", tracing.Repo(id))
	receiveEnded := false
	endReceiveOnce := func() {
		if !receiveEnded {
			receiveEnded = true
			endReceive()
		}
	}
	defer endReceiveOnce()

	type updates struct {
		refs       []wal.RefUpdate
		quarantine string
		ok         bool
		err        error
	}
	fromHook := make(chan updates, 1)
	go func() {
		refs, quarantine, ok, err := ch.readUpdates()
		fromHook <- updates{refs, quarantine, ok, err}
	}()
	exited := make(chan error, 1)
	go func() { exited <- wait() }()

	var committed *wal.Committed
	var refs []wal.RefUpdate
	var forced map[string]bool
	var runErr error
	var refusal contract.Refusal
	select {
	case u := <-fromHook:
		h.observe(phaseReceive, started)
		endReceiveOnce()
		refs = u.refs
		// The pack is on disk: git read all of it before the hook ran and
		// the tee wrote every byte to the spool before handing it on, so
		// what the spool holds now is the whole body and nothing more.
		end := t.size()
		req, parseErr := parseReceiveTo(t.section(end), end)
		verdict := "reject origo: no reference updates"
		switch {
		case u.err != nil:
			verdict = "reject origo: " + u.err.Error()
		case !u.ok:
		case parseErr != nil:
			refusal, verdict = h.streamParseRefusal(ctx, id, parseErr)
		default:
			verdict = h.streamVerdict(ctx, id, rp, quota, u.refs, req, spoolPath, &committed)
		}
		started = time.Now()
		if committed != nil {
			h.phases.Observe(map[string]string{"phase": phaseEntry}, committed.EntryDuration.Seconds())
			h.phases.Observe(map[string]string{"phase": phaseIndex}, committed.IndexDuration.Seconds())
			forced = h.forcedUpdates(ctx, rp, u.refs, u.quarantine)
		}
		if err := ch.writeVerdict(verdict); err != nil {
			h.logger.WarnContext(ctx, "verdict not delivered", "repo", id, "error", err)
		}
		runErr = <-exited
	case runErr = <-exited:
		// git refused the push before the hook ran: bad objects, a failed
		// connectivity check, a client that went away, or the tee that
		// closed git's input on the single-push bound.
		if err := ch.release(); err != nil {
			h.logger.WarnContext(ctx, "hook channel not released", "repo", id, "error", err)
		}
		<-fromHook
		if t.tooLarge() {
			f := &sizeRefusal{limit: limits.LimitPush, bytes: t.size(), max: t.max}
			f.log(ctx, h.logger, id, auth.Subject(ctx))
			refusal = refuseOverQuota
		}
	}
	if runErr != nil {
		h.logger.WarnContext(ctx, "receive-pack ended with an error", "repo", id, "transport", "stream", "error", runErr)
	}
	if committed != nil {
		_, endApply := tracing.Start(ctx, "apply", tracing.Repo(id))
		if runErr == nil {
			if err := h.cache.Advance(rp, committed.Index); err != nil {
				h.logger.ErrorContext(ctx, "local sequence not advanced", "repo", id, "error", err)
			}
		}
		h.observe(phaseApply, started)
		endApply()
		h.pushes.Inc(nil)
		h.logger.InfoContext(ctx, "push", "repo", id, "transport", "stream", "seq", committed.Index.Seq, "refs", len(refs), "forced", len(forced), "subject", auth.Subject(ctx), "actor", auth.Actor(ctx))
		_ = h.events.Enqueue(ctx, id, events.Entry{Header: committed.Header, Refs: refs, Forced: forced})
		if h.compact != nil {
			h.compact.After(id, committed.Index)
		}
	}
	return refusal
}

// streamParseRefusal maps a body this package could not read to the
// verdict the client sees. A body past the reference cap is the same
// over_quota the HTTP path answers; anything else is a malformed
// request.
func (h *Handler) streamParseRefusal(ctx context.Context, id string, err error) (contract.Refusal, string) {
	if errors.Is(err, errTooManyCommands) {
		f := &sizeRefusal{limit: limits.LimitRefs, bytes: limits.MaxRefs + 1, max: limits.MaxRefs}
		f.log(ctx, h.logger, id, auth.Subject(ctx))
		return refuseOverQuota, "reject " + refuseOverQuota.Line()
	}
	h.logger.InfoContext(ctx, "push refused", "repo", id, "code", contract.CodeInvalid, "error", err, "subject", auth.Subject(ctx))
	return refuseInvalid, "reject " + refuseInvalid.Line()
}

// streamVerdict measures the push against spec 012's bounds and commits
// it. The size measurement runs here rather than before git, because a
// stream carries no length: the pack's size is known once the client
// has sent it, which is when the hook fires. A refusal is still the
// hook's verdict, so no entry is written and the client reads the code
// and the sentence in the sideband.
func (h *Handler) streamVerdict(ctx context.Context, id string, rp *repo.Repo, quota int64, refs []wal.RefUpdate, req *receiveRequest, spoolPath string, committed **wal.Committed) string {
	over, err := h.overQuota(ctx, id, rp, req, quota)
	if err != nil {
		h.logger.ErrorContext(ctx, "quota not measured", "repo", id, "error", err)
		return "reject " + contract.Line(contract.CodeStorageUnavailable)
	}
	if over != nil {
		over.log(ctx, h.logger, id, auth.Subject(ctx))
		h.rejected.Inc(nil)
		return "reject " + contract.Line(contract.CodeOverQuota)
	}
	c, verdict := h.commit(ctx, id, rp, refs, req, spoolPath)
	*committed = c
	return verdict
}

// streamCommand builds a git service subprocess against the repository
// under the request deadline. No environment variable a client sent
// reaches it (spec 016): a stream transport refuses the env request, so
// the protocol version is the one git falls back to.
func (h *Handler) streamCommand(ctx context.Context, rp *repo.Repo, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	// The span is the subprocess: the caller defers the cancel this
	// returns and waits for the process before it runs.
	ctx, endSpan := repo.Span(ctx, args)
	cmd := h.cache.Git().Command(ctx, rp.Dir, args...)
	// The hook is a grandchild; killing the group on cancel takes it too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, func() { endSpan(); cancel() }
}

// spoolTee copies the client's bytes to the spool and then to git, in
// that order, so everything git has consumed is already on disk. It
// stops one byte past the single-push bound of spec 012 and closes
// git's input, which ends the push before the pack is indexed.
type spoolTee struct {
	spool  *os.File
	git    io.WriteCloser
	client io.Reader
	max    int64

	mu      sync.Mutex
	written int64
	over    bool

	stopped chan struct{}
}

func newSpoolTee(spool *os.File, git io.WriteCloser, client io.Reader, max int64) *spoolTee {
	return &spoolTee{spool: spool, git: git, client: client, max: max, stopped: make(chan struct{})}
}

func (t *spoolTee) run() {
	defer close(t.stopped)
	defer func() { _ = t.git.Close() }()
	buf := make([]byte, 64<<10)
	for {
		n, err := t.client.Read(buf)
		if n > 0 {
			if !t.write(buf[:n]) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// write records the bytes and hands them on, refusing the push one byte
// past the bound.
func (t *spoolTee) write(p []byte) bool {
	t.mu.Lock()
	if t.written+int64(len(p)) > t.max {
		t.over = true
		t.mu.Unlock()
		return false
	}
	n, err := t.spool.Write(p)
	t.written += int64(n)
	t.mu.Unlock()
	if err != nil {
		return false
	}
	_, err = t.git.Write(p)
	return err == nil
}

// size is how many bytes of the client's stream are on disk. It is read
// while run may still be writing, so it is the snapshot a parse is
// measured against.
func (t *spoolTee) size() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.written
}

// tooLarge reports whether the push crossed the single-push bound.
func (t *spoolTee) tooLarge() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.over
}

// section is the first n bytes of the spool as a reader of its own. It
// reads at an offset, so the file position the tee appends at is never
// moved by the parse.
func (t *spoolTee) section(n int64) io.ReadSeeker { return io.NewSectionReader(t.spool, 0, n) }
