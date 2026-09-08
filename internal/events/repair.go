// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"time"

	"github.com/latere-ai/origo/internal/wal"
)

// listAll pages through every key under prefix.
func (d *Dispatcher) listAll(ctx context.Context, prefix string) ([]wal.Object, error) {
	var out []wal.Object
	after := ""
	for {
		res, err := d.store.List(ctx, wal.ListOptions{Prefix: prefix, StartAfter: after, Max: 1000})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Objects...)
		if !res.Truncated || len(res.Objects) == 0 {
			return out, nil
		}
		after = res.Objects[len(res.Objects)-1].Key
	}
}

// unheard reports whether node's journals are this node's to repair: not
// this node, and last heard more than RepairUnheard ago or never heard
// since this node started.
func (d *Dispatcher) unheard(node string, now time.Time) bool {
	if node == d.node {
		return false
	}
	if d.members == nil {
		return true
	}
	last, heard := d.members.LastHeard(node)
	return !heard || now.Sub(last) > d.repairUnheard
}

// sweep is one repair pass over origo/events/: step 1 hands every
// pending object whose next_at is more than RepairLag in the past to
// the delivery loop, step 2 rebuilds the push events the journals of
// dead nodes name and drops journals older than JournalKeep.
func (d *Dispatcher) sweep(ctx context.Context) {
	now := d.now()
	d.sweepPending(ctx, now)
	journals, err := d.listAll(ctx, d.prefix+"nodes/")
	if err != nil {
		d.logger.WarnContext(ctx, "event journals not listed", "error", err)
		return
	}
	seen := map[string]bool{}
	for _, o := range journals {
		rest := strings.TrimPrefix(o.Key, d.prefix+"nodes/")
		node, file, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		if day, err := time.Parse(time.DateOnly, strings.TrimSuffix(file, ".log")); err == nil && now.Sub(day) > JournalKeep {
			if err := d.store.Delete(ctx, o.Key); err != nil {
				d.logger.WarnContext(ctx, "old event journal not deleted", "key", o.Key, "error", err)
			}
			continue
		}
		if seen[node] || !d.unheard(node, now) {
			continue
		}
		seen[node] = true
		d.repairNode(ctx, node, now)
	}
}

// sweepPending is step 1: every pending object of every kind whose
// next_at is more than RepairLag in the past is queued here. The
// listing's modification time bounds next_at from below, so an object
// written within the lag is skipped without a read.
func (d *Dispatcher) sweepPending(ctx context.Context, now time.Time) {
	objects, err := d.listAll(ctx, d.prefix)
	if err != nil {
		d.logger.WarnContext(ctx, "pending events not listed", "error", err)
		return
	}
	for _, o := range objects {
		rest := strings.TrimPrefix(o.Key, d.prefix)
		if strings.HasPrefix(rest, "dead/") || strings.HasPrefix(rest, "nodes/") || strings.HasSuffix(rest, "/cursor") || !strings.HasSuffix(rest, ".json") {
			continue
		}
		if !o.LastModified.IsZero() && now.Sub(o.LastModified) < RepairLag {
			continue
		}
		obj, _, _, err := d.read(ctx, o.Key)
		if err != nil {
			if !errors.Is(err, wal.ErrNotFound) {
				d.logger.WarnContext(ctx, "pending event not read", "key", o.Key, "error", err)
			}
			continue
		}
		if now.Sub(obj.NextAt) > RepairLag {
			d.schedule(o.Key, now)
		}
	}
}

// repairNode is step 2 for one node: the repositories its journals of
// today and yesterday name.
func (d *Dispatcher) repairNode(ctx context.Context, node string, now time.Time) {
	seen := map[string]bool{}
	for _, day := range []time.Time{now, now.Add(-24 * time.Hour)} {
		lines, err := d.readJournal(ctx, node, day)
		if err != nil {
			d.logger.WarnContext(ctx, "event journal not read", "node", node, "day", dayOf(day), "error", err)
			continue
		}
		for _, repo := range journalRepos(lines) {
			if seen[repo] {
				continue
			}
			seen[repo] = true
			if err := d.repairRepo(ctx, repo, now); err != nil {
				d.logger.WarnContext(ctx, "events not repaired", "repo", repo, "node", node, "error", err)
			}
		}
	}
}

// repairRepo writes the event of every push entry of the newest index
// above the cursor, older than RepairLag, that has no object pending
// or dead, from the entry head, and hands it to the delivery loop. An
// undelete and a push with origo.event=off are skipped as the enqueue
// skips them.
func (d *Dispatcher) repairRepo(ctx context.Context, repo string, now time.Time) error {
	c, err := d.readCursor(ctx, repo)
	if err != nil {
		return err
	}
	ix, _, err := d.log.Newest(ctx, repo, 0, false)
	if errors.Is(err, wal.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var meta *wal.Meta
	for _, e := range ix.Entries {
		if e.Kind != wal.KindPush || e.Seq <= c.Seq {
			continue
		}
		key := d.pushKey(repo, e.Seq)
		if exists, err := d.exists(ctx, key); err != nil || exists {
			if err != nil {
				return err
			}
			continue
		}
		if exists, err := d.exists(ctx, d.deadKey(key)); err != nil || exists {
			if err != nil {
				return err
			}
			continue
		}
		hdr, refs, err := d.readEntryHead(ctx, repo, e.Key)
		if err != nil {
			return err
		}
		if now.Sub(hdr.At) < RepairLag {
			continue
		}
		if meta == nil {
			if meta, err = d.log.ReadMeta(ctx, repo); err != nil {
				return err
			}
		}
		p, ok := build(repo, meta, Entry{Header: hdr, Refs: refs})
		if !ok {
			continue
		}
		err = d.write(ctx, key, p, now, true)
		if errors.Is(err, wal.ErrExists) {
			continue
		}
		if err != nil {
			return err
		}
		d.logger.InfoContext(ctx, "event repaired from the index", "repo", repo, "seq", e.Seq, "id", p.ID)
		d.schedule(key, now)
	}
	return nil
}

func (d *Dispatcher) exists(ctx context.Context, key string) (bool, error) {
	_, err := d.store.Head(ctx, key)
	if errors.Is(err, wal.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (d *Dispatcher) readEntryHead(ctx context.Context, repo, key string) (wal.Header, []wal.RefUpdate, error) {
	rc, _, err := d.store.Get(ctx, d.log.RepoPrefix(repo)+key, "")
	if err != nil {
		return wal.Header{}, nil, err
	}
	defer func() { _ = rc.Close() }()
	hdr, refs, _, err := wal.ReadEntryHead(rc)
	return hdr, refs, err
}

// offset spreads the sweeps of the nodes over the interval by name.
func (d *Dispatcher) offset() time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(d.node))
	return time.Duration(h.Sum32()) % d.repairInterval
}

// runRepair sweeps every RepairInterval, the first one offset by the
// node's name, until ctx ends.
func (d *Dispatcher) runRepair(ctx context.Context) {
	first := time.NewTimer(d.offset())
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	d.sweep(ctx)
	t := time.NewTicker(d.repairInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.sweep(ctx)
		}
	}
}
