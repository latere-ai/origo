// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// refusal is a push spec 012 refuses on size: which bound it crossed,
// what the push would leave behind, and the limit. It travels to the
// client as the hook's verdict, `over_quota: <sentence>` exactly (spec
// 021's rule for a line that carries a code), and the figures go to the
// handler's info log line, which is the one place a refused push's
// numbers are written.
type refusal struct {
	limit string
	bytes int64
	max   int64
}

// log writes the one line a refused push leaves.
func (f *refusal) log(ctx context.Context, logger *slog.Logger, id, subject string) {
	logger.InfoContext(ctx, "push refused", "repo", id, "limit", f.limit,
		"bytes", f.bytes, "max", f.max, "subject", subject)
}

// overQuota measures the push against the two bounds that are known
// before git runs: the repository's size after it, against the
// authorizer's quota_bytes, and the references the index would hold
// after it, against the reference cap. It answers nil when the push
// fits, and an error only when the lfs/ sum could not be read.
func (h *Handler) overQuota(ctx context.Context, id string, rp *repo.Repo, req *receiveRequest, quota int64) (*refusal, error) {
	if refs := refsAfter(rp.Index, req.Commands); refs > limits.MaxRefs {
		return &refusal{limit: limits.LimitRefs, bytes: int64(refs), max: limits.MaxRefs}, nil
	}
	var held int64
	if rp.Index != nil {
		held = rp.Index.SizeBytes
	}
	q, err := h.limits.Measure(ctx, id, held, req.PackSize, quota)
	if err != nil {
		return nil, err
	}
	if !q.Over() {
		return nil, nil
	}
	return &refusal{limit: limits.LimitRepository, bytes: q.Bytes, max: q.Max}, nil
}

// refsAfter is how many references the index holds once the commands
// are applied: a command that creates a name the index does not carry
// adds one, a delete of one it carries removes one, and an update
// changes nothing. HEAD is in the map and is counted with the rest,
// because it is what the index holds.
func refsAfter(ix *wal.Index, commands []wal.RefUpdate) int {
	if ix == nil {
		return len(commands)
	}
	n := len(ix.Refs)
	for _, c := range commands {
		_, present := ix.Refs[c.Ref]
		switch {
		case c.New == wal.ZeroSHA && present:
			n--
		case c.New != wal.ZeroSHA && !present:
			n++
		}
	}
	return n
}

// tooLarge answers a push body past the single-push bound: 413
// over_quota with the push limit, before git ever runs.
func (h *Handler) tooLarge(w http.ResponseWriter, r *http.Request, id string, bytes, max int64) {
	f := &refusal{limit: limits.LimitPush, bytes: bytes, max: max}
	f.log(r.Context(), h.logger, id, auth.Subject(r.Context()))
	contract.Write(w, http.StatusRequestEntityTooLarge, contract.CodeOverQuota, map[string]any{
		"limit": f.limit, "bytes": f.bytes, "max": f.max,
	})
}
