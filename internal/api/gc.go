// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/compact"
	"github.com/latere-ai/origo/internal/limits"
)

// GCInterval is how often one repository may be compacted through the
// endpoint: a compaction, whether a gc or a threshold run started it,
// bars the next gc for an hour. Repacking a repository twice in an hour
// buys nothing and costs the primary a whole repack.
const GCInterval = time.Hour

// ScheduledWithin is what a node that is not the primary tells the
// caller to expect: the primary's sweep interval (spec 006).
const ScheduledWithin = int(compact.SweepInterval / time.Second)

// KindCompacted is the event a finished gc emits.
const KindCompacted = "compacted"

// Compactor is the compaction manager of spec 006 as the gc endpoint
// drives it: GC runs or schedules a compaction and Primary names the
// node that would run it.
type Compactor interface {
	GC(ctx context.Context, id string) (compact.Result, error)
	Primary(id string) string
}

// gc starts a compaction now. On the repository's primary it waits at
// most compact.GCWait for the run, because an ingress cuts a longer
// response; on any other node it leaves the request object of spec 006
// and names the primary, which compacts within its sweep interval.
func (h *Handler) gc(w http.ResponseWriter, r *http.Request) {
	_, ix, ok := h.load(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	if h.compaction == nil {
		h.storageError(w, r, errNoCompaction)
		return
	}
	id := r.PathValue("id")
	compactedAt, err := h.compactedAt(r.Context(), id, ix)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	// A compaction within the hour, a threshold run counted the same,
	// refuses the call: the repack it would run has just been run.
	if compactedAt != nil {
		if wait := GCInterval - h.now().Sub(*compactedAt); wait > 0 {
			h.limits.Refused(limits.LimitRepository)
			limits.WriteRateLimited(w, limits.LimitRepository, wait)
			return
		}
	}
	res, err := h.compaction.GC(r.Context(), id)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	switch {
	case res.Primary != "":
		httpjson.Write(w, http.StatusAccepted, map[string]any{
			"status": "scheduled", "details": map[string]any{"primary": res.Primary, "within_seconds": ScheduledWithin},
		})
	case res.Ran:
		h.emit(r, id, KindCompacted, h.now().UTC(), map[string]any{"before": res.Before, "after": res.After})
		httpjson.Write(w, http.StatusOK, map[string]any{"before": res.Before, "after": res.After})
	default:
		httpjson.Write(w, http.StatusAccepted, map[string]any{
			"status": "running", "details": map[string]any{"running": true, "started_at": res.StartedAt.UTC().Format(time.RFC3339)},
		})
	}
}

// errNoCompaction is a node built without a compaction manager, which
// cmd/origod never is.
var errNoCompaction = errors.New("api: this node runs no compaction manager")
