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

// BindSweepGauges gives origo_orphan_objects and origo_storage_bytes
// their source: this node's last run, or the report the sweeping node
// wrote, so every node reports the same two figures.
func (h *Handler) BindSweepGauges(set *metrics.Set) {
	set.OrphanObjects.Bind(func() float64 { return float64(h.sweepReport().Orphans) })
	set.StorageBytes.Bind(func() float64 { return float64(h.sweepReport().StorageBytes) })
}

func (h *Handler) sweepReport() SweepReport {
	h.sweepMu.Lock()
	defer h.sweepMu.Unlock()
	return h.sweep
}

func (h *Handler) setSweepReport(rep SweepReport) {
	h.sweepMu.Lock()
	h.sweep = rep
	h.sweepMu.Unlock()
}

// RunSweep is the background loop: every SweepTick the node asks
// whether the hour has come and whether it is the first name of the
// live set; the nodes that are not read the last report instead, so
// their gauges carry the same figures.
func (h *Handler) RunSweep(ctx context.Context) error {
	var last time.Time
	wait.Every(ctx, h.sweepTick, func(ctx context.Context) {
		now := h.now().UTC()
		if !sweepDue(now, last) {
			h.readSweepReport(ctx)
			return
		}
		last = now
		if h.sweepNode() != h.node {
			h.readSweepReport(ctx)
			return
		}
		rep, err := h.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			h.logger.WarnContext(ctx, "orphan sweep", "error", err)
			return
		}
		h.setSweepReport(rep)
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
func (h *Handler) sweepNode() string {
	if h.members == nil {
		return h.node
	}
	live := h.members.Live()
	if len(live) == 0 {
		return h.node
	}
	return slices.Min(live)
}

// readSweepReport takes the two gauges from the last run the sweeping
// node wrote.
func (h *Handler) readSweepReport(ctx context.Context) {
	rc, _, err := h.log.Store().Get(ctx, h.log.Prefix()+SweepKey, "")
	if err != nil {
		if !errors.Is(err, wal.ErrNotFound) {
			h.logger.WarnContext(ctx, "sweep report unreadable", "error", err)
		}
		return
	}
	defer func() { _ = rc.Close() }()
	var rep SweepReport
	if err := json.NewDecoder(io.LimitReader(rc, 64<<10)).Decode(&rep); err != nil {
		h.logger.WarnContext(ctx, "sweep report unreadable", "error", err)
		return
	}
	h.setSweepReport(rep)
}

// Sweep is one run: one listing of the whole prefix, every key
// classified against what the deck defines, the orphans counted and the
// ones past the hold deleted, and the report written for the other
// nodes.
func (h *Handler) Sweep(ctx context.Context) (SweepReport, error) {
	now := h.now().UTC()
	rep := SweepReport{At: now, Node: h.node}
	objects, err := h.listPrefix(ctx)
	if err != nil {
		return rep, err
	}
	known, err := h.knownKeys(ctx, objects)
	if err != nil {
		return rep, err
	}
	var errs []error
	for _, o := range objects {
		rep.StorageBytes += o.Size
		rel := strings.TrimPrefix(o.Key, h.log.Prefix())
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
		if err := h.log.Store().Delete(ctx, o.Key); err != nil {
			errs = append(errs, err)
			continue
		}
		rep.Deleted++
	}
	if err := h.writeSweepReport(ctx, rep); err != nil {
		errs = append(errs, err)
	}
	h.logger.InfoContext(ctx, "orphan sweep", "orphans", rep.Orphans, "unknown", rep.Unknown,
		"deleted", rep.Deleted, "storage_bytes", rep.StorageBytes, "keys", slog.Any("keys", rep.Keys))
	return rep, errors.Join(errs...)
}

func (h *Handler) writeSweepReport(ctx context.Context, rep SweepReport) error {
	data, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	_, err = h.log.Store().Put(ctx, h.log.Prefix()+SweepKey, wal.BytesBody(data))
	return err
}

// listPrefix pages through every key under the log's prefix.
func (h *Handler) listPrefix(ctx context.Context) ([]wal.Object, error) {
	var (
		out   []wal.Object
		after string
	)
	for {
		res, err := h.log.Store().List(ctx, wal.ListOptions{Prefix: h.log.Prefix(), StartAfter: after, Max: 1000})
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
func (h *Handler) knownKeys(ctx context.Context, objects []wal.Object) (map[string]bool, error) {
	known := make(map[string]bool, len(objects))
	ids := map[string]bool{}
	for _, o := range objects {
		rel := strings.TrimPrefix(o.Key, h.log.Prefix())
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
		h.knownForRepo(ctx, id, known)
	}
	return known, nil
}

// knownForRepo marks the keys of one repository an index, a marker, or
// its metadata names.
func (h *Handler) knownForRepo(ctx context.Context, id string, known map[string]bool) {
	prefix := h.log.RepoPrefix(id)
	known[prefix+"meta"] = true
	known[prefix+wal.LatestKey] = true
	ix, _, err := h.log.Newest(ctx, id, 0, false)
	switch {
	case errors.Is(err, wal.ErrNotFound):
		// A purge left the tombstone and nothing else, which the two
		// keys above already cover.
		return
	case err != nil:
		// An outage or a corrupt index is not a reason to call a
		// repository's objects orphans: every one of them is left alone
		// until a week from now.
		h.logger.WarnContext(ctx, "sweep left a repository alone", "repo", id, "error", err)
		for _, o := range h.repoObjects(ctx, id, "") {
			known[o] = true
		}
		return
	}
	// Index objects are never orphans: the currency check of spec 006
	// reads a 404 on index/<n+1> as proof a copy is current.
	for _, o := range h.repoObjects(ctx, id, "index/") {
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
	for _, o := range h.repoObjects(ctx, id, "lfs/verified/") {
		known[o] = true
		known[prefix+"lfs/"+path.Base(o)] = true
	}
}

// repoObjects lists the keys under one repository's sub-prefix.
func (h *Handler) repoObjects(ctx context.Context, id, sub string) []string {
	var (
		out   []string
		after string
	)
	prefix := h.log.RepoPrefix(id) + sub
	for {
		res, err := h.log.Store().List(ctx, wal.ListOptions{Prefix: prefix, StartAfter: after, Max: 1000})
		if err != nil {
			h.logger.WarnContext(ctx, "sweep listing failed", "repo", id, "prefix", sub, "error", err)
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
