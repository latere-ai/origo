// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/wal"
)

// Stats is what GET /v1/repos/{id}/stats answers (spec 019): what the
// log holds for the repository, so an operator bounds total storage as
// the sum of size_bytes and lfs_bytes over every repository.
type Stats struct {
	// SizeBytes is the index object's size_bytes (spec 004): the bytes
	// of the listed packs plus the pack bytes of the entries since the
	// last compaction, so the figure falls after a gc.
	SizeBytes int64 `json:"size_bytes"`
	// LFSBytes is the bytes under lfs/, the verification markers
	// excluded (spec 010).
	LFSBytes int64 `json:"lfs_bytes"`
	// Packs and EntriesSinceCompaction are what the newest index lists.
	Packs                  int `json:"packs"`
	EntriesSinceCompaction int `json:"entries_since_compaction"`
	// Refs is the size of the index's reference map, HEAD included.
	Refs int `json:"refs"`
	// PushedAt is the at of the newest push entry, null before the
	// first push; CompactedAt the at of the newest compact entry the
	// index names, null when it names none.
	PushedAt    *time.Time `json:"pushed_at"`
	CompactedAt *time.Time `json:"compacted_at"`
}

// compactedAt is the at of the newest compact entry the index names.
// The index row carries no timestamp, so the entry's header is read;
// a compact entry holds no pack, so the object is two lines.
func (h *Handler) compactedAt(ctx context.Context, id string, ix *wal.Index) (*time.Time, error) {
	for _, v := range slices.Backward(ix.Entries) {
		if v.Kind != wal.KindCompact {
			continue
		}
		hdr, _, err := h.log.EntryHead(ctx, id, v.Key)
		if err != nil {
			return nil, err
		}
		at := hdr.At.UTC()
		return &at, nil
	}
	return nil, nil
}

// stats answers the storage figures of one repository.
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	_, ix, ok := h.load(w, r, auth.ActionRead, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	lfsBytes, err := h.limits.LFSBytes(r.Context(), id)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	compactedAt, err := h.compactedAt(r.Context(), id, ix)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, Stats{
		SizeBytes: ix.SizeBytes, LFSBytes: lfsBytes, Packs: len(ix.Packs),
		EntriesSinceCompaction: len(ix.Entries), Refs: len(ix.Refs),
		PushedAt: ix.PushedAt, CompactedAt: compactedAt,
	})
}
