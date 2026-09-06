---
title: "Degraded storage: what a node does when the bucket is slow, partial, or gone"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/011-observability.md
affects: [internal/wal/, internal/repo/, internal/httpgit/, internal/api/, deploy/]
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

Phase 1 wraps every store call in `pkg/retry` with a bounded policy and a
10 second timeout, and nothing else: a slow bucket makes every request
slow, and an unreachable one makes every request fail after the retries.

## Design

### Breaker per operation class

One `pkg/circuitbreaker` per node for reads (`GET`, `HEAD`, list) and one
for writes (`PUT`, create, delete), so a write-side outage does not stop
reads that the cache can answer. A breaker opens after 5 consecutive
failures or timeouts within 30 seconds, stays open with exponential
backoff from 1 to 60 seconds, and half-opens with one probe request.
`origo_storage_breaker_state{class}` is a gauge and the open transition
is an alert (spec 011).

### Reads while degraded

| Bucket state | Currency check | Served |
|---|---|---|
| healthy | `HEAD index/<n+1>` answers | as spec 005 |
| slow (check exceeds `ORIGO_STORAGE_TIMEOUT`) | counted as a failure toward the breaker | the request waits for the check; no stale serving below the breaker threshold, because a slow check is still a correct one |
| read breaker open, repository warm | skipped | served from the local copy with `Origo-Stale: <seconds since last confirmed check>`, capped at `ORIGO_STALE_MAX` (default 5 minutes); past the cap, 503 `storage_unavailable` |
| read breaker open, repository cold | impossible | 503 `storage_unavailable` |

Stale serving is bounded and labelled so a consumer that must not read
stale (a build fetching a commit it was just told about) can refuse the
response by the header, while a clone that would otherwise fail gets a
recent copy. `refs` and the read API carry the same header.

### Writes while degraded

A push needs the entry written and the index created; neither can be
faked. With the write breaker open, `receive-pack` answers the client
before it uploads a pack: a sideband message `Origo is temporarily
unable to accept pushes; try again in <n> seconds` and the git error
`storage_unavailable`, so a client does not spend a minute uploading a
pack that cannot land. A pack already uploaded when the breaker opens is
retried under the breaker's backoff for at most `ORIGO_STORAGE_TIMEOUT`
times 6, then rejected the same way; the entry, if written, is an orphan
the sweeper removes.

### Partial failure

A single key failing (a 404 for a pack the index names, a checksum
mismatch on an entry) is corruption in the log, not an outage: the node
reports `origo_log_integrity_errors_total`, refuses to serve the affected
repository with 503 `repository_unavailable`, and logs the key. It never
rebuilds the log from a local copy. An operator restores the object from
the provider's versioning or from a replica's cache by a documented
procedure (`docs/operations.md`).

### Recovery

When the breaker closes, every warm repository re-runs its currency check
on the next request and catches up as usual; nothing is scheduled, so a
node that was serving stale returns to consistent reads with no operator
action.

## Acceptance criteria

- With the bucket unreachable, a warm repository clones with the stale
  header for 5 minutes and answers 503 afterwards; a cold one answers 503
  at once; a push is refused before its pack upload with the documented
  message.
- With the bucket answering in 15 seconds, reads and writes fail after
  the timeout and the breaker opens within 5 failures; after the bucket
  recovers, the first request after the half-open probe serves
  consistently and the header is absent.
- A missing pack object makes that repository 503 `repository_unavailable`
  without affecting others and increments the integrity counter.
- All of the above run in the kind stack against MinIO with a fault
  injector on the store (network policy for unreachable, a delaying proxy
  for slow, object deletion for partial).
