// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/wait"

	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/wal"
)

// The weekly orphan sweep of spec 019: one listing of the whole prefix
// on one node, so an installation of any size pays for it once a week.
// An object under a prefix the deck defines that no index, marker, or
// metadata names is an orphan, and so is every object under the prefix
// outside them, which is reported by key so an operator sees what wrote
// it.

// The sweep's schedule and its ages.
const (
	// SweepHour is the hour of Sunday, UTC, the sweep runs at.
	SweepHour = 3
	// SweepTick is how often a node asks whether the hour has come.
	SweepTick = time.Hour
	// OrphanAge is how old an object must be before the sweep counts it
	// as an orphan: a younger one may be an upload in flight.
	OrphanAge = 24 * time.Hour
	// OrphanHold is how long an orphan is kept before it is deleted,
	// which is also how long an LFS object with no verification marker
	// (spec 010) is kept after its upload.
	OrphanHold = 7 * 24 * time.Hour
	// MaxReportedKeys bounds the keys one report names, so a bucket full
	// of unknown objects does not produce an object of its own size.
	MaxReportedKeys = 100
)

// SweepKey is where the sweeping node writes its report, which every
// other node reads its gauges from.
const SweepKey = "sweep/latest"

// SweepReport is one run, as origo/sweep/latest holds it.
type SweepReport struct {
	At   time.Time `json:"at"`
	Node string    `json:"node"`
	// Orphans is the count of objects no index, marker, or metadata
	// names that are older than OrphanAge, the value of
	// origo_orphan_objects.
	Orphans int `json:"orphans"`
	// StorageBytes is the bytes under the prefix, the value of
	// origo_storage_bytes.
	StorageBytes int64 `json:"storage_bytes"`
	// Deleted is how many orphans this run removed.
	Deleted int `json:"deleted"`
	// Keys are the orphans by key, at most MaxReportedKeys of them, so
	// an operator sees what wrote them.
	Keys []string `json:"keys,omitempty"`
	// Unknown is how many of the orphans sit outside every prefix the
	// deck defines, which is what an operator reads first.
	Unknown int `json:"unknown"`
}

// SweeperOptions configures a Sweeper.
type SweeperOptions struct {
	// Log is the log whose prefix the sweep walks; required.
	Log *wal.Log
	// Node is ORIGO_NODE_NAME, the name the schedule compares.
	Node string
	// Members is the live set of spec 005; nil is this node alone.
	Members Members
	// Now is the clock the schedule and the ages run on. The wall clock
	// by default.
	Now func() time.Time
	// Tick is how often the node asks whether the hour has come;
	// SweepTick by default.
	Tick   time.Duration
	Logger *slog.Logger
}

// Sweeper is the weekly orphan sweep of one installation as one node
// holds it: the run, the schedule, and the last report, which is what
// the two gauges of spec 011 read.
type Sweeper struct {
	log     *wal.Log
	node    string
	members Members
	now     func() time.Time
	tick    time.Duration
	logger  *slog.Logger

	mu   sync.Mutex
	last SweepReport
}

// NewSweeper builds the sweep over a log.
func NewSweeper(o SweeperOptions) *Sweeper {
	s := &Sweeper{log: o.Log, node: o.Node, members: o.Members, now: o.Now, tick: o.Tick, logger: o.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.tick <= 0 {
		s.tick = SweepTick
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

// Bind gives origo_orphan_objects and origo_storage_bytes their source:
// this node's last run, or the report the sweeping node wrote, so every
// node reports the same two figures.
func (s *Sweeper) Bind(set *metrics.Set) {
	set.OrphanObjects.Bind(func() float64 { return float64(s.Report().Orphans) })
	set.StorageBytes.Bind(func() float64 { return float64(s.Report().StorageBytes) })
}

// Report is the last run this node ran or read.
func (s *Sweeper) Report() SweepReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Sweeper) setReport(rep SweepReport) {
	s.mu.Lock()
	s.last = rep
	s.mu.Unlock()
}

// Run is the background loop: every Tick the node asks whether the hour
// has come and whether it is the first name of the live set; the nodes
// that are not read the last report instead, so their gauges carry the
// same figures.
func (s *Sweeper) Run(ctx context.Context) error {
	var last time.Time
	wait.Every(ctx, s.tick, func(ctx context.Context) {
		now := s.now().UTC()
		if !sweepDue(now, last) || s.sweepNode() != s.node {
			s.readReport(ctx)
			return
		}
		last = now
		rep, err := s.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "orphan sweep", "error", err)
			return
		}
		s.setReport(rep)
	})
	return ctx.Err()
}

// sweepDue reports whether now is the sweep's hour and no run has been
// started in it.
func sweepDue(now, last time.Time) bool {
	if now.Weekday() != time.Sunday || now.Hour() != SweepHour {
		return false
	}
	return last.IsZero() || now.Sub(last) >= SweepTick
}

// sweepNode is the node that sweeps: the first name of the live set of
// spec 005 in lexical order, so a node that leaves hands the sweep to
// the next name without coordination.
func (s *Sweeper) sweepNode() string {
	if s.members == nil {
		return s.node
	}
	live := s.members.Live()
	if len(live) == 0 {
		return s.node
	}
	return slices.Min(live)
}

// readSweepReport takes the two gauges from the last run the sweeping
// node wrote.
func (s *Sweeper) readReport(ctx context.Context) {
	rc, _, err := s.log.Store().Get(ctx, s.log.Prefix()+SweepKey, "")
	if err != nil {
		if !errors.Is(err, wal.ErrNotFound) {
			s.logger.WarnContext(ctx, "sweep report unreadable", "error", err)
		}
		return
	}
	defer func() { _ = rc.Close() }()
	var rep SweepReport
	if err := json.NewDecoder(io.LimitReader(rc, 64<<10)).Decode(&rep); err != nil {
		s.logger.WarnContext(ctx, "sweep report unreadable", "error", err)
		return
	}
	s.setReport(rep)
}

// Sweep is one run: one listing of the whole prefix, every key
// classified against what the deck defines, the orphans counted and the
// ones past the hold deleted, and the report written for the other
// nodes.
func (s *Sweeper) Sweep(ctx context.Context) (SweepReport, error) {
	now := s.now().UTC()
	rep := SweepReport{At: now, Node: s.node}
	objects, err := s.listPrefix(ctx)
	if err != nil {
		return rep, err
	}
	known, err := s.knownKeys(ctx, objects)
	if err != nil {
		return rep, err
	}
	var errs []error
	for _, o := range objects {
		rep.StorageBytes += o.Size
		rel := strings.TrimPrefix(o.Key, s.log.Prefix())
		if known[o.Key] {
			continue
		}
		age := now.Sub(o.LastModified)
		if o.LastModified.IsZero() || age < OrphanAge {
			continue
		}
		rep.Orphans++
		if !defined(rel) {
			rep.Unknown++
		}
		if len(rep.Keys) < MaxReportedKeys {
			rep.Keys = append(rep.Keys, o.Key)
		}
		if age < OrphanHold {
			continue
		}
		if err := s.log.Store().Delete(ctx, o.Key); err != nil {
			errs = append(errs, err)
			continue
		}
		rep.Deleted++
	}
	if err := s.writeReport(ctx, rep); err != nil {
		errs = append(errs, err)
	}
	s.logger.InfoContext(ctx, "orphan sweep", "orphans", rep.Orphans, "unknown", rep.Unknown,
		"deleted", rep.Deleted, "storage_bytes", rep.StorageBytes, "keys", slog.Any("keys", rep.Keys))
	return rep, errors.Join(errs...)
}

func (s *Sweeper) writeReport(ctx context.Context, rep SweepReport) error {
	data, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	_, err = s.log.Store().Put(ctx, s.log.Prefix()+SweepKey, wal.BytesBody(data))
	return err
}

// listPrefix pages through every key under the log's prefix.
func (s *Sweeper) listPrefix(ctx context.Context) ([]wal.Object, error) {
	var (
		out   []wal.Object
		after string
	)
	for {
		res, err := s.log.Store().List(ctx, wal.ListOptions{Prefix: s.log.Prefix(), StartAfter: after, Max: 1000})
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

// The prefixes the deck defines, under the log's own prefix. A key
// outside them is an orphan whatever its age says, reported by key.
var (
	repoKeyRe  = regexp.MustCompile(`^repos/([0-9a-f-]{36})/(.+)$`)
	otherPaths = []string{"names/", "events/", "gc/", "sweep/", "check/"}
)

// defined reports whether a key sits under a prefix the deck defines.
func defined(rel string) bool {
	if m := repoKeyRe.FindStringSubmatch(rel); m != nil {
		rest := m[2]
		switch {
		case rest == "meta", rest == wal.LatestKey:
			return true
		}
		for _, p := range []string{"wal/", "index/", "packs/", "lfs/"} {
			if strings.HasPrefix(rest, p) {
				return true
			}
		}
		return false
	}
	for _, p := range otherPaths {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// knownKeys is every key an index, a marker, or metadata names. The
// listing is walked once per repository so the newest index and the
// verification markers are read once each.
func (s *Sweeper) knownKeys(ctx context.Context, objects []wal.Object) (map[string]bool, error) {
	known := make(map[string]bool, len(objects))
	ids := map[string]bool{}
	for _, o := range objects {
		rel := strings.TrimPrefix(o.Key, s.log.Prefix())
		if m := repoKeyRe.FindStringSubmatch(rel); m != nil {
			ids[m[1]] = true
			continue
		}
		// Everything outside repos/ that the deck defines is named by
		// what wrote it: the names, the events, the requests, the
		// reports, and the checks.
		if defined(rel) {
			known[o.Key] = true
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.knownForRepo(ctx, id, known)
	}
	return known, nil
}

// knownForRepo marks the keys of one repository an index, a marker, or
// its metadata names.
func (s *Sweeper) knownForRepo(ctx context.Context, id string, known map[string]bool) {
	prefix := s.log.RepoPrefix(id)
	known[prefix+"meta"] = true
	known[prefix+wal.LatestKey] = true
	ix, _, err := s.log.Newest(ctx, id, 0, false)
	switch {
	case errors.Is(err, wal.ErrNotFound):
		// A purge left the tombstone and nothing else, which the two
		// keys above already cover.
		return
	case err != nil:
		// An outage or a corrupt index is not a reason to call a
		// repository's objects orphans: every one of them is left alone
		// until a week from now.
		s.logger.WarnContext(ctx, "sweep left a repository alone", "repo", id, "error", err)
		for _, o := range s.repoObjects(ctx, id, "") {
			known[o] = true
		}
		return
	}
	// Index objects are never orphans: the currency check of spec 006
	// reads a 404 on index/<n+1> as proof a copy is current.
	for _, o := range s.repoObjects(ctx, id, "index/") {
		known[o] = true
	}
	for _, e := range ix.Entries {
		known[prefix+e.Key] = true
	}
	for _, p := range ix.Packs {
		known[prefix+p] = true
		known[prefix+strings.TrimSuffix(p, ".pack")+".idx"] = true
	}
	// An LFS object is named by its verification marker (spec 010); one
	// without a marker is an orphan of its own, deleted OrphanHold
	// after its upload.
	for _, o := range s.repoObjects(ctx, id, "lfs/verified/") {
		known[o] = true
		known[prefix+"lfs/"+path.Base(o)] = true
	}
}

// repoObjects lists the keys under one repository's sub-prefix.
func (s *Sweeper) repoObjects(ctx context.Context, id, sub string) []string {
	var (
		out   []string
		after string
	)
	prefix := s.log.RepoPrefix(id) + sub
	for {
		res, err := s.log.Store().List(ctx, wal.ListOptions{Prefix: prefix, StartAfter: after, Max: 1000})
		if err != nil {
			s.logger.WarnContext(ctx, "sweep listing failed", "repo", id, "prefix", sub, "error", err)
			return out
		}
		for _, o := range res.Objects {
			out = append(out, o.Key)
			after = o.Key
		}
		if !res.Truncated || len(res.Objects) == 0 {
			return out
		}
	}
}
