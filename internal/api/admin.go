// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/wal"
)

// The administration operations of spec 019 beyond the three spec 003
// owns. Each asks the authorizer for admin before it reads meta, writes
// its state, and emits its event through spec 008's dispatcher after
// the write and before the response.

// The event kinds this file emits.
const (
	KindRenamed     = "renamed"
	KindTransferred = "transferred"
	KindFrozen      = "frozen"
	KindUnfrozen    = "unfrozen"
	KindDeleted     = "deleted"
	KindUndeleted   = "undeleted"
)

// registerAdmin mounts the administration routes.
func (h *Handler) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/repos/{id}/transfer", h.transfer)
	mux.HandleFunc("POST /v1/repos/{id}/freeze", h.freeze)
	mux.HandleFunc("POST /v1/repos/{id}/unfreeze", h.unfreeze)
	mux.HandleFunc("GET /v1/repos/{id}/stats", h.stats)
	mux.HandleFunc("POST /v1/repos/{id}/gc", h.gc)
	mux.HandleFunc("GET /v1/repos/{id}/export.bundle", h.export)
	mux.HandleFunc("POST /v1/repos/{id}/import", h.startImport)
	mux.HandleFunc("GET /v1/repos/{id}/import", h.importState)
	mux.HandleFunc("POST /v1/repos/{id}/verify", h.verify)
}

// pusher is the identity of the caller, the field every event kind
// carries in the shape the push event uses.
func pusher(r *http.Request) events.Pusher {
	return events.Pusher{Sub: auth.Subject(r.Context()), Actor: auth.Actor(r.Context())}
}

// emit sends one event of the kind and logs a failure; the dispatcher
// records it and the operation's answer does not depend on it.
func (h *Handler) emit(r *http.Request, id, kind string, at time.Time, extra map[string]any) {
	if err := h.events.Emit(r.Context(), id, kind, at, pusher(r), extra); err != nil {
		h.logger.WarnContext(r.Context(), "event not emitted", "repo", id, "kind", kind, "error", err)
	}
}

// gone answers 410 for a repository whose hold has passed: the tombstone
// meta the purge left (spec 019) is the only object of it that remains.
func gone(w http.ResponseWriter, m *wal.Meta) {
	contract.Write(w, http.StatusGone, contract.CodeGone, map[string]any{"id": m.ID, "purged_at": m.PurgedAt})
}

// notFoundOrGone answers a repository the log holds no index for: 410
// gone when the tombstone of a purge is there, 404 otherwise. It is the
// one extra read of meta a purged repository costs, paid on the refusal
// alone, so a live request never pays for it.
func (h *Handler) notFoundOrGone(w http.ResponseWriter, r *http.Request, id string) {
	if m, err := h.log.ReadMeta(r.Context(), id); err == nil && m.PurgedAt != nil {
		gone(w, m)
		return
	}
	notFound(w, id)
}

// label is one side of a rename or a transfer in an event.
func label(owner, slug string) map[string]any {
	if slug == "" {
		return map[string]any{"owner": owner}
	}
	return map[string]any{"owner": owner, "slug": slug}
}

// rename moves the repository to the labels and emits the event of the
// change: renamed for PATCH, transferred for the transfer endpoint,
// which is the same operation recorded under the kind a consumer acts
// on without inspecting a rename. Nothing is emitted when neither label
// changes, because nothing changed.
func (h *Handler) rename(w http.ResponseWriter, r *http.Request, m *wal.Meta, owner, slug, kind string) (*wal.Meta, bool) {
	if !wal.ValidLabel(owner) || reservedOwners[owner] {
		invalid(w, "owner must be a URL-safe label and not a reserved path segment", "owner")
		return nil, false
	}
	if !wal.ValidLabel(slug) {
		invalid(w, "slug must be a URL-safe label", "slug")
		return nil, false
	}
	from := *m
	renamed, err := h.log.Rename(r.Context(), m.ID, owner, slug)
	if err != nil {
		if errors.Is(err, wal.ErrNameTaken) {
			contract.Write(w, http.StatusConflict, contract.CodeRepoExists, map[string]any{"field": "name", "owner": owner, "slug": slug})
		} else {
			h.storageError(w, r, err)
		}
		return nil, false
	}
	if from.Owner == renamed.Owner && from.Slug == renamed.Slug {
		return renamed, true
	}
	extra := map[string]any{"from": label(from.Owner, from.Slug), "to": label(renamed.Owner, renamed.Slug)}
	if kind == KindTransferred {
		extra = map[string]any{"from": label(from.Owner, ""), "to": label(renamed.Owner, "")}
	}
	h.emit(r, m.ID, kind, renamed.UpdatedAt, extra)
	return renamed, true
}

type transferRequest struct {
	Owner string `json:"owner"`
}

// transfer moves the repository to another owner. The id never changes,
// which is what makes it cheap: no object moves and every token, event,
// and clone by id keeps working.
func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := decode(r, &req); err != nil {
		invalid(w, "body: "+err.Error(), "")
		return
	}
	m, ix, ok := h.load(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	m, ok = h.rename(w, r, m, req.Owner, m.Slug, KindTransferred)
	if !ok {
		return
	}
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

// freeze stops the repository accepting writes. Reads go on: a frozen
// repository is one a consumer archived, not one it lost.
func (h *Handler) freeze(w http.ResponseWriter, r *http.Request) {
	m, ix, ok := h.load(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	if m.FrozenAt != nil {
		contract.Write(w, http.StatusConflict, contract.CodeRepoFrozen, map[string]any{"frozen_at": m.FrozenAt})
		return
	}
	at := h.now().UTC()
	m.FrozenAt = &at
	if err := h.log.WriteMeta(r.Context(), m); err != nil {
		h.storageError(w, r, err)
		return
	}
	h.emit(r, m.ID, KindFrozen, at, nil)
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

// unfreeze lets writes through again. It answers 200 whether or not the
// repository was frozen, so a consumer that lost track of the state
// reaches the state it asked for with one call.
func (h *Handler) unfreeze(w http.ResponseWriter, r *http.Request) {
	m, ix, ok := h.load(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	if m.FrozenAt == nil {
		httpjson.Write(w, http.StatusOK, represent(m, ix))
		return
	}
	m.FrozenAt = nil
	if err := h.log.WriteMeta(r.Context(), m); err != nil {
		h.storageError(w, r, err)
		return
	}
	h.emit(r, m.ID, KindUnfrozen, m.UpdatedAt, nil)
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}
