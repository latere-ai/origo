// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package placement

import (
	"context"

	"github.com/latere-ai/origo/internal/repo"
)

// CacheHolder is the Holder over the repository cache: Held is the
// cache's lock-free answer, and a catch-up is an Acquire for writing
// that returns at once, so the next request finds the copy current.
type CacheHolder struct {
	Cache *repo.Cache
}

// Held reports the sequence the local copy holds.
func (h CacheHolder) Held(id string) (uint64, bool) { return h.Cache.Held(id) }

// CatchUp brings the copy current and releases it.
func (h CacheHolder) CatchUp(ctx context.Context, id string) error {
	_, release, err := h.Cache.Acquire(ctx, id, true)
	if err != nil {
		return err
	}
	release()
	return nil
}
