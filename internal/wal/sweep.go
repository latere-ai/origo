// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Sweeper rules from spec 004.
const (
	// KeepIndexes is how many of the newest index objects are always kept.
	KeepIndexes = 64
	// DeleteHold is how long a deleted repository's objects stay so a
	// consumer can undelete it.
	DeleteHold = 7 * 24 * time.Hour
)

// SweepReport says what one sweep removed.
type SweepReport struct {
	Orphans  int
	Folded   int
	Indexes  int
	Purged   bool
	Deleted  []string
	Warnings []string
}

// Sweep applies the sweeper rules to one repository. minAge is how old an
// object must be before it is deleted: entries below the newest index
// that no index names, entries above it, folded entries, and index
// objects below compacted_through except the newest KeepIndexes. A
// repository whose newest index carries deleted_at older than DeleteHold
// is removed entirely, including its name.
func (l *Log) Sweep(ctx context.Context, repo string, minAge time.Duration) (SweepReport, error) {
	var rep SweepReport
	newest, _, err := l.Newest(ctx, repo, 0, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return rep, nil
		}
		return rep, err
	}
	now := l.now()
	if newest.DeletedAt != nil && now.Sub(*newest.DeletedAt) >= DeleteHold {
		return l.purge(ctx, repo, rep)
	}
	named := make(map[string]bool, len(newest.Entries))
	for _, e := range newest.Entries {
		named[e.Key] = true
	}
	old := func(o Object) bool { return !o.LastModified.IsZero() && now.Sub(o.LastModified) >= minAge }
	del := func(key string) {
		if err := l.store.Delete(ctx, key); err != nil {
			rep.Warnings = append(rep.Warnings, key+": "+err.Error())
			return
		}
		rep.Deleted = append(rep.Deleted, key)
	}

	entries, err := l.listAll(ctx, l.key(repo, "wal/"))
	if err != nil {
		return rep, err
	}
	for _, o := range entries {
		rel := o.Key[len(l.RepoPrefix(repo)):]
		seq, _, ok := ParseEntryKey(rel)
		if !ok || !old(o) {
			continue
		}
		switch {
		case seq <= newest.CompactedThrough:
			rep.Folded++
			del(o.Key)
		case seq > newest.Seq, !named[rel]:
			rep.Orphans++
			del(o.Key)
		}
	}

	indexes, err := l.listAll(ctx, l.key(repo, "index/0"))
	if err != nil {
		return rep, err
	}
	keepFrom := uint64(0)
	if newest.Seq >= KeepIndexes {
		keepFrom = newest.Seq - KeepIndexes + 1
	}
	for _, o := range indexes {
		seq, ok := ParseIndexKey(o.Key[len(l.RepoPrefix(repo)):])
		if ok && seq < newest.CompactedThrough && seq < keepFrom && old(o) {
			rep.Indexes++
			del(o.Key)
		}
	}
	return rep, nil
}

// purge removes every object of a repository and its name.
func (l *Log) purge(ctx context.Context, repo string, rep SweepReport) (SweepReport, error) {
	if m, err := l.ReadMeta(ctx, repo); err == nil {
		if id, err := l.Resolve(ctx, m.Owner, m.Slug); err == nil && id == repo {
			if err := l.store.Delete(ctx, l.nameKey(m.Owner, m.Slug)); err != nil {
				return rep, err
			}
		}
	}
	objects, err := l.listAll(ctx, l.RepoPrefix(repo))
	if err != nil {
		return rep, err
	}
	for _, o := range objects {
		if err := l.store.Delete(ctx, o.Key); err != nil {
			return rep, err
		}
		rep.Deleted = append(rep.Deleted, o.Key)
	}
	rep.Purged = true
	return rep, nil
}

// listAll pages through every key under prefix.
func (l *Log) listAll(ctx context.Context, prefix string) ([]Object, error) {
	var (
		out   []Object
		after string
	)
	for {
		res, err := l.store.List(ctx, ListOptions{Prefix: prefix, StartAfter: after, Max: 1000})
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

// Repos lists every repository id under the prefix.
func (l *Log) Repos(ctx context.Context) ([]string, error) {
	prefix := l.prefix + "repos/"
	var (
		ids   []string
		after string
	)
	for {
		res, err := l.store.List(ctx, ListOptions{Prefix: prefix, StartAfter: after, Max: 1000, Delimiter: "/"})
		if err != nil {
			return nil, err
		}
		for _, p := range res.Prefixes {
			ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/"))
			// start-after compares whole keys, and every key under a
			// repository prefix sorts before the prefix followed by "~"
			// (they all start with a lower-case letter), so the next page
			// begins after this repository rather than inside it.
			after = p + "~"
		}
		if !res.Truncated || len(res.Prefixes) == 0 {
			return ids, nil
		}
	}
}

// SweepAll sweeps every repository and reports the first error after
// trying them all.
func (l *Log) SweepAll(ctx context.Context, minAge time.Duration) error {
	ids, err := l.Repos(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rep, err := l.Sweep(ctx, id, minAge)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(rep.Deleted) > 0 || len(rep.Warnings) > 0 {
			l.logger.InfoContext(ctx, "swept", "repo", id, "orphans", rep.Orphans, "folded", rep.Folded, "indexes", rep.Indexes, "purged", rep.Purged, "warnings", len(rep.Warnings))
		}
	}
	return errors.Join(errs...)
}
