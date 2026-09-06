// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package api serves the repository lifecycle of spec 003 under
// /v1/repos: create with a caller-chosen id, read, rename, delete with
// a hold, and undelete. Every other read operation of spec 009 lands
// with that spec.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// Handler serves /v1/repos.
type Handler struct {
	cache  *repo.Cache
	log    *wal.Log
	logger *slog.Logger
}

// New builds the handler.
func New(cache *repo.Cache, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{cache: cache, log: cache.Log(), logger: logger}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/repos", h.create)
	mux.HandleFunc("GET /v1/repos/{id}", h.get)
	mux.HandleFunc("PATCH /v1/repos/{id}", h.patch)
	mux.HandleFunc("DELETE /v1/repos/{id}", h.delete)
	mux.HandleFunc("POST /v1/repos/{id}/undelete", h.undelete)
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

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decode(r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "body: " + err.Error()})
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}
	switch {
	case !wal.ValidID(req.ID):
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "id must be a lower-case UUID"})
		return
	case !wal.ValidLabel(req.Owner) || reservedOwners[req.Owner] || !wal.ValidLabel(req.Slug):
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "owner and slug must be URL-safe labels"})
		return
	case !wal.ValidRefName("refs/heads/" + req.DefaultBranch):
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "default_branch is not a valid branch name"})
		return
	}
	ix, err := h.log.CreateRepo(r.Context(), wal.Meta{ID: req.ID, Owner: req.Owner, Slug: req.Slug}, req.DefaultBranch)
	if err != nil {
		switch {
		case errors.Is(err, wal.ErrExists):
			httpjson.WriteError(w, http.StatusConflict, httpjson.Error{Code: contract.CodeRepoExists, Message: "a repository with this id exists"})
		case errors.Is(err, wal.ErrNameTaken):
			httpjson.WriteError(w, http.StatusConflict, httpjson.Error{Code: contract.CodeRepoExists, Message: "a repository with this owner and slug exists"})
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
		SizeBytes: ix.SizeBytes, Head: ix.Refs["refs/heads/"+branch], UpdatedAt: m.UpdatedAt,
	}
}

// load reads the metadata and the newest index of a live repository.
func (h *Handler) load(w http.ResponseWriter, r *http.Request, allowDeleted bool) (*wal.Meta, *wal.Index, bool) {
	id := r.PathValue("id")
	m, err := h.log.ReadMeta(r.Context(), id)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			httpjson.WriteError(w, http.StatusNotFound, httpjson.Error{Code: contract.CodeRepoNotFound, Message: "repository not found"})
		} else {
			h.storageError(w, r, err)
		}
		return nil, nil, false
	}
	ix, _, err := h.log.Newest(r.Context(), id, 0, false)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			httpjson.WriteError(w, http.StatusNotFound, httpjson.Error{Code: contract.CodeRepoNotFound, Message: "repository not found"})
		} else {
			h.storageError(w, r, err)
		}
		return nil, nil, false
	}
	if ix.DeletedAt != nil && !allowDeleted {
		httpjson.WriteError(w, http.StatusNotFound, httpjson.Error{Code: contract.CodeRepoNotFound, Message: "repository not found"})
		return nil, nil, false
	}
	return m, ix, true
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	m, ix, ok := h.load(w, r, false)
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
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "body: " + err.Error()})
		return
	}
	m, ix, ok := h.load(w, r, false)
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
		if !wal.ValidLabel(owner) || reservedOwners[owner] || !wal.ValidLabel(slug) {
			httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "owner and slug must be URL-safe labels"})
			return
		}
		renamed, err := h.log.Rename(r.Context(), m.ID, owner, slug)
		if err != nil {
			if errors.Is(err, wal.ErrNameTaken) {
				httpjson.WriteError(w, http.StatusConflict, httpjson.Error{Code: contract.CodeRepoExists, Message: "a repository with this owner and slug exists"})
			} else {
				h.storageError(w, r, err)
			}
			return
		}
		m = renamed
	}
	if req.DefaultBranch != nil && *req.DefaultBranch != ix.DefaultBranch() {
		if !wal.ValidRefName("refs/heads/" + *req.DefaultBranch) {
			httpjson.WriteError(w, http.StatusBadRequest, httpjson.Error{Code: contract.CodeInvalid, Message: "default_branch is not a valid branch name"})
			return
		}
		// HEAD moves through the log like any reference, so every node
		// applies it on its next currency check.
		entry := wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{
			Ref: "HEAD", Old: ix.Refs["HEAD"], New: "ref: refs/heads/" + *req.DefaultBranch,
		}}}
		c, err := h.log.Commit(r.Context(), m.ID, ix, entry, noCatchUp)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		ix = c.Index
	}
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

func noCatchUp(context.Context, *wal.Index) error { return nil }

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	m, ix, ok := h.load(w, r, true)
	if !ok {
		return
	}
	if ix.DeletedAt == nil {
		c, err := h.log.Commit(r.Context(), m.ID, ix, wal.Entry{Kind: wal.KindDelete, Deleted: true}, noCatchUp)
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
	m, ix, ok := h.load(w, r, true)
	if !ok {
		return
	}
	if ix.DeletedAt != nil {
		c, err := h.log.Commit(r.Context(), m.ID, ix, wal.Entry{Kind: wal.KindPush}, noCatchUp)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		ix = c.Index
	}
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

func (h *Handler) storageError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "repository operation failed", "path", r.URL.Path, "error", err)
	httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.Error{Code: contract.CodeStorageUnavailable, Message: "repository unavailable"})
}
