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
| repository size | the authorizer's `quota_bytes`, default 50 GiB, against one figure: `size_bytes` of the held index as spec 004 defines it (the bytes of the listed packs plus the pack bytes of the entries since the last compaction, so a compaction lowers it and the quota counts what the log holds, not every byte ever pushed) plus the bytes under `lfs/` (the sum spec 010's listing produces, cached per repository for 60 seconds so a push does not list the prefix), plus the bytes the write adds | receive-pack, before the entry is written, with the pack's bytes as the addition; the LFS upload batch (spec 010) with the batch's sizes; an import (spec 019) with its pack bytes; a server-side operation (spec 020) with its pack's bytes | on receive-pack the POST answers 200 and the refusal travels as the hook's verdict, so git's own output carries `over_quota: <sentence>` exactly and nothing else, `limit`, `bytes`, and `max` are on the handler's `info` log line, and no entry is written; on the LFS batch, 413 with the LFS body of spec 010; on the JSON API, 413 `over_quota` with `details.limit: "repository"`, `bytes`, and `max` |
| single push | 2 GiB, one entry, one `PUT` (spec 004) | receive-pack, from `Content-Length` when present and while spooling | 413 `over_quota`, `details.limit: "push"` |
| references | 100 000 commands per push and 100 000 references in the map after it | receive-pack, both before git runs: the commands as the body is parsed, the count after the push from the held index and the commands | 413 `over_quota` with `details.limit: "refs"`, `bytes`, and `max` on the command count, answered while the body is parsed and before git runs, where phase 1 answered 400 `invalid_request`; the count after the push is refused the way the size rule is, the POST answering 200 with `over_quota: <sentence>` as the hook's verdict and the figures on the `info` line |
| push options | 1 000 per push | receive-pack | 400 `invalid_request` |
| requests per subject | `ORIGO_REQUESTS_PER_MINUTE`, default 600, per minute, a token bucket per effective subject per node, burst the same figure, `0` off; a bucket not touched for 10 minutes is evicted, so the table holds only active subjects; the `kind` overlay of spec 013 sets 6000 and each cluster scenario mints a subject of its own, because those scenarios drive one node harder than any caller of a live installation drives it | every route of the public listener after authentication | 429 `rate_limited` with `Retry-After` in whole seconds and `details.limit: "subject"` |
| concurrent git subprocesses per node | `ORIGO_MAX_GIT_PROCS`, default 64, one semaphore shared by `internal/httpgit`, `internal/api`, and compaction | before the request's own subprocess starts, one slot per request and not one per subprocess: a helper a request runs while its own subprocess is alive takes no slot of its own, and one slot covers a whole read request, which spec 009 runs as `rev-parse` and then the operation under one budget; a request waits at most 5 seconds for a slot; a compaction (spec 006) that waits more than 5 seconds skips this run and retries on the next sweep | 429 `rate_limited`, `details.limit: "subprocesses"` on a request; `origo_compactions_total{result="skipped"}` on a compaction |
| subprocess wall time | 5 minutes for `upload-pack`, `receive-pack`, and `index-pack`; 30 minutes for the repack of a compaction (spec 006); 30 seconds for a read API operation (spec 009) and the `commits` operation, 5 minutes for the merge family (spec 020); 10 minutes for an export (spec 019) | `internal/repo.Git` and the handlers | the subprocess is killed with its process group; 504 `operation_timeout` (spec 009) on the API, git's own error on the sideband |
| JSON body | 64 KiB | every `/v1/` route except the operation routes of spec 020, which carry their own 64 MiB limit | 400 `invalid_request` |
| LFS batch body | 1 MiB | `POST /{repo}/info/lfs/objects/batch` (spec 010) | 400 with the LFS body of spec 010 carrying the `invalid_request` sentence |
| entry header, transaction, index object | 4 KiB, 64 MiB, 64 MiB | the parsers of spec 004 | the entry or index is refused as corrupt (spec 015) |
| object storage retries | 3 attempts from 50 ms, capped at 2 s, under `ORIGO_STORAGE_TIMEOUT` per attempt (spec 015) | `pkg/s3` | the store's error |

### Headers

| Header | Meaning |
|---|---|
| `RateLimit-Limit` | the requests one effective subject may send this node in a minute, the figure `ORIGO_REQUESTS_PER_MINUTE` names, on every response of the rate-limited surface; the `RateLimit-Limit` field of the IETF draft [RateLimit header fields for HTTP](https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/). A client reads the figure in force rather than assuming the default, which is what lets spec 021's `rate_limited` case send one request more than the limit against any installation. Absent when the limit is off. |

### Where the bounds live

The limits are the node's, not something a caller opts into:
`httpgit.New`, `api.New`, and `lfs.New` build the table above over the
log when they are given none, and a test lowers one figure through an
option. The measurement of the bytes under `lfs/` is one function,
`limits.LFSBytes`, behind the 60 second cache, so the push and the LFS
batch measure the same figure and neither lists the prefix twice in a
minute; `internal/lfs` calls through it and does not list on its own. A
push whose `lfs/` sum cannot be read is refused with
`storage_unavailable`, because a repository that cannot be measured is
not one a quota was checked against.

The per-subject bucket sits behind the verifier and in front of the
whole application mux, which is one place for every surface. A
rate-limited LFS request therefore answers spec 003's envelope and not
the LFS body shape of spec 010; the status and `Retry-After` are what
`git-lfs` reads, so the client backs off either way.

### The bound token's quota

A repository-bound token (spec 007) carries no quota claim, so a write
under one asks the authorizer for the minting subject's figure with the
token's own `sub` and `act` on the bound repository, cached like any
allow. The call supplies the figure and decides nothing: the token's
scope already decided the access. A deny, and an allow that names no
figure, therefore leave `auth.DefaultQuotaBytes` and write a warning
rather than refusing a write the scope allows. An authorizer that
produced no answer is the one exception and fails closed: the write is
refused with `authorizer_unavailable`, which is spec 007's rule that an
outage denies. A figure that cannot be read is not a figure to write
against, and the same outage denies every unbound write on the node, so
riding it out under a bound token would make the token the way around
the outage rule.

This last rule is an item for spec 016's builder, who owns
`internal/auth`: `Guard.quota` today logs the outage and falls back to
the default, and `Guard.Decide` must propagate the `*Unavailable` on a
write instead. `TestBoundTokenWriteFailsClosedDuringAuthorizerOutage`
holds it, and the closing block of
`TestBoundTokenWriteTakesTheMintersQuota`, which asserts the fallback
today, changes with it.

### What 600 requests a minute buys

One push is two requests, the advertisement and the service call, so
the default bounds one subject to 300 back-to-back pushes a minute on
one node. That is above any human client and below a build fleet
pushing in a loop under one service token: an operator running such a
fleet raises `ORIGO_REQUESTS_PER_MINUTE`, the way the `kind` overlay
does. A subject that drives many repositories at once wants a figure of
its own rather than the node's, which spec 020 carries as a builder
item: the authorizer's response gains an optional `requests_per_minute`
(spec 007's table), and a subject the authorizer names a figure for is
bucketed at it.

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
  `quota_bytes` is refused with `over_quota` in git's own output, no
  entry is written, and the `bytes` and `max` of the handler's `info`
  line name the sizes; the LFS bytes count, asserted with an object
  under `lfs/` sized to make the difference (proposed:
  `internal/httpgit`, `TestPushOverQuotaWritesNothing`).
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
`internal/config`. Every criterion has a passing test in the tree, the
frozen-repository one through spec 021's suite since 2026-09-09; the
spec stays at `testing` until that suite's stack run is cited as green
by spec 021.

| Criterion | Test |
|---|---|
| a push past `quota_bytes` is refused with `over_quota`, no entry is written, the figures name the sizes, and the bytes under `lfs/` are what makes the difference | `internal/httpgit`, `TestPushOverQuotaWritesNothing`; `test/e2e`, `TestE2EPushOverQuota` against a built `origod` in the `integration` job |
| a push past the single-push bound is 413 `over_quota` with `details.limit: "push"`, with the spool stopped at the limit | `internal/httpgit`, `TestPushOverTwoGiBIsRefused`, the bound lowered through `limits.Options` |
| the 601st request of a subject in one minute is 429 with `Retry-After`, a second subject sees none, and a bucket idle for ten minutes is gone | `internal/limits`, `TestPerSubjectTokenBucket`, `TestIdleBucketsAreEvicted` |
| with two slots a third caller waits and is then 429 `rate_limited` with `details.limit: "subprocesses"`, and a compaction that finds no slot skips and runs on the next sweep | `internal/limits`, `TestSubprocessCap`; `internal/compact`, `TestCompactionSkipsWhenNoSlot`; `internal/httpgit`, `TestSubprocessSlotsAdmitAndRefuse`, `TestConcurrentPushesShareTheSubprocessCap`; `internal/api`, `TestReadWithoutASubprocessSlotIsRateLimited` |
| a frozen repository accepts a clone and refuses a push with `repo_frozen` | spec 021's `TestContract/019/freeze` in `test/conformance`, run against the stub by `TestStubConforms` and against the stack in the `e2e` job: the clone succeeds, the push is refused at `info/refs` with `remote error: repo_frozen: <sentence>`, a second freeze is 409, and a push lands after the unfreeze (closed 2026-09-09) |

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
  and this spec asks only for `quota_bytes`, so a deny and an answer
  with no figure leave `auth.DefaultQuotaBytes` and write a warning
  rather than refusing a push the scope allows. The build also let an
  authorizer that produced no answer fall back to that default; the
  Design now refuses the write with `authorizer_unavailable` instead,
  which is the one item this spec leaves open in the tree and spec
  016's builder closes. Spec 007's `TestRepositoryBoundTokenScope` now
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
- **A rate-limited LFS request answers spec 003's envelope, not the
  LFS body.** The bucket sits behind the verifier and in front of the
  whole application mux, which is one place for every surface; spec
  010's Outcome records the same reading for the 401. The status and
  `Retry-After` are what `git-lfs` reads, so the client backs off
  either way.
- **A push whose `lfs/` sum cannot be read is refused.** The
  measurement is a storage read, and a repository that cannot be
  measured is not one a quota was checked against
  (`TestQuotaFailsClosedWhenTheListingFails`).
- **The rate is a variable, and the stack raises it.** The first
  draft fixed 600 a minute with no knob, and a client that pushes back
  to back exceeds it: the first cluster run of this build refused
  `TestClusterFiveHundredPushesStayUnder64EntriesAnd6Packs`,
  `TestClusterCompactionKeepsFetchLatencyFlat`,
  `TestClusterDegradedStorage`, and `TestClusterNodeRemovalUnderReadLoad`
  with 429 `rate_limited`, the 500 push scenario alone running at
  about fourteen pushes a second, twenty-eight requests, against a
  refill of ten. Two changes together: each cluster scenario takes a
  subject of its own, which is what spec 013's `ORIGO_TEST_ADMIN_TOKEN`
  default now mints, and the figure is `ORIGO_REQUESTS_PER_MINUTE`, the
  Design's 600 by default, which the `kind` overlay raises to 6000
  beside the storage figures it already tunes. The limit stays in force
  on the stack, so spec 021's `rate_limited` case can still meet it;
  `RateLimit-Limit` on every response of the surface is how that case
  learns the figure to exceed. That 600 a minute refuses a legitimate
  client pushing in a loop is a finding for the deck, not something
  this build settles.

No criterion of this spec is a stack criterion, so none needed the
kind stack; what the stack proves is that the limits do not refuse the
work of the specs that run on it. The cluster tiers run on a tag or a
`workflow_dispatch` since the change that took them off every push,
and the dispatched run 34278489246 on `c43db8e` is green in every job:
the gate, the integration tier, the mutation job, the `e2e` and
`e2e-slow` cluster tiers, and the up-script check, with the overlay at
6000 and each scenario on a subject of its own.

Items another spec closed for this one:

- Spec 020's builder built the per-subject rate from the authorizer,
  the item this spec left it; the settled item above records what was
  built.

Items this spec closes for another:

- Spec 010's builder item is done: `quota_bytes` for a
  repository-bound token is the minting subject's figure from the
  authorizer, not `auth.DefaultQuotaBytes`. Spec 010's Outcome records
  it.
- Spec 010's 60 second `lfs/` byte cache is built, as `limits.LFSBytes`.
- Spec 006's `compact.Slots` seam is filled: `cmd/origod` passes the
  node's semaphore, so a compaction and a request share one budget.

The three items this Outcome left open are settled and are the
Design's rules above:

- The reference row's second half is refused the way the size rule is:
  the receive-pack POST answers 200 and the client reads
  `over_quota: <sentence>` out of git's own output, with the figures on
  the `info` line. Spec 003 fixes the form of a line that carries a
  code and gives `non_fast_forward` the same shape, so the reference
  count needed no new rule.
- The 600 a minute default stays. It bounds one subject to 300
  back-to-back pushes a minute, which is above any human client, and an
  operator running a fleet of tooling under one token raises
  `ORIGO_REQUESTS_PER_MINUTE`. A subject that drives many repositories
  wants a figure of its own: spec 007's response gains an optional
  `requests_per_minute`, and spec 020's builder closed that item on
  2026-09-09. `auth.Decision.RequestsPerMinute` carries the figure with
  no default, so absent stays the variable's value;
  `Buckets.SetRate(subject, perMinute)` makes that subject's bucket
  fill at it and to it, raising the depth at once so a tool the
  authorizer has just granted a higher rate is not held to the one its
  bucket was created with; and `Limits.SetSubjectRate` is called
  wherever an allow arrives, in `api.admit`, `httpgit.decide`, and the
  LFS decision, so no wiring reaches `cmd/origod`. The subject is
  bucketed at its own figure from the request after the first, because
  the bucket runs in front of the authorizer.
  `TestASubjectTheAuthorizerNamesARateForIsBucketedAtIt` in
  `internal/limits` and `TestAuthorizerRateBucketsTheSubject` in
  `internal/api` hold it.
- A bound token's write during an authorizer outage fails closed with
  `authorizer_unavailable`, matching spec 007's rule that an outage
  denies. The first build let `Guard.quota` in `internal/auth` fall
  back to the default on an unreachable authorizer; spec 016's build
  fixed it on 2026-09-09: `quota` returns the `*Unavailable`,
  `Guard.Decide` propagates it on a write, and
  `TestBoundTokenWriteFailsClosedDuringAuthorizerOutage` holds the 503
  through `Admit`, the read that still answers, and the write allowed
  again once the authorizer answers, while the closing block of
  `TestBoundTokenWriteTakesTheMintersQuota` asserts the refusal in
  place of the fallback.

`latere.ai/x/pkg`: neither a token bucket nor a waiting semaphore is in
the shared library; the README's items table carries the row.
