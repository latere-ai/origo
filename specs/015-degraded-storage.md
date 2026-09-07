---
title: "Degraded storage: what a node does when the bucket is slow, partial, or gone"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/011-observability.md
affects: [internal/wal/, internal/repo/, internal/httpgit/, internal/api/, internal/config/, cmd/origod/, deploy/, test/stubs/slowproxy/, test/e2e/, docs/operations.md]
effort: medium
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Degraded storage

## Overview

Every read starts with a request to the bucket and every write ends with
one, so the bucket's health is the service's health. Spec 004 handles a
node dying. This spec handles the bucket failing, in the three ways
object storage fails: slow, partially, and entirely. The rule is the
architecture's: always correct when degraded, fast when healthy. A node
never serves a reference it cannot prove current, and never acknowledges
a push it cannot prove durable, but it keeps serving what it can prove.

## Current state

`pkg/s3` retries a 5xx, a 429, or a transport failure under
`DefaultRetry` (3 attempts from 50 ms, capped at 2 s). The storage
transport in `cmd/origod` has a 60 second response header timeout and a
10 second TLS handshake timeout; there is no per-operation deadline and
`ORIGO_STORAGE_TIMEOUT` is not read. Nothing else: a slow bucket makes
every request slow, and an unreachable one makes every request fail
after the retries with 503 `storage_unavailable`. `pkg/circuitbreaker`
exists and is unused.

One item in `latere.ai/x/pkg`, for the builder, before the wrapper
below can be tested with a fake clock: `circuitbreaker.Breaker` reads
`time.Now` directly and `New(threshold, openDuration)` takes no
option, so its open window cannot be advanced in a test.
`pkg/circuitbreaker` gains `WithClock(func() time.Time)`, an `Option`
on `New`, defaulting to `time.Now`, the way `BackoffConfig.Now` already
does for the other breaker; the wrapper passes its own clock through.

## Design

### Deadline per operation

Every `Store` call in `internal/wal` runs under a context deadline of
`ORIGO_STORAGE_TIMEOUT` (default 10 seconds) per attempt, so the three
attempts of `DefaultRetry` take at most 34 seconds. A timeout is a
failure toward the breaker.

### Breaker per operation class

`internal/wal` wraps the `Store` in two `circuitbreaker.Breaker` from
`latere.ai/x/pkg/circuitbreaker`, each `circuitbreaker.New(5,
30*time.Second, circuitbreaker.WithClock(clock.Now))`: one for reads
(`Get`, `Head`, `List`) and one for writes (`Put`, `Create`, `Delete`),
so a write-side outage does not stop reads the cache can answer. The
wrapper, `wal.BreakerStore`, takes a clock, an interface with one
method `Now() time.Time`, defaulting to `time.Now`, and passes it to
both breakers, so every duration in this spec runs under a fake clock
in the suite. The wrapper calls `Allow` before every `Store` call and
refuses at once with `wal.ErrStorageOpen` when it answers false, which
the handlers map to 503 `storage_unavailable` with `details.op` and
`details.error: "breaker open"`; after the call it records one success
or one failure. One `Store` call is one count
whatever `pkg/s3` did inside it: its three attempts under
`DefaultRetry` are one failure when the last attempt fails, and a
timeout, a transport error, and a 5xx are failures, while a 404, a
412, and a 304 are successes. The breaker does what the package does:
it opens on the 5th consecutive failure, stays open for 30 seconds,
then `Allow` admits one probe and answers false to everyone else until
the probe reports; a success closes the breaker and a failure reopens
it for another 30 seconds. There is no doubling.
`origo_storage_breaker_state{class}` reports the package's `State`: 0
closed, 1 open, 2 half-open, and `OrigoBreakerOpen` (spec 011) fires.
The wrapper records the time it saw the breaker open so it can send
`Retry-After` as the whole seconds of the 30 that remain, at least 1.
`origo_storage_ops_total{op,result}` and `origo_storage_seconds{op}`
are recorded on every call.

### Reads while degraded

| Bucket state | Currency check | Served |
|---|---|---|
| healthy | `HEAD index/<n+1>` answers | as spec 005 |
| slow (a check exceeds `ORIGO_STORAGE_TIMEOUT`) | counted as a failure toward the read breaker | the request waits for the check and fails with 503 `storage_unavailable` on timeout; no stale serving below the breaker threshold, because a slow check is still a correct one |
| read breaker open, repository warm | skipped | served from the local copy with `Origo-Stale`, up to `ORIGO_STALE_MAX` (default 5 minutes) after the last confirmed check; past the cap, 503 `storage_unavailable`. The time of the last check that answered lives in the cache entry of `internal/repo`, beside the sequence the copy holds, written by `Acquire` on every check that answers and read by the handler that decides whether the copy may be served stale, so the bound is per repository and survives nothing a request cannot rebuild: an evicted copy has no last check and is cold |
| read breaker open, repository cold | impossible | 503 `storage_unavailable` |

| Header | Meaning |
|---|---|
| `Origo-Stale` | on a response served without a currency check: the whole seconds since the last check that answered; absent on every consistent response |

Stale serving is bounded and labelled so a consumer that must not read
stale (a build fetching a commit it was just told about) can refuse the
response by the header, while a clone that would otherwise fail gets a
recent copy. `info/refs`, `git-upload-pack`, and every read endpoint of
spec 009 carry the header the same way; `origo_stale_responses_total`
counts them.

### Writes while degraded

A push needs the entry written and the index created; neither can be
faked. With the write breaker open, `info/refs?service=git-receive-pack`
refuses the client before it uploads a pack, in the one form git shows
the user: HTTP 200 with `Content-Type:
application/x-git-receive-pack-advertisement`, `Retry-After` set to
the breaker's remaining open time in whole seconds, and a body of the
`# service=git-receive-pack` line, a flush, and one `ERR
storage_unavailable: <the sentence of spec 003>` pkt-line. Git prints
`fatal: remote error: storage_unavailable: …` and sends no pack; a 503
would show the user only the status. A pack already spooled when the
breaker opens waits for the breaker: the commit polls `Allow` on the
write breaker every 500 ms for at most 60 seconds, runs as the probe
the first time `Allow` answers true, and is refused with the same line
in the sideband when the 60 seconds pass with no admission or the
probe fails; the entry, if written, is an orphan the sweeper removes.
The hook of spec 004 holds git's verdict FIFO for that minute, which
is inside the 5 minute deadline of `receive-pack`.

### Partial failure

A single key failing is corruption in the log, not an outage: a 404 for
a pack the index names, an entry whose length or `pack_sha256` differs
from the header, an index object that does not parse. The node
increments `origo_log_integrity_errors_total`, logs the key, evicts
nothing, and refuses the repository:

| Code | Status | Message | Details |
|---|---|---|---|
| `repository_unavailable` | 503 | This repository cannot be served until an operator restores it. Other repositories are not affected. | `key`, `error` |

It never rebuilds the log from a local copy. An operator restores the
object from the provider's versioning or from a replica's cache by the
procedure in `docs/operations.md`; the next request materializes again.

### Recovery

When a breaker closes, every warm repository re-runs its currency check
on the next request and catches up as usual; nothing is scheduled, so a
node that was serving stale returns to consistent reads with no operator
action, and `Origo-Stale` disappears from the first consistent response.

## Not in this spec

A second bucket or region. Read-through from another node's cache.
Queueing pushes for later commit.

## Acceptance criteria

- With the bucket unreachable, a warm repository clones with
  `Origo-Stale` for 5 minutes (fake clock) and answers 503
  `storage_unavailable` afterwards; a cold one answers 503 at once; a
  push is refused at `info/refs` with the `ERR` pkt-line before any
  pack is uploaded (proposed: `internal/httpgit`,
  `TestReadBreakerServesStaleThenRefuses`).
- With every store call answering after 15 seconds, reads and writes
  fail after `ORIGO_STORAGE_TIMEOUT`, one call with three internal
  attempts counts one failure, and the breaker opens on the 5th and
  refuses with `ErrStorageOpen`; after 30 seconds with the wrapper's
  fake clock one probe is admitted while a concurrent call is refused,
  a failed probe reopens for 30 seconds and not 60, and after the store
  recovers the first request after a successful probe is served
  without `Origo-Stale` (proposed: `internal/wal`,
  `TestBreakerOpensOnTimeoutsAndRecovers` with `MemStore.SetFault`).
- With the write breaker open, `info/refs?service=git-receive-pack`
  answers 200 with the `ERR` pkt-line and `Retry-After`, and `git push`
  exits with the sentence in `remote error` having sent no pack; a
  push whose pack was spooled before the breaker opened is committed
  when the breaker admits a probe within 60 seconds of fake time and
  refused in the sideband when it does not (proposed:
  `internal/httpgit`, `TestWriteBreakerRefusesBeforeUpload`,
  `TestSpooledPushWaitsForTheBreaker`).
- A missing pack object makes that repository 503 `repository_unavailable`
  with `details.key`, increments `origo_log_integrity_errors_total`, and
  leaves another repository served (proposed: `internal/repo`,
  `TestMissingPackIsAnIntegrityError`).
- All of the above run in the kind stack against MinIO with a fault
  injector: a NetworkPolicy for unreachable, `test/stubs/slowproxy` for
  slow (a small Go program that forwards TCP to MinIO and holds each
  connection's first bytes for the delay a control endpoint sets), and
  object deletion for partial, inside the `e2e` job of spec 013
  (proposed: `test/e2e`, `TestDegradedStorage`).
