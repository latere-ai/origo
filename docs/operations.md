# Operating Origo

For whoever runs an Origo installation after [`install.md`](install.md):
what to back up, how to upgrade and scale, how to administer
repositories over years, and what to do when the bucket or a
dependency fails.

## What Origo needs

An S3 compatible bucket, an OIDC issuer, and Kubernetes with a fast local
disk per node. Nothing else: no database, no message queue, no operator.
The bucket is the only durable state. Everything on a node is a cache.

## Backup

The write-ahead log in the bucket is the backup. Every push and every
compaction is an object under `origo/repos/<id>/`, and a repository can be
rebuilt from those objects alone on any node. There is nothing on a node
worth backing up.

Origo manages each object's life itself. It creates objects with
conditional writes, overwrites a few small bookkeeping objects such as a
repository's metadata, and deletes what it no longer needs: entries
folded by compaction, objects no index names once the sweep finds them,
and a deleted repository once its seven day hold has passed. Protect the
bucket in a way that works with that:

- **Turn on object versioning** if your provider offers it. A deleted or
  overwritten object is kept as a noncurrent version, which is what lets
  you restore one object under "Restore a repository" below. Versioning
  does not change how a conditional create behaves; `origod check`
  proves the conditional create against your bucket as it is configured.
- **Expire noncurrent versions, and only those,** after as many days as
  you want to be able to restore, 30 for example. Without such a rule
  the overwritten and deleted objects accumulate for as long as the
  installation runs.
- **Never add a rule that expires, transitions, or rewrites current
  objects** under `origo/`. The log names those objects, and a
  repository whose object is gone or moved to an archive class is
  unavailable until it is restored.
- **Replicate to a second region** if your provider offers it, as long
  as the replica keeps the keys unchanged.

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

Set the new image tag in your overlay and apply it, or set it on the
running workload, with `VERSION` the tag you are moving to:

```sh
kubectl -n origo set image deployment/origod "*=ghcr.io/latere-ai/origod:$VERSION"
kubectl -n origo rollout status deployment/origod --timeout=600s
```

`*=` sets both containers of the pod, the node and the check that runs
before it, which are the same image. The Deployment rolls one pod at a
time with none unavailable; a replaced pod starts cold and warms as
requests arrive. There is no schema and no migration step.
[`upgrades/`](upgrades/README.md) says what a version number promises,
when a rollback is safe, and how to verify a release's signatures
before you run it.

The commands on this page name the Deployment `origod` that
`deploy/base` ships. An overlay that replaces it, as the kind example
does with a StatefulSet of the same name, uses its own kind in their
place.

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

## Administering a repository

Beyond clone, fetch, and push, `/v1/repos/{id}` carries the operations a
repository needs over years. Each asks your authorizer for `admin`
unless the list says otherwise, and each sends an event to
`ORIGO_EVENTS_URL`.

| Operation | What it does |
|---|---|
| `PATCH /v1/repos/{id}` | changes the owner or the slug. The clone URL changes at once and the old one answers 404, never a redirect, so a stale URL cannot keep working past a change of owner. The id never changes, so tokens, events, and `/r/<id>.git` clones keep working |
| `POST /v1/repos/{id}/transfer` | the same move recorded as `transferred`, so a consumer acts on a change of owner without inspecting a rename |
| `POST /v1/repos/{id}/freeze` | stops the repository accepting pushes. A push is refused at `info/refs`, before the client uploads anything, and git prints `remote error: repo_frozen: ...`; clones and fetches go on. A second freeze is 409 |
| `POST /v1/repos/{id}/unfreeze` | lets pushes through again. 200 whether or not it was frozen |
| `POST /v1/repos/{id}/import` | brings an existing repository in from an `https` source with its history. 202 at once; the run has 30 minutes and the repository's quota. Poll `GET /v1/repos/{id}/import` for `running`, `done`, or `failed` |
| `GET /v1/repos/{id}/export.bundle` | the whole repository as one `git bundle`, action `read`. Verify what you receive with `git bundle verify` and `git clone`: a bundle cut by the 10 minute budget is a truncated file, not an error status. A repository with no reference has nothing to bundle and answers 404 `ref_not_found` |
| `GET /v1/repos/{id}/stats` | `size_bytes`, `lfs_bytes`, `packs`, `entries_since_compaction`, `refs`, `pushed_at`, `compacted_at`, action `read`. `refs` counts `HEAD` with the references |
| `POST /v1/repos/{id}/gc` | compacts now. On the repository's primary it waits up to 10 seconds and answers the before and after figures, or 202 `running`; on any other node it answers 202 `scheduled` naming the primary, which compacts within ten minutes. A repository compacted within the last hour, by a `gc` or by a threshold, is 429 `rate_limited` with `details.limit: "repository"` |

An import needs the source host on `ORIGO_EGRESS_ALLOW`; a host that is
not on it is 400 `invalid_request` with `details.reason: "egress"` and
no connection is opened. The source bearer travels in the request body
and then in the subprocess environment, so it is in no process listing
and no log line. A repository that already has history is 409
`repo_not_empty`: import into an empty repository, or create a new one
and transfer the name.

A node that dies mid-import leaves the repository importing. Another
node frees it 45 minutes later with `import_error: "import node lost"`;
a node that restarts under the same name frees its own at start-up with
`"import node restarted"`. Either way the repository accepts a new
import and nothing was committed.

## Deleting and its hold

`DELETE /v1/repos/{id}` answers 202 with `purge_after`, seven days on.
Inside the hold `POST /v1/repos/{id}/undelete` brings it back whole.
After it the objects are gone and every endpoint answers 410 `gone`:
the id stays taken forever, so it can never name another repository,
while the owner and slug are free to be used again.

## Replacing an SSH host key

The host keys are the identity of your installation. Every node
presents the same set, and a client that has seen one and then meets
another prints `WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED`, which
is a security warning arriving for a maintenance reason and teaches
people to click past it. So a replacement is an overlap, not a switch.

`ORIGO_SSH_HOST_KEYS` is an ordered list. The first key of each
algorithm is the one presented; every key in the list is announced to
clients through OpenSSH's `hostkeys-00@openssh.com`, and a client with
`UpdateHostKeys` on writes the ones it does not hold into its
`known_hosts` by itself. That is what makes the overlap work.

| Step | The list | What clients hold |
|---|---|---|
| 1 | `old` | old |
| 2 | `old,new` | old presented; `new` announced, and written into `known_hosts` by every client that reconnects |
| 3 | `new,old` | new presented, and already trusted by the clients of step 2; `old` still announced and still accepted |
| 4 | `new` | new only |

Steps 2 and 3 each wait longer than the reconnect tail of your client
population. Measure it rather than guessing: the tail is how long it
takes for every client that will reconnect to have reconnected, which
for a CI fleet is minutes and for people's laptops is days. A week
between steps is a safe figure for a team; a month is safer for a
population you do not control.

Publish the new fingerprint before step 3, where you published the
first one. `ssh-keygen -lf <key>.pub` prints it, and the node's
start-up line carries the fingerprint of every key it holds. Not every
client learns a key by itself: `UpdateHostKeys` is on by default only
in recent OpenSSH, and JGit, libssh2, and old clients never learn one,
so those people edit `known_hosts` by hand from the fingerprint you
published. The extension shortens the tail; it does not remove it.

Between steps, replace the Secret and restart the pods:

```sh
kubectl -n origo create secret generic origod-ssh-host-key \
	--from-file=ssh_host_ed25519_key=old_key \
	--from-file=ssh_host_ed25519_key_new=new_key \
	--dry-run=client -o yaml | kubectl apply -f -
kubectl -n origo rollout restart deployment/origod
```

with `ORIGO_SSH_HOST_KEYS` naming the two paths in the order the step
above wants. A key is read once at start-up, so a rollout is what puts
a change in force; nothing reads a host key from the bucket, and
nothing ever will.

## Storage

Storage per repository is bounded by compaction: after any compaction
the log holds the packs plus at most 64 entries, and unreachable objects
are dropped by the repack. Deleted repositories go after their hold, and
an LFS object nobody verified goes 7 days after its upload.

Once a week, on Sunday at 03:00 UTC, one node lists the whole bucket
prefix and looks for objects nothing names. It is the node whose
`ORIGO_NODE_NAME` sorts first among the live ones, so an installation of
any size pays for the listing once a week and a node that leaves hands
the sweep to the next name with no configuration. It reports:

- `origo_orphan_objects`, objects older than a day that no index,
  verification marker, or metadata names. A healthy installation reports
  0. An object stays for seven days before it is deleted, and every one
  the sweep found is named by its key in the node's `orphan sweep` log
  line, so you can see what wrote it before it goes.
- `origo_storage_bytes`, the bytes under the prefix.

Every node reports the same two figures: the sweeping node writes its
report to `origo/sweep/latest` and the others read it. To bound total
storage yourself, sum `size_bytes` and `lfs_bytes` over `stats`.

## Limits

Origo bounds what one client can take from a node. A caller sending
more than `ORIGO_REQUESTS_PER_MINUTE` requests a minute, 600 by
default, gets 429 `rate_limited` with `Retry-After`, counted per
subject per node, so one repeating build does not crowd out the rest;
the table holds only the subjects that called in the last ten minutes.
Every response of that surface carries the figure in force as
`RateLimit-Limit`, so a client reads it rather than assuming the
default; `0` turns the limit off. Note that one client pushing back to
back sends two requests per push, so a loop of pushes under one token
meets 600 a minute quickly: raise the figure for a host that carries
bulk work under one service token.

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

With `ORIGO_EVENTS_URL` and `ORIGO_EVENTS_SECRET` set, every push and
every administration change is one signed `POST` to that URL, retried for 24 hours on a sink that
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
The payloads and how a sink verifies a delivery are in
[`api.md`](api.md#push-events).

## Dashboards and alerts

Every metric a node exposes is on `/metrics` of the internal listener
from the first scrape, at 0 until something records it, so a dashboard
panel is never empty because a series has not appeared yet. No label
carries a repository, an owner, a subject, a reference, or a path:
those are span attributes. The ones to watch first are
`origo_storage_breaker_state`, `origo_wal_head_check_seconds`, and
`origo_requests_in_flight`.

| Metric | What it measures |
|---|---|
| `origo_requests_total`, `origo_request_duration_seconds`, `origo_requests_in_flight` | requests on the public listener, their duration, and how many are open |
| `origo_pushes_total`, `origo_pushes_rejected_total`, `origo_push_duration_seconds` | pushes acknowledged, pushes the log refused, and the time spent in each phase of a push |
| `origo_fetches_total` | clones and fetches served |
| `origo_wal_commits_total`, `origo_wal_commit_conflicts_total`, `origo_wal_commit_retries_total`, `origo_wal_entry_bytes_total` | log commits, commits refused because a reference moved, rounds lost to a concurrent writer, and bytes written |
| `origo_wal_head_check_seconds` | the latency of the check that a local copy is current, paid before every read |
| `origo_repo_materialized_total`, `origo_repo_entries_applied_total`, `origo_repo_rebuilt_total`, `origo_repo_materialize_seconds` | copies built from the log, entries applied to copies, copies rebuilt as corrupt, and the time to bring a copy current |
| `origo_cache_bytes`, `origo_cache_repos`, `origo_evictions_total{reason}` | the local cache and why copies left it |
| `origo_compactions_total{result}`, `origo_compaction_seconds` | compaction runs and their duration |
| `origo_storage_ops_total{result}`, `origo_storage_seconds`, `origo_storage_breaker_state` | calls to the bucket, their latency, and each breaker (0 closed, 1 open, 2 probing) |
| `origo_stale_responses_total` | reads served while the bucket was unreachable |
| `origo_log_integrity_errors_total` | objects the log names that are missing or fail their digest |
| `origo_orphan_objects`, `origo_storage_bytes` | the weekly sweep's figures, described under Storage |
| `origo_authorizer_seconds{result}` | calls to the authorization endpoint |
| `origo_rate_limited_total{limit}` | refusals by limit |
| `origo_events_delivered_total`, `origo_events_dead_total` | push events accepted by the sink, and events given up on after 24 hours |
| `origo_gossip_packets_total` | gossip datagrams by direction |
| `origo_ssh_sessions_total`, `origo_ssh_auth_total`, `origo_ssh_keys_seconds` | SSH sessions, authentication attempts, and calls to the key endpoint |

The shipped alerts:

| Alert | Fires when |
|---|---|
| `OrigoStorageErrors` | more than one bucket call in twenty fails for five minutes |
| `OrigoBreakerOpen` | a storage breaker has been open for a minute: reads are served stale and writes refused |
| `OrigoStaleServing` | a response was served from a copy that may be behind the log |
| `OrigoLogIntegrity` | an object the log names is missing or damaged; see "Restore a repository" |
| `OrigoSlowHeadCheck` | the currency check before a read is above 50 ms at the 99th percentile for ten minutes |
| `OrigoSlowMaterialization` | bringing a copy current takes over a minute at the 99th percentile |
| `OrigoCommitStorm` | a node loses more than ten commit rounds a second to concurrent writers for five minutes |
| `OrigoCacheThrash` | the cache evicts under pressure more than a hundred times a minute; raise `ORIGO_CACHE_BYTES` |
| `OrigoDeadEvents` | an event was given up on after 24 hours of refusals by the sink |
| `OrigoReplicasPinned` | the autoscaler has sat at its maximum for fifteen minutes |

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
and the repository and subject as span attributes. Unset, the
spans are created and discarded and the node costs nothing for them.
`OTEL_TRACES_SAMPLER_ARG` is the head-sampling ratio, one root trace in
five by default.

Every request on the public listener also writes one JSON line with its
route, method, status, duration, repository, subject, bytes each way,
and `trace_id`, which is the id the response's `X-Trace-Id` header
and an LFS failure's `request_id` carry, so a report from a user leads
to the line and the trace. A credential is never in a line.
