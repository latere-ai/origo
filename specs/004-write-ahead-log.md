---
title: "Write-ahead log: entries, immutable index, create-if-absent commit, materialization"
status: testing
track: infra
depends_on:
  - specs/002-repository-scaffold.md
affects: [internal/wal/, internal/repo/, internal/httpgit/]
effort: large
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Write-ahead log

## Overview

The log is the repository. Every push and every compaction is one entry
in object storage, and the index is a sequence of immutable objects, one
per committed entry, each holding the full reference map and what a
reader needs to serve the repository at that point. A node applies
entries into a local bare repository to serve git, and that local copy is
disposable. This spec fixes the entry format, the index objects, the
create-if-absent commit that linearizes writes, and how a node
materializes and catches up.

## Current state

Spec 001 fixes the model. The spike in
[docs/spikes/2026-09-06-conditional-writes.md](../docs/spikes/2026-09-06-conditional-writes.md)
ran the probe under `tools/spike/condwrite/` against MinIO and
DigitalOcean Spaces. MinIO honours every conditional primitive. Spaces
honours `PUT If-None-Match: *` (200 on an absent key, 412 on an existing
one, content untouched) and the conditional `GET` (304), and refuses `PUT
If-Match` with 412 on every one of 200 samples: 0 of 320 racing writers
got a 200. `CopyObject` conditions are ignored on both, and versioning
orders writes without refusing the loser. Decision: the index is not a
mutable object updated by `If-Match`. It is a sequence of immutable
objects `index/<seq>`, and a writer commits by creating the next one with
`If-None-Match: *`, which MinIO, Spaces, and AWS S3 all honour. The
probe's create race (20 rounds x 16 writers, exactly one 200 per round)
and the `HEAD` checks (404 on an absent key, 200 with the agreeing ETag on
a present one) were verified on MinIO and on Spaces with the current
build. The first draft's compare-and-swap on an ETag is kept in the
decision record and used nowhere.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| One mutable index, `PUT If-Match: <etag>` | compare-and-swap on the ETag | Spaces answers 412 to every `If-Match` `PUT`; the primitive is not portable |
| `CopyObject` with a destination condition | write the candidate to a scratch key, copy it over the index under `If-Match` | MinIO and Spaces accept the header and ignore it: the copy overwrites |
| Versioned bucket, order by version | unconditional `PUT`, `ListObjectVersions` names the first | refuses no write; a loser learns it lost after its push was acknowledged |
| Immutable index objects, `PUT If-None-Match: *` on the next one | chosen: one create per commit, exactly one creator succeeds | honoured on every store the spike ran; one primitive, no ETag bookkeeping |

## Design

### Objects

Under `origo/repos/<id>/`:

| Key | Content |
|---|---|
| `wal/<seq>.<nonce>.entry` | one push, compaction, or delete: header, reference transaction, pack; immutable |
| `index/<seq>` | the state after entry `seq` is applied; immutable; created once by `If-None-Match: *` |
| `index/latest` | `{"seq": n}`, a hint written unconditionally after each commit; may lag, never leads correctness |
| `packs/<hash>.pack`, `.idx` | packs produced by compaction, named by the index objects that follow it |

`seq` is a zero-padded 12 digit decimal. Zero padding makes the keys sort
in sequence order under a listing.

### Entry

A writer that holds `index/<n>` writes its entry as `wal/<n+1>.<nonce>.entry`,
`nonce` 16 hex characters chosen at random by the writer. Two writers
that both hold `index/<n>` write two different keys, so a loser never
overwrites the winner's entry; the index object names the key it
committed. Format, in order:

| Section | Content |
|---|---|
| header | JSON, one line: `{"v": 1, "kind": "push"\|"compact"\|"delete", "seq", "at", "subject", "actor", "pack_bytes", "pack_sha256"}` |
| refs | JSON, one line: the reference transaction `[{"ref", "old", "new"}]`, `old` `0000…` for a create, `new` `0000…` for a delete |
| pack | the packfile bytes exactly as received from the client (`push`) or produced by repack (`compact`); absent for `delete` |

A push whose packfile exceeds 2 GiB is written as several entries with a
`part` field and committed by the last; the index object references the
group.

### Index object

`index/<n>` is the complete state after entry `n`:

```json
{"v": 1, "seq": 1043, "entry": "wal/000000001043.9f3c1a7be2d40c55.entry",
 "refs": {"refs/heads/main": "<sha>", "HEAD": "ref: refs/heads/main"},
 "entries": [{"seq": 1040, "key": "wal/000000001040.….entry", "kind": "push", "pack_sha256": "…"}, …],
 "packs": ["packs/<hash>.pack"], "compacted_through": 1039,
 "size_bytes": 123456789, "deleted_at": null}
```

`entries` lists every entry after `compacted_through` up to and
including `seq`; earlier history is represented by `packs`. The object is
small by construction: compaction (spec 006) folds entries into packs and
truncates the list. Each index object carries the whole reference map, so
a reader needs exactly one of them.

### Commit by create-if-absent

```mermaid
flowchart TD
  A[receive-pack: pack + ref updates, node holds index/n] --> B[PUT wal/n+1.nonce.entry]
  B --> C[PUT index/n+1 If-None-Match: *]
  C -->|200| D[PUT index/latest = n+1, unconditional]
  D --> E[apply refs locally, ack, gossip n+1]
  C -->|412| F[GET index/n+1, apply its entry locally, n = n+1]
  F --> G{ref updates still fast-forward?}
  G -->|yes| B
  G -->|no| H[reject non_fast_forward, entry becomes garbage]
```

Linearization comes from create-if-absent alone: `index/<n+1>` is
created at most once, so exactly one writer holding `index/<n>` commits
entry `n+1`, and every other writer at `n` sees 412 and moves to `n+1`.
There is no ETag to track and no lock. Formally, for every `n` the
sequence of index objects is a total order because

$$\text{created}(index/n{+}1) \Rightarrow \neg\,\text{created by another writer}(index/n{+}1),$$

and every writer's precondition for creating `index/<n+1>` is holding
`index/<n>`, so the chain has no gaps.

A `PUT` whose response is lost in transport may have been applied. The
writer then reads `index/<n+1>`: if `entry` names its own key, it won and
continues at `D`; otherwise it lost and continues at `F`. An orphaned
entry (written, never named by an index object) is harmless: no node
applies it, and the sweeper of spec 006 deletes entries with `seq` at or
below the newest index that no index object names, and entries above the
newest index, once they are older than one hour.

`index/latest` may go backwards when two committers write it out of
order. It is a hint for cold starts and gossip-less nodes; a reader walks
forward from it.

### Currency check

A node that holds sequence `n` asks `HEAD index/<n+1>`:

- 404: the local copy is current. One round trip, no body.
- 200: a newer index exists. `GET index/<n+1>`, apply its entry, set
  `n = n+1`, and ask again.

The loop ends at the first 404. Gossip (spec 005) carries the newest
sequence between nodes so the common case is one `HEAD` that answers 404;
correctness never depends on gossip arriving. A node with no local
sequence reads `index/latest` for a starting point and walks forward; if
`index/<hint>` is missing (swept, or the hint lags), it lists `index/`
and takes the highest key.

### Materialization

A node that lacks `<id>` locally, or holds an older sequence, does:

1. Find the newest sequence with the currency check and `GET` that index
   object.
2. If the local sequence is below `compacted_through`, or there is no
   local copy, `GET` each pack in `packs` not present locally into
   `objects/pack/` with its `.idx` (packs are stored with their index).
3. For each listed entry above the local sequence, `GET` the entry by its
   key, verify `pack_sha256`, run `git index-pack --fix-thin` into
   `objects/pack/`, apply the reference transaction with
   `git update-ref --stdin`.
4. Record the sequence in `origo.json` beside the repository.

Materialization is idempotent and resumable. A corrupt local repository
(failed `git fsck --connectivity-only` on a sampled check, or any git
error that names corruption) is deleted and rebuilt from the log; the log
is never repaired from a local copy.

### Serving git from the local copy

`upload-pack` runs against the local repository after the currency
check. `receive-pack` runs with a pre-receive hook that captures the pack
and the reference transaction instead of applying them, so the entry is
written and committed before git's own update happens; the post-commit
apply is `git update-ref --stdin` with the same transaction. Git
subprocesses run with `GIT_DIR` set, `core.protectNTFS` and
`receive.fsckObjects` on, and a per-request timeout.

### Compaction and sweeping

Compaction (spec 006) commits the same way as a push: the primary writes
the new packs, writes a `compact` entry, and creates `index/<n+1>` whose
`packs` names the new packs, whose `compacted_through` is `n`, and whose
`entries` holds only the compaction entry. Readers at or above `n+1`
never touch the folded entries again.

The sweeper deletes index objects with `seq` below `compacted_through`
of the newest index once they are older than one hour, and always keeps
the newest 64 index objects. Folded entries are deleted on the same
schedule. A reader that holds a swept sequence is below
`compacted_through` and rebuilds from packs, which is step 2 above.

### Deletion

`DELETE /v1/repos/{id}` commits a `delete` entry whose index object sets
`deleted_at`. Nodes evict the local copy at once and answer 404. The
sweeper deletes every object under the prefix after the 7 day hold
unless a later index object cleared `deleted_at` (`undelete`).

### Durability and sizes

| Value | Limit |
|---|---|
| single push | 2 GiB per entry part, 10 GiB total |
| repository | 50 GiB by default, per consumer authorizer response |
| index object | 1 MiB; compaction runs before it can grow past this |
| entry write | acknowledged after object storage returns 200 with a matching ETag; `Content-MD5` set |
| commit | acknowledged after the create of `index/<n+1>` returns 200 |

## Acceptance criteria

- A push produces one entry and one index object; killing the node
  between the two leaves no visible change and the sweeper removes the
  orphan.
- 100 concurrent pushes to distinct branches from 8 clients all land,
  the newest index object lists 100 entries in sequence order, every ref
  is correct, and no two index objects share a sequence.
- 16 writers holding the same `index/<n>` race to create `index/<n+1>`:
  exactly one gets 200, the rest get 412, and the stored object is the
  winner's, over 20 rounds. The probe under `tools/spike/condwrite/`
  measures this.
- The design runs unchanged on MinIO and DigitalOcean Spaces: the
  probe's create race, `HEAD` 404, and `GET` 304 rows pass on both, and
  the conformance suite (spec 013) passes against both.
- A node holding `index/<n>` whose `HEAD index/<n+1>` answers 404 serves
  references equal to `index/<n>`; after another node commits `n+1`, the
  next `HEAD` answers 200 and the node serves `index/<n+1>`.
- A node with an empty disk materializes a repository of 10 000 entries
  and 3 packs, and `git fsck` on the result passes.
- A local repository with a deliberately corrupted pack is rebuilt from
  the log and serves the same history.
- Fuzz tests on the entry header, the reference transaction, and the
  index parser find no panic in 10 minutes.

## Outcome

Phase 1 shipped the log on 2026-09-06 in `internal/wal` (entries, index
objects, the create-if-absent commit, the currency check, metadata, the
sweeper, the S3 store) and `internal/repo` (materialization into a bare
repository under `ORIGO_DATA_DIR`, catch-up, reconciliation of the
reference map to the index, corruption rebuild). The receive path of
`internal/httpgit` captures the pack and the transaction into an entry
and commits the index object before git's own update.

Acceptance:

- A push produces one entry and one index object; the end-to-end suite
  kills the node between the two at the `commit.before-index` failpoint,
  the client sees a failure, nothing is visible, the sweeper removes the
  orphan, and a retry lands. Verified on MinIO.
- 16 writers over 20 rounds: exactly one index object per sequence, every
  writer's every push lands, verified on the in-process store
  (`TestSixteenWritersTwentyRoundsOneWinnerPerSequence`) and the create
  race itself on MinIO (`TestS3Suite`). Two nodes pushing different
  branches at once land both (`TestConcurrentPushesToDifferentBranchesOnTwoNodes`).
- The currency check: a holder of `index/<n>` whose `HEAD index/<n+1>`
  answers 404 serves `index/<n>`; after another node commits `n+1` the
  next check answers 200 and the node serves `index/<n+1>` (repo cache
  tests and the httpgit two-node tests).
- A deliberately corrupted pack is detected by the sampled connectivity
  check or by the failing apply, the copy is evicted, and the next open
  rebuilds it from the log (`TestCorruptCopyIsRebuiltFromTheLog`).
- Fuzz tests on the entry header, the reference transaction, the index
  parser, and pkt-line framing: 40 seconds each without a finding on the
  three parsers; the 10 minute run is a CI budget question left open.
- 100 concurrent pushes from 8 clients, 10 000 entries with 3 packs, and
  the Spaces conformance run are not yet exercised: the first two wait
  for the conformance suite of spec 013 and compaction of spec 006, the
  third for a Spaces run with credentials. The design is the one the
  spike verified on both providers; the S3 client sends `If-None-Match`
  only, asserted by the fake endpoint in the unit tests.

Measurements, one node on this machine (Apple silicon laptop) against
MinIO in a podman virtual machine, so a floor for the client path rather
than a production figure:

| Measurement | Value |
|---|---|
| Pushes sustained for 60 s, 4 clients, 1 KiB commits, one node | 9.7 pushes/s (582 in 60 s) |
| Push latency seen by the client | p50 335 ms, p99 849 ms, including the git client process and the pack upload |
| `HEAD` currency check, 404 (current) | p50 0.36 ms, p99 2.28 ms (500 samples) |
| `HEAD` currency check, 200 (a newer index exists) | p50 0.42 ms, p99 3.93 ms (500 samples) |
| Materialize 1000 entries onto an empty disk (first `ls-remote`) | 70.7 s, one `GET`, one `index-pack`, and one `update-ref` per entry |
| Full clone after that | 0.69 s |
| Writing the 1000 entries through the log | 98 s, dominated by `pack-objects` in the test client |

Materialization is bound by two git subprocesses per entry, about 35 ms
each here. The reference map is reconciled once at the end, so the
per-entry `update-ref` can be dropped from a bulk apply, and entries
could be fetched ahead of `index-pack`; both are phase 2 work, and
compaction (spec 006) is what removes the entry count from the path.

Divergences and details this spec did not fix:

- `index/000000000000` is created with the repository: it holds the
  reference map with `HEAD` on the default branch and names no entry, so
  a writer always holds an index object and the chain has no special
  first case.
- `HEAD` is a symbolic reference in the map (`ref: refs/heads/main`) and
  a transaction on `HEAD` carries symbolic values; `PATCH
  /v1/repos/{id}` moves the default branch that way.
- The header carries `push_options` when the client sent any, for spec
  008 to read.
- Repository metadata lives in `meta` beside the log and the
  owner/slug name in `origo/names/<owner>/<slug>`, both created by
  create-if-absent (spec 003, Outcome).
- Materialization reconciles the whole reference map to the index after
  applying entries, so a copy converges whatever it held; reference
  values are set, not compared.
- The multi-part entry for pushes above 2 GiB (`part`) is not
  implemented: a push is one entry, and the size limits of the table are
  not enforced yet (spec 012).
- A push whose commit is refused by the store leaves its entry as an
  orphan; the sweeper deletes it after `ORIGO_SWEEP_MIN_AGE` (one hour
  by default). Lost rounds of the commit are replayed after a jittered
  pause of at most 16 ms, bounded at 4096 rounds, which no live
  repository reaches.
- Without compaction (spec 006) the `entries` list of an index object
  grows by one row per push; the 1 MiB ceiling holds for roughly ten
  thousand pushes.
- The AWS SDK was not added: the S3 client is signed by the standard
  library and checked against the vector the S3 documentation publishes.
- The sampled connectivity check runs on every 256th write open of a
  repository.
