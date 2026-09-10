// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"strconv"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/wal"
)

// The collection route of spec 026, GET /v1/repos, in two modes. Neither
// mode takes a repository id, which is what every other route of this
// package is keyed by: the directory mode asks the authorizer which
// repositories the subject may see, and the name mode turns an
// <owner>/<slug> into the representation the id route serves.

// collectionPage is the directory mode's body. NextCursor is the
// authorizer's, passed through unread, and null on the last page.
type collectionPage struct {
	Repos      []Repository `json:"repos"`
	NextCursor *string      `json:"next_cursor"`
}

// collection dispatches on the query. The two modes are exclusive: a
// request that mixes them names both one repository and a page, which is
// two answers, so it is refused rather than resolved by precedence.
func (h *Handler) collection(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	owner, slug := q.Get("owner"), q.Get("slug")
	cursor, limit := q.Get("cursor"), q.Get("limit")
	if owner == "" && slug == "" {
		h.directory(w, r, cursor, limit)
		return
	}
	switch {
	case cursor != "" || limit != "":
		invalid(w, "modes", "")
	case owner == "":
		invalid(w, "owner and slug are given together", "owner")
	case slug == "":
		invalid(w, "owner and slug are given together", "slug")
	default:
		h.byName(w, r, owner, slug)
	}
}

// byName is the name mode. It resolves through the same index the git
// label form reads and then answers exactly as GET /v1/repos/{id} does,
// authorization before lookup included: the authorizer sees the owner
// and the slug whether or not the name resolved, a deny is 403 either
// way, and only an allowed caller learns that the name is free.
//
// Origo-Prefer is written after the allow and never before it, because
// a header on a refused request would tell a refused caller that the
// name exists, and the score needs the id (spec 005).
func (h *Handler) byName(w http.ResponseWriter, r *http.Request, owner, slug string) {
	id, err := h.log.Resolve(r.Context(), owner, slug)
	if err != nil && !errors.Is(err, wal.ErrNotFound) {
		h.storageError(w, r, err)
		return
	}
	d, ok := h.guard.Admit(w, r, auth.RepoRef{ID: id, Owner: owner, Slug: slug}, auth.ActionRead)
	if !ok {
		return
	}
	if id == "" {
		contract.Write(w, http.StatusNotFound, contract.CodeRepoNotFound, map[string]any{"owner": owner, "slug": slug})
		return
	}
	placement.SetHeader(w.Header(), h.placement, id, d.Replicas)
	h.limits.SetSubjectRate(auth.Subject(r.Context()), d.RequestsPerMinute)
	m, ix, ok := h.read(w, r, id, false)
	if !ok {
		return
	}
	httpjson.Write(w, http.StatusOK, represent(m, ix))
}

// directory is the directory mode. One authorizer call answers which
// repositories the subject may see, and this node then renders each id
// it named. It asks nothing further about them: a read decision per
// entry would turn one page of 50 into 51 calls on the request path,
// against spec 007's rule that the endpoint answers from memory near the
// nodes, and the list answer is already that decision for this route.
//
// An id the authorizer named and the log no longer holds is dropped
// rather than refused: the two stores drift, and a page shorter than the
// authorizer's is the honest answer. next_cursor is the authorizer's for
// the same reason, never the count served, so paging stays exact even
// when a page empties.
func (h *Handler) directory(w http.ResponseWriter, r *http.Request, cursor, rawLimit string) {
	limit := auth.DefaultListLimit
	if rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 || n > auth.MaxListLimit {
			invalid(w, "limit", "limit")
			return
		}
		limit = n
	}
	dir, err := h.guard.Directory(r.Context(), auth.FromContext(r.Context()), cursor, limit)
	if err != nil {
		auth.WriteRefusal(w, r, err, h.logger)
		return
	}
	switch {
	case !dir.Supported:
		contract.Write(w, http.StatusNotImplemented, contract.CodeDirectoryUnsupported,
			map[string]any{"reason": "the authorizer has no directory"})
		return
	case !dir.Allowed:
		contract.Write(w, http.StatusForbidden, contract.CodeForbidden, map[string]any{
			"action": string(auth.ActionList), "subject": auth.Subject(r.Context()), "reason": dir.Reason,
		})
		return
	}
	page := collectionPage{Repos: make([]Repository, 0, len(dir.Repos))}
	for _, entry := range dir.Repos {
		m, ix, ok, err := h.entry(r, entry.ID)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		if ok {
			page.Repos = append(page.Repos, represent(m, ix))
		}
	}
	if dir.NextCursor != "" {
		next := dir.NextCursor
		page.NextCursor = &next
	}
	httpjson.Write(w, http.StatusOK, page)
}

// entry reads one directory entry. ok is false for an id the log no
// longer holds: no metadata, a purge tombstone, or a deleted index. An
// error is a storage failure and refuses the whole page, because a page
// that silently lost an entry to an outage would read as a revocation.
func (h *Handler) entry(r *http.Request, id string) (*wal.Meta, *wal.Index, bool, error) {
	if !wal.ValidID(id) {
		return nil, nil, false, nil
	}
	m, err := h.log.ReadMeta(r.Context(), id)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if m.PurgedAt != nil {
		return nil, nil, false, nil
	}
	ix, _, err := h.log.Newest(r.Context(), id, 0, false)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if ix.DeletedAt != nil {
		return nil, nil, false, nil
	}
	return m, ix, true, nil
}
