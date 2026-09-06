---
title: "Degraded storage: what a node does when the bucket is slow, partial, or gone"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/011-observability.md
affects: [internal/wal/, internal/repo/, internal/httpgit/, internal/api/, internal/config/, cmd/origod/, deploy/, docs/operations.md]
effort: medium
created: 2026-09-06
updated: 2026-09-06
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

## Design

### Deadline per operation

Every `Store` call in `internal/wal` runs under a context deadline of
`ORIGO_STORAGE_TIMEOUT` (default 10 seconds) per attempt, so the three
attempts of `DefaultRetry` take at most 34 seconds. A timeout is a
failure toward the breaker.

### Breaker per operation class

`internal/wal` wraps the `Store` in two `circuitbreaker.BackoffBreaker`
from `latere.ai/x/pkg/circuitbreaker` (`NewBackoff(BackoffConfig{BaseDelay: 1s, MaxDelay: 60s})`):
one for reads (`Get`, `Head`, `List`) and one for writes (`Put`,
`Create`, `Delete`), so a write-side outage does not stop reads the
cache can answer. A breaker opens on the 5th consecutive failure or
timeout, stays open for the backoff (1 s, doubled per reopening, capped
at 60 s, no jitter), and half-opens with one probe request; a success
closes it. `origo_storage_breaker_state{class}` reports 0, 1, or 2, and
`OrigoBreakerOpen` (spec 011) fires. `origo_storage_ops_total{op,result}`
and `origo_storage_seconds{op}` are recorded on every call.

### Reads while degraded

| Bucket state | Currency check | Served |
|---|---|---|
| healthy | `HEAD index/<n+1>` answers | as spec 005 |
| slow (a check exceeds `ORIGO_STORAGE_TIMEOUT`) | counted as a failure toward the read breaker | the request waits for the check and fails with 503 `storage_unavailable` on timeout; no stale serving below the breaker threshold, because a slow check is still a correct one |
| read breaker open, repository warm | skipped | served from the local copy with `Origo-Stale`, up to `ORIGO_STALE_MAX` (default 5 minutes) after the last confirmed check; past the cap, 503 `storage_unavailable` |
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
answers the client before it uploads a pack: 503 `storage_unavailable`
with `Retry-After` set to the breaker's remaining backoff in whole
seconds and the sideband line `storage_unavailable: <the sentence of
spec 003>`, so a client does not spend a minute uploading a pack that
cannot land. A pack already spooled when
the breaker opens is committed under the breaker's half-open probes for
at most 60 seconds, then refused the same way; the entry, if written, is
an orphan the sweeper removes.

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
  push is refused at `info/refs` with the documented sideband message
  before any pack is uploaded (proposed: `internal/httpgit`,
  `TestReadBreakerServesStaleThenRefuses`, `TestWriteBreakerRefusesBeforeUpload`).
- With every store call answering after 15 seconds, reads and writes
  fail after `ORIGO_STORAGE_TIMEOUT` and the breaker opens on the 5th
  failure; after the store recovers, the first request after the
  half-open probe is served without `Origo-Stale` (proposed:
  `internal/wal`, `TestBreakerOpensOnTimeoutsAndRecovers` with
  `MemStore.SetFault`).
- A missing pack object makes that repository 503 `repository_unavailable`
  with `details.key`, increments `origo_log_integrity_errors_total`, and
  leaves another repository served (proposed: `internal/repo`,
  `TestMissingPackIsAnIntegrityError`).
- All of the above run in the kind stack against MinIO with a fault
  injector: a NetworkPolicy for unreachable, a delaying proxy for slow,
  object deletion for partial (proposed: `test/e2e`,
  `TestDegradedStorage` on the stack of spec 013).
