---
title: "Observability: metrics, traces, logs, alerts"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
affects: [internal/, deploy/]
effort: small
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Observability

## Overview

Every signal is named here so nothing is added ad hoc. Telemetry goes out
over OTLP with the standard exporter variables; labels come from fixed
vocabularies so a hostile client cannot grow cardinality.

## Design

### Metrics

| Metric | Type | Labels |
|---|---|---|
| `origo_requests_total`, `origo_request_duration_seconds` | counter, histogram | `route` (template), `status_class` |
| `origo_pushes_total` | counter | `result` (`ok`, `non_fast_forward`, `rejected`, `error`) |
| `origo_push_duration_seconds` | histogram | `phase` (`receive`, `entry`, `index`, `apply`) |
| `origo_index_check_seconds` | histogram | `result` (`304`, `200`) |
| `origo_cas_conflicts_total` | counter | |
| `origo_materializations_total`, `origo_materialization_seconds` | counter, histogram | `reason` (`miss`, `catchup`, `repair`) |
| `origo_cache_bytes`, `origo_cache_repos` | gauge | |
| `origo_evictions_total` | counter | `reason` (`pressure`, `idle`) |
| `origo_compactions_total`, `origo_compaction_seconds` | counter, histogram | `result` |
| `origo_gossip_packets_total` | counter | `direction` |
| `origo_events_delivered_total`, `origo_events_dead_total` | counter | |
| `origo_authorizer_seconds` | histogram | `result` |
| `origo_storage_ops_total`, `origo_storage_seconds` | counter, histogram | `op` (`get`, `put`, `create`, `head`, `delete`, `list`), `result` |
| `origo_storage_breaker_state` | gauge | `class` (`read`, `write`); 0 closed, 1 open, 2 half-open (spec 015) |
| `origo_stale_responses_total` | counter | (spec 015) |
| `origo_log_integrity_errors_total` | counter | (spec 015) |
| `origo_requests_in_flight` | gauge | the autoscaler's signal (spec 005) |

No label ever carries a repository id, owner, slug, subject, or ref.

### Traces

One trace per request; a push's spans are `receive`, `entry.put`,
`index.cas`, `apply`, `event.enqueue`; a read's are `index.check`,
`materialize`, `git.<command>`. Repository id is a span attribute, never a
metric label.

### Logs

Structured, one line per request with route, status, duration, repository
id, subject, actor, bytes; never a token, never object bytes. Push entries
are also audit records in the log itself (spec 004).

### Alerts

| Alert | Condition |
|---|---|
| storage unavailable | `origo_storage_ops_total{result="error"}` rate over 5% for 5 minutes |
| CAS storms | `origo_cas_conflicts_total` rate over 10 per second on one node for 5 minutes |
| dead events | any increase in `origo_events_dead_total` |
| slow index checks | p99 `origo_index_check_seconds` over 50 ms for 10 minutes |
| cache thrash | `origo_evictions_total{reason="pressure"}` over 100 per minute |
| storage breaker open | `origo_storage_breaker_state` at 1 for 1 minute |
| stale serving | any increase in `origo_stale_responses_total` |
| log integrity | any increase in `origo_log_integrity_errors_total` |
| slow materialization | p99 `origo_materialization_seconds` over 60 for 10 minutes |
| replicas pinned at maximum | HPA at its maximum for 15 minutes |

## Acceptance criteria

- A test asserts every metric name above exists after a fixture load and
  that no label value is a repository id, subject, or ref.
- A push produces one trace with the named spans.
- Alert rules are checked into `deploy/` and validated by the release
  pipeline.
