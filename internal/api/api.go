// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package api serves the repository lifecycle of spec 003 under
// /v1/repos: create with a caller-chosen id, read, rename, delete with
// a hold, and undelete, the repository-bound tokens of spec 007, and
// the read API of spec 009: refs, commits, compare, tree, blob, and the
// archive, each served by git from the local copy after the currency
// check. Every request is authorized before the repository is looked up.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// Options configures the handler.
type Options struct {
	Cache  *repo.Cache
	Logger *slog.Logger
	// Guard decides every request; required.
	Guard *auth.Guard
	// Signer mints repository-bound tokens; required.
	Signer *auth.Signer
	// Placement answers Origo-Prefer (spec 005); the node's live set.
	// Nil writes no header.
	Placement placement.Placer
	// ReadTimeout is the budget of the git subprocesses of one read
	// request; DefaultReadTimeout when zero.
	ReadTimeout time.Duration
	// Events carries the push event of a default_branch change and the
	// undeleted event of an undelete (spec 008); nil, or one with no
	// sink, sends nothing.
	Events *events.Dispatcher
	// Limits holds the subprocess semaphore of spec 012, taken once per
	// read request and held across its subprocesses; one of its own,
	// with the spec's defaults, when nil.
	Limits *limits.Limits
}

// Handler serves /v1/repos.
type Handler struct {
	cache     *repo.Cache
	log       *wal.Log
	logger    *slog.Logger
	guard     *auth.Guard
	signer    *auth.Signer
	events    *events.Dispatcher
	placement placement.Placer
	limits    *limits.Limits

	readTimeout time.Duration
}

// New builds the handler.
func New(o Options) *Handler {
	if o.Guard == nil || o.Signer == nil {
		panic("api: the handler needs a guard and a signer")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := o.ReadTimeout
	if timeout == 0 {
		timeout = DefaultReadTimeout
	}
	bounds := o.Limits
	if bounds == nil {
		bounds = limits.New(limits.Options{Log: o.Cache.Log(), Logger: logger})
	}
	return &Handler{cache: o.Cache, log: o.Cache.Log(), logger: logger, guard: o.Guard, signer: o.Signer, placement: o.Placement, readTimeout: timeout, events: o.Events, limits: bounds}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/repos", h.create)
	mux.HandleFunc("GET /v1/repos/{id}", h.get)
	mux.HandleFunc("PATCH /v1/repos/{id}", h.patch)
	mux.HandleFunc("DELETE /v1/repos/{id}", h.delete)
	mux.HandleFunc("POST /v1/repos/{id}/undelete", h.undelete)
	mux.HandleFunc("POST /v1/repos/{id}/tokens", h.tokens)
	h.registerRead(mux)
}

// Repository is the representation spec 003 fixes.
type Repository struct {
	ID            string    `json:"id"`
	Owner         string    `json:"owner"`
	Slug          string    `json:"slug"`
	DefaultBranch string    `json:"default_branch"`
	SizeBytes     int64     `json:"size_bytes"`
	Head          string    `json:"head"`
	UpdatedAt     time.Time `json:"updated_at"`
	// PushedAt is the time of the newest push, from the index object
	// the node holds (spec 009); null for a repository with no push.
	PushedAt *time.Time `json:"pushed_at"`
}

// reservedOwners are path prefixes the public surface uses itself.
var reservedOwners = map[string]bool{"r": true, "v1": true}

type createRequest struct {
	ID            string `json:"id"`
	Owner         string `json:"owner"`
	Slug          string `json:"slug"`
	DefaultBranch string `json:"default_branch"`
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// invalid answers 400 with the validation failure in the developer
// register and the field at fault when there is one.
func invalid(w http.ResponseWriter, reason, field string) {
	details := map[string]any{"reason": reason}
	if field != "" {
		details["field"] = field
	}
	contract.Write(w, http.StatusBadRequest, contract.CodeInvalid, details)
}

func notFound(w http.ResponseWriter, id string) {
	contract.Write(w, http.StatusNotFound, contract.CodeRepoNotFound, map[string]any{"id": id})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decode(r, &req); err != nil {
		invalid(w, "body: "+err.Error(), "")
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}
	switch {
	case !wal.ValidID(req.ID):
		invalid(w, "id must be a lower-case UUID", "id")
		return
	case !wal.ValidLabel(req.Owner) || reservedOwners[req.Owner]:
		invalid(w, "owner must be a URL-safe label and not a reserved path segment", "owner")
		return
	case !wal.ValidLabel(req.Slug):
		invalid(w, "slug must be a URL-safe label", "slug")
		return
	case !wal.ValidRefName("refs/heads/" + req.DefaultBranch):
		invalid(w, "default_branch is not a valid branch name", "default_branch")
		return
	}
	// The authorizer sees the id, owner, and slug the body names.
	if !h.guard.Allow(w, r, auth.RepoRef{ID: req.ID, Owner: req.Owner, Slug: req.Slug}, auth.ActionAdmin) {
		return
	}
	ix, err := h.log.CreateRepo(r.Context(), wal.Meta{ID: req.ID, Owner: req.Owner, Slug: req.Slug}, req.DefaultBranch)
	if err != nil {
		switch {
		case errors.Is(err, wal.ErrExists):
			contract.Write(w, http.StatusConflict, contract.CodeRepoExists, map[string]any{"field": "id", "id": req.ID})
		case errors.Is(err, wal.ErrNameTaken):
			contract.Write(w, http.StatusConflict, contract.CodeRepoExists, map[string]any{"field": "name", "owner": req.Owner, "slug": req.Slug})
		default:
			h.storageError(w, r, err)
		}
		return
	}
	m, err := h.log.ReadMeta(r.Context(), req.ID)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, represent(m, ix))
}

func represent(m *wal.Meta, ix *wal.Index) Repository {
	branch := ix.DefaultBranch()
	return Repository{
		ID: m.ID, Owner: m.Owner, Slug: m.Slug, DefaultBranch: branch,
		SizeBytes: ix.SizeBytes, Head: ix.Refs["refs/heads/"+branch], UpdatedAt: m.UpdatedAt, PushedAt: ix.PushedAt,
	}
}

// admit is the guard on the id the path names, with Origo-Prefer (spec
// 005) on the response whatever the status: the id is the path's, so
// the header is set before the guard with k = 1 and again with the
// allow's replicas.
func (h *Handler) admit(w http.ResponseWriter, r *http.Request, id string, action auth.Action) bool {
	placement.SetHeader(w.Header(), h.placement, id, auth.DefaultReplicas)
	d, ok := h.guard.Admit(w, r, auth.RepoRef{ID: id}, action)
	if ok {
		placement.SetHeader(w.Header(), h.placement, id, d.Replicas)
	}
	return ok
}

// load authorizes the action on the id the path names and only then
// reads the metadata and the newest index of the repository (spec 007,
// authorization before lookup).
func (h *Handler) load(w http.ResponseWriter, r *http.Request, action auth.Action, allowDeleted bool) (*wal.Meta, *wal.Index, bool) {
	id := r.PathValue("id")
	if !h.admit(w, r, id, action) {
		return nil, nil, false
	}
	m, err := h.log.ReadMeta(r.Context(), id)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			notFound(w, id)
		} else {
			h.storageError(w, r, err)
		}
		return nil, nil, false
	}
	ix, _, err := h.log.Newest(r.Context(), id, 0, false)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			notFound(w, id)
		} else {
			h.storageError(w, r, err)
		}
		return nil, nil, false
	}
	if ix.DeletedAt != nil && !allowDeleted {
		notFound(w, id)
		return nil, nil, false
	}
	return m, ix, true
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	m, ix, ok := h.load(w, r, auth.ActionRead, false)
	if !ok {
		return
	}
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

type patchRequest struct {
	Owner         *string `json:"owner"`
	Slug          *string `json:"slug"`
	DefaultBranch *string `json:"default_branch"`
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	var req patchRequest
	if err := decode(r, &req); err != nil {
		invalid(w, "body: "+err.Error(), "")
		return
	}
	m, ix, ok := h.load(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	if req.Owner != nil || req.Slug != nil {
		owner, slug := m.Owner, m.Slug
		if req.Owner != nil {
			owner = *req.Owner
		}
		if req.Slug != nil {
			slug = *req.Slug
		}
		if !wal.ValidLabel(owner) || reservedOwners[owner] {
			invalid(w, "owner must be a URL-safe label and not a reserved path segment", "owner")
			return
		}
		if !wal.ValidLabel(slug) {
			invalid(w, "slug must be a URL-safe label", "slug")
			return
		}
		renamed, err := h.log.Rename(r.Context(), m.ID, owner, slug)
		if err != nil {
			if errors.Is(err, wal.ErrNameTaken) {
				contract.Write(w, http.StatusConflict, contract.CodeRepoExists, map[string]any{"field": "name", "owner": owner, "slug": slug})
			} else {
				h.storageError(w, r, err)
			}
			return
		}
		m = renamed
	}
	if req.DefaultBranch != nil && *req.DefaultBranch != ix.DefaultBranch() {
		if !wal.ValidRefName("refs/heads/" + *req.DefaultBranch) {
			invalid(w, "default_branch is not a valid branch name", "default_branch")
			return
		}
		// HEAD moves through the log like any reference, so every node
		// applies it on its next currency check.
		entry := wal.Entry{Kind: wal.KindPush, Subject: auth.Subject(r.Context()), Actor: auth.Actor(r.Context()), Refs: []wal.RefUpdate{{
			Ref: "HEAD", Old: ix.Refs["HEAD"], New: "ref: refs/heads/" + *req.DefaultBranch,
		}}}
		c, err := h.log.Commit(r.Context(), m.ID, ix, entry, noCatchUp)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		ix = c.Index
		// One push event with the single HEAD update (spec 008); a
		// failed enqueue is logged by the dispatcher and repaired.
		_ = h.events.Enqueue(r.Context(), m.ID, events.Entry{Header: c.Header, Refs: entry.Refs})
	}
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

func noCatchUp(context.Context, *wal.Index) error { return nil }

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	m, ix, ok := h.load(w, r, auth.ActionAdmin, true)
	if !ok {
		return
	}
	if ix.DeletedAt == nil {
		entry := wal.Entry{Kind: wal.KindDelete, Deleted: true, Subject: auth.Subject(r.Context()), Actor: auth.Actor(r.Context())}
		c, err := h.log.Commit(r.Context(), m.ID, ix, entry, noCatchUp)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		ix = c.Index
	}
	// Nodes evict the local copy at once and answer 404 from here on.
	h.cache.Evict(m.ID)
	httpjson.Write(w, http.StatusAccepted, map[string]any{
		"id": m.ID, "deleted_at": ix.DeletedAt, "purge_after": ix.DeletedAt.Add(wal.DeleteHold),
	})
}

func (h *Handler) undelete(w http.ResponseWriter, r *http.Request) {
	m, ix, ok := h.load(w, r, auth.ActionAdmin, true)
	if !ok {
		return
	}
	if ix.DeletedAt != nil {
		entry := wal.Entry{Kind: wal.KindPush, Subject: auth.Subject(r.Context()), Actor: auth.Actor(r.Context())}
		c, err := h.log.Commit(r.Context(), m.ID, ix, entry, noCatchUp)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		ix = c.Index
		// The undelete's push entry produces no push event, from the
		// enqueue or from the repair sweep; its one event is spec 019's
		// undeleted, emitted after the write and before the response.
		_ = h.events.Enqueue(r.Context(), m.ID, events.Entry{Header: c.Header})
		_ = h.events.Emit(r.Context(), m.ID, "undeleted", c.Header.At, events.Pusher{Sub: entry.Subject, Actor: entry.Actor}, nil)
	}
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

type tokenRequest struct {
	Scope string `json:"scope"`
	TTL   int    `json:"ttl"`
}

// tokens mints a repository-bound token (spec 007): action admin, then
// the repository must exist, then the signer's own checks on the scope
// and the lifetime.
func (h *Handler) tokens(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := decode(r, &req); err != nil {
		invalid(w, "body: "+err.Error(), "")
		return
	}
	scope := auth.Scope(req.Scope)
	if scope != auth.ScopeRead && scope != auth.ScopeWrite {
		invalid(w, "scope must be read or write", "scope")
		return
	}
	ttl := time.Duration(req.TTL) * time.Second
	if ttl < auth.MinTokenTTL || ttl > auth.MaxTokenTTL {
		invalid(w, "ttl must be 1 to 3600 seconds", "ttl")
		return
	}
	m, _, ok := h.load(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	token, expires, err := h.signer.Mint(auth.FromContext(r.Context()), m.ID, scope, ttl)
	if err != nil {
		invalid(w, err.Error(), "")
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"token": token, "expires_at": expires})
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
	h.logger.ErrorContext(r.Context(), "repository operation failed", "path", r.URL.Path, "error", err)
	h.retryAfter(w, err)
	contract.Write(w, http.StatusServiceUnavailable, contract.CodeStorageUnavailable, wal.ErrorDetails(err))
}

// retryAfter sets Retry-After to the whole seconds that remain of the
// open window of the breaker that refused the call (spec 015). The
// class comes from the operation the error names, never from an
// assumption: a create refused while only the write breaker is open
// would otherwise read the closed read breaker, whose window is zero,
// and answer no header at all.
func (h *Handler) retryAfter(w http.ResponseWriter, err error) {
	bs := h.log.Breakers()
	if bs == nil || !errors.Is(err, wal.ErrStorageOpen) {
		return
	}
	if d := bs.RetryAfter(wal.ClassOf(err)); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d/time.Second)))
	}
}
