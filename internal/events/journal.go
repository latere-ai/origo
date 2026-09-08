// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latere-ai/origo/internal/wal"
)

// journal is the node's in-memory copy of its journals: one line
// <repo> <seq> per push entry the node enqueued, per UTC day, in
// enqueue order. A day is dirty until it is flushed whole.
type journal struct {
	mu    sync.Mutex
	days  map[string][]string
	repos map[string]map[string]bool
	dirty map[string]bool
}

func newJournal() *journal {
	return &journal{days: map[string][]string{}, repos: map[string]map[string]bool{}, dirty: map[string]bool{}}
}

func dayOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// add appends the line and reports whether it is the first line for
// the repository in the day, which is flushed at once.
func (j *journal) add(now time.Time, repo string, seq uint64) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	day := dayOf(now)
	j.days[day] = append(j.days[day], repo+" "+strconv.FormatUint(seq, 10))
	j.dirty[day] = true
	if j.repos[day] == nil {
		j.repos[day] = map[string]bool{}
	}
	first := !j.repos[day][repo]
	j.repos[day][repo] = true
	j.forget(day)
	return first
}

// load seeds a day with the lines an earlier process wrote, before any
// line of this process, so the next flush rewrites the whole day. A
// line already in memory is one this process flushed before the load
// read the object back, and is not taken twice.
func (j *journal) load(day time.Time, lines []string) {
	if len(lines) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	key := dayOf(day)
	have := map[string]bool{}
	for _, l := range j.days[key] {
		have[l] = true
	}
	var fresh []string
	for _, l := range lines {
		if !have[l] {
			fresh = append(fresh, l)
		}
	}
	j.days[key] = append(fresh, j.days[key]...)
	if j.repos[key] == nil {
		j.repos[key] = map[string]bool{}
	}
	for _, l := range fresh {
		repo, _, _ := strings.Cut(l, " ")
		j.repos[key][repo] = true
	}
}

// forget drops the days older than yesterday from memory; their
// objects stay until the sweep deletes them.
func (j *journal) forget(today string) {
	for day := range j.days {
		if day < today && day < dayOf(mustDay(today).Add(-24*time.Hour)) {
			delete(j.days, day)
			delete(j.repos, day)
			delete(j.dirty, day)
		}
	}
}

func mustDay(day string) time.Time {
	t, _ := time.Parse(time.DateOnly, day)
	return t
}

// take returns the dirty days with their lines and marks them clean;
// a failed flush marks a day dirty again through undo.
func (j *journal) take() map[string][]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string][]string{}
	for day := range j.dirty {
		out[day] = slices.Clone(j.days[day])
		delete(j.dirty, day)
	}
	return out
}

func (j *journal) undo(day string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.days[day]; ok {
		j.dirty[day] = true
	}
}

// flushJournal writes every dirty day whole. A day whose write fails
// stays dirty for the next tick.
func (d *Dispatcher) flushJournal(ctx context.Context) error {
	var errs []error
	for day, lines := range d.journal.take() {
		key := d.prefix + "nodes/" + d.node + "/" + day + ".log"
		body := strings.Join(lines, "\n") + "\n"
		if _, err := d.store.Put(ctx, key, wal.BytesBody([]byte(body))); err != nil {
			d.journal.undo(day)
			errs = append(errs, fmt.Errorf("journal %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// runJournal flushes every FlushInterval until ctx ends.
func (d *Dispatcher) runJournal(ctx context.Context) {
	t := time.NewTicker(FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.flushJournal(ctx); err != nil && ctx.Err() == nil {
				d.logger.WarnContext(ctx, "event journal not flushed", "node", d.node, "error", err)
			}
		}
	}
}

// readJournal reads one journal object; a missing one is no lines.
func (d *Dispatcher) readJournal(ctx context.Context, node string, day time.Time) ([]string, error) {
	rc, _, err := d.store.Get(ctx, d.journalKey(node, day), "")
	if errors.Is(err, wal.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var lines []string
	sc := bufio.NewScanner(io.LimitReader(rc, 64<<20))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}

// journalRepos is the set of repositories the lines name, in order of
// first mention.
func journalRepos(lines []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		repo, _, _ := strings.Cut(l, " ")
		if !seen[repo] && wal.ValidID(repo) {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	return out
}

// Lines reports the node's in-memory journal of the day, for the tests.
func (d *Dispatcher) Lines(day time.Time) []string {
	d.journal.mu.Lock()
	defer d.journal.mu.Unlock()
	return slices.Clone(d.journal.days[dayOf(day)])
}

// Days reports the days held in memory, for the tests.
func (d *Dispatcher) Days() []string {
	d.journal.mu.Lock()
	defer d.journal.mu.Unlock()
	return slices.Sorted(maps.Keys(d.journal.days))
}
