// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/cache"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
)

// The state of a repository that refuses a push (spec 019): a freeze a
// consumer set, and an import running on some node. Both live in meta,
// which the push path reads once for the advertisement and the upload
// that follows it.

// MetaTTL is how long the meta of a push is reused, the lifetime of an
// advertisement in git: the advertisement and the git-receive-pack that
// answers it are one push and read meta once between them.
const MetaTTL = 60 * time.Second

// metaCache holds one repository's meta for a push. The key carries the
// request's token as well as the repository, so one caller's read is
// never served to another and a state change is at most MetaTTL behind
// for the caller that fetched it.
type metaCache struct {
	log   *wal.Log
	cache *cache.TTLCache[string, *wal.Meta]
}

func newMetaCache(log *wal.Log, ttl time.Duration, now func() time.Time) *metaCache {
	if ttl <= 0 {
		ttl = MetaTTL
	}
	if now == nil {
		now = time.Now
	}
	return &metaCache{log: log, cache: cache.New[string, *wal.Meta](ttl, cache.WithClock[string, *wal.Meta](now))}
}

// key is the token hash and the repository id. The token is hashed
// because the key is held in memory beside the value and a bearer
// belongs in neither.
func metaKey(r *http.Request, id string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(r.Header.Get("Authorization"))))
	return hex.EncodeToString(sum[:8]) + ":" + id
}

// meta reads the repository's meta for this push, once: the
// advertisement fills the entry and the upload that follows reads it
// without a second GET.
func (c *metaCache) meta(ctx context.Context, r *http.Request, id string) (*wal.Meta, error) {
	key := metaKey(r, id)
	if m, ok := c.cache.Get(key); ok {
		return m, nil
	}
	m, err := c.log.ReadMeta(ctx, id)
	if err != nil {
		return nil, err
	}
	c.cache.Set(key, m)
	return m, nil
}

// stateRefusal is the code a repository's own state refuses a push
// with, or "" when it accepts one. A freeze is the consumer's own
// switch; an import holds the repository until it finishes, because a
// push during one would be folded away by the import's entry.
func stateRefusal(m *wal.Meta) (code string, details map[string]any) {
	switch {
	case m.FrozenAt != nil:
		return contract.CodeRepoFrozen, map[string]any{"frozen_at": m.FrozenAt}
	case m.ImportingSince != nil:
		return contract.CodeRepoImporting, map[string]any{"started_at": m.ImportingSince}
	}
	return "", nil
}

// refuseState refuses a push at info/refs before the client uploads a
// pack. A freeze travels as the ERR pkt-line of spec 015's shape, so
// git prints "remote error: repo_frozen: <sentence>"; an import travels
// as the 409 its row states, which is what a consumer driving a
// migration reads.
func (h *Handler) refuseState(w http.ResponseWriter, service, code string, details map[string]any) {
	if code == contract.CodeRepoImporting {
		contract.Write(w, http.StatusConflict, code, details)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	_ = writePkt(w, "# service="+service+"\n")
	_ = flushPkt(w)
	_ = writePkt(w, "ERR "+code+": "+contract.Sentence(code)+"\n")
}
