---
title: "Degraded storage: what a node does when the bucket is slow, partial, or gone"
status: complete
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/011-observability.md
  - specs/013-test-stubs-and-kind-overlay.md
affects: [internal/wal/, internal/repo/, internal/httpgit/, internal/api/, internal/config/, cmd/origod/, deploy/, test/stubs/slowproxy/, test/stubs/cmd/, test/e2e/, docs/operations.md]
effort: medium
created: 2026-09-06
updated: 2026-09-08
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
`ORIGO_STORAGE_TIMEOUT` (default 10 seconds) for the whole call, not
per attempt: `pkg/s3` runs the three attempts of `DefaultRetry` under
the one context it is given and `retry.Policy` has no per-attempt
deadline, so a slow bucket fails a call after `ORIGO_STORAGE_TIMEOUT`
however many attempts fitted inside it. A per-attempt deadline is an
item for `latere.ai/x/pkg` below. A timeout is a failure toward the
breaker.

A `Get` is the one call bounded to the arrival of the object's
headers; its body is read afterwards under the caller's own context,
because an entry of up to 2 GiB cannot be read inside any per-call
deadline, and the request's own git deadline bounds that read.

### Breaker per operation class

`internal/wal` wraps the `Store` in two breakers of its own, each with
the threshold 5 and the open window 30 seconds: one for reads (`Get`,
`Head`, `List`) and one for writes (`Put`, `Create`, `Delete`), so a
write-side outage does not stop reads the cache can answer. The
breaker is local, in `internal/wal/breaker.go`, with the semantics of
`latere.ai/x/pkg/circuitbreaker` and a clock function, because that
package reads `time.Now` and takes no clock option, the item the
Current state records; `circuitbreaker.State` is still the package's
type and the gauge's values, and the comment on the local breaker
names its home.

The wrapper, `wal.BreakerStore`, takes a clock, an interface with one
method `Now() time.Time`, defaulting to `time.Now`, and passes it to
both breakers, so every duration in this spec runs under a fake clock
in the suite. The wrapper calls `Allow` before every `Store` call and
refuses at once with `wal.ErrStorageOpen` when it answers false, which
the handlers map to 503 `storage_unavailable` with `details.op` and
`details.error: "breaker open"`; after the call it records one success
or one failure.

One `Store` call is one count whatever `pkg/s3` did inside it: its three attempts under
`DefaultRetry` are one failure when the last attempt fails, and a
timeout, a transport error, and a 5xx are failures, while a 404, a
412, and a 304 are successes. The local breaker does what the package
does: it opens on the 5th consecutive failure, stays open for 30 seconds,
then `Allow` admits one probe and answers false to everyone else until
the probe reports; a success closes the breaker and a failure reopens
it for another 30 seconds. There is no doubling.
`origo_storage_breaker_state{class}` reports `circuitbreaker.State`: 0
closed, 1 open, 2 half-open, and `OrigoBreakerOpen` (spec 011) fires.

A call the caller's own context ended counts toward neither side of
the breaker: the bucket did not fail it. Only a failure the wrapper's
own deadline or the store itself produced counts, so the cancelled
fetches of a busy node cannot open its read breaker.

`origo_storage_ops_total{op,result}` is recorded on every call, a
refusal under an open breaker included, which counts
`result="error"`; `origo_storage_seconds{op}` is observed only on a
call that ran, so a refusal is counted and not timed. A 304 counts
`result="ok"`, the label vocabulary having no `not_modified` value.

The wrapper records the time it saw the breaker open so it can send
`Retry-After` as the whole seconds of the 30 that remain, at least 1.
`Retry-After` is on every refusal a breaker produced, not on the
receive-pack advertisement alone: the 503 `storage_unavailable` of the
git routes and of the JSON API carries it too, with the remaining open
interval of the breaker that refused. Spec 003's header table defines
it.

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
counts them. Stale serving is for reads only: `Cache.Acquire(ctx, id,
true)` under an open read breaker answers `ErrStorageOpen` at once,
which the handlers map to 503 `storage_unavailable`, because a write
needs a current index object as its base and a stale one would only
lose its round or refuse a reference that did not move; so a push, an
administration operation, or a server-side operation is refused before
it does any work while the read breaker is open, whatever the write
breaker says.

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
breaker opens waits for the breaker: the commit polls `Admits` on the
write breaker every 500 ms for at most 60 seconds, runs as the probe
the first time it answers true, and is refused with the same line
in the sideband when the 60 seconds pass with no admission or the
probe fails; the entry, if written, is an orphan the sweeper removes.
The hook of spec 004 holds git's verdict FIFO for that minute, which
is inside the 5 minute deadline of `receive-pack`. The poll asks
`Admits`, which reports whether the open window has passed without
taking the probe slot, and the commit's first write is then the probe;
polling `Allow` would take the slot and refuse the commit's own write.

### Partial failure

A single key failing is corruption in the log, not an outage: a 404 for
a pack the index names, a 404 for an entry the index names, an entry
whose length or `pack_sha256` differs from the header, an entry that
does not parse, an entry whose pack is not a version 2 packfile, and
an index object that does not parse. Each is one key the log names
that does not hold what the log says. The node increments
`origo_log_integrity_errors_total`, logs the key as the bucket names
it, the full object key, evicts nothing, and refuses the repository:

| Code | Status | Message | Details |
|---|---|---|---|
| `repository_unavailable` | 503 | This repository cannot be served until an operator restores it. Other repositories are not affected. | `key`, `error` |

One case next to those three is not a repository state and does not
take this code: a thin pack whose base object is in no entry the log
holds, which fails materialization with git's `did not receive expected
object` (spec 005, the materialization budget). The base is missing
from the objects the log stores, so the inconsistency is storage-side,
the same class as a 5xx or an unreachable bucket, and the answer is 503
`storage_unavailable` with `details.op: "index-pack"` and git's message
in `details.error`.
`repository_unavailable` says an operator must restore this repository
before it can be served; `storage_unavailable` says the bucket did not
give the node what it asked for, which is what happened. It increments
`origo_log_integrity_errors_total` all the same, because a base that is
in no object the log holds is an integrity error of the log like the
three above, and the node logs it under the repository, git's message
naming the object.

It never rebuilds the log from a local copy. An operator restores the
object from the provider's versioning or from a replica's cache by the
procedure in `docs/operations.md`; the next request materializes again.

### Readiness

`/readyz` (spec 002) stays ready while the write breaker is open, and
while the read breaker is open once the bucket has answered this
replica at least once since it started. The node still serves its warm
repositories stale and refuses every write with a code and a
`Retry-After`, which is the degraded design working; a not-ready node
would leave the Service's rotation and lose those reads for no gain.
A replica the bucket has never answered has nothing warm to serve and
stays unready until a probe succeeds. The `storage` check fails as
before while the bucket is slow or gone and the breaker is still
closed, so a replica is out of rotation from its second failed probe
until the breaker opens.

The readiness listing runs detached from the probe's 2 second budget,
under the storage deadline alone, and is shared by the probes that
arrive while it runs, so the listing is what counts toward the breaker
of a replica no request reaches, and a probe whose own budget ends
first reports the wait.

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
  pack is uploaded, and an `Acquire` for writing answers
  `storage_unavailable` at once while the clone is still served stale
  (proposed: `internal/httpgit`,
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
- A thin pack whose base object is in no entry the log holds is served
  503 `storage_unavailable`, not `repository_unavailable`, with the
  git message in `details.error` (proposed: `internal/repo`,
  `TestThinPackWithoutBaseIsStorageUnavailable`).
- The three fault modes, unreachable, slow, and partial, run in the
  kind stack against MinIO with a fault injector, through node 1 of
  spec 013's ports table with its counters read from that node's
  internal host port: a NetworkPolicy for unreachable,
  `test/e2e/testdata/cut-storage.yaml` applied with
  `cluster.ApplyManifest` of spec 013's `test/e2e/cluster` and removed
  by its cleanup, enforced because the stack's CNI is Cilium (a row of
  spec 013's overlay table; kindnet enforces no policy), with
  `ORIGO_STALE_MAX` set to `30s` and `ORIGO_STORAGE_TIMEOUT` to `5s` on
  the stack's nodes by the overlay, so the read breaker opens in a few
  seconds and the warm clone carries `Origo-Stale` for most of its 30
  seconds and answers 503 after; `test/stubs/slowproxy` for slow, a package this spec
  builds and the `origo-stubs` binary of spec 013 runs: the proxy
  starts only when `-slowproxy-target` (MinIO's Service) is set,
  forwards TCP from its data listener `-slowproxy-data` (default
  `0.0.0.0:8086`, what `ORIGO_S3_ENDPOINT` on every node names) to
  the target, and holds each connection's first bytes for the delay
  its control endpoint `-slowproxy-listen` sets (host port 30085 of
  spec 013's ports table); the `slowproxy` row of spec 013's overlay
  table is this spec's; and a deletion
  through the MinIO host port with the `ORIGO_TEST_S3_ENDPOINT` family
  the job exported for partial, inside the `e2e` job of spec 013
  (proposed: `test/e2e`, `TestClusterDegradedStorage`).

## Outcome

Built on 2026-09-08 in ten commits: the breaker store in
`internal/wal` (`breaker.go`: the local breaker, `BreakerStore`,
`OpError`, `ErrorDetails`), `MemStore.SetLatency` and the log's
`IntegrityError`, a materialization fix, the cache's `Lease` with the
stale bound and the integrity errors, the handlers, the code, the two
variables, the node's wiring and readiness, `test/stubs/slowproxy`,
the overlay, the cluster test, and the documentation.

| Criterion | Test |
|---|---|
| unreachable: a warm repository clones with `Origo-Stale` for `ORIGO_STALE_MAX` and answers 503 after, a cold one 503 at once, a push refused at `info/refs` with the `ERR` pkt-line before any pack, an `Acquire` for writing refused at once while the clone is served stale | `internal/httpgit`, `TestReadBreakerServesStaleThenRefuses`; the cache's half in `internal/repo`, `TestStaleLeaseIsBoundedByStaleMax`; the read API in `internal/api`, `TestReadAPIServesStaleAndRepositoryUnavailable` |
| slow: a call fails after `ORIGO_STORAGE_TIMEOUT`, one call of three attempts counts one failure, the 5th opens, an open breaker refuses with `ErrStorageOpen`, after 30 seconds of fake time one probe runs while a concurrent call is refused, a failed probe reopens for 30 and not 60, a successful probe closes | `internal/wal`, `TestBreakerOpensOnTimeoutsAndRecovers` (`MemStore.SetLatency` for the slow store, `SetFault` for the failed probe), `TestOneCallWithThreeAttemptsIsOneFailure` (the S3 adapter over `s3test` under three 503s), `TestWaitPollsTheWriteBreaker`, `TestGetDeadlineBoundsTheCallNotTheBody`; the first consistent response after the probe in `TestReadBreakerServesStaleThenRefuses` |
| write breaker open: `info/refs?service=git-receive-pack` answers 200 with the `ERR` pkt-line and `Retry-After`, `git push` exits with the sentence in `remote error` having sent no pack; a spooled push is committed when the breaker admits a probe within 60 seconds of fake time and refused in the sideband when it does not, or when the probe fails | `internal/httpgit`, `TestWriteBreakerRefusesBeforeUpload` (protocol versions 0 and 2), `TestSpooledPushWaitsForTheBreaker` |
| a missing pack object is 503 `repository_unavailable` with `details.key`, counts `origo_log_integrity_errors_total`, leaves another repository served | `internal/repo`, `TestMissingPackIsAnIntegrityError` (a pack, a gone entry, an entry whose digest differs, an entry that does not parse); `internal/httpgit`, `TestIntegrityErrorIsRepositoryUnavailable`; the thin-pack rule below in `TestThinPackWithoutBaseIsStorageUnavailable` |
| the stack: unreachable under `cut-storage.yaml`, slow under the proxy's delay through host port 30085, partial after a deletion through the MinIO host port, through node 1 with its counters from its internal host port | `test/e2e`, `TestClusterDegradedStorage`, in the `e2e` job of spec 013 |
| the variables, the wiring, readiness | `internal/config`, `TestLoadAppliesDefaults`, `TestLoadReadsEveryOptionalValue`, `TestStorageTimeoutMustBeAboveZero`; `cmd/origod`, `TestReadyzStaysReadyWhileTheBreakerIsOpen`, `TestStorageTimeoutBoundsTheReadinessListing` |
| the proxy | `test/stubs/slowproxy`, `TestProxyHoldsTheFirstBytesForTheDelay`, `TestSettingTheDelayClosesPooledConnections`, `TestControlRefusesABadDelayAndSetReportsIt`, `TestProxyEndsWithAnUnreachableTargetAndOnClose`; `test/stubs/cmd/origo-stubs`, `TestRunsEveryStubFromFlags` |

Divergences from the first draft and interpretations, each kept,
with the reason; the Design above states each as the rule, so a
reader finds one answer:

- The deadline is per call, not per attempt. `pkg/s3` runs its three
  attempts under the one context it is given and `retry.Policy` has no
  per-attempt deadline, so the wrapper bounds the whole call: a slow
  bucket fails a call after `ORIGO_STORAGE_TIMEOUT`, which is what the
  criterion states, not after three times it as the Design's "about 30
  seconds" says. One call is still one count, and
  `TestOneCallWithThreeAttemptsIsOneFailure` proves the three attempts
  inside it. A per-attempt deadline is a pkg item below.
- A `Get` is bounded to the arrival of the object's headers; its body
  is read under the caller's own context, because an entry of up to
  2 GiB cannot be read inside any per-call deadline. The git deadline
  of the request bounds the read.
- The breaker is local. `pkg/circuitbreaker.Breaker` reads `time.Now`
  and has no `WithClock`, the item the Current state records, so
  `internal/wal/breaker.go` holds a breaker with the package's
  semantics and a clock function, its home named in its comment.
  `circuitbreaker.State` is still the package's type and the gauge's
  values. The wrapper's clock is an interface with `Now()`, as the
  Design says; the deadline runs on real time, so the unit test of the
  timeout uses a short real deadline and the fake clock for the
  window and the stale bound.
- A call the caller's own context ended counts toward neither side of
  the breaker: the bucket did not fail it. The first stack run of the
  slow tier showed why: under 200 concurrent clones a busy node's
  cancelled fetches opened its read breaker and a push through it was
  refused. The readiness listing, which the probe's 2 second budget
  would end before the deadline, runs detached under the storage
  deadline alone and is shared by the probes that arrive while it
  runs, so it is what opens the breaker of a replica no request
  reaches, and a probe whose budget ends first reports the wait.
- Readiness, which the spec leaves open: the `storage` check passes
  while the read breaker is open, once the bucket has answered the
  replica at least once since it started. A replica that serves warm
  repositories stale and refuses writes with the retry sentence is
  serving the degraded design; out of the endpoint list a client would
  see neither. A replica the bucket never answered has nothing warm
  and stays unready until a probe succeeds, which is what the first
  stack run showed: with the proxy not started, three cold nodes went
  ready through the rule alone. The check fails as before while the
  bucket is slow or gone and the breaker is still closed, so a replica
  is out of rotation from its second failed probe until the breaker
  opens, at most about 50 seconds of listings when no request reaches
  it and a few seconds under traffic, then ready again; the cluster
  test waits for node 1's return before it asserts the stale
  responses (`TestReadyzStaysReadyWhileTheBreakerIsOpen`,
  `TestClusterDegradedStorage`). The Design's Readiness section states
  the rule.
- A refused call counts on `origo_storage_ops_total{result="error"}`
  and is not observed on `origo_storage_seconds`: nothing ran. A 304
  counts as `ok`, the table having no `not_modified` value.
- The storage span is named by the op exactly (`get`, `head`, ...),
  as spec 011's Outcome left it to this spec; the transport span of
  `otel.Transport` stays beside it.
- `origo_stale_responses_total` is counted by the cache when it hands
  out a stale lease, one place for the git routes and the read API,
  rather than by each handler as it writes the header.
- The push refused at `info/refs` covers both breakers: the write
  breaker asked before the lease, and the read breaker through the
  lease's refusal or its stale verdict, because a push needs a current
  index object as its base (the decisions table's rule). `Retry-After`
  is the remaining window of the breaker that refused. The same
  `Retry-After` goes on a 503 `storage_unavailable` the read breaker
  refused, on the git routes and the repository API alike.
- The spooled push's wait polls `Admits`, which answers whether the
  window has passed without taking the probe slot, and the commit's
  first write is the probe; polling `Allow` itself would take the slot
  and refuse the commit's own write. The sideband line for a wait that
  passes and for a probe that fails is `storage_unavailable: <the
  sentence>`; the line for a commit the log refuses for another reason
  keeps its text until spec 021's `TestRejectLinesAreTheTableSentences`
  lands.
- A thin pack whose base is in no entry and no pack, spec 005's item,
  is `storage_unavailable` with `details.op: "index-pack"` and the
  git message in `details.error`, not `repository_unavailable`, and
  counts once on `origo_log_integrity_errors_total`: git refuses the
  batch (`repo.IsMissingBase`, checked after `IsCorruption` so a
  damaged local pack that prints the same phrase is still rebuilt),
  nothing is evicted, and the base pushed later completes the history
  (`TestThinPackWithoutBaseIsStorageUnavailable`, the rule the
  verifier of this spec fixed and the coordinator's decision on the
  counter).
- The integrity error covers, beside the three the Design lists, an
  entry the index names that answers 404, one that does not parse, and
  one whose pack is not a version 2 packfile: each is one key the log
  names that does not hold what the log says. The key is the full
  object key, so the operator's restore names it as the bucket does.
- The slow proxy's `SetDelay` closes every forwarded connection, so a
  node's pooled connections meet the delay on their next request; a
  delay on new connections alone would leave a warm node's requests
  unaffected. Its control endpoint answers a GET of its root with the
  state, a PUT of `/delay` with a body `{"delay": "5s"}` sets the
  delay, and a DELETE of `/delay` clears it; the package's `Set`
  drives it from a test.
- The `test-source` component's patch replaces the stubs container's
  argument list, so `-slowproxy-target` is repeated there; the first
  stack run started no proxy and port 30085 never answered.
- On the stack, `ORIGO_STORAGE_TIMEOUT` is `5s` beside the
  `ORIGO_STALE_MAX=30s` spec 013's overlay row already set, so the read
  breaker opens in a few seconds and the warm copy is observed stale
  for most of its 30 seconds; at 10 seconds the breaker would open
  near the bound, and at 2 seconds the slow tier's 8-replica load
  through the proxy exceeded the deadline. `cut-storage.yaml` now also denies the nodes egress
  to the stubs pod's port 8086, the proxy's data listener that
  `ORIGO_S3_ENDPOINT` names, keeping the stubs' other ports open; the
  proxy's data listener has a Service of its own, `slowproxy`, and its
  control port joins the stubs' NodePort Service at 30085, which
  `up.sh` and `TestClusterUpScript` wait for.
- `Cache.Acquire` keeps its signature and calls the new `Cache.Lease`,
  which carries the stale verdict; a reader that calls `Acquire` under
  an open breaker gets the stale copy without the verdict, which the
  handlers never do.
- The LFS handler's storage failures stay `storage_unavailable` in the
  LFS body shape; an integrity error cannot reach it, because it reads
  index objects and its own markers only.

A defect found in existing code and fixed at its root, recorded in
spec 005's Outcome: a worker's failure in the batched materialization
was masked by the indexer's cancelled `index-pack` run, so a missing
entry read as `context canceled`
(`TestWorkerErrorIsNotMaskedByTheCancelledIndexer`).

`latere.ai/x/pkg`, two items:

- `pkg/circuitbreaker` has no clock option: `New(threshold,
  openDuration)` reads `time.Now`, so its window cannot be advanced in
  a test. `WithClock(func() time.Time)` as an `Option` on `New`, the
  way `BackoffConfig.Now` works, would let `internal/wal/breaker.go`
  go.
- `pkg/retry` and `pkg/s3` have no per-attempt deadline: `retry.Do`
  passes one context to every attempt. A `Timeout` on `retry.Policy`,
  applied to each attempt's context, would make "10 seconds per
  attempt" expressible from outside the client.

Both open items are settled by the fifteenth review round and are the
Design's rules above:

- Readiness stays ready while the write breaker is open, and while the
  read breaker is open once the bucket has answered the replica at
  least once since it started, because the node still serves stale
  reads and refuses writes with a code, and a not-ready node would
  leave the Service's rotation and lose those reads. Spec 002 owns
  `/readyz` and its probe row names this spec as the reason.
- `Retry-After` is on every breaker-refused 503, on the git routes and
  on the JSON API alike, not on the receive-pack advertisement alone.
  Spec 003's header table defines the header, with the limits of spec
  012 and the breakers of this spec as its senders. The
  receive-pack advertisement passes the class that refused; every
  other call site asks the read breaker's window whatever class
  refused, so a write the open write breaker refused outside the
  advertisement carries no header while the read breaker is closed.
  The class belongs to the refusing call, and the fix is a builder
  item of spec 012, whose round owns `internal/httpgit` and
  `internal/api`.

The cluster criterion is proved by the `e2e` job of spec 013, which
runs on a tag or a `workflow_dispatch` since the change that took the
cluster tiers off every push; the kind stack cannot run on this
machine (spec 013's Outcome). `TestClusterDegradedStorage` passed in
the dispatched run 34270906850 on `472efa7`, with every other job of
that run green: the gate, the integration tier, the mutation job, the
`e2e-slow` tier, and the up-script check. The `e2e` job itself
reports failure in that run on two scenarios of other specs,
`TestClusterFiveHundredPushesStayUnder64EntriesAnd6Packs` (spec 006)
and `TestClusterNodeRemovalUnderReadLoad` (spec 005), each refused
with 429 `rate_limited` by spec 012's 600 requests per minute per
subject, a load those scenarios exceed by design and spec 012's
Outcome records; nothing of this spec is in that failure. Spec 012 is
fixing that limit, and the next green dispatched run is spec 012's to
cite: this spec's stack sentence keeps run 34270906850 as the run that
proved its own criterion. Six stack
rounds preceded the pass, each a defect found by the job and fixed
at its root: the component's argument list without the proxy target,
readiness that let a replica the bucket never answered into
rotation, the pooled connections that hid a replica leaving the
endpoint list, the cleanup order that deleted the repository the
health wait read, the cancelled fetch masking the worker's error, and
the caller's cancellation counted as the bucket's failure.

One defect of this build, found by review and fixed on 2026-09-08 by
spec 012's builder: `Retry-After` on a 503 `storage_unavailable` was
read from the read breaker whatever operation had been refused, so a
write refused by an open write breaker while the read breaker was
closed carried no header at all, the closed breaker's remaining
window being zero. The class now comes from the operation the
`OpError` names: `wal.ClassOf` over `opClass`, the one place the
mapping lives, which `BreakerStore.call` also reads in place of the
class each method passed. `internal/wal`,
`TestClassOfIsTheOperationsClass`; `internal/api`,
`TestWriteRefusedByTheWriteBreakerCarriesRetryAfter`;
`internal/httpgit`, `TestStorageErrorTakesTheClassOfTheFailedOperation`.

Two defects found by the `v0.1.0` tag's run 34416522013, where
`TestClusterDegradedStorage/unreachable` failed at
`degraded_cluster_test.go:224` with `metrics on 30190: nothing
answered`. The cause is the test's: its wait for node 1's return read
one answered request on the public host port, which kube-proxy serves
for a second after the kubelet flips `Ready` to false, so the run's own
log shows the internal port answering while `origod-0` reported
`Ready=False`, and every assertion after the wait ran inside that
withdrawal lag. The wait now reads the pod's `Ready` condition and both
host ports (`inRotation`), and `nodeMetric` waits for a node out of
rotation instead of failing at once, which is the rule `tryNodeMetric`
already stated. Beside it, readiness keyed on `wal.ErrStorageOpen`
rather than on the read breaker's state, so of the three ways a listing
fails under a tripped breaker it answered ready to one: a half-open
probe's own failure and a probe whose budget ended while the shared
listing ran both took a replica serving stale out of the endpoint list,
once every open window for as long as the outage lasted. The verdict
now reads `BreakerStore.Tripped(ClassRead)`; `cmd/origod`,
`TestReadinessFollowsTheReadBreakerAndNotTheListingsError`.

A third, found by the dispatched run 34430866983 where the first two
made `unreachable` pass: the slow scenario read one node's failed head
operations before and after its slow check and required the delta to be
one, which no delta of that counter can prove. Every `Head` the process
makes lands on it, the marker of an event delivered after the push that
precedes the scenario and the events repair sweep included, and a 30
second cut leaves node 1 with recovery work a 7 second one did not. The
assertion is now on the count of `origo_wal_head_check_seconds` under
`result="error"`, which only `Log.HasIndex` records, so it counts the
currency check the request made and nothing else; the operations delta
is logged beside it.
That one call of three attempts counts one failure stays the
`internal/wal` criterion it always was. The partial scenario's delta of
`origo_log_integrity_errors_total` has the same shape and is left as it
is: nothing on a node writes that counter but the refusal the scenario
causes.
