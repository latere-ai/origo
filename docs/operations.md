# Operating Origo

For whoever runs an Origo installation. The design is in `specs/`; this
page is what to do.

## What Origo needs

An S3 compatible bucket, an OIDC issuer, and Kubernetes with a fast local
disk per node. Nothing else: no database, no message queue, no operator.
The bucket is the only durable state. Everything on a node is a cache.

## Backup

The write-ahead log in the bucket is the backup. Every push and every
compaction is an object under `origo/repos/<id>/`, and a repository can be
rebuilt from those objects alone on any node. Protect the bucket the way
you protect any durable data: versioning on, a lifecycle rule that keeps
deleted versions for at least the 7 day undelete hold, and replication to
a second region if your provider offers it. There is nothing on a node
worth backing up.

## Restore a repository

A node that finds a local copy corrupt rebuilds it from the log by
itself. If an object in the log itself is missing or corrupt, a node
built with spec 015 reports `origo_log_integrity_errors_total` and
answers 503 `repository_unavailable` for that repository; until that
spec lands the node answers 503 `storage_unavailable` and the key is
in its log line. Either way:

1. Find the key in the node's log line.
2. Restore that object from the bucket's version history, or copy it from
   another node's warm cache (`/var/lib/origo/repos/<id>.git/objects/pack/`
   holds the same pack bytes the entry carries).
3. The next request to the repository materializes it again.

## Upgrade

Push a tag. The release pipeline builds the image, applies `deploy/prod`,
and waits for the rollout. The Deployment rolls one pod at a time with
none unavailable; a replaced pod starts cold and warms as requests
arrive. No migration step exists because there is no schema. A
software bill of materials and build provenance ship with the release
pipeline of spec 017; a release cut before it lands carries neither.

## Scale

Replicas are the one thing that scales. The HorizontalPodAutoscaler in
`deploy/base` scales on CPU only, 70% of the request, between 2 and 32
replicas, up after 30 seconds and down after 10 minutes so a burst of
clones does not churn the cache. No metrics adapter is installed;
`origo_requests_in_flight` is a signal for a dashboard, not an
autoscaler input. Reads scale with replicas: every node serves any
repository after one materialization. Pushes to one repository do not,
by design; if one repository needs more than about ten pushes per
second sustained, that is a design conversation, not a replica count.

Nodes find each other over gossip on UDP 7946 through the headless
Service `origod-gossip`, with every datagram signed under
`ORIGO_GOSSIP_SECRET` from the `origod-auth` Secret: a node without
the secret is never in the live set. Every response that names a
repository carries `Origo-Prefer`, the nodes that hold it warm, highest
first, for an ingress that can route by pod; a request is served
wherever it lands.

Each pod's cache is bounded by `ORIGO_CACHE_BYTES`, 80% of the volume
by default: the least recently used copies go first, a copy used in
the last 10 minutes is kept, and a copy idle for 24 hours goes whatever
the pressure. `origo_cache_bytes`, `origo_cache_repos`, and
`origo_evictions_total{reason}` show it; a pod that restarts on the
same volume is warm, one that moves starts cold and warms as requests
arrive.

## When the bucket is unhealthy

With spec 015 in place, reads of warm repositories keep working with an
`Origo-Stale` header for up to five minutes and pushes are refused with
a message telling the client to retry; the alerts `OrigoBreakerOpen`
and `OrigoStaleServing` of spec 011 fire. Until then every request
fails with 503 `storage_unavailable` after the client's retries. In
both cases there is nothing to do on the Origo side but wait for the
bucket; when it returns, nodes catch up on their own.

## Push events

With `ORIGO_EVENTS_URL` and `ORIGO_EVENTS_SECRET` set, every push is
one signed `POST` to that URL, retried for 24 hours on a sink that
fails (1 s, 10 s, 1 min, 10 min, then hourly). After the 24 hours the
event sits under `origo/events/dead/<repo>/` in the bucket and
`origo_events_dead_total` counts it; to retry a dead event, move its
object back under `origo/events/<repo>/` and a node's repair sweep
delivers it within `ORIGO_REPAIR_INTERVAL`. A node that dies between
a push and its delivery is covered by the other nodes: the sweep reads
the dead node's journal under `origo/events/nodes/<node>/` once it has
been unheard for `ORIGO_REPAIR_UNHEARD` and rebuilds the event from
the log, so a sink sees an event more than once at worst and keys on
its `id`. Nothing under `origo/events/` needs a backup: a pending
event is rebuilt from the log, and a dead one is kept for the operator.

## Dashboards and alerts

Spec 011 adds the PrometheusRule to `deploy/base`; until it lands there
is no rule file to apply. The metrics are listed in spec 011; the ones
to watch first are `origo_storage_breaker_state` (spec 015),
`origo_wal_head_check_seconds` (in the tree), and
`origo_requests_in_flight` (spec 011).
