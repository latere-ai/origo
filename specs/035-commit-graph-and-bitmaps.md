---
title: "Commit-graph and bitmaps: compaction writes git's history indexes once and every node installs them"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/006-compaction.md
  - specs/009-read-api-and-archive.md
  - specs/017-release-and-versioning.md
  - specs/019-repository-administration.md
affects: [internal/compact/, internal/repo/, internal/wal/, internal/api/, docs/internals/architecture.md, specs/001-architecture.md, specs/004-write-ahead-log.md, specs/006-compaction.md, specs/019-repository-administration.md, specs/README.md]
effort: medium
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Commit-graph and bitmaps

## Overview

Two files make git's history walks cheap, and no node has the first while
only one node has the second. A commit-graph holds every commit's parents,
root tree, and generation number, and with changed-path filters a Bloom
filter of the paths each commit touched, so a path-limited walk skips the
commits that cannot have changed the path without opening their trees. A
reachability bitmap answers "which objects does this clone need" without
walking the history.

Measured with git 2.54.0 on a synthetic repository of 20 000 commits over
40 000 files (237 051 objects, 64 MiB of packs and pack indexes), the
walk behind `GET /v1/repos/{id}/commits` with a `path` that one commit
touched:

| Copy | Walk | Bloom statistics |
|---|---|---|
| no commit-graph (every node today) | 89.9 ms | none |
| commit-graph without changed paths | 49.3 ms | none |
| commit-graph with changed paths | 15.1 ms | 19 998 `definitely_not`, 2 `maybe`, 1 false positive |

On the same fixture, counting the objects of a full clone takes 380 ms
without a bitmap and 14 ms with one, and the `pack-objects` run behind the
clone 688 ms against 149 ms. A node runs with a CPU request of 250m (spec
016), so these figures are a floor.

This spec makes compaction write the commit-graph with changed-path filters
(`git commit-graph write --reachable --changed-paths`), upload it beside
the multi-pack index and bitmap the compaction already builds, and makes
every node install the three when it builds or rebuilds a copy. Nothing in
the index object changes. The commit-graph is built first, the bitmap
second (Build order).

## Current state

Not built. Compaction (`internal/compact/run.go`, `Manager.compact`) runs
`git repack --geometric=2 --write-midx --write-bitmap-index` in
`Manager.repack`, reads the multi-pack index's `PNAM` chunk in `packSet`
(`internal/compact/midx.go`), uploads only each pack's `.idx` and `.pack`
in `Manager.upload`, and in `Manager.swapPacks` deletes the superseded
packs and runs `git multi-pack-index write --bitmap`. The bitmap therefore
exists on the compaction primary alone. An import (`internal/api/importer.go`,
`Handler.mirror`) runs `git repack -a -d` and uploads `.idx` and `.pack`
the same way.

A node materializes in `repo.Cache.Apply` (`internal/repo/cache.go`):
`Cache.fetchPack` downloads each listed pack's `.idx` and `.pack`,
`Cache.applyEntries` indexes entry packs with `index-pack` under names of
its own batches, and `Cache.initBare` sets `gc.auto 0` and
`receive.autogc false`. Nothing writes a commit-graph anywhere, so
`Handler.commits` (`internal/api/read.go`) runs `git rev-list
--no-commit-header --format=... <oid> -- <path>` over a copy with neither
file.

`wal.ParseIndex` (`internal/wal/format.go`) decodes with
`DisallowUnknownFields`, so a new field in an index object is a new log
format version. `wal.Log.Sweep` (`internal/wal/sweep.go`) skips a key under
`wal/` that `ParseEntryKey` does not parse and deletes any key under
`packs/` the newest index does not list; `api.Sweeper.knownForRepo`
(`internal/api/sweep.go`) calls a key no index names an orphan.

With spec 005's default of one replica, the preferred node of a
repository is its compaction primary, so the bitmap reaches the node that
serves most clones already; a node that serves a repository with
`replicas` above 1, or while the primary is away, has none.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| A field on the index object naming the files | `graph`, `midx` on the index a compaction commits | `ParseIndex` refuses an unknown field, so the field is a format version. Under spec 017's rule a node writes the newest version, and under the rule spec 032 proposes the lowest version that can hold the object; under either, every compacted index carries the new version and a release before this spec refuses every compacted repository, for an accelerator whose absence costs nothing but time |
| Every node writes its own | `commit-graph write` and `multi-pack-index write --bitmap` after each materialization | the graph took 0.50 s for 20 000 commits on the machine measured and grows with the history, the bitmap walks every commit, and every node repeats what one primary does once |
| A pack bitmap | `pack-<hash>.bitmap` beside one pack | the geometric roll-up leaves several packs, a pack bitmap covers one, and the primary already builds the multi-pack bitmap over all of them |
| Files named by the compaction's own entry | `wal/<seq>.<nonce>.graph`, `.midx`, `.bitmap` | chosen: no format change, a key every node derives from the index it already read, and the life of the entry they describe |

## Design

### What git does, checked

Every row was checked with git 2.54.0 in a scratch repository; the image
ships 2.47 (spec 017), which the build's tests run against.

| Behavior | Observed |
|---|---|
| a commit-graph written in one copy, installed in another holding the same commits in other packs | `git commit-graph verify` passes and the walk uses it: 18.8 ms on that copy |
| 300 commits pushed after the graph was written | they are outside the graph; the walk diffs their trees and still answers exactly: 16.0 ms against 15.1 ms, the filters consulted only for the 20 000 commits inside |
| a graph naming commits the copy does not hold | reads still answer, but `git commit-graph verify` exits 1 and `git fsck --connectivity-only` exits 16 |
| `commit-graph write --split` over a single-file graph | moves the file to `objects/info/commit-graphs/graph-<hash>.graph`, `<hash>` the file's trailing SHA-1, and adds a layer; 70 ms for 300 new commits |
| a second compaction cycle with the graph present: `repack --geometric=2 --write-midx --write-bitmap-index`, the superseded packs deleted, `multi-pack-index write --bitmap` | the repack leaves the graph file untouched, `commit-graph verify` and `fsck --connectivity-only` pass after each step, and a new single-file graph written over it verifies |
| a pack bitmap after a later pack arrives | still used: 302 bitmap misses, 39.6 ms to count against 13.6 ms |
| a pack bitmap whose pack was repacked away | ignored without a message; counting falls back to 390 ms |
| the multi-pack index and bitmap of the primary, installed in a copy holding the same packs plus another and no `.rev` files | `multi-pack-index verify` and `fsck` pass, the bitmap is used (52 misses), a full `pack-objects` takes 135 ms |
| `multi-pack-index write --stdin-packs --preferred-pack=<p> --bitmap` over the same packs | byte-identical multi-pack index and bitmap; without `--bitmap` the index lacks its reverse-index chunk and differs |
| file names | the multi-pack bitmap is `multi-pack-index-<hash>.bitmap`, `<hash>` the trailing checksum of `multi-pack-index`; the graph starts with `CGPH`, the multi-pack index with `MIDX` |

The third row is why a node installs a graph only after the packs it was
written from are on disk: spec 004's sampled connectivity check would
otherwise evict the copy.

### What compaction writes

Spec 006's procedure gains three things, in its own step numbers:

1. After step 3 proves connectivity, the primary runs `git commit-graph
   write --reachable --changed-paths` in its copy, still under the read
   lock, through `compact.Slots` and under the run's deadline like every
   subprocess of the run. It covers every commit reachable from the
   references of the held index `n`, all of which are in the packs step 2
   left current. The primary's own walks use it from this moment.
2. Step 4 draws the 16 hex nonce of the entry it will commit and, after the
   packs, uploads three objects under the stem `wal/<n+1>.<nonce>`: the
   graph, and the multi-pack index and bitmap step 2 wrote, which name
   exactly the packs the new index will list (spec 006, step 4). An upload
   that fails is a warning, the run goes on without that object, and the
   commit does not wait for it.
3. Step 5 commits the entry under that nonce, so the entry key is
   `wal/<n+1>.<nonce>.entry` and the three objects share its stem. A run
   that loses its round leaves them with no entry the index names, and the
   sweep removes them as it removes the orphan entry.

Step 6 is unchanged: its rewrite of the multi-pack index on the primary
covers the same packs, and where git picks another preferred pack its bytes
differ from the uploaded one's while both stay valid for those packs.

An import (spec 019) writes the graph after its `fsck`, uploads it under
its own entry's stem, and uploads no bitmap; the repository gets one at its
first compaction.

### The objects

| Key | Content |
|---|---|
| `wal/<seq>.<nonce>.graph` | the commit-graph file as git wrote it, beginning `CGPH` and ending with the SHA-1 of the bytes before it |
| `wal/<seq>.<nonce>.midx` | the multi-pack index of step 2, beginning `MIDX`; its `PNAM` chunk names the packs of the index that commits the entry |
| `wal/<seq>.<nonce>.bitmap` | the bitmap git wrote beside it, valid for that multi-pack index alone |

Each belongs to the entry `wal/<seq>.<nonce>.entry`, the way a pack's
`.idx` belongs to its `.pack`. They are immutable, never listed in an
index object, and never needed for correctness: a copy without them
answers every request the same, more slowly.

### Installing on a node

`Cache.Apply` gains one step between spec 004's step 2 (packs) and step 3
(entries). When the first row of the index's `entries` is a `compact`
entry, a compaction's or an import's, and the copy has not yet tried that
entry's stem:

1. `GET wal/<seq>.<nonce>.graph`, check the magic and that the trailer is
   the SHA-1 of the bytes before it, and write it to
   `objects/info/commit-graph` atomically, replacing any graph the copy
   holds. Every commit it names is in the packs step 2 just fetched.
2. `GET` the `.midx` and read its `PNAM` with the parser spec 006 already
   has (moved from `internal/compact` to `internal/wal`). When the names
   equal the index's `packs` and each is a file under `objects/pack`,
   `GET` the `.bitmap`, write it as
   `objects/pack/multi-pack-index-<hash>.bitmap` and then the index as
   `objects/pack/multi-pack-index`, each atomically, and remove any other
   `multi-pack-index-*.bitmap`. Packs the copy holds outside the multi-pack
   index, from entries and from earlier compactions, are read by git as
   ordinary packs.
3. Record the stem in the copy's state file, `<id>.origo.json`, as
   `"sidecars": "<seq>.<nonce>"`, whether or not anything was installed.

A 404, a failed check, or a name mismatch installs nothing from that object
and is a `warn` log line naming the key, never an integrity error, never a
retry: the stem is recorded and the next compaction brings a new one. A
copy that keeps an older graph keeps a valid one: only the primary deletes
packs (spec 006, step 6), in the run that rewrote its own graph, so every
other copy still holds every commit the older graph names. The downloads run inside `Apply` under the write lock, like the
packs they describe.

| Cost per compaction, per node | Size on the fixture |
|---|---|
| graph | 1 483 676 bytes for 20 000 commits, about 75 bytes a commit |
| multi-pack index | 7 634 888 bytes for 238 551 objects, about 32 bytes an object |
| bitmap | 2 250 754 bytes |

The three add 17% to the 67 MB of packs and pack indexes a cold
materialization of the fixture downloads, and three `GET` requests per
compaction to a warm one.

### No incremental graph after a push

A node does not write a commit-graph layer after a push. On the fixture, 300
commits outside the graph cost 0.9 ms on the walk above, while writing a
`--split` layer for them took 70 ms, would run on every node that applies
the push, and would take the repository's write lock. The commits outside
the graph are what the repository received since its last compaction, which
spec 006's thresholds bound to 64 entries or 256 MiB. A history that
arrives in a single push of less than that, the first push of an existing
project, has no graph until the next compaction; `POST /v1/repos/{id}/gc`
(spec 019) writes one at once, and an import writes its own.

### Sweeping

| Deleter | Rule |
|---|---|
| `wal.Log.Sweep` | reads `wal/<seq>.<nonce>.<ext>`, `<ext>` one of `graph`, `midx`, `bitmap`, as belonging to `wal/<seq>.<nonce>.entry` and applies that entry's rules: deleted once older than `ORIGO_SWEEP_MIN_AGE` when `<seq>` is at or below `compacted_through`, above the newest index, or names an entry the newest index does not list |
| purge (spec 019) | unchanged: every object under the prefix but `meta` |
| the weekly orphan sweep (spec 019) | a sidecar of an entry the newest index names is known; any other is an orphan |

### Versions

This spec writes no field into an index object or an entry header, so every
object stays at `v: 1` and nothing calls for spec 017's version rule. A
release before this spec ignores the three keys: its sweep skips them under
`wal/` and its weekly orphan sweep deletes them after 7 days. A rollback
costs the speed and nothing else; after rolling forward again a node finds
404 for those stems and waits for the next compaction.

### Build order

The commit-graph is built and released first, the bitmap after it:

- the graph stays valid however the copy's packs change, so installing it
  depends on nothing but the packs being fetched; the bitmap is valid for
  one exact pack set and must be replaced at every compaction;
- the graph serves the node that serves most reads too, since no node has
  one today, while the bitmap reaches only nodes other than the primary,
  which spec 005's default sends few clones;
- the graph speeds up the read API and every history walk (the path walk
  above, blame in spec 039, merge bases for spec 020's operations); the
  bitmap speeds up clones and fetches, and spec 036 sends the bulk of a
  large clone to the bucket instead;
- the graph adds about 2% to a cold materialization, the multi-pack index
  and bitmap about 15%.

### Names the build adds

| Name | Kind | Meaning |
|---|---|---|
| `wal.Entry.Nonce` | Go field | the nonce a caller draws for its entry; empty draws one inside `Log.Commit` as today |
| `wal.SidecarKey` | Go function | `wal/<seq>.<nonce>.<ext>` for an entry key and an extension |
| `wal.MidxPacks` | Go function | the `PNAM` parser of `internal/compact/midx.go`, moved so a node reads it |
| `sidecars` | state file field | the stem whose sidecars the copy tried, in `<id>.origo.json` |

None is a variable, metric, header, endpoint, or error code; the key table
above moves into spec 004's Objects table with the build.

## Not in this spec

A per-push or per-node incremental graph (above). A pack bitmap for an
import. Bloom filter settings other than git's default. Hiding commits from
the graph. Using the graph's generation numbers for anything git does not
already use them for. Cloning from the bucket (spec 036).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A compaction of a 2 000-commit fixture uploads `.graph`, `.midx`, and `.bitmap` under its entry's stem before the commit, the `.midx` naming exactly the new index's packs, against the real git | `internal/compact`, `TestCompactionUploadsGraphAndBitmap` | not built |
| A run that loses its round leaves sidecars the sweep deletes after `ORIGO_SWEEP_MIN_AGE` with the orphan entry, and the sidecars of a folded compaction go with its entry | `internal/wal`, `TestSweepRemovesSidecarsWithTheirEntry` | not built |
| A fresh node materializing the compacted fixture installs the graph and the bitmap, `commit-graph verify`, `multi-pack-index verify`, and `fsck --connectivity-only` pass, and a path-limited `rev-list` reports changed-path statistics under `GIT_TRACE2_PERF` | `internal/repo`, `TestApplyInstallsSidecars` | not built |
| A `.midx` whose packs differ from the index's, a graph with a bad trailer, and a 404 each install nothing, log one `warn` line, record the stem, and leave `Apply` successful with no second `GET` on the next apply | `internal/repo`, `TestSidecarFailuresAreHarmless` | not built |
| A node that keeps an older graph after a newer compaction whose graph is missing passes the sampled connectivity check | `internal/repo`, `TestOlderGraphStaysValid` | not built |
| An import of the 5 000-commit fixture uploads a graph under its entry's stem and a cold node installs it | `internal/api`, `TestImportWritesGraph` | not built |
| The weekly sweep counts a named compaction's sidecars as known and a lost round's as orphans | `internal/api`, `TestOrphanSweepKnowsSidecars` | not built |
| A log written by the previous release, with no sidecars, materializes on this one with no `warn` beyond one per stem, and a log with sidecars still reads under `TestPreviousReleaseFixture` | `test/conformance`, `TestPreviousReleaseFixture` extended | not built |
| Against the stack: 2 000 pushes to one repository, a clone through a node that is not the primary, and `GET /v1/repos/{id}/commits` with `path` on that node, whose trace shows the `rev-list` reading changed-path filters; `TestMeasure` prints the walk with and without the graph | `test/e2e`, `TestClusterNodesInstallTheGraph` | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 001 | the key table gains `origo/repos/<id>/wal/<seq>.<nonce>.graph`, `.midx`, and `.bitmap` |
| 004 | the Objects table gains the three rows above; Materialization gains the install step; the Sweeper table gains the sidecar rule; the state file carries `sidecars` |
| 006 | the Procedure writes the graph after step 3, uploads the three objects in step 4, and commits under the drawn nonce in step 5 |
| 019 | the import writes and uploads a graph; the weekly sweep knows the sidecars of a named entry |
