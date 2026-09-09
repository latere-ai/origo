// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/latere-ai/origo/internal/contract"
)

// DefaultExportTimeout bounds the git bundle subprocess of one export
// (spec 012's budget for a long read). A bundle cut by it is a
// truncated body, never a status, because the response headers went
// with the first byte; the client's git bundle verify refuses it.
const DefaultExportTimeout = 10 * time.Minute

// exportContentType is what a bundle is served as.
const exportContentType = "application/x-git-bundle"

// export streams the complete repository as one portable file: git
// bundle create - --all, which is what a consumer takes out of Origo
// and what an import reads back.
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	rr, release, ok := h.open(w, r, "export", func() error { return nil })
	if !ok {
		return
	}
	defer release()
	// git refuses to bundle a repository with no reference, and that is
	// a state of the repository rather than a failure of the node.
	if !hasRefs(rr) {
		contract.Write(w, http.StatusNotFound, contract.CodeRefNotFound, map[string]any{"ref": "--all"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.exportTimeout)
	defer cancel()
	s, err := rr.start(ctx, nil, "bundle", "create", "-", "--all")
	if err != nil {
		rr.fail(ctx, err)
		return
	}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	for {
		n, rerr := s.out.Read(buf)
		if n > 0 {
			if !rr.wrote {
				w.Header().Set("Content-Type", exportContentType)
				w.WriteHeader(http.StatusOK)
				rr.wrote = true
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				_ = s.finish(true)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := s.finish(false); err != nil {
		rr.fail(ctx, err)
	}
}

// hasRefs reports whether the index holds a reference other than HEAD,
// which is what git bundle needs to write anything.
func hasRefs(rr *readRequest) bool {
	for name := range rr.repo.Index.Refs {
		if name != "HEAD" && strings.HasPrefix(name, "refs/") {
			return true
		}
	}
	return false
}
