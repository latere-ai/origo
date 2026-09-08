// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package placement

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"latere.ai/x/pkg/wait"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/repo"
)

// The eviction rules of spec 005.
const (
	// EvictEvery is how often the evictor runs.
	EvictEvery = time.Minute
	// Floor: a copy acquired this recently is never evicted for pressure.
	Floor = 10 * time.Minute
	// Idle: a copy not acquired for this long is evicted whatever the
	// pressure, so idle repositories hold no copy anywhere.
	Idle = 24 * time.Hour
)

// Copies is what the evictor needs of the cache; *repo.Cache is one.
type Copies interface {
	Copies() []repo.Copy
	Stats() (bytes int64, repos int)
	TryEvict(id string) bool
}

// EvictorOptions configures an Evictor.
type EvictorOptions struct {
	Cache Copies
	// Ceiling is ORIGO_CACHE_BYTES.
	Ceiling int64
	// Now is the clock; the wall clock by default. It must be the
	// cache's clock, because the last-acquired times come from there.
	Now     func() time.Time
	Logger  *slog.Logger
	Metrics *metrics.Set
}

// Evictor keeps the cache under the ceiling, least recently acquired
// first, and removes idle copies.
type Evictor struct {
	cache     Copies
	ceiling   int64
	now       func() time.Time
	logger    *slog.Logger
	evictions *pkgmetrics.Counter
}

// NewEvictor builds the evictor and binds the two gauges of spec 011's
// table to the cache, read at every scrape.
func NewEvictor(o EvictorOptions) (*Evictor, error) {
	if o.Cache == nil || o.Ceiling <= 0 {
		return nil, errors.New("placement: the evictor needs a cache and a positive ceiling")
	}
	e := &Evictor{cache: o.Cache, ceiling: o.Ceiling, now: o.Now, logger: o.Logger}
	if e.now == nil {
		e.now = time.Now
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	e.evictions = set.Evictions
	set.CacheBytes.Bind(func() float64 {
		b, _ := e.cache.Stats()
		return float64(b)
	})
	set.CacheRepos.Bind(func() float64 {
		_, n := e.cache.Stats()
		return float64(n)
	})
	return e, nil
}

// Run sweeps every EvictEvery until ctx is done.
func (e *Evictor) Run(ctx context.Context) error {
	wait.Every(ctx, EvictEvery, func(ctx context.Context) { e.Sweep(ctx) })
	return ctx.Err()
}

// Sweep is one run: idle copies go first whatever the pressure, then
// the least recently acquired copies above the floor until the total
// is under the ceiling. A copy in use is skipped; it is acquired
// recently by definition. It returns what was evicted by reason.
func (e *Evictor) Sweep(ctx context.Context) (pressure, idle int) {
	copies := e.cache.Copies()
	sort.Slice(copies, func(i, j int) bool { return copies[i].Acquired.Before(copies[j].Acquired) })
	now := e.now()
	var total int64
	for _, c := range copies {
		total += c.Bytes
	}
	gone := map[string]bool{}
	for _, c := range copies {
		if now.Sub(c.Acquired) >= Idle && e.cache.TryEvict(c.ID) {
			gone[c.ID] = true
			total -= c.Bytes
			idle++
			e.evictions.Inc(map[string]string{"reason": "idle"})
		}
	}
	for _, c := range copies {
		if total <= e.ceiling {
			break
		}
		if gone[c.ID] || now.Sub(c.Acquired) < Floor {
			continue
		}
		if e.cache.TryEvict(c.ID) {
			total -= c.Bytes
			pressure++
			e.evictions.Inc(map[string]string{"reason": "pressure"})
		}
	}
	if pressure > 0 || idle > 0 {
		e.logger.InfoContext(ctx, "cache evicted", "pressure", pressure, "idle", idle, "bytes", total, "ceiling", e.ceiling)
	}
	return pressure, idle
}
