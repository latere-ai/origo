// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package httpgit serves git's smart HTTP protocol over subprocesses on
// the materialized repository: info/refs, git-upload-pack, and
// git-receive-pack. A push is captured on the way through: the packfile
// and the reference transaction become a log entry that is committed
// before git's own update runs, so the client is acknowledged only when
// the push is durable (spec 001, invariant 1).
//
// The subprocess plumbing, the body spooling, and the write-before-ack
// ordering follow Latere's data plane product's git handler, adapted to
// the log.
package httpgit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/tracing"
	"github.com/latere-ai/origo/internal/wal"
)

// Options configures the handler.
type Options struct {
	Cache *repo.Cache
	// Guard decides every request before the repository is looked up;
	// required.
	Guard *auth.Guard
	// Timeout bounds one git subprocess. 5 minutes by default.
	Timeout time.Duration
	Logger  *slog.Logger
	Metrics *metrics.Set
	// Events enqueues the push event of every committed push (spec
	// 008); nil, or one with no sink, enqueues nothing.
	Events *events.Dispatcher
	// Placement answers Origo-Prefer (spec 005); the node's live set.
	// Nil writes no header.
	Placement placement.Placer
	// Compaction sees the index of every push this node commits and
	// schedules a compaction when a threshold is crossed (spec 006);
	// nil triggers nothing.
	Compaction Compactor
	// BreakerPoll is how often a spooled push asks the write breaker
	// for admission while it waits (spec 015); DefaultBreakerPoll when
	// zero. A test lowers it.
	BreakerPoll time.Duration
	// Limits holds the subprocess semaphore, the single-push bound, and
	// the repository quota rule (spec 012); one of its own over the
	// log, with the spec's defaults, when nil.
	Limits *limits.Limits
}

// Compactor is spec 006's after-push trigger. It returns before the
// compaction starts, so a push is never held by one.
type Compactor interface {
	After(id string, ix *wal.Index)
}

// The wait of a spooled push for the write breaker (spec 015): the
// commit polls every DefaultBreakerPoll for at most BreakerWait, inside
// the 5 minute deadline of receive-pack.
const (
	DefaultBreakerPoll = 500 * time.Millisecond
	BreakerWait        = 60 * time.Second
)

// Handler serves the smart HTTP routes.
type Handler struct {
	cache       *repo.Cache
	log         *wal.Log
	guard       *auth.Guard
	placement   placement.Placer
	timeout     time.Duration
	breakerPoll time.Duration
	logger      *slog.Logger

	events   *events.Dispatcher
	compact  Compactor
	limits   *limits.Limits
	pushes   *pkgmetrics.Counter
	rejected *pkgmetrics.Counter
	fetches  *pkgmetrics.Counter
	phases   *pkgmetrics.Histogram

	// beforeVerdict, when set, sees the quarantine and the forced flags
	// after they are computed and before the verdict is written; a test
	// asserts there that the objects are still in quarantine.
	beforeVerdict func(quarantine string, forced map[string]bool)
	// beforeCommit, when set, runs once the pack is spooled and the
	// hook has handed over the transaction, before the commit asks the
	// write breaker; a test opens the breaker there (spec 015).
	beforeCommit func()
}

// pushPhases are the labels of origo_push_duration_seconds (spec 011):
// receive is the request until the hook hands over the transaction,
// entry and index the two writes of the commit, apply the verdict
// through git's own reference update and the local advance.
const (
	phaseReceive = "receive"
	phaseEntry   = "entry"
	phaseIndex   = "index"
	phaseApply   = "apply"
)

// New builds the handler.
func New(o Options) *Handler {
	if o.Guard == nil {
		panic("httpgit: the handler needs a guard")
	}
	h := &Handler{cache: o.Cache, log: o.Cache.Log(), guard: o.Guard, placement: o.Placement, timeout: o.Timeout, breakerPoll: o.BreakerPoll, logger: o.Logger, events: o.Events, compact: o.Compaction, limits: o.Limits}
	if h.timeout == 0 {
		h.timeout = 5 * time.Minute
	}
	if h.breakerPoll <= 0 {
		h.breakerPoll = DefaultBreakerPoll
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	if h.limits == nil {
		h.limits = limits.New(limits.Options{Log: h.log, Metrics: set, Logger: h.logger})
	}
	h.pushes, h.rejected, h.fetches, h.phases = set.Pushes, set.PushesRejected, set.Fetches, set.PushDuration
	return h
}

// Register mounts the routes in both URL forms spec 003 names: the id
// form /r/<id>.git and the label form /<owner>/<slug>.git. The label
// form is one wildcard route dispatched by byName: a pattern
// /{owner}/{slug}/info/refs and the read API's /v1/repos/{id}/refs both
// match /v1/repos/info/refs with neither more specific, which the mux
// refuses, while every /v1/ route is more specific than the wildcard.
// The owners r and v1 are reserved for the same reason (spec 003).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /r/{id}/info/refs", h.infoRefs)
	mux.HandleFunc("POST /r/{id}/git-upload-pack", h.uploadPack)
	mux.HandleFunc("POST /r/{id}/git-receive-pack", h.receivePack)
	mux.HandleFunc("/{owner}/{slug}/{service...}", h.byName)
}

// byName dispatches the label form on the method and the service, and
// answers what the unknown-route handler answers to anything else.
func (h *Handler) byName(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.PathValue("service") {
	case "GET info/refs":
		h.infoRefs(w, r)
	case "POST git-upload-pack":
		h.uploadPack(w, r)
	case "POST git-receive-pack":
		h.receivePack(w, r)
	default:
		contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, map[string]any{"reason": "no such route"})
	}
}

// resolve maps the request path to a repository and asks the guard for
// the action before anything of the repository is read (spec 007,
// authorization before lookup). The id form sends the id from the path;
// the name form first resolves the name, because an authorizer keys on
// the id, and sends the owner and slug alone when it did not resolve.
// So a deny is 403 whether or not the repository exists, and 404 is
// answered only to a caller the authorizer allowed.
//
// Origo-Prefer (spec 005) goes on every response that names a
// repository: in the id form the id is the path's, so the header is
// set before the guard with k = 1 and again with the allow's replicas;
// in the name form it is set once the name resolved and the guard
// allowed, so a refused caller learns nothing about the name.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, action auth.Action) (string, bool) {
	id, _, ok := h.decide(w, r, action)
	return id, ok
}

// decide is resolve with the authorizer's decision behind the allow,
// for the push path, which reads quota_bytes off it (spec 012).
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, action auth.Action) (string, auth.Decision, bool) {
	var ref auth.RepoRef
	if id := r.PathValue("id"); id != "" {
		ref.ID = strings.TrimSuffix(id, ".git")
		placement.SetHeader(w.Header(), h.placement, ref.ID, auth.DefaultReplicas)
	} else {
		ref.Owner, ref.Slug = r.PathValue("owner"), strings.TrimSuffix(r.PathValue("slug"), ".git")
		id, err := h.log.Resolve(r.Context(), ref.Owner, ref.Slug)
		if err != nil && !errors.Is(err, wal.ErrNotFound) {
			h.storageError(w, r, err)
			return "", auth.Decision{}, false
		}
		ref.ID = id
	}
	d, ok := h.guard.Admit(w, r, ref, action)
	if !ok {
		return "", auth.Decision{}, false
	}
	if ref.ID == "" {
		contract.Write(w, http.StatusNotFound, contract.CodeRepoNotFound, map[string]any{"owner": ref.Owner, "slug": ref.Slug})
		return "", auth.Decision{}, false
	}
	placement.SetHeader(w.Header(), h.placement, ref.ID, d.Replicas)
	return ref.ID, d, true
}

// acquire opens the repository for the request and maps the refusals.
// A read served without a currency check while the read breaker is
// open carries Origo-Stale, the whole seconds since the last check that
// answered (spec 015).
func (h *Handler) acquire(w http.ResponseWriter, r *http.Request, id string, write bool) (*repo.Repo, func(), bool) {
	l, err := h.cache.Lease(r.Context(), id, write)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrDeleted):
			contract.Write(w, http.StatusNotFound, contract.CodeRepoNotFound, map[string]any{"id": id})
		default:
			h.storageError(w, r, err)
		}
		return nil, nil, false
	}
	if l.Stale {
		w.Header().Set(contract.HeaderStale, strconv.Itoa(int(l.StaleFor/time.Second)))
	}
	return l.Repo, l.Release, true
}

// storageError answers a failure of the log: 503 repository_unavailable
// naming the key for an integrity error of the log (spec 015), 503
// storage_unavailable with the op, the key, and the error otherwise,
// with Retry-After when a breaker refused the call.
func (h *Handler) storageError(w http.ResponseWriter, r *http.Request, err error) {
	if ie, ok := errors.AsType[*wal.IntegrityError](err); ok {
		h.logger.ErrorContext(r.Context(), "repository unavailable until restored", "path", r.URL.Path, "key", ie.Key, "error", ie.Err)
		contract.Write(w, http.StatusServiceUnavailable, contract.CodeRepositoryUnavailable, map[string]any{"key": ie.Key, "error": ie.Err.Error()})
		return
	}
	h.logger.ErrorContext(r.Context(), "repository unavailable", "path", r.URL.Path, "error", err)
	if errors.Is(err, wal.ErrStorageOpen) {
		h.retryAfter(w, wal.ClassRead)
	}
	contract.Write(w, http.StatusServiceUnavailable, contract.CodeStorageUnavailable, wal.ErrorDetails(err))
}

// retryAfter sets Retry-After to the whole seconds that remain of the
// class's open window, at least one (spec 015).
func (h *Handler) retryAfter(w http.ResponseWriter, class wal.Class) {
	if bs := h.log.Breakers(); bs != nil {
		if d := bs.RetryAfter(class); d > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(d/time.Second)))
		}
	}
}

// refuseAdvertisement refuses a push at info/refs in the one form git
// shows the user (spec 015): HTTP 200 with the advertisement content
// type, Retry-After, and a body of the service line, a flush, and one
// ERR pkt-line carrying the code and its sentence, which git prints as
// "remote error: storage_unavailable: ..." and sends no pack after.
func (h *Handler) refuseAdvertisement(w http.ResponseWriter, service string, class wal.Class) {
	h.retryAfter(w, class)
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	_ = writePkt(w, "# service="+service+"\n")
	_ = flushPkt(w)
	_ = writePkt(w, "ERR "+contract.CodeStorageUnavailable+": "+contract.Sentence(contract.CodeStorageUnavailable)+"\n")
}

// gitCommand builds a git service subprocess against the repository
// with the client's protocol version and the request deadline.
func (h *Handler) gitCommand(ctx context.Context, r *http.Request, rp *repo.Repo, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	cmd := h.cache.Git().Command(ctx, rp.Dir, args...)
	if proto := r.Header.Get("Git-Protocol"); proto != "" {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+proto)
	}
	// The hook is a grandchild; killing the group on cancel takes it too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, cancel
}

// infoRefs advertises references for the requested service.
func (h *Handler) infoRefs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	action := auth.ActionRead
	switch service {
	case "git-upload-pack":
	case "git-receive-pack":
		action = auth.ActionWrite
	default:
		contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, map[string]any{"reason": "smart HTTP only: service must be git-upload-pack or git-receive-pack", "field": "service"})
		return
	}
	id, ok := h.resolve(w, r, action)
	if !ok {
		return
	}
	// A push is refused before the client uploads a pack when either
	// breaker is open (spec 015): the write breaker, asked here, and
	// the read breaker, whose refusal or stale copy the lease reports,
	// because a push needs a current index object as its base.
	if action == auth.ActionWrite {
		if bs := h.log.Breakers(); bs != nil && !bs.Admits(wal.ClassWrite) {
			h.refuseAdvertisement(w, service, wal.ClassWrite)
			return
		}
	}
	l, err := h.cache.Lease(r.Context(), id, false)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrDeleted):
			contract.Write(w, http.StatusNotFound, contract.CodeRepoNotFound, map[string]any{"id": id})
		case action == auth.ActionWrite && errors.Is(err, wal.ErrStorageOpen):
			h.refuseAdvertisement(w, service, wal.ClassRead)
		default:
			h.storageError(w, r, err)
		}
		return
	}
	defer l.Release()
	if l.Stale {
		if action == auth.ActionWrite {
			h.refuseAdvertisement(w, service, wal.ClassRead)
			return
		}
		w.Header().Set(contract.HeaderStale, strconv.Itoa(int(l.StaleFor/time.Second)))
	}
	rp := l.Repo
	slot, ok := h.limits.Slot(w, r)
	if !ok {
		return
	}
	defer slot()
	cmd, cancel := h.gitCommand(r.Context(), r, rp, strings.TrimPrefix(service, "git-"), "--stateless-rpc", "--advertise-refs", rp.Dir)
	defer cancel()
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		h.logger.ErrorContext(r.Context(), "advertise failed", "repo", id, "service", service, "error", err, "stderr", stderr.String())
		h.storageError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	_ = writePkt(w, "# service="+service+"\n")
	_ = flushPkt(w)
	_, _ = w.Write(out.Bytes())
}

// uploadPack serves a fetch or clone.
func (h *Handler) uploadPack(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolve(w, r, auth.ActionRead)
	if !ok {
		return
	}
	rp, release, ok := h.acquire(w, r, id, false)
	if !ok {
		return
	}
	defer release()
	body, closeBody, err := requestBody(r)
	if err != nil {
		contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, map[string]any{"reason": err.Error()})
		return
	}
	defer closeBody()
	slot, ok := h.limits.Slot(w, r)
	if !ok {
		return
	}
	defer slot()
	cmd, cancel := h.gitCommand(r.Context(), r, rp, "upload-pack", "--stateless-rpc", rp.Dir)
	defer cancel()
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = body, w, &stderr
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if err := cmd.Run(); err != nil {
		h.logger.ErrorContext(r.Context(), "upload-pack failed", "repo", id, "error", err, "stderr", stderr.String())
		return
	}
	h.fetches.Inc(nil)
}

// requestBody returns the request body, inflated when the client sent
// it compressed.
func requestBody(r *http.Request) (io.Reader, func(), error) {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return r.Body, func() {}, nil
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, func() {}, fmt.Errorf("gzip body: %w", err)
	}
	return gz, func() { _ = gz.Close() }, nil
}

// receivePack serves a push. The body is spooled and parsed for the
// pack, git receive-pack runs with the pre-receive hook, the hook hands
// over the transaction, the entry is committed, and only then does the
// hook let git update the references.
func (h *Handler) receivePack(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	id, decision, ok := h.decide(w, r, auth.ActionWrite)
	if !ok {
		return
	}
	// The first of the push's five spans (spec 011). It is the request
	// until the hook hands over the transaction; the two writes of the
	// commit and the enqueue open their own under the request's span.
	_, endReceive := tracing.Start(r.Context(), "receive", tracing.Repo(id))
	receiveEnded := false
	endReceiveOnce := func() {
		if !receiveEnded {
			receiveEnded = true
			endReceive()
		}
	}
	defer endReceiveOnce()
	rp, release, ok := h.acquire(w, r, id, true)
	if !ok {
		return
	}
	defer release()

	maxPush := h.limits.MaxPush()
	if r.ContentLength > maxPush {
		h.tooLarge(w, r, id, r.ContentLength, maxPush)
		return
	}
	spool, err := spoolBody(r, h.cache.SpoolDir(), maxPush)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			h.tooLarge(w, r, id, maxPush+1, maxPush)
			return
		}
		contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, map[string]any{"reason": err.Error()})
		return
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	req, err := parseReceive(spool)
	if err != nil {
		if errors.Is(err, errTooManyCommands) {
			h.logger.InfoContext(r.Context(), "push refused", "repo", id, "limit", limits.LimitRefs, "bytes", limits.MaxRefs+1, "max", limits.MaxRefs, "subject", auth.Subject(r.Context()))
			contract.Write(w, http.StatusRequestEntityTooLarge, contract.CodeOverQuota, map[string]any{
				"limit": limits.LimitRefs, "bytes": limits.MaxRefs + 1, "max": limits.MaxRefs,
			})
			return
		}
		contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, map[string]any{"reason": err.Error()})
		return
	}
	// The repository size rule of spec 012, measured before git runs:
	// what the log holds plus the bytes under lfs/ plus this pack, and
	// the references the index would hold after the push. A refusal
	// travels as the hook's verdict, so the client reads the code and
	// the sentence in the sideband and no entry is written.
	refusal, err := h.overQuota(r.Context(), id, rp, req, decision.QuotaBytes)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	if err := installHook(rp.Dir); err != nil {
		h.storageError(w, r, err)
		return
	}
	ch, err := newHookChannel(h.cache.SpoolDir())
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	defer ch.close()

	slot, ok := h.limits.Slot(w, r)
	if !ok {
		return
	}
	defer slot()

	cmd, cancel := h.gitCommand(r.Context(), r, rp, "receive-pack", "--stateless-rpc", rp.Dir)
	defer cancel()
	cmd.Env = append(cmd.Env, "ORIGO_HOOK_DIR="+ch.dir)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spool, w, &stderr
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if err := cmd.Start(); err != nil {
		h.storageError(w, r, err)
		return
	}

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
	go func() { exited <- cmd.Wait() }()

	var committed *wal.Committed
	var refs []wal.RefUpdate
	var forced map[string]bool
	var runErr error
	select {
	case u := <-fromHook:
		h.observe(phaseReceive, started)
		endReceiveOnce()
		refs = u.refs
		verdict := "reject origo: no reference updates"
		switch {
		case u.err != nil:
			verdict = "reject origo: " + u.err.Error()
		case u.ok && refusal != nil:
			refusal.log(r.Context(), h.logger, id, auth.Subject(r.Context()))
			verdict = "reject " + contract.CodeOverQuota + ": " + contract.Sentence(contract.CodeOverQuota)
		case u.ok:
			committed, verdict = h.commit(r.Context(), id, rp, u.refs, req, spool.Name())
		}
		started = time.Now()
		if committed != nil {
			h.phases.Observe(map[string]string{"phase": phaseEntry}, committed.EntryDuration.Seconds())
			h.phases.Observe(map[string]string{"phase": phaseIndex}, committed.IndexDuration.Seconds())
			// The pushed objects are still in quarantine: the forced
			// flag needs them and is known before the verdict (spec 008).
			forced = h.forcedUpdates(r.Context(), rp, u.refs, u.quarantine)
			if h.beforeVerdict != nil {
				h.beforeVerdict(u.quarantine, forced)
			}
		}
		if err := ch.writeVerdict(verdict); err != nil {
			h.logger.WarnContext(r.Context(), "verdict not delivered", "repo", id, "error", err)
		}
		runErr = <-exited
	case runErr = <-exited:
		// git refused the push before the hook ran: bad objects, a
		// failed connectivity check, or a client that went away. The
		// reader is ended with the terminator the hook never wrote.
		if err := ch.release(); err != nil {
			h.logger.WarnContext(r.Context(), "hook channel not released", "repo", id, "error", err)
		}
		<-fromHook
	}
	if runErr != nil {
		h.logger.WarnContext(r.Context(), "receive-pack ended with an error", "repo", id, "error", runErr, "stderr", stderr.String())
	}
	if committed != nil {
		_, endApply := tracing.Start(r.Context(), "apply", tracing.Repo(id))
		if runErr == nil {
			if err := h.cache.Advance(rp, committed.Index); err != nil {
				h.logger.ErrorContext(r.Context(), "local sequence not advanced", "repo", id, "error", err)
			}
		}
		h.observe(phaseApply, started)
		endApply()
		h.pushes.Inc(nil)
		h.logger.InfoContext(r.Context(), "push", "repo", id, "seq", committed.Index.Seq, "refs", len(req.Commands), "forced", len(forced), "pack_bytes", req.PackSize, "subject", auth.Subject(r.Context()), "actor", auth.Actor(r.Context()))
		// The entry is in the log whatever git did after the verdict, so
		// the event is enqueued for it; a failed enqueue is logged by the
		// dispatcher and the repair sweep covers the push.
		_ = h.events.Enqueue(r.Context(), id, events.Entry{Header: committed.Header, Refs: refs, Forced: forced})
		// The compaction trigger (spec 006) checks the thresholds
		// against the index this push produced and returns; the run, or
		// the request object on a node that is not the primary, happens
		// in the background.
		if h.compact != nil {
			h.compact.After(id, committed.Index)
		}
	}
}

// observe records a phase of the push that started at since.
func (h *Handler) observe(phase string, since time.Time) {
	h.phases.Observe(map[string]string{"phase": phase}, time.Since(since).Seconds())
}

// forcedUpdates reports which updates are not fast-forwards: one git
// merge-base --is-ancestor per update that moves an existing reference
// to a new object, with the quarantine as an alternate object store
// because the objects are not yet in the repository. Exit 0 is a
// fast-forward, 1 is forced; a create, a delete, and HEAD are never
// forced, and a git failure is logged and reads as not forced.
func (h *Handler) forcedUpdates(ctx context.Context, rp *repo.Repo, refs []wal.RefUpdate, quarantine string) map[string]bool {
	forced := map[string]bool{}
	for _, u := range refs {
		if u.Ref == "HEAD" || u.Old == wal.ZeroSHA || u.New == wal.ZeroSHA {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, h.timeout)
		cmd := h.cache.Git().Command(ctx, rp.Dir, "merge-base", "--is-ancestor", "--end-of-options", u.Old, u.New)
		if quarantine != "" {
			cmd.Env = append(cmd.Env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+quarantine)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			forced[u.Ref] = true
		default:
			h.logger.WarnContext(ctx, "forced flag not computed", "repo", rp.ID, "ref", u.Ref, "error", err, "stderr", stderr.String())
		}
	}
	return forced
}

// commit writes the entry and creates the index object. It returns the
// verdict for the hook: "ok", or "reject <code>: <message>", which git
// relays to the client as the hook's stderr.
func (h *Handler) commit(ctx context.Context, id string, rp *repo.Repo, refs []wal.RefUpdate, req *receiveRequest, spoolPath string) (*wal.Committed, string) {
	pack, err := packBody(spoolPath, req.PackOffset, req.PackSize)
	if err != nil {
		return nil, "reject " + contract.CodeStorageUnavailable + ": " + err.Error()
	}
	if h.beforeCommit != nil {
		h.beforeCommit()
	}
	// A pack spooled while the write breaker is open waits for the
	// breaker (spec 015): the commit polls for admission and runs as the
	// probe the first time it is admitted; when the wait passes with no
	// admission, or the probe fails below, the push is refused in the
	// sideband with the table's sentence.
	bs := h.log.Breakers()
	if bs != nil {
		if err := bs.Wait(ctx, wal.ClassWrite, h.breakerPoll, BreakerWait); err != nil {
			h.rejected.Inc(nil)
			h.logger.ErrorContext(ctx, "commit refused, the write breaker stayed open", "repo", id, "error", err)
			return nil, "reject " + contract.CodeStorageUnavailable + ": " + contract.Sentence(contract.CodeStorageUnavailable)
		}
	}
	entry := wal.Entry{
		Kind: wal.KindPush, Subject: auth.Subject(ctx), Actor: auth.Actor(ctx), Refs: refs, Pack: pack,
		PushOptions: req.Options,
	}
	committed, err := h.log.Commit(ctx, id, rp.Index, entry, func(ctx context.Context, ix *wal.Index) error {
		return h.cache.Apply(ctx, rp, ix)
	})
	if err != nil {
		h.rejected.Inc(nil)
		if conflict, ok := errors.AsType[*wal.ConflictError](err); ok {
			return nil, fmt.Sprintf("reject %s: %s moved to %s since you fetched; fetch first", contract.CodeNonFastForward, conflict.Ref, short(conflict.Actual))
		}
		h.logger.ErrorContext(ctx, "commit failed", "repo", id, "error", err)
		if errors.Is(err, wal.ErrStorageOpen) || (bs != nil && !bs.Admits(wal.ClassWrite)) {
			// Refused by the breaker, or the probe that failed and
			// reopened it: the sentence, as at the advertisement.
			return nil, "reject " + contract.CodeStorageUnavailable + ": " + contract.Sentence(contract.CodeStorageUnavailable)
		}
		return nil, "reject " + contract.CodeStorageUnavailable + ": the push was not recorded, retry"
	}
	return committed, "ok"
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// packBody is the pack section of the spooled body as a re-readable
// Body, hashed once.
func packBody(path string, offset, size int64) (wal.Body, error) {
	if size == 0 {
		return wal.Body{}, nil
	}
	open := func() (io.ReadCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
		return &sectionReader{Reader: io.LimitReader(f, size), f: f}, nil
	}
	rc, err := open()
	if err != nil {
		return wal.Body{}, err
	}
	defer func() { _ = rc.Close() }()
	sum := sha256.New()
	m := md5.New() //nolint:gosec // Content-MD5
	if _, err := io.Copy(io.MultiWriter(sum, m), rc); err != nil {
		return wal.Body{}, err
	}
	return wal.Body{
		Open: open, Size: size,
		SHA256: hex.EncodeToString(sum.Sum(nil)),
		MD5:    base64.StdEncoding.EncodeToString(m.Sum(nil)),
	}, nil
}

type sectionReader struct {
	io.Reader
	f *os.File
}

func (s *sectionReader) Close() error { return s.f.Close() }
