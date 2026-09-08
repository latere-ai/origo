// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package compact is spec 006: a repository that receives many pushes
// accumulates many small entries, and serving a fetch from thousands of
// thin packs is slow. One node, the first name of Origo-Prefer (spec
// 005), repacks and records the result as a compact entry, so every
// other node downloads packs instead of repacking. A node that is not
// the primary never compacts; it writes the request object origo/gc/<id>
// and the primary's sweep picks it up.
package compact

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/wait"

	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/placement"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// The thresholds and the budgets of spec 006.
const (
	// MaxEntries is how many entries an index may list before the next
	// push schedules a compaction.
	MaxEntries = 64
	// MaxEntryBytes is the sum of pack_bytes over those entries above
	// which a compaction is scheduled.
	MaxEntryBytes = 256 << 20
	// MaxIndexBytes is the size of the index object above which a
	// compaction is scheduled; spec 004 designs for 1 MiB.
	MaxIndexBytes = 512 << 10

	// RunDeadline bounds one whole run, the repack included, in place of
	// the 5 minutes of spec 004.
	RunDeadline = 30 * time.Minute
	// SweepInterval is how often the primary looks for work.
	SweepInterval = 10 * time.Minute
	// SlotWait is how long a git subprocess of a run waits for a slot of
	// the subprocess semaphore (spec 012) before the run is skipped.
	SlotWait = 5 * time.Second
	// GCWait is how long POST /v1/repos/{id}/gc (spec 019) waits for a
	// run before it answers 202: an ingress cuts a longer response.
	GCWait = 10 * time.Second
	// RequestMaxAge is how old a request object may be before any node's
	// sweep deletes it; only a primary that never ran leaves one behind.
	RequestMaxAge = 24 * time.Hour
)

// Outcome is what one run did, the result label of
// origo_compactions_total.
type Outcome string

// The outcomes of spec 011's table.
const (
	// OutcomeOK is a run that committed its compact entry.
	OutcomeOK Outcome = "ok"
	// OutcomeStale is a run a push overtook between step 1 and step 5.
	OutcomeStale Outcome = "stale"
	// OutcomeError is a run that failed.
	OutcomeError Outcome = "error"
	// OutcomeSkipped is a run that found no subprocess slot in SlotWait.
	OutcomeSkipped Outcome = "skipped"
)

// Slots is the subprocess semaphore of spec 012, shared by the handlers
// and compaction. Acquire takes one slot, waiting at most d, and reports
// false when none came free in time. Nil takes no slot, which is every
// node until spec 012 lands.
type Slots interface {
	Acquire(ctx context.Context, d time.Duration) (release func(), ok bool)
}

// Figures are the before and after of a run, the body spec 019's gc
// answers with.
type Figures struct {
	Packs     int   `json:"packs"`
	Entries   int   `json:"entries"`
	SizeBytes int64 `json:"size_bytes"`
}

// figures reads them off an index object.
func figures(ix *wal.Index) Figures {
	return Figures{Packs: len(ix.Packs), Entries: len(ix.Entries), SizeBytes: ix.SizeBytes}
}

// Result is what GC answers. Primary is set when this node is not the
// repository's primary, so the caller answers 202 naming it; Running
// says a run is still going and StartedAt when it started; Ran says the
// run finished inside the wait, and then Before and After hold its
// figures.
type Result struct {
	Ran       bool
	Running   bool
	StartedAt time.Time
	Primary   string
	Before    Figures
	After     Figures
}

// Options configures a Manager.
type Options struct {
	// Cache holds the local copies; required.
	Cache *repo.Cache
	// Placement answers the preferred nodes of a repository, whose first
	// name is the compaction primary (spec 005); required.
	Placement placement.Placer
	// Node is this node's name, ORIGO_NODE_NAME.
	Node string
	// Slots is the subprocess semaphore of spec 012; nil takes no slot.
	Slots Slots
	// Interval is how often Run sweeps. SweepInterval by default.
	Interval time.Duration
	// GCWait bounds the wait of GC. GCWait by default.
	GCWait time.Duration
	// Deadline bounds one run. RunDeadline by default.
	Deadline time.Duration
	// Now is the clock the request objects and the sweep run on. The
	// wall clock by default.
	Now    func() time.Time
	Logger *slog.Logger
	// Metrics holds the two handles compaction records through,
	// registered by internal/metrics (spec 011). A set of its own by
	// default.
	Metrics *metrics.Set
}

// Manager owns the compaction of every repository this node is the
// primary of: the trigger after a push, the gc request, and the sweep.
type Manager struct {
	cache     *repo.Cache
	log       *wal.Log
	git       *repo.Git
	placement placement.Placer
	node      string
	slots     Slots
	interval  time.Duration
	gcWait    time.Duration
	deadline  time.Duration
	now       func() time.Time
	logger    *slog.Logger

	runs     *pkgmetrics.Counter
	duration *pkgmetrics.Histogram

	mu       sync.Mutex
	inflight map[string]*run
	base     context.Context //nolint:containedctx // the background context of Run, which a trigger's run outlives its request on
	wg       sync.WaitGroup

	// afterRepack, when set, runs between step 4 and step 5 so a test
	// lands a push in the window the stale outcome names.
	afterRepack func(id string)
	// beforeRepack, when set, runs between step 1 and step 2 so a test
	// holds a run inside the read lock.
	beforeRepack func(id string)
}

// run is one compaction in flight.
type run struct {
	started time.Time
	done    chan struct{}
	outcome Outcome
	before  Figures
	after   Figures
	err     error
}

// New builds the manager over the two series of spec 011's table.
func New(o Options) (*Manager, error) {
	if o.Cache == nil || o.Placement == nil {
		return nil, errors.New("compact: a cache and a placer are required")
	}
	m := &Manager{
		cache: o.Cache, log: o.Cache.Log(), placement: o.Placement, node: o.Node,
		slots: o.Slots, interval: o.Interval, gcWait: o.GCWait, deadline: o.Deadline,
		now: o.Now, logger: o.Logger, inflight: map[string]*run{}, base: context.Background(),
	}
	if m.interval <= 0 {
		m.interval = SweepInterval
	}
	if m.gcWait <= 0 {
		m.gcWait = GCWait
	}
	if m.deadline <= 0 {
		m.deadline = RunDeadline
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	// The repack of a compaction runs under the run's deadline rather
	// than the 5 minutes of spec 004, so the git wrapper is the cache's
	// with that timeout.
	g := o.Cache.Git()
	m.git = &repo.Git{Bin: g.Bin, Home: g.Home, Timeout: m.deadline}
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	m.runs, m.duration = set.Compactions, set.CompactionSeconds
	return m, nil
}

// Primary is the repository's compaction primary, the first name of
// Origo-Prefer (spec 005).
func (m *Manager) Primary(id string) string {
	prefer := m.placement.Prefer(id, 1)
	if len(prefer) == 0 {
		return ""
	}
	return prefer[0]
}

// isPrimary reports whether this node compacts the repository.
func (m *Manager) isPrimary(id string) bool { return m.Primary(id) == m.node }

// Crossed reports whether an index object crosses a threshold of spec
// 006: too many entries, too many pack bytes in them, or an index object
// too large.
func Crossed(ix *wal.Index) bool {
	if len(ix.Entries) > MaxEntries || ix.EntriesBytes() > MaxEntryBytes {
		return true
	}
	// The encoder fills the empty collections, so it runs on a copy: the
	// index this reads is the one a request holds.
	body, err := wal.EncodeIndex(ix.Clone())
	return err == nil && len(body) > MaxIndexBytes
}

// After is the trigger of spec 006: every node checks the thresholds
// against the index a push it served produced. The primary schedules the
// run in the background, a node that is not the primary writes the
// request object, and either way the call returns at once so the push is
// acknowledged without waiting.
func (m *Manager) After(id string, ix *wal.Index) {
	if ix == nil || !Crossed(ix) {
		return
	}
	if m.isPrimary(id) {
		m.schedule(id)
		return
	}
	m.mu.Lock()
	ctx := m.base
	m.mu.Unlock()
	m.wg.Go(func() {
		if err := m.request(ctx, id, ReasonThreshold); err != nil {
			m.logger.WarnContext(ctx, "compaction not requested", "repo", id, "error", err)
		}
	})
}

// GC is POST /v1/repos/{id}/gc (spec 019). On a node that is not the
// primary it writes the request object and answers with the primary's
// name, compacting nothing. On the primary it starts a run whatever the
// thresholds say and waits for it at most GCWait; a run already going is
// answered with its start time.
func (m *Manager) GC(ctx context.Context, id string) (Result, error) {
	if !m.isPrimary(id) {
		if err := m.request(ctx, id, ReasonGC); err != nil {
			return Result{}, err
		}
		return Result{Primary: m.Primary(id)}, nil
	}
	r, fresh := m.schedule(id) //nolint:contextcheck // a run outlives the request that triggered it and takes the manager's own context
	if !fresh {
		return Result{Running: true, StartedAt: r.started}, nil
	}
	timer := time.NewTimer(m.gcWait)
	defer timer.Stop()
	select {
	case <-r.done:
		if r.err != nil {
			return Result{}, r.err
		}
		return Result{Ran: true, Before: r.before, After: r.after}, nil
	case <-timer.C:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return Result{Running: true, StartedAt: r.started}, nil
}

// schedule starts a run unless one is already going for the repository,
// and reports whether this call started it. At most one compaction per
// repository runs on a node at a time; a trigger that finds one running
// is a no-op and the sweep starts the next one.
func (m *Manager) schedule(id string) (*run, bool) {
	m.mu.Lock()
	if r, ok := m.inflight[id]; ok {
		m.mu.Unlock()
		return r, false
	}
	r := &run{started: m.now(), done: make(chan struct{})}
	m.inflight[id] = r
	ctx := m.base
	m.mu.Unlock()
	m.wg.Go(func() {
		defer close(r.done)
		defer func() {
			m.mu.Lock()
			delete(m.inflight, id)
			m.mu.Unlock()
		}()
		started := time.Now()
		r.outcome, r.before, r.after, r.err = m.compact(ctx, id)
		m.runs.Inc(map[string]string{"result": string(r.outcome)})
		if r.outcome == OutcomeOK {
			m.duration.Observe(nil, time.Since(started).Seconds())
		}
		if r.err != nil {
			m.logger.WarnContext(ctx, "compaction failed", "repo", id, "result", string(r.outcome), "error", r.err)
		}
	})
	return r, true
}

// Running reports the start of the run in flight for the repository.
func (m *Manager) Running(id string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.inflight[id]
	if !ok {
		return time.Time{}, false
	}
	return r.started, true
}

// Wait blocks until no run and no request write is in flight. The node
// calls it on shutdown and a test after a trigger.
func (m *Manager) Wait() { m.wg.Wait() }

// Run sweeps every Interval until ctx is done. It also fixes the context
// a trigger's run outlives its request on.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()
	wait.Every(ctx, m.interval, func(ctx context.Context) {
		if _, err := m.Sweep(ctx); err != nil && ctx.Err() == nil {
			m.logger.WarnContext(ctx, "compaction sweep", "error", err)
		}
	})
	m.Wait()
	return ctx.Err()
}
