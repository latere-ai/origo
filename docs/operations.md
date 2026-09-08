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
itself. If an object in the log itself is missing or corrupt, that
repository alone answers 503 `repository_unavailable` with the object's
key in `details.key`, `origo_log_integrity_errors_total` counts it, and
the alert `OrigoLogIntegrity` fires; every other repository is served
as before, and nothing on any node is removed. To restore it:

1. Find the key: it is in the error's `details.key` and in the node's
   log line `log integrity error`.
2. Restore that object from the bucket's version history, or copy it
   from another node's warm cache
   (`/var/lib/origo/repos/<id>.git/objects/pack/` holds the same pack
   bytes the entry or pack object carries).
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

## Limits

Origo bounds what one client can take from a node. A caller sending
more than `ORIGO_REQUESTS_PER_MINUTE` requests a minute, 600 by
default, gets 429 `rate_limited` with `Retry-After`, counted per
subject per node, so one repeating build does not crowd out the rest;
the table holds only the subjects that called in the last ten minutes.
Set it to `0` to turn the per-subject limit off, which is what a stack
driven by a load generator under one token wants. Note that one client
pushing back to back sends two requests per push, so a loop of pushes
under one token meets 600 a minute quickly.

Each node runs at most `ORIGO_MAX_GIT_PROCS` git subprocesses at once,
64 by default. A request waits up to five seconds for a slot and is
then 429 as well; a compaction that finds no slot skips and runs on the
next sweep, so background work never crowds out a clone. Raise the
figure with the pod's CPU limit, not past it: every slot is a `git`
process with the memory of the repository it serves.

A repository is bounded by `quota_bytes`, the figure your authorizer
answers with, 50 GiB when it names none. The measurement is what the
log holds plus the objects under the repository's `lfs/` prefix, so a
compaction lowers it and deleting LFS objects lowers it. A push past
the limit is refused in git's own output with `over_quota` and nothing
is written; the node's log line for it carries the figures. A single
push is at most 2 GiB whatever the quota says.

`origo_rate_limited_total{limit}` counts the refusals: `subject` for
the rate, `subprocesses` for the slots. A rising `subprocesses` figure
means the node is saturated and wants replicas or a higher cap; a
rising `subject` figure means one caller is looping.

## When the bucket is unhealthy

Every call to the bucket has a deadline, `ORIGO_STORAGE_TIMEOUT` (10
seconds by default), and a node keeps two breakers, one for reads and
one for writes. Five failed calls in a row open a breaker; it stays
open for 30 seconds, then lets one call through as a probe, and closes
when the probe succeeds. `origo_storage_breaker_state` shows each one
(0 closed, 1 open, 2 probing) and `OrigoBreakerOpen` fires after a
minute open.

While the read breaker is open, a repository the node already holds is
served from its local copy for up to `ORIGO_STALE_MAX` (5 minutes by
default) after its last successful check against the bucket, with the
header `Origo-Stale` carrying the seconds since that check, so a
consumer that must not read stale can refuse the response;
`origo_stale_responses_total` counts them and `OrigoStaleServing`
fires. A repository the node does not hold, or one past the bound,
answers 503 `storage_unavailable` at once with a `Retry-After`.

Pushes are refused while either breaker is open, before the client
uploads anything: `git push` prints `remote error: storage_unavailable:
The repository is temporarily unavailable. Nothing was lost. Try again
in a few minutes.` A push whose pack had already arrived waits up to a
minute for the write breaker and is refused the same way if it does not
close. Nothing is queued and nothing is lost: a refused push was never
recorded.

A replica stays in rotation while its breaker is open, because it
still serves what it holds and refuses the rest with that message;
`/readyz` fails while the bucket is slow or unreachable and the breaker
has not opened yet, and on a replica the bucket has never answered
since it started, which has nothing to serve. There is nothing to do on the Origo side
but wait for the bucket: when it answers again, the next probe closes
the breaker, every repository is checked against the log on its next
request, and `Origo-Stale` disappears.

To see the bucket's health from a node's side, watch
`origo_storage_ops_total{result="error"}` against the total and
`origo_storage_seconds`; `OrigoStorageErrors` fires when more than one
call in twenty fails for five minutes.

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

Every metric a node exposes is on `/metrics` of the internal listener
from the first scrape, at 0 until something records it, so a dashboard
panel is never empty because a series has not appeared yet. The names
are spec 011's table; the ones to watch first are
`origo_storage_breaker_state`, `origo_wal_head_check_seconds`, and
`origo_requests_in_flight`. No label carries a repository, an owner, a
subject, a reference, or a path: those are span attributes.

The alerts are `deploy/base/prometheusrule.yaml`, a PrometheusRule the
operator of a cluster running the Prometheus operator applies beside the
base. It is not a resource of the base's kustomization, because applying
it needs that operator's CustomResourceDefinition and Origo does not
require one. Without the operator, the same expressions go into whatever
rule file the installation already has.

## Traces and logs

`OTEL_EXPORTER_OTLP_ENDPOINT` is the one variable that turns telemetry
on. With it set, the node exports traces, metrics, and log records over
OTLP/HTTP to that endpoint: one trace per request on the public
listener, with a span per phase of a push and per object storage call,
and the repository, subject, and actor as span attributes. Unset, the
spans are created and discarded and the node costs nothing for them.
`OTEL_TRACES_SAMPLER_ARG` is the head-sampling ratio, one root trace in
five by default.

Every request on the public listener also writes one JSON line with its
route, method, status, duration, repository, subject, actor, bytes each
way, and `trace_id`, which is the id the response's `X-Trace-Id` header
and an LFS failure's `request_id` carry, so a report from a user leads
to the line and the trace. A credential is never in a line.
