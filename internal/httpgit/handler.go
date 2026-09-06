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
	"strings"
	"syscall"
	"time"

	"latere.ai/x/pkg/httpjson"
	"latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// Options configures the handler.
type Options struct {
	Cache *repo.Cache
	// Timeout bounds one git subprocess. 5 minutes by default.
	Timeout time.Duration
	Logger  *slog.Logger
	Metrics *metrics.Registry
}

// Handler serves the smart HTTP routes.
type Handler struct {
	cache   *repo.Cache
	log     *wal.Log
	timeout time.Duration
	logger  *slog.Logger

	pushes   *metrics.Counter
	rejected *metrics.Counter
	fetches  *metrics.Counter
}

// New builds the handler.
func New(o Options) *Handler {
	h := &Handler{cache: o.Cache, log: o.Cache.Log(), timeout: o.Timeout, logger: o.Logger}
	if h.timeout == 0 {
		h.timeout = 5 * time.Minute
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	reg := o.Metrics
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	h.pushes = reg.Counter("origo_pushes_total", "pushes acknowledged")
	h.rejected = reg.Counter("origo_pushes_rejected_total", "pushes refused by the log")
	h.fetches = reg.Counter("origo_fetches_total", "upload-pack requests served")
	for _, c := range []*metrics.Counter{h.pushes, h.rejected, h.fetches} {
		c.Add(nil, 0) // the series reads 0 before the first event
	}
	return h
}

// Register mounts the routes in both URL forms spec 003 names: the id
// form /r/<id>.git and the label form /<owner>/<slug>.git.
func (h *Handler) Register(mux *http.ServeMux) {
	for _, prefix := range []string{"/r/{id}", "/{owner}/{slug}"} {
		mux.HandleFunc("GET "+prefix+"/info/refs", h.infoRefs)
		mux.HandleFunc("POST "+prefix+"/git-upload-pack", h.uploadPack)
		mux.HandleFunc("POST "+prefix+"/git-receive-pack", h.receivePack)
	}
}

// resolve maps the request path to a repository id.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := r.PathValue("id"); id != "" {
		return strings.TrimSuffix(id, ".git"), true
	}
	id, err := h.log.Resolve(r.Context(), r.PathValue("owner"), strings.TrimSuffix(r.PathValue("slug"), ".git"))
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			httpjson.WriteError(w, http.StatusNotFound, httpjson.Error{Code: contract.CodeRepoNotFound, Message: "repository not found"})
		} else {
			h.storageError(w, r, err)
		}
		return "", false
	}
	return id, true
}

// acquire opens the repository for the request and maps the refusals.
func (h *Handler) acquire(w http.ResponseWriter, r *http.Request, id string, write bool) (*repo.Repo, func(), bool) {
	rp, release, err := h.cache.Acquire(r.Context(), id, write)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrDeleted):
			httpjson.WriteError(w, http.StatusNotFound, httpjson.Error{Code: contract.CodeRepoNotFound, Message: "repository not found"})
		default:
			h.storageError(w, r, err)
		}
		return nil, nil, false
	}
	return rp, release, true
}

func (h *Handler) storageError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "repository unavailable", "path", r.URL.Path, "error", err)
	httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.Error{Code: contract.CodeStorageUnavailable, Message: "repository unavailable"})
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
	if service != "git-upload-pack" && service != "git-receive-pack" {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "smart HTTP only: service must be git-upload-pack or git-receive-pack"})
		return
	}
	id, ok := h.resolve(w, r)
	if !ok {
		return
	}
	rp, release, ok := h.acquire(w, r, id, false)
	if !ok {
		return
	}
	defer release()
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
	id, ok := h.resolve(w, r)
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
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: err.Error()})
		return
	}
	defer closeBody()
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
	id, ok := h.resolve(w, r)
	if !ok {
		return
	}
	rp, release, ok := h.acquire(w, r, id, true)
	if !ok {
		return
	}
	defer release()

	spool, err := spoolBody(r, h.cache.SpoolDir())
	if err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: err.Error()})
		return
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	req, err := parseReceive(spool)
	if err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: err.Error()})
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
		refs []wal.RefUpdate
		ok   bool
		err  error
	}
	fromHook := make(chan updates, 1)
	hookDone := make(chan struct{})
	go func() {
		defer close(hookDone)
		refs, ok, err := ch.readUpdates()
		fromHook <- updates{refs, ok, err}
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var committed *wal.Committed
	var runErr error
	select {
	case u := <-fromHook:
		verdict := "reject origo: no reference updates"
		if u.err != nil {
			verdict = "reject origo: " + u.err.Error()
		} else if u.ok {
			committed, verdict = h.commit(r.Context(), id, rp, u.refs, req, spool.Name())
		}
		if err := ch.writeVerdict(r.Context(), verdict); err != nil {
			h.logger.WarnContext(r.Context(), "verdict not delivered", "repo", id, "error", err)
		}
		runErr = <-exited
	case runErr = <-exited:
		// git refused the push before the hook ran: bad objects, a
		// failed connectivity check, or a client that went away.
		ch.drain(hookDone)
		<-fromHook
	}
	if runErr != nil {
		h.logger.WarnContext(r.Context(), "receive-pack ended with an error", "repo", id, "error", runErr, "stderr", stderr.String())
	}
	if committed != nil {
		if runErr == nil {
			if err := h.cache.Advance(rp, committed.Index); err != nil {
				h.logger.ErrorContext(r.Context(), "local sequence not advanced", "repo", id, "error", err)
			}
		}
		h.pushes.Inc(nil)
		h.logger.InfoContext(r.Context(), "push", "repo", id, "seq", committed.Index.Seq, "refs", len(req.Commands), "pack_bytes", req.PackSize, "subject", auth.Subject(r.Context()))
	}
}

// commit writes the entry and creates the index object. It returns the
// verdict for the hook: "ok", or "reject <code>: <message>", which git
// relays to the client as the hook's stderr.
func (h *Handler) commit(ctx context.Context, id string, rp *repo.Repo, refs []wal.RefUpdate, req *receiveRequest, spoolPath string) (*wal.Committed, string) {
	pack, err := packBody(spoolPath, req.PackOffset, req.PackSize)
	if err != nil {
		return nil, "reject " + contract.CodeStorageUnavailable + ": " + err.Error()
	}
	entry := wal.Entry{
		Kind: wal.KindPush, Subject: auth.Subject(ctx), Refs: refs, Pack: pack,
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
