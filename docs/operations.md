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
itself. If an object in the log itself is missing or corrupt (the node
reports `origo_log_integrity_errors_total` and answers 503
`repository_unavailable` for that repository):

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

The HorizontalPodAutoscaler scales on CPU only, between 2 and 32
replicas; `origo_requests_in_flight` is a signal for a dashboard, not an
autoscaler input. Reads scale with replicas. Pushes to one repository do
not, by design; if one repository needs more than about ten pushes per
second sustained, that is a design conversation, not a replica count.

## When the bucket is unhealthy

Reads of warm repositories keep working with an `Origo-Stale` header for
up to five minutes; pushes are refused with a message telling the client
to retry. The alerts `storage breaker open` and `stale serving` fire.
Nothing to do on the Origo side but wait for the bucket; when it returns,
nodes catch up on their own.

## Dashboards and alerts

`deploy/base` carries the PrometheusRule. The metrics are listed in spec
011; the ones to watch first are `origo_storage_breaker_state`,
`origo_wal_head_check_seconds`, and `origo_requests_in_flight`.
