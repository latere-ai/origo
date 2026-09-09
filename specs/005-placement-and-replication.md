---
title: "Placement and replication: rendezvous hashing, gossip, consistent reads, cache eviction"
status: complete
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
  - specs/013-test-stubs-and-kind-overlay.md
affects: [internal/placement/, internal/repo/, internal/wal/, internal/config/, cmd/origod/, deploy/, deploy/examples/kind/, Makefile, test/e2e/]
effort: medium
created: 2026-09-06
updated: 2026-09-08
author: changkun
---

# Placement and replication

## Overview

Any node can serve any repository, because the log is the truth. This
spec is about making the common case fast: which node a request should
prefer so the local copy is warm, how nodes learn about new entries
without a round trip, how a read is proven current, and when a local copy
is thrown away. It also fixes what scales with replicas and what does
not, the autoscaler, the disk, and the materialization budget.

## Current state

Built on 2026-09-08 as the Design describes, with the divergences the
Outcome records. Before it, spec 004 gave one node correct behaviour:
`internal/repo.Cache.Acquire` ran the `HEAD index/<n+1>` currency check
on every open and applied what the copy lacked; `cmd/origod` read the
gossip socket and discarded every datagram; `ORIGO_NODE_NAME` and
`ORIGO_GOSSIP_PEERS` were read and unused, `ORIGO_GOSSIP_SECRET` not
read, `ORIGO_CACHE_BYTES` resolved and unused; `deploy/base` had no
HorizontalPodAutoscaler and `internal/placement` did not exist.

## Design

### Placement

Rendezvous hashing over the live node set: for repository `id` and node
name `n`, `score(n, id)` is the first 8 bytes of `SHA-256(n || "\n" || id)`
as a big-endian integer, and the `k` nodes with the highest scores are
the preferred replicas. `k` is per repository from the authorizer's
`replicas` field (spec 007), default 1, at most the node count. A
request that made no authorizer call has no `replicas` value: a
repository-bound token (spec 007) skips the authorizer, and such a
request uses `k` = 1, so the first name, which is what the compaction
primary and the header's first entry need, is the same whatever the
credential. There is no table; a node joining or leaving moves only the
repositories that hash to it.

Membership is by heartbeat. Every 10 seconds a node sends one datagram
in the gossip format below with an empty `repo` and `seq` 0 to every
peer address, the resolution refreshed every 10 seconds.
`ORIGO_GOSSIP_PEERS` (spec 002) takes two forms: a comma separated list
of `host:port` entries, each sent to as given, or one DNS name, which
resolves to every node's address on the port of `ORIGO_GOSSIP_ADDR`;
a value containing a comma or a port is the list, anything else the
name. Kubernetes uses the name (the headless Service `origod-gossip`),
and two local nodes use two entries on loopback with distinct ports,
which is how the end-to-end harness starts the two nodes of the tests
below, each with its own `ORIGO_GOSSIP_ADDR` on `127.0.0.1` and the
other's address in its peer list. The live node set is the names heard, by heartbeat or by an
announcement that carried a valid MAC, in the last 60 seconds, plus the
node's own name, which is always in the set. A node with a single-node
set is preferred for everything. The set is what placement, the
compaction primary (spec 006), the import lease (spec 019), the orphan
sweep (spec 019), and the event repair sweep (spec 008) read; each of
those also needs the time a node was last heard, which the set records
per name.

| Header | Meaning |
|---|---|
| `Origo-Prefer` | on every response to a request that names a repository by its id, whatever the status: the comma separated names of the preferred nodes for that repository, highest score first, with `k` = 1 on a refused request because it made no authorizer call. Absent on a response that names no repository (`POST /v1/repos`, the probes, `GET /.well-known/jwks.json`), and absent on a 401 or 403 to a request that names a repository by owner and slug, whether or not the name resolved: a header only on a resolved name would tell a refused caller the repository exists, which spec 007's authorization before lookup forbids, and the score needs the id. A request is served wherever it lands, so the header is a hint for an ingress or a client that can route by pod, never a redirect |

The compaction primary of spec 006 is the first name.

### Gossip

After every index object a node creates, it sends one datagram three
times, 10 ms apart, to every peer address `ORIGO_GOSSIP_PEERS` names or
resolves to, with no acknowledgement. A datagram is
a 32 byte tag followed by a payload: the tag is the HMAC-SHA256 of the
payload bytes under `ORIGO_GOSSIP_SECRET` (spec 002: required whenever
`ORIGO_GOSSIP_PEERS` is set, the same value on every node; a single node
with no peers runs with neither, which is what `make dev` does, while
the end-to-end harness sets both for its two-node tests from this spec
on (today it sets `ORIGO_GOSSIP_ADDR` only), the bootstrap
Secret template of `deploy/bootstrap` carries the key, and the kind
overlay of spec 013 sets a fixed value), and the payload is one JSON
object:

```json
{"v": 1, "node": "<ORIGO_NODE_NAME>", "repo": "<id>", "seq": 1044, "at": "<RFC 3339>"}
```

The same payload with `repo` empty and `seq` 0 is the heartbeat above.
A receiver recomputes the tag over the payload and compares it in
constant time; a datagram shorter than 33 bytes, or whose tag differs,
is dropped before the payload is parsed and counted on
`origo_gossip_packets_total{direction="dropped"}`, so a sender without
the secret cannot join the live set, cannot keep a dead name in it, and
cannot trigger a catch-up (spec 016, the membership forgery row). `at`
more than 60 seconds from the receiver's clock is dropped the same way,
which bounds a replay to the window a live sender would fill anyway. A
valid heartbeat records the sender's name and the time and does nothing
else. A valid announcement whose receiver holds the repository locally
below `seq` schedules one background catch-up (an `Acquire` for writing
that returns at once) so the next request finds the copy current, at
most one catch-up per repository per second whatever the datagram rate,
which bounds what a flood from a node that holds the secret can cost
(spec 016). A datagram that fails to parse, carries another version, or
names a repository the node does not hold is dropped and counted the
same way. The MAC authenticates membership and nothing more: gossip
grants no read and no write, the currency check still decides what is
served, and a lost packet costs one `HEAD` that answers 200 instead of
404 on the next read. `origo_gossip_packets_total{direction}` counts
`sent`, `received` (valid), and `dropped`.

The loop starts in two calls: `Bind` takes the socket the node opened
and resolves the peer addresses before anything is served, and `Run`
serves the datagrams. Announcing from the first commit would otherwise
race the loop's start.

### Consistent reads

Every request that serves repository state begins with the currency
check of spec 004 (`HEAD index/<n+1>`, timed on
`origo_wal_head_check_seconds`). A 404 serves at once; a 200 catches up
first. The cost of the 404 is the floor for any request. A node never
serves from a local copy without the check, except under the read
breaker of spec 015, and the index object it holds in memory is only the
base of the next commit, never a substitute for the check.

### Cache eviction

`internal/placement` runs an evictor every minute over the repositories
under `<ORIGO_DATA_DIR>/repos/`, with the bytes of each directory
measured after every apply and walked once at start-up:

| Rule | Value |
|---|---|
| ceiling | `ORIGO_CACHE_BYTES` |
| order | least recently acquired first; a copy found on disk at start-up, which no request has acquired in this process, counts as acquired at the process start time, so a restarted node keeps its warm set for the floor below and then ranks it by use |
| floor | a copy acquired in the last 10 minutes is never evicted for pressure |
| idle | a copy not acquired for 24 hours is evicted whatever the pressure, so idle repositories hold no copy anywhere |
| in use | eviction takes the repository's write lock with `TryLock` and skips the copy when the lock is held, so it never removes a copy mid-request or mid-apply and never waits behind one; a copy in use was acquired recently by definition, so the floor covers it and the next run takes it |
| what it does | `Cache.TryEvict`: deletes the directory and the state file under the lock it took; nothing is written back |

`origo_cache_bytes` and `origo_cache_repos` are gauges;
`origo_evictions_total{reason}` counts `pressure` and `idle`.

### Node lifecycle

A node is ready when it can list the bucket and write its disk (spec
002). Draining is the shutdown of spec 002: unready, 3 seconds, then 60
seconds of grace for in-flight requests; a push whose entry is written
completes its commit or leaves an orphan the sweeper removes. The cache
is not deleted on shutdown: a pod that restarts on the same volume is
warm, and a pod that moves starts cold and warms as requests arrive. A
deploy therefore costs one materialization per repository per replaced
pod, bounded by the pack download rate, and never blocks writes.

### Scaling

Nodes are interchangeable, so scaling is a replica count and nothing
else.

| Load | Scales with replicas | Bound |
|---|---|---|
| reads: clone, fetch, the read API, archive | linearly; every node serves any repository after one materialization | the bucket's request rate and each node's disk |
| pushes to different repositories | linearly; commits to different repositories never contend | same |
| pushes to one repository | does not scale; every push to a repository serializes on its create-if-absent commit, whichever node receives it | on the order of ten pushes per second per repository against object storage (phase 1 measured 9.7 per second on MinIO in a laptop virtual machine, spec 004 Outcome); a busier repository needs batching of concurrent pushes into one commit, which is out of scope for every spec in the deck and listed as a future spec in `specs/README.md` |

The autoscaler is a HorizontalPodAutoscaler in `deploy/base`:
`minReplicas: 2`, `maxReplicas: 32`, target CPU utilization 70% of the
request, scale-up stabilization 30 seconds, scale-down stabilization 600
seconds so a burst of clones does not churn the cache. A CPU target
needs the resource metrics API, which the kind overlay of spec 013
provides with `metrics-server`, one row of its overlay table; this spec
adds to that overlay the patch that sets the scale-down window to 60
seconds, so the autoscaler test below leaves a small cluster for the
next test inside the job budget. That overlay runs the nodes as the
StatefulSet `origod` so each pod has a host port of its own (spec 013,
ports table), and patches the autoscaler's `scaleTargetRef` to it
(`patches/hpa-statefulset.yaml`); the base keeps the Deployment, and
`deploy/prod` carries the autoscaler at 2 to 32. The overlay's pods
request 50m CPU, not the base's 250m: the runner has one kind node, and
at 250m a fourth replica cannot schedule, so neither the 4 nor the 8
replica count of the criteria below could exist there. The 70% target
is of that request, which is what the clone load drives past. CPU is the only signal: the base and
every example overlay of spec 018 scale on it and install no metrics
adapter. `origo_requests_in_flight` (spec 011) stays defined for
dashboards and is not an autoscaler input; `docs/operations.md` says
the same. The PodDisruptionBudget keeps `minAvailable: 1`, pod anti-affinity
prefers spreading replicas across nodes, and the rolling update stays
`maxSurge: 1, maxUnavailable: 0`.

### Disk

Each pod has one local volume for the cache: `emptyDir` with a
`sizeLimit` in `deploy/base` (20 GiB), replaced in an overlay by a
generic ephemeral volume on a fast storage class or a local volume where
the platform offers one. Never a network file system: git on a network
mount is the one configuration that makes every operation slow.
`ORIGO_CACHE_BYTES` defaults to 80% of the file system holding
`ORIGO_DATA_DIR`. A pod whose volume is lost restarts cold and is
correct; it is only slower until warm.

### Materialization budget

Phase 1 applied entries one at a time, one `GET`, one `index-pack`, and
one `update-ref` each, and measured 70.7 seconds for 1 000 entries,
about 70 ms per entry, on a laptop against MinIO (spec 004 Outcome).
`internal/repo.Cache.Apply` changes in two ways: what runs concurrently
and how many `index-pack` subprocesses run at all.

One subprocess per entry cannot meet the budget on the pattern every
push produces, a chain of thin packs each based on the entry before it.
Measured on 1 000 such entries: one worker 53.8 s, four workers 52.5 s,
eight workers 48.5 s, with 999 of the 1 000 first runs failing on a
missing base and running again. Entry i+1 cannot index before entry i
has landed, so the chain serializes the work whatever the worker count,
and the retry doubles it.

The packfile format allows one subprocess per batch instead: a 12 byte
header with an object count, self-delimiting objects whose `OFS_DELTA`
offsets are relative and whose `REF_DELTA` bases are named by hash, and
a SHA-1 trailer. So the workers, 4 by default, fetch, verify (length
and `pack_sha256`), and spool concurrently, and one indexer joins
consecutive spooled packs, at most 256 entries or 256 MiB per batch,
into one pack for one `git index-pack --fix-thin --strict` run, the
batches in sequence order. Every base is then in the batch or already
in the store, there is no retry, and a cold copy of 1 000 pushes holds
4 packs rather than 1 000, which every later git command opens. Two
entries can carry one object, which `index-pack --strict` refuses in
one pack with `REF_DELTA already resolved (duplicate base)`: a joined
batch it refuses is indexed one pack at a time in sequence order, each
with `--fix-thin`, which is what one run per entry produces. A pack
whose base is in no entry at all fails the run with git's `did not
receive expected object`; that is an integrity error of the log, served
`storage_unavailable` (spec 015).

The per-entry `update-ref` is dropped: references are applied once at
the end by step 4 of materialization, which reconciles the whole map to
the index. Measured, 1 000 entries onto an empty disk take 0.65 s, from
42 s with one `index-pack` per entry and 70.7 s in phase 1. The
acceptance threshold on the CI runner is 30 seconds for 1 000 entries,
asserted by a plain end-to-end test that runs on every push, not by the
measurement run `ORIGO_E2E_MEASURE` selects, which only prints.
Compaction
(spec 006) keeps the entry count under 64 for any repository that is
pushed to, so the budget matters on a cold node after a compaction gap,
not on every request. A materialization over 60 seconds is visible on
`origo_repo_materialize_seconds` and the alert of spec 011.

## Not in this spec

Batching concurrent pushes to one repository into one commit: out of
scope for every spec in the deck, and the future spec `specs/README.md`
lists under Later. Routing by `Origo-Prefer` at the ingress. Deleting
the cache on shutdown.

## Acceptance criteria

- With 3 nodes and `replicas = 1`, a clone through each of nodes 1, 2,
  and 3 of spec 013's ports table (their public host ports) answers
  `Origo-Prefer` with the same first name, and a second clone sent to
  that node's own public host port leaves `origo_repo_materialized_total`,
  read from that node's internal host port, unchanged (proposed:
  `internal/placement`, `TestRendezvousAgreesAcrossNodes`; `test/e2e`,
  `TestClusterPreferredNodeIsWarm`).
- A push acknowledged on node A is returned by a fetch on node B, the
  fetch a `git ls-remote` with protocol version 1 so it is one request
  and one currency check (a version 2 `ls-remote` sends an `ls-refs`
  POST after the advertisement, a second check): with gossip, the test
  waits until B's `origo_repo_entries_applied_total` rose by one, which
  is the background catch-up the announcement scheduled, then reads on
  B and finds the pushed commit with B's
  `origo_wal_head_check_seconds{result="404"}` count risen by one while
  its `result="200"` count did not, so the catch-up happened before the
  request; with gossip disabled (`ORIGO_GOSSIP_PEERS` unset) the first
  read on B returns the pushed commit with the `result="200"` count
  risen by one and the `result="404"` count by one beside it, because a
  check that finds a newer index walks `HEAD index/<m+1>` forward until
  a 404. The test also records the time from A's acknowledgement until
  B's counter rose (proposed: `test/e2e`,
  `TestE2EGossipShortensTheCatchUp`, two nodes of the one-node run's
  harness).
- Three nodes exchanging heartbeats agree on the live set within 60
  seconds of a node joining and drop a node 60 seconds after its last
  datagram with a fake clock, and a node's own name is in its set with
  no peers (proposed: `internal/placement`, `TestMembershipByHeartbeat`).
- A datagram with no tag, a tag under another secret, a tag over a
  changed payload, or an `at` 61 seconds old is dropped and counted on
  `origo_gossip_packets_total{direction="dropped"}` without entering
  the live set or scheduling a catch-up, and the same payload under the
  right secret is accepted (proposed: `internal/placement`,
  `TestGossipDropsABadMAC`).
- 10 000 gossip datagrams for one repository in one second cause at most
  one catch-up on the receiver (proposed: `internal/placement`,
  `TestGossipCatchUpIsRateLimited`).
- Filling the cache past `ORIGO_CACHE_BYTES` with 20 repositories evicts
  the least recently acquired ones, never one holding a lock, and a
  subsequent read materializes an evicted one with identical
  `rev-list --all` (proposed: `internal/placement`, `TestEvictionIsLRUAndNeverInUse`).
- A repository not acquired for 24 hours is evicted at the next evictor
  run with a fake clock (proposed: `internal/placement`, `TestIdleEviction`).
- Deleting node 2 of spec 013's ports table with
  `cluster.DeletePod("origod-1")` (that spec's `test/e2e/cluster`
  helper) during a load of 50 clones per second through the balanced
  port causes no failed request, and the replacement pod is ready
  before the test ends (proposed: `test/e2e`,
  `TestClusterNodeRemovalUnderReadLoad`).
- Under a synthetic read load of 200 clones of a 10 MiB repository on
  the kind stack of spec 013, with a push every second beside it, no
  push fails and no clone fails at 2, 4, and 8 replicas; each
  replica count is set with `cluster.ApplyManifest` of
  `test/e2e/testdata/hpa-<n>.yaml`, a HorizontalPodAutoscaler named
  `origod` with `minReplicas` and `maxReplicas` both `<n>` (this spec
  writes `hpa-4.yaml` and `hpa-8.yaml`; `hpa-2.yaml` is spec 013's,
  the fixture of its helper test), which
  replaces the overlay's autoscaler of the same name rather than
  adding a second one, so `cluster.HPAStatus("origod")` reads it and
  `cluster.Apply` of the overlay restores the original; each count is
  confirmed with `cluster.HPAStatus("origod")` before the load starts; the three
  clones-per-second figures are recorded in the test output as
  measurements and nothing about them is asserted, because the
  runner's CPU, not the design, bounds them. Monotonicity over the
  three counts is asserted only in `TestMeasure` under
  `ORIGO_E2E_MEASURE=1`, run against the stack, which no job selects.
  The autoscaler test applies
  `test/e2e/testdata/hpa-scale.yaml`, this spec's third fixture: the
  autoscaler named `origod` free between 3 and 8 replicas on CPU, with
  a scale-up window of 0 and the overlay's 60 second scale-down window.
  It is needed because spec 013 holds the overlay's own autoscaler at 3
  replicas, so the three host ports always answer and the other cluster
  tests see three pods, and an autoscaler with `maxReplicas: 3` can
  never reach 4. The test then drives CPU past the target and reads
  `cluster.HPAStatus("origod")` until it reports 4 replicas, within 60
  seconds of the crossing, and restores the overlay's own autoscaler
  with `cluster.Apply("deploy/examples/kind")`; scale-down is not
  asserted, the overlay's 60 second window only keeps the cluster
  small for the next test (proposed: `test/e2e` in the `e2e-slow` job
  of spec 013, `TestSlowReplicasScaleReads`, `TestSlowAutoscalerScalesUp`).
- A drain during 100 concurrent pushes loses none: every push is either
  acknowledged and in the newest index, or refused with
  `storage_unavailable` and absent from it, or failed on the connection
  after the listeners closed and absent from it, the third outcome
  because a client that arrives after the drain delay is not refused by
  the node but by its closed socket. The newest index sequence equals
  the number acknowledged, and how the failed ones failed is recorded
  in the test output (proposed: `test/e2e`, `TestE2EDrainLosesNoPush`).
- Materializing 1 000 entries from an empty disk with 4 workers
  finishes under 30 seconds against MinIO on the CI runner, and a chain
  of thin entries, each based on the one before it, lands in the joined
  batches: the packs on disk are one per batch, the history equals the
  source, `git fsck` passes, and a batch carrying one object twice
  falls back to one pack per entry. The
  fixture is written by the harness through `Log.Commit`, one entry per
  commit with a pack the harness builds, not by `git push`, under a
  budget of its own of 5 minutes, because phase 1 measured 98 seconds
  for the same 1 000 entries through the git client (spec 004 Outcome)
  (proposed: `test/e2e`, `TestE2EMaterializeThousandEntriesUnderBudget`,
  a plain test of the one-node run that runs on every push and shares
  its fixture builder with `TestMeasure`; `internal/repo`,
  `TestConcurrentWorkersApplyThinEntries` and
  `TestBatchWithADuplicateObjectFallsBackToOnePackPerEntry`).

## Outcome

Built on 2026-09-08 in twelve commits: the `result` label and the
`OnCommit` callback on the log, `ORIGO_GOSSIP_SECRET`, the guard's
decision, the cache's copies and workers, the lost-sequence rebuild,
`internal/placement`, `Origo-Prefer` on both handlers, the node
wiring, the manifests, the batched indexer, the one-check catch-up,
and the tests.

| Criterion | Test |
|---|---|
| placement agrees across nodes; the preferred node is warm on the stack | `internal/placement`, `TestRendezvousAgreesAcrossNodes`; `test/e2e`, `TestClusterPreferredNodeIsWarm` (`e2e` job) |
| a push on A is returned by a fetch on B, with gossip before the request and without it on the first fetch; the catch-up time is recorded | `test/e2e`, `TestE2EGossipShortensTheCatchUp` (two nodes of the one-node harness, `integration` job) |
| three nodes agree on the live set, a quiet node is dropped after 60 seconds, a node's own name is in its set | `internal/placement`, `TestMembershipByHeartbeat`; the peer forms in `TestGossipResolvesTheDNSForm` |
| a bad MAC, a changed payload, or an `at` 61 seconds old is dropped and counted, the same payload under the right secret accepted | `internal/placement`, `TestGossipDropsABadMAC` |
| 10 000 datagrams for one repository in one second cause at most one catch-up | `internal/placement`, `TestGossipCatchUpIsRateLimited` |
| eviction is least recently acquired first, never a copy in use or under the floor, and an evicted copy materializes again | `internal/placement`, `TestEvictionIsLRUAndNeverInUse` |
| a copy idle for 24 hours is evicted | `internal/placement`, `TestIdleEviction` |
| deleting node 2 under 50 clones per second fails no request | `test/e2e`, `TestClusterNodeRemovalUnderReadLoad` (`e2e` job) |
| 200 clones of 10 MiB with a push every second at 2, 4, and 8 replicas fail nothing; the autoscaler reaches 4 replicas within 60 seconds of the crossing | `test/e2e`, `TestSlowReplicasScaleReads`, `TestSlowAutoscalerScalesUp` (`e2e-slow` job); monotonicity in `TestMeasure` |
| a drain during 100 concurrent pushes loses none | `test/e2e`, `TestE2EDrainLosesNoPush` |
| 1 000 entries materialize under 30 seconds; a thin entry over the previous one lands | `test/e2e`, `TestE2EMaterializeThousandEntriesUnderBudget`; `internal/repo`, `TestConcurrentWorkersApplyThinEntries` |

Measurements on an Apple silicon laptop against MinIO in a podman
virtual machine: node B applied an announced entry 23 ms after node A
acknowledged the push; 100 concurrent pushes into a draining node were
all acknowledged, none lost; 1 000 entries materialized onto an empty
disk in 0.65 s, from 42 s with the per-entry design below and 70.7 s
in phase 1.

Divergences from the first draft, each kept and now the rule the Design
states:

- The materialization budget is met by batching, not by the first
  draft's concurrent `index-pack` per entry, which cannot meet its own
  budget on the chained thin packs every push produces: measured, one
  worker 53.8 s, four workers 52.5 s, eight workers 48.5 s on 1 000
  such entries, with 999 of the 1 000 first runs failing on a missing
  base and running again. The Materialization budget section states the
  batched design and the figures.
  `TestConcurrentWorkersApplyThinEntries` keeps its name and asserts
  the batching with a test-set bound of 5 entries per batch. The
  duplicate-object fallback was found by the 8-replica load on the
  stack (`REF_DELTA already resolved (duplicate base)`) and is held by
  `TestBatchWithADuplicateObjectFallsBackToOnePackPerEntry`.
- The `Origo-Prefer` header on a refused request: on the id form the
  header is present on a 403 with k = 1, because the id is the path's;
  on the name form it is absent on a 403 whether or not the name
  resolved. The first draft covered the unresolved case only; the
  resolved-and-refused case follows from spec 007's authorization
  before lookup, and the Header table now states both.
- A datagram that names a repository the node does not hold is counted
  `dropped`, as the Design says, and still records its sender in the
  live set, because the MAC was valid and the Design's live set is
  "the names heard by heartbeat or by an announcement that carried a
  valid MAC".
- The evictor skips a copy whose write lock is held rather than wait
  behind the request: `Cache.TryEvict` takes the lock only when it is
  free, which the eviction table states.
- The gossip loop is split into `Bind` and `Run`, which the Gossip
  section states.
- `ORIGO_GOSSIP_SECRET` shorter than 32 bytes is refused whenever it
  is set, peers or not, as a malformed value in the one start-up
  message.
- `TestE2EGossipShortensTheCatchUp` measures the counts over a
  `git ls-remote` with protocol version 1, and a check that finds a
  newer index walks forward until a 404, so on the node without gossip
  the 404 count rises by one beside the 200. Both are in the criterion.
- `TestSlowAutoscalerScalesUp` needed a fixture the first draft did not
  name: the overlay's own autoscaler is held at 3 replicas by spec 013,
  and one with `maxReplicas: 3` can never reach 4. It scales under
  `test/e2e/testdata/hpa-scale.yaml` applied with
  `cluster.ApplyManifest`, which the criterion states. The overlay's
  autoscaler is the base's patched (`patches/hpa-statefulset.yaml`), as
  spec 013's Outcome foresaw.
- `TestE2EDrainLosesNoPush` asserts what the criterion states, that an
  acknowledged push is in the newest index and a failed one is absent,
  and records how the failed ones failed; on this machine the node
  acknowledged all 100 inside its grace period, so neither failing
  branch ran. The criterion names the third outcome, a connection
  refused after the listeners closed, because the first draft named two
  and the test saw that one on the runner.
- The base's `HorizontalPodAutoscaler` names the Deployment; the
  overlay's patch names the StatefulSet. `deploy/prod` therefore
  carries the autoscaler at 2 to 32, which the Scaling section states.
- The kind overlay's pods request 50m CPU, not the base's 250m: the
  first stack run left `origod-3` unschedulable, `Insufficient cpu`, on
  the runner's one kind node, so neither 4 nor 8 replicas could exist
  there. The Scaling section states the request and what the 70% target
  is of.
- `TestClusterPreferredNodeIsWarm` reads the three headers until they
  agree, for up to the 60 seconds of the membership criterion, because
  the test before it replaces two pods and a node that joined a second
  earlier has not heard the others yet.

Two defects found in existing code, fixed at the root with a failing
test each and recorded in spec 004's Outcome: a warm copy whose bucket
was reset answered `storage_unavailable`, and is now rebuilt from the log
(`TestLostSequenceRebuildsFromTheLog`); a reader that found a newer
index ran the whole currency check again under the write lock, a
second `HEAD` and a second `GET` of the index it had read
(`TestReaderUpgradeAppliesTheIndexItRead`).

Items for other specs:

- Spec 006, settled: compaction never deletes an index object, so
  `HEAD index/<n+1>` stays the currency check and a 404 stays proof
  that a copy is current. Truncation removes folded entries and
  superseded packs only. An index object is one small object per push,
  which is the cheaper side of the trade. Spec 006 states the rule and
  its builder implements it; today `internal/wal/sweep.go` still
  deletes index objects below `compacted_through`, which nothing
  reaches because `compacted_through` is always 0 until 006 lands.
- Spec 015, settled: a thin pack whose base is in no entry fails the
  batch with git's `did not receive expected object` and is served
  `storage_unavailable`, not `repository_unavailable`: a base the log
  does not hold is a storage-side inconsistency, not a state of the
  repository. Spec 015 holds it with
  `TestThinPackWithoutBaseIsStorageUnavailable`.
- Spec 011: `origo_wal_head_check_seconds` carries `result`;
  `origo_gossip_packets_total`, `origo_evictions_total`,
  `origo_cache_bytes`, and `origo_cache_repos` were registered by the
  packages that record them until `internal/metrics/register.go`
  landed. Done on 2026-09-08 with spec 011: all four are rows of that
  spec's table, the gossip and eviction counters are handles the
  packages take, and the evictor binds the two gauges to the cache.
- `latere.ai/x/pkg`: nothing new was needed; `pkg/cache` is the
  catch-up rate limiter and `pkg/wait` the evictor's ticker.

A defect in the batched materialization found and fixed by spec 015
on 2026-09-08: a worker's failure cancelled the indexer, which may
have been running `index-pack` on an earlier batch, and the caller saw
that run's `context canceled` instead of the worker's error, so a
missing entry read as an outage rather than the integrity error it
is. `applyEntries` now reports the first worker error over the
cancelled run (`TestWorkerErrorIsNotMaskedByTheCancelledIndexer`).

The cluster criteria are proved by the `e2e` and `e2e-slow` jobs of
spec 013; the kind stack cannot run on this machine (spec 013's
Outcome). Both jobs are green on `main` at `ee6a5c6` (run
34241628620), with every other job, and the spec is complete. Four
rounds on the stack preceded it, each a defect found by the jobs and
fixed at its root: `up.sh` waited for the identity path through the
balanced port alone (spec 013's Outcome records the fix); the
overlay's pods requested the base's 250m CPU and a fourth could not
schedule; a node that had joined a second earlier answered a
different `Origo-Prefer` until it heard the others; and a joined
batch with one object in two entries was refused by `index-pack`.
On the runner, the read load measured 3.4 to 9.7 clones per second
of the 10 MiB repository at 2 replicas and 4.6 to 10.2 at 4, with
the push every second beside it, and 8 replicas scheduled once the
request was lowered; the figures vary with the runner and nothing
about them is asserted, as the criterion says.

A race in `TestMembershipByHeartbeat`, seen on the race gate of a
push run and fixed by spec 016's build on 2026-09-09: the third node
started before the clock advanced, so its first heartbeat could reach
node 1 at the old time and `LastHeard` stayed there. The clock now
moves before the third starts, so every heartbeat of that round is
heard at the new time.

A second race, in `cmd/origod`'s `TestGossipWiresTwoNodes`, seen on
the test gate of a push run and fixed the same day: the test reserved
node B's port by binding and closing a socket before node A started,
so the kernel could hand the same port to A's own gossip socket, whose
peer list then named itself and whose own heartbeat counted as a
packet received from B while B had sent nothing. The reservation is
held until A has bound, and both counters are waited for, because the
send is counted after the datagram left.

`TestMembershipByHeartbeat` failed a third time, on the race gate of
the push run 34343994354, at its first assertion and not on a clock:
the two nodes had ten seconds to agree, which is `HeartbeatEvery`
itself, so one datagram the loopback socket lost put the next
heartbeat exactly on the deadline. A membership test that waits on
delivery has that coin flip in it whatever the wait. The test now
runs the three nodes over a synchronous in-memory `net.PacketConn`
written in the test file: `WriteTo` puts the datagram on the
addressed socket's queue before it returns, the test drains every
queue into `Gossip.Handle` after each round of heartbeats, and every
assertion reads the set with no wall clock between the send and the
read. It asserts what it asserted before, the live set of two nodes
and then three, `LastHeard` for a peer and for the node itself, the
drop at 60 seconds and not at 59, the own name of a node with no
peers, and the refusal of a datagram to a node with no secret, and it
runs 20 times under `-race` in under a second. The loopback socket
and the read loop of `Run` stay covered by
`TestGossipResolvesTheDNSForm` and `TestAnnounceReachesEveryPeer`,
which bind real sockets, and by
`TestGossipWiresTwoNodes` in `cmd/origod`, which runs two nodes over
loopback; the package's coverage is unchanged at 92.6%.
