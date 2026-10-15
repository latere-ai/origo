---
title: "Randomized simulation: seeded runs of several nodes against a faulty bucket, with the log's invariants checked after every step"
status: drafted
track: infra
depends_on:
  - specs/001-architecture.md
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/006-compaction.md
  - specs/013-test-stubs-and-kind-overlay.md
  - specs/015-degraded-storage.md
  - specs/019-repository-administration.md
affects: [internal/sim/, internal/wal/, internal/compact/, Makefile, .github/workflows/verify.yml, docs/internals/testing.md, specs/013-test-stubs-and-kind-overlay.md, specs/README.md]
effort: large
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Randomized simulation

## Overview

The log is correct because of orderings between writers that never talk
to each other: two commits racing for one `index/<n+1>`, a sweep's
listing against a reader's apply, a purge against an undelete, an
eviction against a request. The tree tests each ordering its authors
thought of with a scripted interleaving:
`TestSixteenWritersTwentyRoundsOneWinnerPerSequence` (`internal/wal`),
`TestStalePushIsRefusedAndConcurrentBranchesLand` (`internal/httpgit`),
a `MemStore.Fault` function per test, and two one-shot failpoints. The
eleven fuzz targets cover parsers and validators. Nothing runs many
operations on several nodes against a bucket that fails in the ways spec
015 names, and nothing checks spec 001's invariants after each of them.

Writing this spec found a race that no test covered. A push in flight
while another node deleted the repository committed on top of the
delete, and the repository was live again (Current state). It is fixed
in the tree, with a scripted test written after the fact; a simulation
would have found it without anyone thinking of the ordering. Spec 032 (forks) and
spec 034 (batched pushes) add races of the same kind: a pending pin
against a sweep, and several pushes in one commit, each refused on its
own, across lost rounds.

This spec adds a seeded simulation. Several nodes run in one process,
each the real log, cache, compaction manager, placement, gossip, smart
HTTP handler, and API handler, over one in-memory bucket that injects
faults. Every store call is a scheduling point the seed decides, time is
a fake clock, the invariants are checked after every step, a failing
seed replays exactly and shrinks to a short trace, and a short run is in
the gate while a long one runs every week.

## Current state

Not built. What exists, read in the tree:

| Piece | Where | What it gives |
|---|---|---|
| `wal.MemStore` | `internal/wal/memstore.go` | create-if-absent, `Fault func(op, key string) error` before every call, `ErrLostResponse` (applied, then a transport error), `SetLatency` (real time), `SetClock`, `Touch`, `Keys` |
| `wal.BreakerStore` | `internal/wal/breaker.go` | per-node breakers over a store, `BreakerOptions{Store, Timeout, Clock, Threshold, OpenFor}`; `Wait` polls with `wait.Sleep` in real time |
| failpoints | `wal.FailpointBeforeIndex`, `events.FailpointBeforeEnqueue` | `ORIGO_FAILPOINT` names one, and the node exits there (`node.failpoint`, `cmd/origod/node.go`); a kill point for the cluster tier, not a fault model |
| fuzz | `FuzzParseHeader`, `FuzzParseTransaction`, `FuzzParseIndex`, `FuzzValidRefName`, `FuzzValidLabel`, `FuzzReadPkt`, `FuzzParseReceive`, `FuzzParseToken`, `FuzzSSHPath`, `FuzzOperationBody`, `FuzzValidPath` | parsers and validators, 40 seconds each under `make fuzz` in the weekly `fuzz` job |
| placement | `placement.NewSet`, `Set.Heard`, `Set.Prefer`; `Gossip.Handle` and `Seal` | membership and catch-up without a socket |
| compaction | `compact.New`, `Manager.GC` | a run takes the manager's own context, `context.Background()` (`compact.go`) |
| sweeper and purge | `wal.Log.Sweep(ctx, repo, minAge)` | purge inside it once `deleted_at` is older than `wal.DeleteHold` |
| several nodes over one store | `newNode` in `internal/httpgit/handler_test.go`, `newHarnessOn` in `internal/compact/compact_test.go` | the shape to follow |

No package imports `math/rand`, and the suite's only test flags are
`-fixture` and `-update`. The gate runs `go vet ./...` and `go test
-race` over `./...` with Go's default 10 minute timeout per package and
no `-short`.

The race, fixed since. `wal.Log.Commit` replayed a lost round on the
stored index once the caller's catch-up had applied it, and checked only
the references the transaction named; `nextIndex` clears `deleted_at` on
every push commit.
Three push paths pass a catch-up that accepts a deleted index: the
receive path (`Cache.Apply`, `internal/httpgit/handler.go`), the
server-side operations (`internal/api/operations_git.go`), and the
default branch change of `PATCH /v1/repos/{id}` (`noCatchUp`,
`internal/api/api.go`). A push whose round is lost to a delete entry
therefore landed on top of it: the deleter had 202 and a `deleted`
event, no `undeleted` event followed, and the pusher needed `repo.write`,
not the `repo.undelete` spec 028 keeps apart. A probe against the log
confirmed it: the push landed at sequence 3 over the delete at 2,
`deleted_at` null. `Log.Commit` now stops such a round with
`wal.ErrDeleted` (`TestAPushThatLostItsRoundToADeleteIsRefused` in
`internal/wal`), and the three callers answer `repo_not_found`.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| More scripted race tests | one test per interleaving | covers the orderings an author thinks of; the race above passed every test in the tree until one was written for it |
| A model checker over a protocol model | the log as a formal model, checked exhaustively | checks the model, not the code; the race is in how three callers wire their catch-up, which a protocol model states correctly |
| `testing/synctest` | Go's bubble for fake time and `synctest.Wait` for quiescence | a bubble idles only when every goroutine is durably blocked. A push parked at a store call holds git in its hook, reading the verdict FIFO, and a goroutine in `cmd.Wait`: both in system calls, which are not durable, so the bubble never idles. `sync.RWMutex` waits are not durable either (the package documentation, Go 1.27) |
| Random operations against the cluster tier | the kind stack with MinIO | real time, network, and scheduler: a failure comes with no seed and does not replay |
| In-process nodes, store calls as the only scheduling points, one fake clock | this spec | chosen: the code under test runs as built, the seed decides every interleaving and fault, and a trace replays call for call |

## Design

### Where it lives

`internal/sim`, test files only, so nothing ships in a binary and the
gate's `go test ./...` runs its short form. It imports exported API
alone. The files are the cluster, the bucket, the scheduler, the
operations, the model and its checks, and the trace and its shrinker.

### Nodes in one process

A seed picks 2 to 5 nodes and 1 to 4 repositories. Each node is built
from the same constructors `cmd/origod` uses:

| Part | Built with |
|---|---|
| store | `wal.NewBreakerStore` over the simulation's bucket, `Clock` the fake clock |
| log | `wal.New` with `Now`, `Sleep` the simulation's sleep, `Rand` a seeded reader, `OnCommit` recording the announcement |
| cache | `repo.New` on the node's own directory, `Now`, `Workers: 1`, `FsckEvery` from the seed (1 to 256) |
| placement | `placement.NewSet(name, now)`, fed the live set the simulation chooses |
| gossip | `placement.NewGossip` with `placement.CacheHolder`, no socket; datagrams go to `Handle` |
| compaction | `compact.New` with the node's set, `Now`, `GCWait` an hour, and `Context` the node's compaction actor |
| smart HTTP | `httpgit.New` with no events and no compaction trigger, `Now`, `BreakerPoll` |
| API | `internal/api`'s handler as its tests build it, events off |
| guard | `auth.NewGuard` over an in-process `auth.Authorizer` that allows every action |

Requests reach the handlers through `ServeHTTP` on an
`httptest.ResponseRecorder`, the principal set as `newNode` sets it; no
socket is opened. A push is the two requests a git client sends: the
receive-pack advertisement, then a `git-receive-pack` body the harness
builds (the commands, `report-status side-band-64k`, a flush, the pack),
with a pack a `gittest.Source` per repository writes thin against the
advertised references. The whole receive path runs as built, from the
spool through git's own reference update and `Cache.Advance`, and the
harness reads report-status out of the sideband.

Every real-time deadline the driven code sets is raised to an hour
(`BreakerOptions.Timeout`, `httpgit.Options.Timeout`,
`repo.Options.GitTimeout`), so time spent parked at the scheduler never
fires one; a storage deadline is a fault instead.

### The bucket

One `wal.MemStore` behind a store of the simulation's own that
implements `wal.Store`. Every call passes the scheduler's gate, which
reads the calling actor from the context, then the fault table:

| Fault | Calls | Effect | Rate, flaky profile |
|---|---|---|---|
| error | every call | a transport error, nothing applied | 2% |
| lost | `Create`, `Put`, `Delete` | applied, then a transport error | 1% |
| conflict | `Create` | applied, then `ErrExists`: a retried create whose first attempt landed | 0.5% |
| timeout | every call | the actor sleeps the node's storage timeout of fake time and the call fails with `context.DeadlineExceeded`; half of the writes are applied first | 0.5% |
| slow | every call | the actor sleeps 10 ms to 5 s of fake time first | 5% |
| partial | `Put`, `Create` | the body is read to a random offset, nothing is stored, a transport error; the next attempt must reopen the body | 1% |
| listing delay | `List` | keys created in the last 0 to 30 s of fake time are left out | 10% |
| stale listing | `List` | keys deleted in the last 0 to 30 s of fake time are still listed | 2% |

A seed runs one profile: calm (no fault), flaky (the rates above), or
storm (five times them), weighted 1, 2, 1. Each decision comes from a
generator keyed by the seed, the actor, and the actor's call number, so
dropping an operation while shrinking moves no other decision. In a
deployment `latere.ai/x/pkg/s3` retries beneath the breaker; here no
layer retries, which is harsher than production on purpose.

Conflicts between writers come from the schedule. With 30% a step starts
two pushes to one repository on two nodes, and with 20% it pairs a push
with a delete, a compaction with a sweep, or an eviction with a read, on
one repository and different nodes.

### Time

One fake clock per run, from 2026-01-01T00:00:00Z, is every component's
`Now` and the bucket's `SetClock`. It moves only when the scheduler
moves it. Three waits go through the simulation's sleep, which parks
the actor until the clock reaches its wake time: the log's `Sleep`
between lost rounds, `BreakerStore.Wait` between polls (`Sleep`, a new
option), and the slow and timeout faults. The loops that run on real
timers in a node are not started: gossip's `Run`, the evictor's `Run`,
compaction's `Run`; their work is operations below.

The `clock` operation runs alone in its step. Inside a step the longest
chain of waits is the breaker's `BreakerWait` plus storage timeouts, so
the harness fails a step that spans more than 5 minutes of fake time as
an error of its own: that is the request deadline the sweeper's age
rule relies on (spec 004), and `minAge` stays at the default of
`ORIGO_SWEEP_MIN_AGE`.

### Scheduling

An actor is an operation of the step, or a goroutine with a context of
its own: a compaction run, and the batcher of spec 034. The context
carries the actor. Per step the scheduler:

1. starts each operation on its own goroutine;
2. waits until every actor is parked at a store call, sleeping, waiting
   on another actor, or done, as each reports through the gate, the
   sleep, or the operation's own wait;
3. when an actor is parked, picks one with the scheduling generator,
   over the parked actors in actor order, releases its call, and goes
   back to 2;
4. when none is parked and some sleep, moves the clock to the earliest
   wake time, wakes those, and goes back to 2;
5. when all are done, runs the checks.

Between two scheduling points an actor runs alone, so its git
subprocesses never overlap another actor's work. Two rules keep every
in-process wait visible: a step never holds two operations that take
one node's lock on one repository (`Cache.Lease`), and `Apply` runs one
worker, so an actor calls the store from one goroutine. A step fails as
a harness error, naming the actor, when one actor has two calls parked
at once or runs for more than 60 seconds of real time.

### Operations

| Operation | Weight | What it does | Driven through |
|---|---|---|---|
| push | 30 | 1 to 3 updates on 4 branches, built on the newest acknowledged value (70%), an older one (20%), or a new branch (10%); 5% of updates delete a branch | `POST /{repo}/git-receive-pack` |
| read | 20 | the upload-pack advertisement; its references are recorded | `GET /{repo}/info/refs` |
| list | 5 | the reference listing | `GET /v1/repos/{id}/refs` |
| announce | 8 | delivers one node's newest commit of a repository to another, or drops it | `Gossip.Handle` with a sealed datagram, then `Gossip.Wait` |
| compact | 6 | on any node, primary or not | `Manager.GC` |
| sweep | 6 | one node sweeps one repository | `Log.Sweep` |
| evict | 5 | one node drops its copy unless it is in use | `Cache.TryEvict` |
| restart | 2 | a node starts again on its own directory | a new `repo.Cache` over it |
| wipe | 2 | a node starts again on an empty directory | a new `repo.Cache`, the directory removed |
| delete | 2 | on any node | `DELETE /v1/repos/{id}` |
| undelete | 1 | on any node, inside the hold | `POST /v1/repos/{id}/undelete` |
| create | 1 | a repository, at most 4 | `POST /v1/repos` |
| membership | 2 | a node leaves or rejoins the live set, which moves the compaction primary | `Set.Heard` and the clock |
| clock | 4 | 1 second to 2 hours; 5% of them past `wal.DeleteHold` | the fake clock |

A step starts 1 to 4 operations. The end of a seed drains: the clock
moves past `ORIGO_SWEEP_MIN_AGE` and `wal.DeleteHold`, every node sweeps
every repository twice, and every node reads every repository.

### Invariants

The model is the store's own record of every index object created and
every response each operation got. After every step:

| Invariant | From | Checked as |
|---|---|---|
| an acknowledged push is in the log | spec 001, 1 | a push whose report-status says `ok` for every reference has an index object, created before its response, whose entry carries its transaction and subject; one refused with a code (`non_fast_forward`, `over_quota`, `repo_not_found`) has none; one answered `storage_unavailable` may have either |
| one index object per sequence, no gap | spec 001, 2 | for a repository not purged, the index objects are 0 to n; each `Create` of an index key succeeded once; index k is index k-1 with entry k applied, its entries, packs, and `size_bytes` by spec 004's rules |
| a node's disk holds nothing the log cannot rebuild | spec 001, 3 | every 25 steps and at the end, a cache on an empty directory materializes each repository: its references equal the newest index object's and `git fsck` passes; every node's copy, once a read brings it current, has the same references and `git rev-list --objects --all` |
| a read is never older than an acknowledged write | spec 001, 4 | a read's references are those of an index object at or after the newest one any write acknowledged before the read began produced; a read answered with `Origo-Stale` is exempt by spec 015's design and was checked within `ORIGO_STALE_MAX` |
| compaction moves no reference and no object | spec 001, 5 | a compact entry's index object has its predecessor's references, and a cold copy at it lists the objects the compacting copy held |
| the log names nothing missing | specs 004, 006, 015 | no answer is `repository_unavailable`, `origo_log_integrity_errors_total` stays 0 on every node, no index object of a repository whose `meta` carries no `purged_at` is deleted, and no sweep deletes what the newest index lists |
| a deletion holds until an undelete | specs 004, 019, 028 | after the index object of a delete entry, every index object carries `deleted_at` until one whose entry is an undelete; a purge comes no sooner than `deleted_at` plus `wal.DeleteHold` and leaves `meta` with `purged_at` and no name |
| nothing is left behind | spec 004, Sweeper | after the drain: no orphan entry, no unlisted pack, nothing of a purged repository but `meta` |
| every answer is an answer | spec 003 | every response is a success or a code of spec 003's table with its status; no 500, no panic, no hang |

### The race it should have caught

`wal.Log.Commit` stops when a lost round brings an index object that
carries `deleted_at` while its own base did not and the entry is a push:
it writes nothing more and returns `wal.ErrDeleted`. An undelete starts
from a deleted base and goes on; a delete, a compaction, and an import
are unchanged. The receive path answers with the sideband line of
`repo_not_found`; the server-side operations and the default branch
change of `PATCH /v1/repos/{id}` answer 404 `repo_not_found`. A fixed
seed that reproduces the race on the tree with that check removed stays
in the gate's list, so the simulation proves it finds the race on its
own.

### Seeds, traces, and replay

| Test flag | Default | Meaning |
|---|---|---|
| `-sim.seed` | unset | run this one seed |
| `-sim.seeds` | 0 | fresh seeds to run after the fixed ones |
| `-sim.steps` | the gate's step count | steps per seed |
| `-sim.budget` | unset | no new seed starts after this much wall time |
| `-sim.trace` | unset | the directory a failing seed writes its trace to |
| `-sim.replay` | unset | a trace to replay call for call |
| `-sim.shrink` | false | shrink a failing trace before writing it |

The flags are the test binary's, so reproduction needs no variable of
the node's. A failure prints one line:

```
sim: seed 0x5f2c… step 212 invariant "a deletion holds until an undelete": index 9 has deleted_at null after the delete at 7; replay: go test ./internal/sim -run TestSimulation -args -sim.seed=0x5f2c…
```

A trace is JSON lines: a header (seed, nodes, repositories, profile, the
git version), then per step its operations with their operands, every
scheduling point (actor, call number, method, key, fault, outcome), and
every check. Replay follows the record and stops at the first parked
call whose method or key differs from it, naming both. Same seed, same
git, same trace is a criterion: a difference is a path whose order of
store calls depends on something the seed does not fix, which is a
defect to fix (a sorted listing, a seeded nonce), not to tolerate. A
trace names a content-addressed key (`packs/<hash>`) by its order of
first appearance, because git's threaded delta search may write
different bytes for the same objects. The nonce of an entry key is the
one source in the tree that changes keys themselves, and
`wal.Options.Rand` removes it.

### Shrinking

With `-sim.shrink`, a failing trace is cut, keeping a candidate only
when its replay fails the same invariant:

1. whole steps, in halves, then quarters, down to single steps;
2. single operations within the steps left;
3. faults, one at a time, largest first;
4. concurrency: the operations of a step run one after another.

A candidate follows the recorded schedule for the actors it keeps and
the seed's order for the rest. Shrinking stops after 200 replays or 10
minutes and writes the smallest trace that still fails.

### Budget

`TestSimulation` in the gate runs the fixed seeds, 4 to start with and
one more for each failure ever found, each for the gate's step count,
in at most 60 seconds under `-race` on the CI runner; the build sets
the step count by measurement and `docs/internals/testing.md` records
it. Fixed seeds keep the gate deterministic: a fresh seed's failure
belongs to the weekly job, not to whoever pushed next.

`make sim` runs `go test ./internal/sim -run TestSimulation -count=1
-timeout 60m -args -sim.seeds=1000000 -sim.budget=45m -sim.shrink
-sim.trace=out/sim`. The `sim` job of `.github/workflows/verify.yml`
runs it on the weekly cron beside `fuzz`, and on dispatch, without
`-race`, which the gate's run carries, and uploads `out/sim` when it
fails. A seed it finds joins the fixed list with its fix.

### Adding operations

An operation is a value in one table: its name, its weight, a generator
from the seed and the model to a call, the call against the cluster, and
what the result does to the model. An invariant is a named check over
the model and the cluster. A goroutine with its own context registers
as an actor and reports when it waits for work.

| Spec | Adds |
|---|---|
| 032, forks | `fork` (a fork of a live repository or of a fork, up to `limits.MaxForkDepth`), purges of parents with forks, a step bias of a fork with a compaction and a sweep of its parent, and the invariant that every live fork materializes cold after every sweep |
| 034, batched pushes | `push.burst` (2 to 16 pushes to one repository on one node in one step), the batcher as an actor, and the invariant that the verdicts equal applying the burst's pushes one at a time in their order in the entry |

Each lands with its operations on and the gate's fixed seeds green, and
a weekly run of fresh seeds green before the spec moves to `testing`.

Built first, the simulation runs on today's tree and is green with the
fix above, so a failure after spec 032 or 034 lands is that spec's.
Both specs change exactly what scripted tests miss: spec 032's safety
rests on an ordering between a fork's read and a sweep's listing, and
spec 034 refuses pushes one by one inside a commit that can lose
rounds.

### Names the build adds

| Name | Kind | Meaning |
|---|---|---|
| `wal.Options.Rand` | Go field | the reader entry nonces come from, `crypto/rand.Reader` by default |
| `wal.BreakerOptions.Sleep` | Go field | the wait between the polls of `Wait`, `wait.Sleep` by default |
| `compact.Options.Context` | Go field | the context a run takes, `context.Background()` by default |
| `-sim.seed`, `-sim.seeds`, `-sim.steps`, `-sim.budget`, `-sim.trace`, `-sim.replay`, `-sim.shrink` | test flags | above |
| `make sim`, the `sim` job | Makefile target, CI job | the weekly run |

None is a code, variable, metric, event, failpoint, header, or route,
so no defining table moves; spec 013 gains the target and the job.

## Not in this spec

Push events and their delivery, LFS, the SSH framing (the receive path
under it is the one simulated), imports, migration, the authorizer, a
bucket that corrupts what it stores, and `Apply` with more than one
worker. The cluster tier stays as it is. Each can join as an operation
later.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Two runs of one seed of 200 steps on one git write identical traces, every scheduling point and check included | `internal/sim`, `TestSameSeedSameTrace` | not built |
| A replay of a trace whose operation was changed to list a map in random order stops at the first differing call and names both keys | `internal/sim`, `TestReplayNamesTheFirstDivergence` | not built |
| Each invariant catches a mutant planted for it (a bucket whose second `Create` of an index key succeeds, a sweep with `minAge` 0 during an apply, a read served from a copy one sequence behind, a compaction that drops a reference, a purge before the hold) within 500 steps of a fixed seed | `internal/sim`, `TestEveryInvariantCatchesItsMutant` | not built |
| With the `wal.ErrDeleted` check removed, the race seed fails "a deletion holds until an undelete" within its fixed steps; with the check, it passes | `internal/sim`, `TestPushRacingADelete` | not built |
| A failing trace of 500 steps with one planted defect shrinks within 200 replays to at most 10 operations that fail the same invariant | `internal/sim`, `TestShrinkFindsTheSmallTrace` | not built |
| Across the fixed seeds every fault fires on each of `Create`, `Put`, `Get`, `Head`, `List`, `Delete` it applies to, and every operation runs at least 20 times | `internal/sim`, `TestSimulationCoverage` | not built |
| The fixed seeds pass in at most 60 seconds under the gate's `-race` on the CI runner | `internal/sim`, `TestSimulation` | not built |
| `make sim` runs fresh seeds for its budget, and on a planted failure prints the replay line and the `sim` job uploads the shrunk trace | `.github/workflows/verify.yml`, job `sim`, one dispatched run | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 013 | the `sim` job and `make sim` in the jobs table; `internal/sim`'s fixed seeds in the hermetic suite |
