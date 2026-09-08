// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package compact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/latere-ai/origo/internal/wal"
)

// The reasons a request object carries.
const (
	// ReasonThreshold is a push a node that is not the primary served
	// that crossed a threshold.
	ReasonThreshold = "threshold"
	// ReasonGC is POST /v1/repos/{id}/gc on a node that is not the
	// primary (spec 019).
	ReasonGC = "gc"
)

// maxRequestBytes bounds the read of a request object.
const maxRequestBytes = 4 << 10

// Request is the object origo/gc/<id>: a node that is not the primary
// asking the primary to compact. An existing one is left alone whichever
// reason arrives second, so the object is written by create-if-absent
// and never rewritten.
type Request struct {
	RequestedAt time.Time `json:"requested_at"`
	Node        string    `json:"node"`
	Reason      string    `json:"reason"`
}

// requestKey is origo/gc/<id> under the log's prefix.
func (m *Manager) requestKey(id string) string { return m.log.Prefix() + "gc/" + id }

// request writes the request object unless one is already there.
func (m *Manager) request(ctx context.Context, id, reason string) error {
	if !wal.ValidID(id) {
		return errors.New("compact: invalid repository id")
	}
	body, err := json.Marshal(Request{RequestedAt: m.now().UTC(), Node: m.node, Reason: reason})
	if err != nil {
		return err
	}
	if _, err := m.log.Store().Create(ctx, m.requestKey(id), wal.BytesBody(body)); err != nil && !errors.Is(err, wal.ErrExists) {
		return err
	}
	return nil
}

// ReadRequest reads the request object of a repository, or ErrNotFound.
func (m *Manager) ReadRequest(ctx context.Context, id string) (Request, error) {
	var req Request
	rc, _, err := m.log.Store().Get(ctx, m.requestKey(id), "")
	if err != nil {
		return req, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxRequestBytes))
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, err
	}
	return req, nil
}

// requests lists every repository with a request object.
func (m *Manager) requests(ctx context.Context) ([]string, error) {
	prefix := m.log.Prefix() + "gc/"
	var (
		ids   []string
		after string
	)
	for {
		res, err := m.log.Store().List(ctx, wal.ListOptions{Prefix: prefix, StartAfter: after, Max: 1000})
		if err != nil {
			return nil, err
		}
		for _, o := range res.Objects {
			if id := strings.TrimPrefix(o.Key, prefix); wal.ValidID(id) {
				ids = append(ids, id)
			}
			after = o.Key
		}
		if !res.Truncated || len(res.Objects) == 0 {
			return ids, nil
		}
	}
}

// SweepReport says what one compaction sweep did.
type SweepReport struct {
	// Requested is the request objects this node acted on and removed.
	Requested int
	// Expired is the request objects older than RequestMaxAge removed,
	// which only a primary that never ran leaves behind.
	Expired int
	// Threshold is the local copies a threshold started a run for.
	Threshold int
}

// Sweep is the primary's periodic look for work, every Interval: the
// request objects other nodes wrote, then the thresholds of the
// repositories this node holds locally. A repository pushed to through
// other nodes is therefore found whether or not this node holds it.
func (m *Manager) Sweep(ctx context.Context) (SweepReport, error) {
	var rep SweepReport
	ids, err := m.requests(ctx)
	if err != nil {
		return rep, err
	}
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		req, err := m.ReadRequest(ctx, id)
		if err != nil && !errors.Is(err, wal.ErrNotFound) {
			return rep, err
		}
		// Any node removes a request nobody acted on within a day; the
		// next push through any node writes a fresh one.
		if !req.RequestedAt.IsZero() && m.now().Sub(req.RequestedAt) >= RequestMaxAge {
			if err := m.log.Store().Delete(ctx, m.requestKey(id)); err != nil {
				return rep, err
			}
			rep.Expired++
			continue
		}
		if !m.isPrimary(id) {
			continue
		}
		requested[id] = true
		// The run materializes the repository when this node holds no
		// copy: step 1 acquires it, which is what materializing is.
		if out := m.runNow(ctx, id); out != OutcomeOK {
			continue
		}
		if err := m.log.Store().Delete(ctx, m.requestKey(id)); err != nil {
			return rep, err
		}
		rep.Requested++
	}
	for _, copy := range m.cache.Copies() {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		if requested[copy.ID] || !m.isPrimary(copy.ID) || !m.crossedLocally(ctx, copy.ID) {
			continue
		}
		rep.Threshold++
		m.runNow(ctx, copy.ID)
	}
	return rep, nil
}

// crossedLocally brings the local copy current and reports whether its
// newest index crosses a threshold.
func (m *Manager) crossedLocally(ctx context.Context, id string) bool {
	r, release, err := m.cache.Acquire(ctx, id, false)
	if err != nil {
		return false
	}
	ix := r.Index
	release()
	return Crossed(ix)
}

// runNow starts a run and waits for it, or waits for the one already in
// flight, which a trigger started; the outcome is that run's.
func (m *Manager) runNow(ctx context.Context, id string) Outcome {
	r, _ := m.schedule(id) //nolint:contextcheck // a run takes the manager's own context, not the sweep's
	select {
	case <-r.done:
		return r.outcome
	case <-ctx.Done():
		return OutcomeError
	}
}
