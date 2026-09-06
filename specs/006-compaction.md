---
title: "Compaction: primary-only repacks and log truncation"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
affects: [internal/compact/, internal/wal/, internal/repo/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Compaction

## Overview

A repository that receives many pushes accumulates many small entries.
Serving a fetch from thousands of thin packs is slow and materializing
from them slower. Compaction repacks on one node and records the result
as a log entry, so every other node downloads packs instead of
repacking.

## Current state

Spec 004 stores entries and lists packs in the index: `wal.Entry` has
`Kind: compact`, `Packs`, and `CompactedThrough`; `Log.Commit` builds
the index object for one (`packs` replaced, `entries` reset to the
compaction entry, `compacted_through` set); `repo.Cache.Apply` fetches
listed packs (`TestCompactionPacksAreFetched`); the sweeper deletes
folded entries and superseded index objects. Nothing produces a
compaction entry: `internal/compact` does not exist.

## Design

### Trigger

The first name in `Origo-Prefer` for a repository (spec 005) is its
compaction primary; with one node, every repository's primary is that
node. The primary checks the trigger after every push it serves and in a
sweep every 10 minutes over the repositories it holds locally, and
compacts when any of these holds for the newest index:

| Condition | Threshold |
|---|---|
| entries since compaction | `len(entries) > 64` |
| bytes of those entries | sum of `pack_bytes` over `entries` > 256 MiB |
| size of the index object | > 512 KiB |

A node that is not the primary never compacts; it only downloads packs.
Two primaries after a placement change are harmless: create-if-absent
lets one commit and the other aborts.

### Procedure

`internal/compact.Run(repo)` on the primary:

1. `Acquire` the repository for writing, which runs the currency check
   and applies every missing entry; hold `index/<n>`.
2. `git repack -d --geometric=2 --write-midx --write-bitmap-index`,
   which folds small packs into geometrically sized ones and leaves large
   packs alone.
3. `git fsck --connectivity-only --no-progress`; a failure aborts with
   `origo_compactions_total{result="error"}` and the copy is evicted for
   rebuild (spec 004).
4. Upload every pack now under `objects/pack/` that the held index does
   not list, with its `.idx`, as `packs/<hash>.pack` and `packs/<hash>.idx`,
   `<hash>` from the file name `pack-<hash>.pack`; the `.idx` first so a
   reader never sees a pack without one.
5. Commit a `compact` entry with an empty transaction, no pack,
   `Packs` = every pack now under `objects/pack/`, and
   `CompactedThrough = n`, through `Log.Commit`. The catch-up callback
   refuses: a lost round means a push landed after step 1, and a replay
   would produce an index whose `entries` no longer names that push. The
   compaction aborts with `result="stale"`, its entry becomes an orphan
   the sweeper removes, the uploaded packs stay for the next run, and
   the primary starts again from step 1.
6. On success, `result="ok"`, `origo_compaction_seconds` observed, and
   the sweeper (spec 004) deletes the folded entries and the superseded
   index objects once they are older than `ORIGO_SWEEP_MIN_AGE`.

The sweeper gains one rule: a pack under `packs/` that the newest index
does not list and that is older than `ORIGO_SWEEP_MIN_AGE` is deleted.
The age keeps a node that started materializing on the previous index
able to finish.

### Invariants

Compaction never changes a reference and never drops a reachable
object: the transaction is empty, the reference map is copied, and step 3
proves connectivity before anything is uploaded. It commits by the same
create-if-absent as a push, so a push and a compaction never interleave
in the index and the index stays a total order.

### Cost

Compaction is CPU on one node and upload bandwidth once; replicas trade
bandwidth for CPU. The tuning target is a repository receiving 100 pushes
per minute staying under 100 entries with its fetch latency flat.

## Not in this spec

Repacking on a node other than the primary. Compacting LFS objects.
`origo_orphan_objects` accounting (spec 019).

## Acceptance criteria

- After 500 pushes of 1 KiB commits to one repository through one node,
  the newest index lists at most 64 entries and at most 6 packs, and
  `git fsck` passes on a fresh node's copy (proposed: `internal/compact`,
  `TestFiveHundredPushesStayUnder64EntriesAnd6Packs`).
- A push that lands between step 1 and step 5 makes the compaction
  abort with `result="stale"`, the push is in the newest index, and the
  next run folds it (proposed: `internal/compact`,
  `TestPushDuringCompactionIsPreserved`).
- A node that read the previous index before compaction and fetches its
  entries after it completes materializes successfully as long as the
  entries are younger than `ORIGO_SWEEP_MIN_AGE` (proposed:
  `internal/compact`, `TestDelayedReaderSurvivesCompaction` with a fake
  clock).
- A pack no index lists is deleted by the sweeper after
  `ORIGO_SWEEP_MIN_AGE` and one that the newest index lists is kept
  (proposed: `internal/wal`, `TestSweepRemovesUnlistedPacks`).
- Fetch latency of a 1 GiB repository after 1 000 pushes is within 10%
  of its latency after 10 pushes, measured as the p50 of 20 clones each
  (proposed: `test/e2e`, `TestMeasureCompaction` under `ORIGO_E2E_MEASURE=1`).
