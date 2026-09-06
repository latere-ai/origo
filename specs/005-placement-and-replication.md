---
title: "Placement and replication: rendezvous hashing, gossip, consistent reads, cache eviction"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
affects: [internal/placement/, internal/repo/, cmd/origod/, deploy/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
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

Spec 004 gives one node correct behaviour: `internal/repo.Cache.Acquire`
runs the `HEAD index/<n+1>` currency check on every open and applies
what the copy lacks. `cmd/origod` opens the gossip socket on
`ORIGO_GOSSIP_ADDR`, reads datagrams, and discards them; `ORIGO_NODE_NAME`
and `ORIGO_GOSSIP_PEERS` are read and unused. `ORIGO_CACHE_BYTES` is
resolved and unused: nothing evicts. `deploy/base` has a Deployment with
2 replicas, `maxSurge: 1, maxUnavailable: 0`, a PodDisruptionBudget with
`minAvailable: 1`, an `emptyDir` of 20 GiB for `/var/lib/origo`, the
headless Service `origod-gossip`, and no HorizontalPodAutoscaler.
`internal/placement` does not exist.

## Design

### Placement

Rendezvous hashing over the live node set: for repository `id` and node
name `n`, `score(n, id)` is the first 8 bytes of `SHA-256(n || "\n" || id)`
as a big-endian integer, and the `k` nodes with the highest scores are
the preferred replicas. `k` is per repository from the authorizer's
`replicas` field (spec 007), default 1, at most the node count. There is
no table; a node joining or leaving moves only the repositories that
hash to it. The live node set is the addresses `ORIGO_GOSSIP_PEERS`
resolves to, refreshed every 10 seconds, mapped to names by the gossip
announcements heard from each address in the last 60 seconds; a node
with a single-node set is preferred for everything.

| Header | Meaning |
|---|---|
| `Origo-Prefer` | on every response of the public listener: the comma separated names of the preferred nodes for the repository, highest score first; a request is served wherever it lands, so the header is a hint for an ingress or a client that can route by pod, never a redirect |

The compaction primary of spec 006 is the first name.

### Gossip

After every index object a node creates, it sends one datagram three
times, 10 ms apart, to every address `ORIGO_GOSSIP_PEERS` resolves to on
the gossip port, with no acknowledgement:

```json
{"v": 1, "node": "<ORIGO_NODE_NAME>", "repo": "<id>", "seq": 1044}
```

A receiver that holds the repository locally below `seq` schedules one
background catch-up (an `Acquire` for writing that returns at once) so
the next request finds the copy current. A datagram that fails to parse,
carries another version, or names a repository the node does not hold is
dropped. Gossip carries no secret and grants nothing: the currency check
still decides, so a forged announcement costs at most one `HEAD`. It is
an optimization: a lost packet costs one `HEAD` that answers 200 instead
of 404 on the next read. `origo_gossip_packets_total{direction}` counts
`sent`, `received`, and `dropped`.

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
| order | least recently acquired first |
| floor | a copy acquired in the last 10 minutes is never evicted for pressure |
| idle | a copy not acquired for 24 hours is evicted whatever the pressure, so idle repositories hold no copy anywhere |
| in use | eviction takes the repository's write lock, so it never removes a copy mid-request or mid-apply |
| what it does | `Cache.Evict`: deletes the directory and the state file; nothing is written back |

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
| pushes to one repository | does not scale; every push to a repository serializes on its create-if-absent commit, whichever node receives it | on the order of ten pushes per second per repository against object storage (phase 1 measured 9.7 per second on MinIO in a laptop virtual machine, spec 004 Outcome); a busier repository needs batching of concurrent pushes into one commit, which is not in this spec |

The autoscaler is a HorizontalPodAutoscaler in `deploy/base`:
`minReplicas: 2`, `maxReplicas: 32`, target CPU utilization 70% of the
request, scale-up stabilization 30 seconds, scale-down stabilization 600
seconds so a burst of clones does not churn the cache. The second signal,
`origo_requests_in_flight` averaged at 64 per pod, needs a metrics
adapter and is added by the example overlays of spec 018 that carry
one. The PodDisruptionBudget keeps `minAvailable: 1`, pod anti-affinity
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

Phase 1 applied entries one at a time with two git subprocesses each and
measured 70.7 seconds for 1 000 entries (spec 004 Outcome). The budget is
10 seconds per 1 000 entries against MinIO on the CI runner, reached by
two changes to `internal/repo.Cache.Apply`: entries are fetched ahead
with 4 concurrent downloads while the previous one is indexed, and the
per-entry `update-ref` is dropped because step 4 of materialization
reconciles the whole map once. Compaction (spec 006) keeps the entry
count under 64 for any repository that is pushed to. A materialization
over 60 seconds is visible on `origo_repo_materialize_seconds` and the
alert of spec 011.

## Not in this spec

Batching concurrent pushes into one commit. Routing by `Origo-Prefer` at
the ingress. Deleting the cache on shutdown.

## Acceptance criteria

- With 3 nodes and `replicas = 1`, a clone through any node answers
  `Origo-Prefer` with the same first name on every node, and a second
  clone sent to that node leaves `origo_repo_materialized_total` on it
  unchanged (proposed: `internal/placement`, `TestRendezvousAgreesAcrossNodes`;
  `test/e2e`, `TestPreferredNodeIsWarm`).
- A push on node A is visible to a fetch on node B within 100 ms with
  gossip, and within one request with gossip disabled (`ORIGO_GOSSIP_PEERS`
  unset), measured as the time until B's `HEAD` answers 200 (proposed:
  `test/e2e`, `TestGossipShortensTheCatchUp`).
- Filling the cache past `ORIGO_CACHE_BYTES` with 20 repositories evicts
  the least recently acquired ones, never one holding a lock, and a
  subsequent read materializes an evicted one with identical
  `rev-list --all` (proposed: `internal/placement`, `TestEvictionIsLRUAndNeverInUse`).
- A repository not acquired for 24 hours is evicted at the next evictor
  run with a fake clock (proposed: `internal/placement`, `TestIdleEviction`).
- Removing one of 3 nodes during a load of 50 clones per second causes
  no failed request (proposed: `test/e2e`, `TestNodeRemovalUnderReadLoad`).
- Under a synthetic read load of 200 clones of a 10 MiB repository,
  going from 2 to 8 replicas raises clones per second at least 3 times
  with no push failure; the HPA reaches 4 replicas within 60 seconds of
  CPU crossing the target and returns to 2 after the 600 second window
  (proposed: `test/e2e` on the kind stack of spec 013,
  `TestReplicasScaleReads`, `TestAutoscalerFollowsLoad`).
- A drain during 100 concurrent pushes loses none: every push is either
  acknowledged and in the newest index or refused with
  `storage_unavailable` and absent (proposed: `test/e2e`,
  `TestDrainLosesNoPush`).
- Materializing 1 000 entries from an empty disk finishes under 10
  seconds against MinIO on the CI runner (`test/e2e`, `TestMeasure`,
  `materialize 1000 entries`, with the threshold asserted).
