---
title: "Placement and replication: rendezvous hashing, gossip, consistent reads, cache eviction"
status: validated
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

Spec 004 gives one node correct behaviour: `internal/repo.Cache.Acquire`
runs the `HEAD index/<n+1>` currency check on every open and applies
what the copy lacks. `cmd/origod` opens the gossip socket on
`ORIGO_GOSSIP_ADDR`, reads datagrams, and discards them; `ORIGO_NODE_NAME`
and `ORIGO_GOSSIP_PEERS` are read and unused; `ORIGO_GOSSIP_SECRET` is
not read. `ORIGO_CACHE_BYTES` is resolved and unused: nothing evicts. `deploy/base` has a Deployment with
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
| `Origo-Prefer` | on every response to a request that names a repository, whatever the status, with one exception: the comma separated names of the preferred nodes for that repository, highest score first; absent on a response that names none (`POST /v1/repos`, the probes, `GET /.well-known/jwks.json`) and absent on a 401 or 403 to a name that did not resolve, because the score needs the id and a refused caller is told nothing about it (spec 007, "Authorization before lookup"); a request is served wherever it lands, so the header is a hint for an ingress or a client that can route by pod, never a redirect |

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
ports table), and patches the autoscaler's `scaleTargetRef` to it; the
base keeps the Deployment. CPU is the only signal: the base and
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
`internal/repo.Cache.Apply` changes in two ways. Entries are fetched and
indexed by concurrent workers, 4 by default: each worker takes the next
entry in sequence order, downloads it, verifies its length and
`pack_sha256`, and runs `git index-pack --stdin --fix-thin --strict`
against the shared object store, which is safe because objects are
content addressed and git writes packs atomically. A thin pack can need
objects from an earlier entry; a worker whose `index-pack` fails with a
missing base waits until every lower entry has landed and runs once
more, and a failure after that is corruption (spec 004). The per-entry
`update-ref` is dropped: references are applied once at the end by step
4 of materialization, which reconciles the whole map to the index. On
the phase 1 numbers that reaches about 20 ms per entry with 4 workers,
so 1 000 entries in about 20 seconds on the laptop; the acceptance
threshold on the CI runner is 30 seconds for 1 000 entries, asserted by
a plain end-to-end test that runs on every push, not by the measurement
run `ORIGO_E2E_MEASURE` selects, which only prints. Compaction
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
- A push acknowledged on node A is returned by a fetch on node B: with
  gossip, the test waits until B's `origo_repo_entries_applied_total`
  rose by one, which is the background catch-up the announcement
  scheduled, then fetches on B and finds the pushed commit with B's
  `origo_wal_head_check_seconds{result="404"}` count risen by one while
  its `result="200"` count did not, so the catch-up happened before the
  request; with gossip disabled (`ORIGO_GOSSIP_PEERS` unset) the first
  fetch on B returns the pushed commit with the `result="200"` count
  risen by one. The test also records the time from A's acknowledgement
  until B's counter rose (proposed: `test/e2e`,
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
  `origod` with `minReplicas` and `maxReplicas` both `<n>`, which
  replaces the overlay's autoscaler of the same name rather than
  adding a second one, so `cluster.HPAStatus("origod")` reads it and
  `cluster.Apply` of the overlay restores the original; each count is
  confirmed with `cluster.HPAStatus("origod")` before the load starts; the three
  clones-per-second figures are recorded in the test output as
  measurements and nothing about them is asserted, because the
  runner's CPU, not the design, bounds them. Monotonicity over the
  three counts is asserted only in `TestMeasure` under
  `ORIGO_E2E_MEASURE=1`, run against the stack, which no job selects.
  The autoscaler test restores the overlay's own autoscaler
  with `cluster.Apply("deploy/examples/kind")`, drives CPU past the
  target, and reads `cluster.HPAStatus("origod")` until it reports 4
  replicas, within 60 seconds of the crossing; scale-down is not
  asserted, the overlay's 60 second window only keeps the cluster
  small for the next test (proposed: `test/e2e` in the `e2e-slow` job
  of spec 013, `TestSlowReplicasScaleReads`, `TestSlowAutoscalerScalesUp`).
- A drain during 100 concurrent pushes loses none: every push is either
  acknowledged and in the newest index or refused with
  `storage_unavailable` and absent (proposed: `test/e2e`,
  `TestE2EDrainLosesNoPush`).
- Materializing 1 000 entries from an empty disk with 4 workers
  finishes under 30 seconds against MinIO on the CI runner, and a thin
  entry whose base is in the previous entry lands on the retry. The
  fixture is written by the harness through `Log.Commit`, one entry per
  commit with a pack the harness builds, not by `git push`, under a
  budget of its own of 5 minutes, because phase 1 measured 98 seconds
  for the same 1 000 entries through the git client (spec 004 Outcome)
  (proposed: `test/e2e`, `TestE2EMaterializeThousandEntriesUnderBudget`,
  a plain test of the one-node run that runs on every push and shares
  its fixture builder with `TestMeasure`; `internal/repo`,
  `TestConcurrentWorkersApplyThinEntries`).
