// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package limits

import (
	"context"
	"time"

	"latere.ai/x/pkg/cache"

	"github.com/latere-ai/origo/internal/wal"
)

// listPage is the page size of the lfs/ listing the sum walks.
const listPage = 1000

// LFSBytes is the sum of the bytes under a repository's lfs/ prefix,
// the second half of the repository quota beside what the log holds
// (spec 012). One listing is reused for the TTL, so a push and an
// upload batch measure against the same figure without listing the
// prefix again; a sum that is up to a minute behind can only
// undercount by what one client uploaded in that minute, which the
// next measurement catches.
type LFSBytes struct {
	log   *wal.Log
	ttl   time.Duration
	now   func() time.Time
	cache *cache.TTLCache[string, int64]

	// lists counts the listings performed, for a test that asserts the
	// sum was reused.
	lists func()
}

// NewLFSBytes builds the cache over the log. A nil log answers zero
// bytes for every repository, which is a node with no LFS surface.
func NewLFSBytes(log *wal.Log, ttl time.Duration, now func() time.Time) *LFSBytes {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = LFSTTL
	}
	return &LFSBytes{
		log: log, ttl: ttl, now: now,
		cache: cache.New[string, int64](ttl, cache.WithClock[string, int64](now)),
	}
}

// Bytes answers the sum for the repository, listing the prefix when the
// held sum has expired.
func (c *LFSBytes) Bytes(ctx context.Context, repo string) (int64, error) {
	if c == nil || c.log == nil {
		return 0, nil
	}
	if n, ok := c.cache.Get(repo); ok {
		return n, nil
	}
	n, err := c.list(ctx, repo)
	if err != nil {
		return 0, err
	}
	c.cache.Set(repo, n)
	return n, nil
}

// Invalidate drops the held sum for the repository, so the next
// measurement lists again.
func (c *LFSBytes) Invalidate(repo string) {
	if c == nil {
		return
	}
	c.cache.Invalidate(repo)
}

// list sums the objects under lfs/. The delimiter groups lfs/verified/
// into a prefix, so the markers are listed once as a prefix rather than
// one key each.
func (c *LFSBytes) list(ctx context.Context, repo string) (int64, error) {
	if c.lists != nil {
		c.lists()
	}
	store := c.log.Store()
	prefix := c.log.RepoPrefix(repo) + "lfs/"
	var (
		total int64
		after string
	)
	for {
		res, err := store.List(ctx, wal.ListOptions{Prefix: prefix, StartAfter: after, Max: listPage, Delimiter: "/"})
		if err != nil {
			return 0, err
		}
		for _, o := range res.Objects {
			total += o.Size
		}
		if !res.Truncated {
			return total, nil
		}
		next := after
		if n := len(res.Objects); n > 0 {
			next = res.Objects[n-1].Key
		}
		// A page may be all prefixes; continue after the last of them,
		// the way wal.Log.Repos does.
		for _, p := range res.Prefixes {
			if p+"~" > next {
				next = p + "~"
			}
		}
		if next == after {
			return total, nil
		}
		after = next
	}
}

// Quota is one measurement of a repository against the authorizer's
// figure: what the log holds plus the bytes under lfs/ plus what the
// write would add, and the limit it is measured against.
type Quota struct {
	// Bytes is the size the repository would have after the write.
	Bytes int64
	// Max is the authorizer's quota_bytes.
	Max int64
}

// Over reports whether the measurement is past the limit.
func (q Quota) Over() bool { return q.Bytes > q.Max }

// Measure sizes the repository after a write of addition bytes: held is
// size_bytes of the index the caller holds (spec 004), the bytes under
// lfs/ come from the cache, and max is the authorizer's quota_bytes,
// DefaultQuotaBytes when it names none.
func (l *Limits) Measure(ctx context.Context, repo string, held, addition, max int64) (Quota, error) {
	if max <= 0 {
		max = DefaultQuotaBytes
	}
	stored, err := l.LFSBytes(ctx, repo)
	if err != nil {
		return Quota{}, err
	}
	return Quota{Bytes: held + stored + addition, Max: max}, nil
}
