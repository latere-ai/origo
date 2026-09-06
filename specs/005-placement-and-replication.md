---
title: "Placement and replication: rendezvous hashing, gossip, consistent reads, cache eviction"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
affects: [internal/placement/, internal/repo/, cmd/origod/]
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
is thrown away.

## Current state

Spec 004 gives a single node correct behaviour and a conditional GET on
the index as the freshness check.

## Design

### Placement

Rendezvous hashing over the live node set: for repository `id`, every node
computes `score(node, id)` and the top `k` nodes are the preferred
replicas. `k` is per repository, from the consumer's authorizer response
(`replicas`), default 1, and a hot repository can be raised to the whole
node set. There is no table; a node joining or leaving moves only the
repositories that hash to it. The ingress or the consumer routes with the
same function when it can (an `Origo-Prefer` header names the preferred
node; a request landing elsewhere is served anyway after materialization).

### Gossip

Nodes announce `(repo id, seq, etag)` after every index update over UDP
to `ORIGO_GOSSIP_PEERS`, three packets, no acknowledgement. A node that
hears a higher sequence for a repository it holds fetches the index and
catches up in the background. Gossip is an optimization: a lost packet
costs one conditional GET returning 200 instead of 304 on the next read.

### Consistent reads

Every request that serves repository state begins with a conditional GET
on the index with the held ETag. A 304 serves at once; a 200 catches up
first (spec 004). The cost of the 304 is the floor for any request and is
measured as `origo_index_check_seconds`. A node never serves from a local
copy without the check, and never caches an index for longer than the
request.

### Cache eviction

The local disk holds repositories up to `ORIGO_CACHE_BYTES`. Eviction is
least recently used with a floor of 10 minutes since last access, never
mid-request, and never of a repository whose entries are being applied.
Eviction deletes the directory; nothing is written back because nothing
local is authoritative. A repository with no reads in 24 hours is evicted
regardless of pressure so idle repositories hold no copy anywhere.

### Node lifecycle

A node is ready when it can read the bucket and write its disk. Draining
a node finishes in-flight requests and deletes its cache. A new node
starts cold and warms as requests arrive; a deploy therefore costs one
materialization per repository per replaced node, bounded by the pack
download rate, and never blocks writes.

### Scaling

Nodes are interchangeable, so scaling is a replica count and nothing
else. What scales and what does not:

| Load | Scales with replicas | Bound |
|---|---|---|
| reads: clone, fetch, the read API, archive | linearly; every node serves any repository after one materialization | the bucket's request rate and each node's disk |
| pushes to different repositories | linearly; commits to different repositories never contend | same |
| pushes to one repository | does not scale; every push to a repository serializes on its create-if-absent commit, whichever node receives it | one repository sustains on the order of ten pushes per second against object storage (phase 1 measured 9.7 per second on MinIO in a laptop VM, spec 004 Outcome); a busier repository needs batching of concurrent pushes into one commit, which is a later spec, not more nodes |

The autoscaler is a HorizontalPodAutoscaler on two signals, whichever is
higher: CPU at 70% of the request, and `origo_requests_in_flight` per pod
at 64. Minimum replicas 2, maximum 32, scale-up stabilization 30 seconds,
scale-down stabilization 10 minutes so a burst of clones does not churn
the cache. A PodDisruptionBudget keeps at least 2 available, pod
anti-affinity spreads replicas across nodes, and the Deployment's rolling
update runs `maxSurge: 1, maxUnavailable: 0`. Scale-down drains as above:
the node stops accepting connections, finishes in-flight pushes (a push
is never abandoned after its entry is written; the commit completes or
the sweeper removes the orphan), and its cache is deleted. Placement
adjusts by itself because rendezvous hashing has no table to update; a
scale event moves the repositories that hashed to the changed node and
nothing else.

### Disk

Each pod has one local volume for the cache: an ephemeral local volume
from the node's fast disk where the platform offers one, else a generic
ephemeral volume on the default storage class, sized by
`ORIGO_CACHE_BYTES` plus 20% headroom. Never a network file system: git
on a network mount is the one configuration that makes every operation
slow. The eviction ceiling defaults to 80% of the volume. A pod whose
volume is lost restarts cold and is correct; it is only slower until warm.

### Materialization budget

Phase 1 applied entries one at a time with two git subprocesses each and
measured 70 seconds for 1 000 entries. The budget is one second per 100
entries on a warm bucket connection, reached by: prefetching entries with
4 concurrent downloads while applying, indexing packs with one
`git index-pack` per entry but applying references once with a single
`update-ref --stdin` at the end, and relying on compaction (spec 006) to
keep the entry count under 64 for any repository that is fetched at all.
A materialization that exceeds 60 seconds is reported on
`origo_materialization_seconds` and the alert of spec 011.

## Acceptance criteria

- With 3 nodes and `replicas = 1`, 95% of reads for a repository hit the
  node rendezvous hashing prefers when the client sets `Origo-Prefer`.
- A push on node A is visible on node B within 100 ms with gossip and
  within one request without it (gossip disabled in the test).
- Filling the cache past the ceiling evicts the least recently used
  repositories and never one in use; a subsequent read materializes it
  again with identical history.
- Removing a node from the set during a load test causes no failed
  requests, only warm-up latency on its former repositories.
- Under a synthetic read load, adding replicas from 2 to 8 raises served
  clones per second at least 3x with no push regression; the HPA scales
  up within 60 seconds of the in-flight threshold and down after the
  stabilization window; a drain during 100 concurrent pushes loses none.
- Materializing 1 000 entries from an empty disk finishes under 10
  seconds against MinIO on the CI runner.
