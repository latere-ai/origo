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
updated: 2026-09-07
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
node. The primary checks the thresholds after every push it serves and
in a sweep every 10 minutes over the repositories it holds locally, and
compacts when any of these holds for the newest index:

| Condition | Threshold |
|---|---|
| entries since compaction | `len(entries) > 64` |
| bytes of those entries | sum of `pack_bytes` over `entries` > 256 MiB |
| size of the index object | > 512 KiB |

The after-push check schedules the compaction in the background and
returns; the request never waits for it. At most one compaction per
repository runs on a node at a time: a trigger that finds one running
is a no-op, and the sweep starts the next one. A node that is not the
primary never compacts; it only downloads packs. Two primaries after a
placement change are harmless: create-if-absent lets one commit and the
other aborts.

This spec owns compaction wherever it is asked for. `POST
/v1/repos/{id}/gc` (spec 019) on the primary runs the procedure below at
once, whatever the thresholds say, and answers when it is done. On any
other node it forwards nothing and compacts nothing: it creates the
request object `origo/gc/<id>` holding `{"requested_at", "node"}` (an
existing one is left alone) and answers 202 naming the primary in the
body's `details`. The primary's 10 minute sweep lists `origo/gc/`,
usually empty, and for every id whose primary it is runs the procedure
and deletes the request object; the sweep on a node that is not the
primary of a listed id leaves it. A request is therefore acted on within
10 minutes while the primary is up; the compaction sweep on any node
deletes a request object older than 24 hours, which only a primary that
never ran leaves behind.

### Procedure

`internal/compact.Run(repo)` on the primary, under its own deadline of
30 minutes for the whole run (the repack subprocess runs with that
deadline rather than the 5 minutes of spec 004, which spec 012 records):

```mermaid
sequenceDiagram
  participant C as compaction
  participant L as repository lock
  participant G as git
  participant S as object storage
  C->>L: acquire read (currency check, apply missing entries)
  C->>G: repack into new packs, old packs kept
  C->>G: fsck connectivity
  C->>S: PUT packs/<hash>.idx, packs/<hash>.pack
  C->>L: release read, acquire write
  C->>S: commit compact entry (create index/n+1)
  alt won
    C->>G: delete packs the new index does not list
  else lost round
    C->>C: abort stale, uploaded packs stay
  end
  C->>L: release write
```

1. `Acquire` the repository for reading, which runs the currency check
   and applies every missing entry; hold `index/<n>`. The read lock
   keeps eviction and a rebuild away from the directory; fetches share
   it. The cache's lock is a `sync.RWMutex`, so a push that arrives
   while the repack runs waits for the write lock, and readers that
   arrive after that push queue behind it. That wait is bounded by the
   next step, not by the 30 minute deadline.
2. `git repack --geometric=2 --write-midx --write-bitmap-index` without
   `-d`: it writes new packs that fold the small ones into geometrically
   sized ones, leaves large packs alone, and deletes nothing, so every
   pack the held index lists is still on disk. After a compaction gap of
   at most 64 thin entries this is seconds; a full repack happens once,
   after an import (spec 019) or a first compaction of a large history.
3. `git fsck --connectivity-only --no-progress`; a failure aborts with
   `origo_compactions_total{result="error"}` and the copy is evicted for
   rebuild (spec 004).
4. Upload every pack now under `objects/pack/` that the held index does
   not list, with its `.idx`, as `packs/<hash>.pack` and `packs/<hash>.idx`,
   `<hash>` from the file name `pack-<hash>.pack`; the `.idx` first so a
   reader never sees a pack without one.
5. Release the read lock and take the write lock; a push that landed in
   between has advanced the local sequence, and `Log.Commit` sees it in
   the next step. Commit a `compact` entry with an empty transaction, no
   pack, `Packs` = the packs of step 2 (the new ones and the large ones
   left alone), and `CompactedThrough = n`, through `Log.Commit`. The
   catch-up callback refuses: a lost round means a push landed after
   step 1, and a replay would produce an index whose `entries` no longer
   names that push. The compaction aborts with `result="stale"`, its
   entry becomes an orphan the sweeper removes, the uploaded packs stay
   for the next run, the write lock is released, and the primary starts
   again from step 1 at the next trigger.
6. On success, still under the write lock, swap the pack list: delete
   the packs under `objects/pack/` that the new index does not list
   (`git repack -d` semantics, done by the node so the set on disk equals
   the index) and rewrite the multi-pack index. Release the write lock,
   `result="ok"`, `origo_compaction_seconds` observed, and the sweeper
   (spec 004) deletes the folded entries and the superseded index
   objects once they are older than `ORIGO_SWEEP_MIN_AGE`.

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
`origo_orphan_objects` accounting (spec 019). The `gc` endpoint's
request and response shape, rate limit, and event (spec 019).

## Acceptance criteria

- After 500 pushes of 1 KiB commits to one repository through one node,
  the newest index lists at most 64 entries and at most 6 packs, and
  `git fsck` passes on a fresh node's copy (proposed: `internal/compact`,
  `TestFiveHundredPushesStayUnder64EntriesAnd6Packs`).
- A push that lands between step 1 and step 5 makes the compaction
  abort with `result="stale"`, the push is in the newest index, and the
  next run folds it (proposed: `internal/compact`,
  `TestPushDuringCompactionIsPreserved`).
- The after-push trigger returns before the compaction starts, a second
  trigger while one runs starts none, and a clone served during step 2
  succeeds (proposed: `internal/compact`,
  `TestTriggerIsBackgroundAndSingle`).
- A `gc` request on a node that is not the primary creates
  `origo/gc/<id>`, answers 202 naming the primary, and the primary's
  sweep compacts within one sweep interval and deletes the request
  object with a fake clock (proposed: `internal/compact`,
  `TestGcRequestIsPickedUpByThePrimary`).
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
