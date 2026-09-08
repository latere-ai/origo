// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package compact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// errStale is what the commit's catch-up callback answers: a push landed
// after step 1, so a replay would produce an index whose entries no
// longer name that push.
var errStale = errors.New("compact: a push landed while the repack ran")

// errNoSlot is a git subprocess that waited SlotWait for a slot of the
// subprocess semaphore (spec 012) and got none.
var errNoSlot = errors.New("compact: no subprocess slot")

// errFsck is a copy that failed the connectivity check of step 3. The
// copy is evicted for rebuild (spec 004) once the read lock is released.
var errFsck = errors.New("compact: the local copy failed the connectivity check")

// compact runs the procedure of spec 006 on the primary, under one
// deadline for the whole run:
//
//  1. acquire the repository for reading, which runs the currency check
//     and applies every missing entry, and hold index/<n>;
//  2. repack geometrically without -d, so every pack the held index
//     lists is still on disk for the fetches that share the read lock;
//  3. prove connectivity;
//  4. upload the packs the repack left that the held index does not
//     name, the .idx before the .pack so a reader never sees a pack
//     without one;
//  5. release the read lock, take the write lock, and commit a compact
//     entry on index/<n>, which a push that landed meanwhile has taken;
//  6. on success, delete the packs the new index does not list and
//     rewrite the multi-pack index over what is left.
func (m *Manager) compact(ctx context.Context, id string) (Outcome, Figures, Figures, error) {
	ctx, cancel := context.WithTimeout(ctx, m.deadline)
	defer cancel()

	r, release, err := m.cache.Acquire(ctx, id, false)
	if err != nil {
		return OutcomeError, Figures{}, Figures{}, err
	}
	held, dir := r.Index, r.Dir
	before := figures(held)
	packs, err := m.repack(ctx, r)
	if err != nil {
		release()
		if errors.Is(err, errNoSlot) {
			return OutcomeSkipped, before, Figures{}, nil
		}
		if errors.Is(err, errFsck) {
			// Evicting takes the write lock, so it waits for the read
			// lock this run held until the line above.
			m.cache.Evict(id)
		}
		return OutcomeError, before, Figures{}, err
	}
	keys, bytes, err := m.upload(ctx, id, dir, held, packs)
	release()
	if err != nil {
		return OutcomeError, before, Figures{}, err
	}

	if m.afterRepack != nil {
		m.afterRepack(id)
	}
	// The write lock: a push that landed since step 1 has advanced the
	// local sequence, and the commit below is refused on the index it
	// took. The base is the index held at step 1, never the one this
	// acquire applied, because the packs prove the history through that
	// sequence alone.
	w, release, err := m.cache.Acquire(ctx, id, true)
	if err != nil {
		return OutcomeError, before, Figures{}, err
	}
	defer release()
	entry := wal.Entry{Kind: wal.KindCompact, Packs: keys, PacksBytes: bytes, CompactedThrough: held.Seq}
	committed, err := m.log.Commit(ctx, id, held, entry, func(context.Context, *wal.Index) error { return errStale })
	if err != nil {
		if errors.Is(err, errStale) {
			return OutcomeStale, before, Figures{}, nil
		}
		return OutcomeError, before, Figures{}, err
	}
	if err := m.cache.Advance(w, committed.Index); err != nil {
		return OutcomeError, before, Figures{}, err
	}
	// Step 6 runs after the log is already correct, so a failure here is
	// a warning: the copy holds packs the index does not list, which the
	// next run's repack folds again and this step then removes.
	if err := m.swapPacks(ctx, w, committed.Index); err != nil {
		m.logger.WarnContext(ctx, "compacted, pack list on disk not swapped", "repo", id, "error", err)
	}
	m.logger.InfoContext(ctx, "compacted", "repo", id, "seq", committed.Index.Seq, "compacted_through", held.Seq,
		"packs", len(committed.Index.Packs), "entries", len(held.Entries), "size_bytes", committed.Index.SizeBytes)
	return OutcomeOK, before, figures(committed.Index), nil
}

// repack is steps 2 and 3: fold the small packs into geometrically sized
// ones without deleting anything, then prove connectivity. It answers
// the pack files the repack leaves current, which is what the
// multi-pack-index it wrote names.
func (m *Manager) repack(ctx context.Context, r *repo.Repo) ([]string, error) {
	if m.beforeRepack != nil {
		m.beforeRepack(r.ID)
	}
	if _, err := m.run(ctx, r.Dir, "repack", "--geometric=2", "--write-midx", "--write-bitmap-index"); err != nil {
		return nil, err
	}
	if _, err := m.run(ctx, r.Dir, "fsck", "--connectivity-only", "--no-progress"); err != nil {
		if errors.Is(err, errNoSlot) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errFsck, err)
	}
	return packSet(r.Dir)
}

// upload is step 4: every pack the repack left current that the held
// index does not already name goes to the log under the key of the name
// git gave the file, the .idx first. It answers the pack keys the
// compact entry lists and their .pack bytes, the index's size_bytes.
func (m *Manager) upload(ctx context.Context, id, dir string, held *wal.Index, packs []string) ([]string, int64, error) {
	listed := make(map[string]bool, len(held.Packs))
	for _, p := range held.Packs {
		listed[p] = true
	}
	keys := make([]string, 0, len(packs))
	var total int64
	for _, file := range packs {
		key := wal.PackKey(file)
		keys = append(keys, key)
		path := filepath.Join(dir, "objects", "pack", file)
		info, err := os.Stat(path)
		if err != nil {
			return nil, 0, err
		}
		total += info.Size()
		if listed[key] {
			continue
		}
		for _, ext := range []string{".idx", ".pack"} {
			from := trimPackExt(path) + ext
			to := m.log.RepoPrefix(id) + trimPackExt(key) + ext
			body, err := wal.FileBody(from)
			if err != nil {
				return nil, 0, err
			}
			if _, err := m.log.Store().Put(ctx, to, body); err != nil {
				return nil, 0, fmt.Errorf("compact: upload %s: %w", to, err)
			}
		}
	}
	return keys, total, nil
}

// trimPackExt drops the .pack suffix of a pack file or key.
func trimPackExt(s string) string { return strings.TrimSuffix(s, ".pack") }

// swapPacks is step 6: the packs on disk become exactly what the new
// index lists, and the multi-pack index is rewritten over them, so the
// bitmap the repack built covers the set that is there.
func (m *Manager) swapPacks(ctx context.Context, r *repo.Repo, ix *wal.Index) error {
	listed := make(map[string]bool, len(ix.Packs))
	for _, p := range ix.Packs {
		listed[wal.PackFile(p)] = true
	}
	dir := filepath.Join(r.Dir, "objects", "pack")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		base, ok := packBase(name)
		if !ok || listed[base+".pack"] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	if len(ix.Packs) == 0 {
		return nil
	}
	_, err = m.run(ctx, r.Dir, "multi-pack-index", "write", "--bitmap")
	return err
}

// packBase reads the pack a file under objects/pack belongs to:
// pack-<hash>.idx, .pack, and .rev all belong to pack-<hash>. Anything
// else, the multi-pack index and its bitmap included, is not a pack
// file and is left alone.
func packBase(name string) (string, bool) {
	if !strings.HasPrefix(name, "pack-") {
		return "", false
	}
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		if base, ok := strings.CutSuffix(name, ext); ok {
			return base, true
		}
	}
	return "", false
}

// run is one git subprocess of the run: it takes a slot of the
// subprocess semaphore (spec 012) first, and a wait longer than SlotWait
// skips the run rather than holding a slot the requests need.
func (m *Manager) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if m.slots != nil {
		release, ok := m.slots.Acquire(ctx, SlotWait)
		if !ok {
			return nil, errNoSlot
		}
		defer release()
	}
	return m.git.Run(ctx, dir, nil, args...)
}
