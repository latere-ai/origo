---
title: "Observability: metrics, traces, logs, alerts"
status: validated
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
affects: [internal/, cmd/origod/, deploy/, .github/workflows/, tools/specindex/]
effort: small
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Observability

## Overview

Every signal is named here so nothing is added ad hoc. Metrics are the
`latere.ai/x/pkg/metrics` registry served on `GET /metrics`; traces and
logs go out over OTLP through `latere.ai/x/pkg/otel` with the standard
exporter variables; labels come from fixed vocabularies so a hostile
client cannot grow cardinality. This spec owns every metric name in the
deck.

## Current state

`cmd/origod` serves the registry on the internal listener and logs one
JSON line per event through `log/slog` to stdout. `internal/wal`,
`internal/repo`, and `internal/httpgit` record the twelve phase 1
metrics below; every counter reads 0 before its first event. There are
no traces, no request log line, no PrometheusRule in `deploy/base`, and
`OTEL_*` is not read: `pkg/otel` is not imported.

## Design

### Metrics

Phase 1 names are the code's and are kept; the first draft's
origo_index_check_seconds, origo_cas_conflicts_total,
origo_materializations_total, and origo_materialization_seconds were
never recorded and are replaced by the names below.

| Metric | Type | Labels | Recorded by |
|---|---|---|---|
| `origo_wal_commits_total` | counter | | index objects this node created (004, phase 1) |
| `origo_wal_commit_conflicts_total` | counter | | commits refused because a reference moved (004, phase 1) |
| `origo_wal_commit_retries_total` | counter | | commit rounds lost to another writer and replayed (004, phase 1) |
| `origo_wal_entry_bytes_total` | counter | | bytes written as entries (004, phase 1) |
| `origo_wal_head_check_seconds` | histogram, buckets 1 ms to 2.5 s | `result` (`404`, `200`, `error`), added by 005 so a test can tell a check that found the copy current from one that found a newer index; phase 1 records it unlabelled | the `HEAD` currency check (004, phase 1) |
| `origo_pushes_total` | counter | | pushes acknowledged (003, phase 1) |
| `origo_pushes_rejected_total` | counter | | pushes refused by the log (003, phase 1) |
| `origo_fetches_total` | counter | | upload-pack requests served (003, phase 1) |
| `origo_repo_materialized_total` | counter | | repositories built from the log onto an empty disk (004, phase 1) |
| `origo_repo_entries_applied_total` | counter | | entries applied to local copies (004, phase 1) |
| `origo_repo_rebuilt_total` | counter | | local copies removed as corrupt and rebuilt (004, phase 1) |
| `origo_repo_materialize_seconds` | histogram, buckets 10 ms to 60 s | | time to bring a local copy current (004, phase 1) |
| `origo_requests_total` | counter | `route` (the mux pattern), `status_class` (`2xx` to `5xx`) | the `pkg/otel` metrics hook on the public listener |
| `origo_request_duration_seconds` | histogram | `route`, `status_class` | same |
| `origo_requests_in_flight` | gauge | | requests started and not finished on the public listener, counted by a middleware in `cmd/origod` around the public handler, because the `pkg/otel` hook fires only after a request ends; the autoscaler's second signal (005) |
| `origo_push_duration_seconds` | histogram | `phase` (`receive`, `entry`, `index`, `apply`) | the receive path (004) |
| `origo_cache_bytes`, `origo_cache_repos` | gauge | | the evictor (005) |
| `origo_evictions_total` | counter | `reason` (`pressure`, `idle`) | the evictor (005) |
| `origo_gossip_packets_total` | counter | `direction` (`sent`, `received`, `dropped`) | gossip (005) |
| `origo_compactions_total` | counter | `result` (`ok`, `stale`, `error`, `skipped`) | compaction (006); `skipped` is a run that found no subprocess slot within 5 seconds (012) |
| `origo_compaction_seconds` | histogram | | compaction (006) |
| `origo_authorizer_seconds` | histogram | `result` (`allow`, `deny`, `error`) | the authorizer client (007) |
| `origo_events_delivered_total`, `origo_events_dead_total` | counter | | event delivery (008) |
| `origo_rate_limited_total` | counter | `limit` (`subject`, `subprocesses`, `repository`) | limits (012), the per-repository limits of 019 and 020 |
| `origo_storage_ops_total` | counter | `op` (`get`, `put`, `create`, `head`, `delete`, `list`), `result` (`ok`, `not_found`, `exists`, `error`) | the store adapter (015) |
| `origo_storage_seconds` | histogram | `op` | same |
| `origo_storage_breaker_state` | gauge | `class` (`read`, `write`); 0 closed, 1 open, 2 half-open | the breakers (015) |
| `origo_stale_responses_total` | counter | | responses served with `Origo-Stale` (015) |
| `origo_log_integrity_errors_total` | counter | | a pack or entry the log names that is missing or fails its digest (015) |
| `origo_orphan_objects` | gauge | | objects no index names, from the weekly sweep (019) |
| `origo_storage_bytes` | gauge | | bytes under the prefix, from the weekly sweep (019) |

No label ever carries a repository id, owner, slug, subject, reference,
or path. Gauges are registered with `Registry.Gauge` and read at scrape
time.

### Traces

`cmd/origod` calls `otel.Bootstrap(ctx, otel.Config{ServiceName:
"origod", Version, Replica, Stdout})` with `Stdout` a JSON handler on
standard output, because `Bootstrap` defaults to standard error and the
node's log lines stay on standard output (spec 002), wraps the public handler in
`otel.Handler` with `WithRouteTemplate` returning the mux pattern and
`WithMetricsHook` feeding the two request metrics, and wraps the storage
transport in `otel.Transport`. One trace per request; a push's spans are
`receive`, `entry.put`, `index.create`, `apply`, `event.enqueue`; a
read's are `index.check`, `materialize`, `git.<command>`; every object
storage call is a child span named by its `op`. Repository id, subject,
and actor are span attributes, never metric labels. Without
`OTEL_EXPORTER_OTLP_ENDPOINT` the spans are created and discarded.

### Logs

JSON through `log/slog`, one line per request from the `otel.Handler`
with `route`, `method`, `status`, `duration_ms`, `repo`, `subject`,
`actor`, `bytes_in`, `bytes_out`, `trace_id`; never a token, never
object bytes, never a basic auth password. A push logs its sequence.
Entries in the log itself are the audit record (spec 004).

### Alerts

A PrometheusRule `origod` in `deploy/base`, validated by
`promtool check rules` in the verify workflow:

| Alert | Condition |
|---|---|
| `OrigoStorageErrors` | `rate(origo_storage_ops_total{result="error"}[5m]) / rate(origo_storage_ops_total[5m]) > 0.05` for 5 minutes |
| `OrigoCommitStorm` | `rate(origo_wal_commit_retries_total[5m]) > 10` on one node for 5 minutes |
| `OrigoDeadEvents` | `increase(origo_events_dead_total[10m]) > 0` |
| `OrigoSlowHeadCheck` | `histogram_quantile(0.99, rate(origo_wal_head_check_seconds_bucket[5m])) > 0.05` for 10 minutes |
| `OrigoCacheThrash` | `rate(origo_evictions_total{reason="pressure"}[1m]) > 100/60` |
| `OrigoBreakerOpen` | `origo_storage_breaker_state == 1` for 1 minute |
| `OrigoStaleServing` | `increase(origo_stale_responses_total[5m]) > 0` |
| `OrigoLogIntegrity` | `increase(origo_log_integrity_errors_total[5m]) > 0` |
| `OrigoSlowMaterialization` | `histogram_quantile(0.99, rate(origo_repo_materialize_seconds_bucket[10m])) > 60` for 10 minutes |
| `OrigoReplicasPinned` | `kube_horizontalpodautoscaler_status_current_replicas == kube_horizontalpodautoscaler_spec_max_replicas` for 15 minutes |

## Not in this spec

Dashboards. Profiling endpoints. Sampling policy beyond the exporter's
defaults.

## Acceptance criteria

- After a fixture load (create, push, clone, a refused push, a
  compaction), `GET /metrics` contains every name in the table with the
  listed labels and no label value equal to a repository id, subject,
  reference, or path used by the fixture (proposed: `cmd/origod`,
  `TestMetricsVocabulary`).
- A push against an in-memory OTLP receiver produces one trace whose
  spans are the five named, with the repository id as an attribute
  (proposed: `cmd/origod`, `TestPushTrace`).
- The request log line for a push carries the listed fields and no
  `Authorization` value when the request used basic auth (proposed:
  `cmd/origod`, `TestRequestLogRedactsCredentials`).
- `promtool check rules deploy/base/prometheusrule.yaml` passes in the
  verify workflow, and every alert's metric names are in the table
  (proposed: `tools/specindex` reads the rule file; `verify.yml` runs
  `promtool`).
