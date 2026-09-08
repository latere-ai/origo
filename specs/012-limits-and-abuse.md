---
title: "Limits and abuse controls"
status: testing
track: infra
depends_on:
  - specs/007-authentication-and-delegation.md
  - specs/004-write-ahead-log.md
  - specs/006-compaction.md
affects: [internal/limits/, internal/httpgit/, internal/api/, internal/lfs/, internal/auth/, internal/config/, cmd/origod/]
effort: small
created: 2026-09-06
updated: 2026-09-08
author: changkun
---

# Limits and abuse controls

## Overview

Origo runs behind a consumer that owns quotas per user or organization;
Origo enforces per-repository and per-node bounds so one client cannot
exhaust a node, and exposes the one knob the consumer sets through the
authorizer, `quota_bytes`.

## Current state

Phase 1 bounds what its parsers accept: a JSON body of 64 KiB, at most
100 000 commands and 1 000 push options in a receive-pack request (both
answered 400 `invalid_request`), a 4 KiB entry header, a 64 MiB
transaction, a 64 MiB index object. Every git subprocess has a 5 minute
deadline. Object storage calls use `pkg/s3`'s `DefaultRetry`: 3 attempts
from 50 ms, capped at 2 s, on a 5xx, a 429, or a transport failure.
Nothing limits a push's size, a repository's size, a subject's request
rate, or the number of concurrent subprocesses. `internal/limits` does
not exist.

One item for the builder, from spec 010: `quota_bytes` for a
repository-bound token is `auth.DefaultQuotaBytes` there, because the
token's claims carry no quota and the authorizer never sees the token.
This spec asks the authorizer for the minting subject's figure when a
bound token writes (the token's `sub`, `act` as the actor, action
`write`, on the bound repository), cached like any allow, so a bound
token's uploads are held to the figure its minter's pushes are, and
spec 010's interim rule ends.

## Design

| Limit | Value | Enforced at | Answer |
|---|---|---|---|
| repository size | the authorizer's `quota_bytes`, default 50 GiB, against one figure: `size_bytes` of the held index as spec 004 defines it (the bytes of the listed packs plus the pack bytes of the entries since the last compaction, so a compaction lowers it and the quota counts what the log holds, not every byte ever pushed) plus the bytes under `lfs/` (the sum spec 010's listing produces, cached per repository for 60 seconds so a push does not list the prefix), plus the bytes the write adds | receive-pack, before the entry is written, with the pack's bytes as the addition; the LFS upload batch (spec 010) with the batch's sizes; an import (spec 019) with its pack bytes; a server-side operation (spec 020) with its pack's bytes | sideband `over_quota` with `details.limit: "repository"`, `bytes`, `max`; on the LFS batch, 413 with the LFS body of spec 010; on the JSON API, 413 `over_quota` |
| single push | 2 GiB, one entry, one `PUT` (spec 004) | receive-pack, from `Content-Length` when present and while spooling | 413 `over_quota`, `details.limit: "push"` |
| references | 100 000 commands per push and 100 000 references in the map after it | receive-pack, both before git runs: the commands as the body is parsed, the count after the push from the held index and the commands | 413 `over_quota`, `details.limit: "refs"`, on the command count; the sideband `over_quota` on the count after the push (phase 1 answered 400 `invalid_request` for the command count) |
| push options | 1 000 per push | receive-pack | 400 `invalid_request` |
| requests per subject | 600 per minute, a token bucket per effective subject per node, burst 600; a bucket not touched for 10 minutes is evicted, so the table holds only active subjects | every route of the public listener after authentication | 429 `rate_limited` with `Retry-After` in whole seconds and `details.limit: "subject"` |
| concurrent git subprocesses per node | `ORIGO_MAX_GIT_PROCS`, default 64, one semaphore shared by `internal/httpgit`, `internal/api`, and compaction | before a subprocess starts; a request waits at most 5 seconds for a slot; a compaction (spec 006) that waits more than 5 seconds skips this run and retries on the next sweep | 429 `rate_limited`, `details.limit: "subprocesses"` on a request; `origo_compactions_total{result="skipped"}` on a compaction |
| subprocess wall time | 5 minutes for `upload-pack`, `receive-pack`, and `index-pack`; 30 minutes for the repack of a compaction (spec 006); 30 seconds for a read API operation (spec 009) and the `commits` operation, 5 minutes for the merge family (spec 020); 10 minutes for an export (spec 019) | `internal/repo.Git` and the handlers | the subprocess is killed with its process group; 504 `operation_timeout` (spec 009) on the API, git's own error on the sideband |
| JSON body | 64 KiB | every `/v1/` route except the operation routes of spec 020, which carry their own 64 MiB limit | 400 `invalid_request` |
| LFS batch body | 1 MiB | `POST /{repo}/info/lfs/objects/batch` (spec 010) | 400 with the LFS body of spec 010 carrying the `invalid_request` sentence |
| entry header, transaction, index object | 4 KiB, 64 MiB, 64 MiB | the parsers of spec 004 | the entry or index is refused as corrupt (spec 015) |
| object storage retries | 3 attempts from 50 ms, capped at 2 s, under `ORIGO_STORAGE_TIMEOUT` per attempt (spec 015) | `pkg/s3` | the store's error |

`receive.fsckObjects` (on since phase 1) rejects malformed objects on
the way in; `transfer.fsckObjects` and `core.protectHFS` (spec 016) are
added to the repository configuration of spec 004. A frozen repository
(spec 019) accepts reads and refuses writes with `repo_frozen`. A deleted
repository is unreadable at once and purged after the hold (spec 004).
`origo_rate_limited_total{limit}` counts every refusal by `subject` or
`subprocesses`.

## Not in this spec

Per-organization quotas, which live in the consumer's authorizer.
Connection limits at the ingress. Bandwidth shaping.

## Acceptance criteria

- A push that would take `size_bytes` plus the LFS bytes past
  `quota_bytes` is refused in the sideband with `over_quota`, no entry
  is written, and `details.bytes` and `max` name the sizes; the LFS
  bytes count, asserted with an object under `lfs/` sized to make the
  difference (proposed: `internal/httpgit`,
  `TestPushOverQuotaWritesNothing`).
- A push whose `Content-Length` or spooled body exceeds 2 GiB is 413
  `over_quota` with `details.limit: "push"`, with the spool stopped at
  the limit (proposed: `internal/httpgit`, `TestPushOverTwoGiBIsRefused`,
  with the limit lowered by an option in the test).
- A subject sending 601 requests in one minute sees 429 with
  `Retry-After` on the 601st and a second subject sees none, and a
  bucket idle for 10 minutes is gone from the table with a fake clock
  (proposed: `internal/limits`, `TestPerSubjectTokenBucket`,
  `TestIdleBucketsAreEvicted`).
- With `ORIGO_MAX_GIT_PROCS=2`, a third concurrent clone waits up to 5
  seconds then answers 429 `rate_limited` with
  `details.limit: "subprocesses"`, and a compaction started under the
  same two held slots skips with `result="skipped"` and runs on the
  next sweep once a slot is free (proposed: `internal/limits`,
  `TestSubprocessCap`; `internal/compact`, which spec 006 builds and
  this spec therefore depends on, `TestCompactionSkipsWhenNoSlot`).
- A frozen repository accepts a clone and refuses a push with
  `repo_frozen`: owned by spec 021, whose `TestContract` carries spec
  019's freeze case; this criterion passes when spec 021 lands and
  this spec's `depends_on` does not name it, because nothing here is
  built from it.

## Outcome

Built on 2026-09-08 as `internal/limits`, enforced by `internal/httpgit`
(the push quota, the single-push bound, the reference cap, and a slot
per smart HTTP subprocess), `internal/api` (a slot per read request),
`internal/lfs` (the batch quota through the same measurement), and
`cmd/origod` (the bucket table behind the verifier, the semaphore
handed to compaction). `ORIGO_MAX_GIT_PROCS` is read by
`internal/config`. Every criterion but the frozen-repository one, which
spec 021 owns, has a passing test in the tree, which is why the spec is
at `testing`.

| Criterion | Test |
|---|---|
| a push past `quota_bytes` is refused with `over_quota`, no entry is written, the figures name the sizes, and the bytes under `lfs/` are what makes the difference | `internal/httpgit`, `TestPushOverQuotaWritesNothing`; `test/e2e`, `TestE2EPushOverQuota` against a built `origod` in the `integration` job |
| a push past the single-push bound is 413 `over_quota` with `details.limit: "push"`, with the spool stopped at the limit | `internal/httpgit`, `TestPushOverTwoGiBIsRefused`, the bound lowered through `limits.Options` |
| the 601st request of a subject in one minute is 429 with `Retry-After`, a second subject sees none, and a bucket idle for ten minutes is gone | `internal/limits`, `TestPerSubjectTokenBucket`, `TestIdleBucketsAreEvicted` |
| with two slots a third caller waits and is then 429 `rate_limited` with `details.limit: "subprocesses"`, and a compaction that finds no slot skips and runs on the next sweep | `internal/limits`, `TestSubprocessCap`; `internal/compact`, `TestCompactionSkipsWhenNoSlot`; `internal/httpgit`, `TestSubprocessSlotsAdmitAndRefuse`, `TestConcurrentPushesShareTheSubprocessCap`; `internal/api`, `TestReadWithoutASubprocessSlotIsRateLimited` |
| a frozen repository accepts a clone and refuses a push with `repo_frozen` | spec 021's `TestContract`; deferred, as this spec's Acceptance criteria say |

The reference row and the `lfs/` byte cache have no criterion of their
own and are held by `TestPushOverTheReferenceCapIsRefused` and
`TestReferencesAfterAPushAreCounted` in `internal/httpgit` and by
`TestLFSBytesAreReusedForTheTTL` and `TestLFSBytesWalksEveryPage` in
`internal/limits`. The bound token's figure is
`internal/auth`, `TestBoundTokenWriteTakesTheMintersQuota`.

Divergences and interpretations, each kept and the reason:

- **A refused push carries the code and the sentence in the sideband
  and its figures in the log line.** The Answer column asks for
  `details.limit`, `bytes`, and `max` in the sideband, and the
  decisions table of `specs/README.md` fixes a line that carries a code
  as `<code>: <sentence>` exactly, with the figures of a refused push
  on the handler's `info` line, and names this spec among the specs
  that rely on it. The verdict is therefore
  `over_quota: The request exceeds this repository's storage limit.`
  and the `push refused` line carries `limit`, `bytes`, and `max`. The
  criterion's "`details.bytes` and `max` name the sizes" is met there.
- **A slot is admission control for a request's own subprocess, not for
  every subprocess it starts.** The forced-flag `git merge-base` runs
  of spec 008 and the materialization inside `repo.Cache.Acquire` run
  while the request's own subprocess is alive; if they took slots of
  their own, two concurrent pushes at `ORIGO_MAX_GIT_PROCS=2` would
  wait for slots only each other can free and both fail after the wait.
  `TestConcurrentPushesShareTheSubprocessCap` holds the rule.
- **One slot covers a whole read request.** Spec 009 runs `rev-parse`
  and then the operation in sequence under one budget; taking the slot
  again between them would refuse a request that had already begun, so
  `api.open` takes one slot and releases it with the read lock.
- **A repository-bound token's authorizer call supplies the figure and
  decides no access.** Spec 007 makes the token's scope the decision
  and this spec asks only for `quota_bytes`, so a deny, an answer with
  no figure, and an authorizer that produced no answer all leave
  `auth.DefaultQuotaBytes` and write a warning rather than refusing a
  push the scope allows. Spec 007's `TestRepositoryBoundTokenScope` now
  expects the one authorizer call a bound write makes.
- **The command cap answers 413 before git runs.** The row names the
  code and the limit but no status; 413 is spec 003's status for
  `over_quota` and the refusal happens while the body is parsed, before
  any sideband exists. The second half of the row, the references the
  index holds after the push, is counted from the held index and the
  commands (`refsAfter`) at the same point and refused in the sideband
  like the size rule.
- **`origo_rate_limited_total` records `subject` and `subprocesses`
  only.** The `repository` value of spec 011's vocabulary belongs to
  the per-repository rate limits of specs 019 and 020, and `over_quota`
  is a different code, not a rate-limited refusal.
- **The handlers build the spec's limits when none are given.** A nil
  `*limits.Limits` enforces nothing, which is the `compact.Slots` seam;
  `httpgit.New`, `api.New`, and `lfs.New` therefore build one over the
  log with this spec's defaults, so the bounds are what a node does
  rather than something a caller opts into, and a test lowers a figure
  through `limits.Options`.
- **The `lfs/` sum moved out of `internal/lfs`.** The listing and its
  paging are `limits.LFSBytes` behind the 60 second cache this spec
  asks for, so the batch and the push measure the same figure and
  neither lists twice in a minute. `internal/lfs` calls through it and
  spec 010's `TestQuotaCountsEveryPageOfTheListing` still holds the
  paging.
- **A push whose `lfs/` sum cannot be read is refused.** The
  measurement is a storage read, and a repository that cannot be
  measured is not one a quota was checked against
  (`TestQuotaFailsClosedWhenTheListingFails`).

Items this spec closes for another:

- Spec 010's builder item is done: `quota_bytes` for a
  repository-bound token is the minting subject's figure from the
  authorizer, not `auth.DefaultQuotaBytes`. Spec 010's Outcome records
  it.
- Spec 010's 60 second `lfs/` byte cache is built, as `limits.LFSBytes`.
- Spec 006's `compact.Slots` seam is filled: `cmd/origod` passes the
  node's semaphore, so a compaction and a request share one budget.

Open, for whoever needs them settled:

- The Design's Answer column for the reference row names no status and
  no side for the "references in the map after it" half; this build
  answers 413 for the command count and the sideband for the count
  after the push.
- Whether a repository-bound token's write should be refused while the
  authorizer is unavailable, rather than falling back to the default
  quota, is a question for spec 016's threat model: the figure is a
  limit, not a permission, and refusing would take a build down on an
  outage that spec 007 lets a bound token ride out.

`latere.ai/x/pkg`: neither a token bucket nor a waiting semaphore is in
the shared library; the README's items table carries the row.
