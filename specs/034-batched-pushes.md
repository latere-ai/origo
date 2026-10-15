---
title: "Batched pushes: concurrent pushes to one repository share one log commit"
status: drafted
track: infra
depends_on:
  - specs/001-architecture.md
  - specs/003-protocol-contract.md
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/008-push-events.md
  - specs/011-observability.md
  - specs/012-limits-and-abuse.md
  - specs/015-degraded-storage.md
  - specs/017-release-and-versioning.md
  - specs/024-ssh-access.md
  - specs/041-randomized-simulation.md
affects: [internal/wal/, internal/repo/, internal/httpgit/, internal/sshd/, internal/events/, internal/limits/, internal/metrics/, internal/sim/, test/e2e/, docs/operations.md, docs/upgrades/, CHANGELOG.md, specs/001-architecture.md, specs/003-protocol-contract.md, specs/004-write-ahead-log.md, specs/005-placement-and-replication.md, specs/008-push-events.md, specs/011-observability.md, specs/012-limits-and-abuse.md, specs/017-release-and-versioning.md, specs/041-randomized-simulation.md, specs/README.md]
effort: large
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Batched pushes

## Overview

One repository takes on the order of ten pushes a second. Spec 004
measured 9.7 a second from 4 clients on one node against MinIO, one push
every 103 ms, and spec 005's scaling table says a busier repository
needs concurrent pushes batched into one commit. That was filed as a
request on this repository and closed as not planned until a consumer
needs it; an agent runtime whose sessions each push a checkpoint every
turn into one shared repository is the kind of consumer that would.

Two things bound the rate, not one. Every push to a repository
serializes on the create-if-absent of the next index object, whichever
node takes it. Before that, on one node, a push holds the repository's
write lock from before it reads the client's pack until git has moved
the references, so a node runs one push per repository at a time, the
client's upload included, and every clone of that repository on that
node waits behind it. Batching the commit alone would find one push to
batch.

This spec does both. A push holds no lock while its pack arrives and git
checks it; it hands its transaction to one batcher per repository on the
node; the batcher commits every push waiting as one entry, checks each
push against its own old values so a stale one is refused alone, and
answers each push's hook. One commit then carries up to
`limits.MaxBatchPushes` pushes, so the commit rate stops bounding the
push rate. The packfile format already lets one `index-pack` run index
the pushes' packs together, as spec 005's materialization does.

## Current state

Not built. Read in the tree:

| Step of a push | Where | Today |
|---|---|---|
| lock | `Handler.receivePack` (`internal/httpgit/handler.go`) calls `h.acquire(w, r, id, true)` before `spoolBody`; `Server.run` (`internal/sshd/sshd.go`) calls `Cache.Lease(ctx, id, true)` before `Handler.ReceivePack` | the repository's write lock (`Repo.lock`, a `sync.RWMutex`) from before the pack arrives to the end of the request; readers' `RLock` waits behind it |
| check and verdict | the pre-receive hook (`hook.go`) hands the transaction and quarantine over the `updates` FIFO and blocks on `verdict` | one git receive-pack, one hook, one verdict per push |
| commit | `Handler.commit` calls `Log.Commit` with `Cache.Apply` as catch-up | one entry, one index object; a lost round reads the winner's index, applies it, checks the transaction, and uploads the whole entry again under a new nonce (`Log.writeEntry`) |
| entry | `EncodeEntryHead` (`internal/wal/format.go`) | one header (one `subject`, one `push_options`), one transaction line, one pack |
| after | `forcedUpdates`, `writeVerdict`, git's own reference update, `Cache.Advance`, `events.Enqueue`, `Compactor.After` | per push |
| event | `Dispatcher.Enqueue` (`internal/events/events.go`) | key `<seq>.json`, id the UUID v5 of `<repo>:<seq>`, the cursor's `seq` the highest delivered |

`measurePushes` (`test/e2e/measure_test.go`) produced the 9.7 a second
and asserts one sequence per push. `Cache.TryEvict` skips a copy whose
write lock it cannot take.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| Forward every push to the repository's primary | one batcher per repository in the installation, no lost rounds | a node-to-node path for git's streams over two transports, which spec 005 avoids ("served wherever it lands"), one more hop on every push, and the primary's disk and CPU for every push's receive-pack |
| Narrow the lock only | one commit per push, many pushes in flight per node | the rate becomes the commit chain's, an entry write and an index create per push, with every concurrent push but one losing its round and uploading its pack again |
| A fixed window | every push waits a few milliseconds for others | adds the window at low load, where nothing waits today |
| Each pack its own object, a small entry naming them | a lost round rewrites only the entry | k + 1 writes per commit instead of one; a busy repository's cost is requests, and a lost round already applies the winner's entry |
| A batcher per repository on each node, group commit | this spec | chosen: a push is still served where it lands, an idle node commits a push at once, and a busy one commits what queued during the previous commit |

## Design

### Which node batches

Each node batches the pushes it serves; nodes still race at
create-if-absent as today. With the pushes of one repository spread over
N nodes, each node's batches hold about a 1/N share and every lost round
costs its node an apply and an upload, so the rate is best when one node
takes them. An ingress that routes by `Origo-Prefer` (spec 005) does
that already; `docs/operations.md` says so beside the scaling table.
The rate is about the batch size times the repository's commit rate, and
the batch size grows with the load until `limits.MaxBatchPushes`.

### A push without the lock

| Step | Holds |
|---|---|
| 1. `Cache.Lease` for reading, which catches up as today, then a pin on the copy, then the lease is released; over HTTP in `receivePack` and over SSH in `Server.run` alike | the read lock briefly |
| 2. the spool, the quota of spec 012 against the copy's index, git receive-pack, the hook's hand-over | the pin |
| 3. the hand-over to the repository's batcher, then a wait for the outcome | the pin |
| 4. on commit: the forced flag, `ok` to the hook, git's own update; on refusal: the hook's `reject` line | the pin; the batcher holds the write lock |
| 5. done reported to the batcher; the event enqueued | nothing |

A pin is a counter on `Repo`, taken with the copy's eviction count.
`Cache.TryEvict` skips a pinned copy as it skips a locked one.
`Cache.Evict` (a delete, a corrupt copy) takes the write lock without
waiting for pins and adds one to the count, and the batcher refuses a
push pinned under an older count, `repo_not_found` when the newest index
carries `deleted_at` and `storage_unavailable` otherwise: its quarantine
went with the copy, so git could not finish it. The sampled `git fsck`
of spec 004 counts the batcher's write locks where it counted the
pushes' write leases. Git runs
concurrent receive-packs on one repository safely: each quarantines its
own objects, and a reference update checks its old value under git's
lock file. While receive-packs wait in their hooks, a catch-up may apply
another node's entry to the copy: it adds objects and moves references,
and any push whose references it moved is refused by the batch check.

### The batcher

One per repository per node, started by the first hand-over and gone
once its queue is empty. It runs one commit at a time:

1. When idle, it takes the queue at once. A push whose request ended
   while queued leaves the queue.
2. It cuts a batch from the head of the queue in arrival order, up to
   `limits.MaxBatchPushes` pushes, `limits.MaxBatchBytes` of pack, and
   `limits.MaxRefs` reference commands. A push larger than the byte
   bound alone is a batch of one.
3. It checks each push against the running state (below), writes one
   entry with the pushes that pass, and creates the next index object.
4. On a lost round it takes the write lock, applies the winner's index
   through `Cache.Apply`, releases the lock, checks every push of the
   batch again against the new base, refuses those that now fail, and
   writes the rest under a new nonce. Pushes that arrived meanwhile wait
   for the next batch.
5. On the win it takes the write lock, gives each push its outcome,
   waits for every push of the batch to report done, then calls
   `Cache.Advance` with the batch's index when every git exited 0, and
   releases the lock. When one did not, the copy stays behind and the
   next open applies the entry, as today after a failed git update.
   The compaction trigger, `Compactor.After`, runs once with the
   batch's index object.

The commit runs under a context of the batcher's own, so one client
leaving does not end it for the others; its deadline is the earliest of
the batch's request deadlines. Under an open write breaker the batcher
waits once per batch (`BreakerWait`, spec 015) and refuses the batch
when it stays open. The `commit.before-index` failpoint stays between
the entry and the index create.

Readers of the copy on that node wait only for step 5 of each batch,
the forced flags and git's reference updates, where today they wait for
whole pushes, uploads included.

### Each push against its own old values

The running state starts at the base, the index object the batcher
holds, and takes each passing push in order:

| Case | Outcome for that push alone |
|---|---|
| the base carries `deleted_at` | refused `repo_not_found`, as spec 041 fixes for one push |
| every reference's `old` equals the running state | passes, and its updates join the running state |
| a reference's `old` differs from the running state and from the base | refused `non_fast_forward` |
| a reference's `old` equals the base but an earlier push of the batch moved it | refused `non_fast_forward`: the earlier push won the reference |
| a reference's `old` equals what an earlier push of the batch set | deferred: back to the head of the queue for the next batch, whose base holds that value; git's own update in the copy would otherwise race the earlier push's |
| spec 012's measure, with the running size in place of the index's, passes its decision's `quota_bytes`, or the running references pass `limits.MaxRefs` | refused `over_quota` |

A batch is therefore the same as committing its pushes one after
another in arrival order, each against the state the ones before it
left, with a deferred push after them. A push's own references stay
all or nothing, as `atomic` promises. When no push passes, nothing is
written.

### The entry

A batch of one writes the version 1 entry of today, byte for byte. A
batch of more writes version 2: a node writes the lowest version that
holds an object, the rule spec 032 also gives spec 017:

| Section | Version 2 |
|---|---|
| header | `{"v": 2, "kind": "push", "seq", "at", "subject": "", "pack_bytes", "pack_sha256", "pushes"}`: `pack_bytes` and `pack_sha256` cover the whole pack section, `pushes` is the count; no `push_options` |
| second line | `[{"subject", "push_options", "pack_bytes", "refs": [{"ref", "old", "new"}]}]`, one object per push in order, `push_options` present only when the client sent any, at most 64 MiB like today's transaction |
| packs | each push's pack as the client sent it, back to back in order, each a version 2 packfile of its own `pack_bytes` |

The index object is unchanged and stays version 1: its row for the entry
carries the section's `pack_bytes` and `pack_sha256` and `kind: push`,
its references are the state after every push, and `pushed_at` is the
entry's `at`. `wal.ReadEntry` reads either version as a list of pushes,
a version 1 entry as one, for materialization, the event repair, and
`stats`. Materialization verifies the section as today, splits it into
the pushes' packs, checks each is a packfile, and hands them to the
batch indexer as consecutive spooled packs, which `indexBatch` already
joins, and indexes one by one when a duplicate object makes
`index-pack` refuse the join. `limits.MaxBatchPushes` and
`limits.MaxBatchBytes` equal spec 005's materialization batch, so one
entry is at most one indexing run.

A release that reads version 1 alone refuses a repository whose newest
index lists a version 2 entry, through `NewerFormatError` and
`repository_unavailable`, and serves every other. A compaction lists no
entry after it, so such a repository reads again on that release after
its next compaction. `docs/upgrades/2.md` says so, written by this spec
or extended when spec 032 wrote it first.

### Hooks, report-status, events

Each push keeps its own receive-pack, hook, and verdict, so git writes
each client's report-status as today: `ok` for every reference, or
`pre-receive hook declined` with the hook's line, `<code>: <sentence>`,
in the sideband. A refused push of a batch reads exactly what a refused
push reads today.

Every push of a committed batch gets its own event, enqueued by its own
request after its git exits:

| Field or key | A push at position i of an entry |
|---|---|
| event object | `origo/events/<repo>/<seq>.json` at position 0; `<seq>.p<iii>.json`, three digits, for 1 and later, so key order is position order |
| `id` | the UUID v5 of `<repo>:<seq>` at position 0, as today; of `<repo>:<seq>:<i>` for 1 and later |
| `position` | i, present on every event of an entry of more than one push |
| `seq`, `at`, `pusher`, `updates`, `operation` | the entry's sequence and `at`, the push's own subject, transaction, and options |
| cursor | gains `position`, the highest delivered within `seq`; absent reads as every position of `seq`, which is what an earlier release wrote |
| repair | rebuilds every push above the cursor's `(seq, position)` that has no object, from `wal.ReadEntry` |

A push with `origo.event=off` has no event, and its position is skipped.

### Metrics and logs

`origo_pushes_total` counts pushes and `origo_wal_commits_total` counts
batches, so their ratio is the mean batch size. Each push observes its
batch's `entry` and `index` phases of `origo_push_duration_seconds`. Two
histograms are new: the pushes of each committed batch, and the time a
push waits from its hand-over to the start of its batch's commit. The
`push` log line gains `batch` and `position`.

### Limits

| Constant | Value | Why |
|---|---|---|
| `limits.MaxBatchPushes` | 256 | spec 005's entries per indexing run |
| `limits.MaxBatchBytes` | 256 MiB | spec 005's bytes per indexing run, and the most one lost round uploads again |
| `limits.MaxRefs` | as today | the commands of one batch, which keeps the second line inside the parser's 64 MiB |

### Latency and the target

There is no window. An idle batcher commits a push at once, as today
less the lock. Under load a push waits for the commit in flight and its
step 5, then its own commit: at most one commit and one step 5 more than
in an idle repository, against today's wait for every earlier push on
the node.

| Run | Load | Target |
|---|---|---|
| `TestMeasure` with `ORIGO_E2E_MEASURE=1`, one node, MinIO, 1 KiB commits | 4, 16, 64 clients, each on its own branch, 60 s each, batched and with batches of one | 100 pushes a second at 64 clients; the 4 client p99 latency within 1.2 times the unbatched run's; printed and recorded in the Outcome |
| the `e2e` job's stack | 32 clients through one node | at least 4 times the pushes per second of batches of one in the same run, every push landed |

`measurePushes` asserts the newest index's references equal each
client's last push and the pushes over the entries sum to the pushes
acknowledged, in place of one sequence per push.

### In the simulation

Spec 041 gains `push.burst`, 2 to 16 pushes to one repository on one
node in one step, and the batcher as an actor that reports when it waits
for work, through `httpgit.Options.Idle`, nil in a deployment. Its
invariant: every push's verdict and the final references equal
committing the burst's pushes one at a time in their order in the entry.
This spec moves to `testing` only with the gate's fixed seeds and one
weekly run green with `push.burst` on.

### Names the build adds

| Name | Kind | Meaning |
|---|---|---|
| origo_push_batch_size | metric, histogram | pushes per committed batch, buckets 1, 2, 4, 8, 16, 32, 64, 128, 256 |
| origo_push_queue_seconds | metric, histogram | from a push's hand-over to the start of its batch's commit |
| `limits.MaxBatchPushes`, `limits.MaxBatchBytes` | constants | above |
| `wal.Log.CommitBatch` | Go method | the commit of a list of pushes, with one outcome per push |
| `wal.ReadEntry` | Go function | an entry of either version as a list of pushes |
| `httpgit.Options.Idle` | Go field | called when a batcher waits for work, for spec 041 |
| `position`, `pushes` | event and header fields | above |

The build moves the two metrics into spec 011's table and the constants
into spec 012's.

## Not in this spec

Forwarding pushes between nodes. A second commit of one repository in
flight on one node while the first one's references are applied.
Batching the server-side operations of spec 020 and the `PATCH` of a
default branch, which commit through `Log.Commit` as today. Reading a
pack once per lost round instead of uploading it again.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Eight pushes handed over during one commit become one version 2 entry and one index object whose references equal the eight applied in arrival order, and one push alone writes the version 1 entry of today byte for byte | `internal/wal`, `TestCommitBatchWritesOneEntry` | not built |
| In one batch a push whose `old` is stale is refused `non_fast_forward`, one past the quota `over_quota`, one chained on an earlier push's `new` is deferred to the next batch and lands there, and every other push lands | `internal/wal`, `TestEachPushIsCheckedAlone` | not built |
| A batch that loses two rounds to another node re-checks every push, refuses the ones the winner moved, and lands the rest under a third nonce | `internal/wal`, `TestBatchLosingRounds` | not built |
| A version 2 entry materializes onto an empty disk, including one whose pushes carry one blob twice, and `git fsck` passes; a release reading version 1 alone refuses the repository with `repository_unavailable` and reads it again after a compaction | `internal/repo`, `TestMaterializeABatchedEntry`; `internal/repo`, `TestNewerLogFormatIsRefused` extended | not built |
| Sixteen concurrent `git push` clients over HTTP and four over SSH to one repository on one node all get `ok`, the copy's references equal the index, and a clone running meanwhile is not blocked for longer than one batch's step 5 | `internal/httpgit`, `TestConcurrentPushesShareCommits`; `internal/sshd`, `TestSSHPushesBatch` | not built |
| A refused push of a batch reads the code's line in its sideband, and the others of its batch read `ok` | `internal/httpgit`, `TestRefusedPushInABatch` | not built |
| A batch of three yields three events with keys `.json`, `.p001.json`, `.p002.json`, ids from position, `position` 0 to 2, and the repair sweep rebuilds a missing middle one with the same id | `internal/events`, `TestEventsOfABatch` | not built |
| An evictor run during a push skips the pinned copy, and a delete served by the same node or another during a push leaves the push refused `repo_not_found` with no index object | `internal/repo`, `TestPinnedCopyIsNotEvicted`; `internal/httpgit`, `TestPushDuringADelete` | not built |
| Spec 041's fixed seeds pass with `push.burst` on | `internal/sim`, `TestSimulation` | not built |
| On the stack, 32 clients through one node push at least 4 times the rate of batches of one, every push lands, and the newest index's references match every client's last push | `test/e2e`, `TestClusterBatchedPushRate`, `e2e` job | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 001 | the write path: several pushes may share one entry and one index object |
| 003 | a push event may share its `seq` with others of one entry and then carries `position`; a consumer orders by `seq`, then `position` |
| 004 | the entry's version 2 and `wal.ReadEntry`; `Log.CommitBatch` in the commit table; materialization splits a version 2 pack section; the sizes table's single push row says batch |
| 005 | the scaling row and Not in this spec: batching is built, per node, best behind an ingress routing by `Origo-Prefer` |
| 008 | the keys, ids, `position`, the cursor's `position`, and repair per push |
| 011 | the two metrics; the `push` log line's `batch` and `position` |
| 012 | the two constants; quota and reference cap per push within a batch |
| 017 | a node writes the lowest version that can hold an object, unless spec 032 already said so; `docs/upgrades/2.md` names version 2 entries |
| 041 | `push.burst`, the batcher actor, and its invariant |
