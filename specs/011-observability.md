---
title: "Observability: metrics, traces, logs, alerts"
status: complete
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
affects: [internal/, internal/metrics/, cmd/origod/, deploy/, .github/workflows/, tools/specindex/]
effort: small
created: 2026-09-06
updated: 2026-09-08
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
JSON line per event through `log/slog` to stdout. Six packages record
metrics and each registers its own with an `Add(nil, 0)`, so the series
reads 0 before its first event: `internal/wal`, `internal/repo`, and
`internal/httpgit` the twelve phase 1 metrics below, `internal/auth`
the authorizer histogram (spec 007), `internal/events` the two delivery
counters (spec 008), and `internal/placement` the gossip, eviction, and
cache metrics (spec 005). There are
no traces, no request log line, no PrometheusRule in `deploy/base`, and
`OTEL_*` is not read: `pkg/otel` is not imported.

One change to the tree, for the builder: registration moves out of all
six into one place, `internal/metrics/register.go` (spec 002's layout),
which registers every name in the table below on the registry at
start-up and hands the handles to the packages that record them, so a
metric of a spec not built yet still exists at 0 and the presence test
below needs no fixture. A package keeps only the recording. The `internal/metrics` of
phase 1 that moved to `latere.ai/x/pkg/metrics` (spec 002, Outcome)
was the registry; this is the list of names over it.

A second change, from spec 010: `internal/lfs` sends `request_id` in
the LFS error body and has nothing to read today, so it sends a fresh
UUID. The trace id the request log line below carries is what it
needs: the builder puts the id on the request's context in the
`otel.Handler` wrapping and reads it in `internal/lfs`, so the LFS
body and the log line name one request. `otel.TraceIDs` of `pkg/otel`
is not imported before this spec, because it pulls the OpenTelemetry
SDK onto the node's build list, which this spec is what adds.

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
| `origo_requests_in_flight` | gauge | | requests started and not finished on the public listener, counted by a middleware in `cmd/origod` around the public handler, because the `pkg/otel` hook fires only after a request ends; a dashboard signal (`docs/operations.md`), not an autoscaler input, which is CPU only (005) |
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
or path. Every metric in the table is registered at start-up in
`internal/metrics/register.go`, one function `Register(reg) *Set` that
returns the handles the recording packages take, so `GET /metrics`
carries every name at 0 before anything is recorded; a labelled counter
is registered with one `Add` of 0 per label value in its vocabulary.
Gauges are registered with `Registry.Gauge` and read at scrape time.

### Traces

`cmd/origod` calls `otel.Bootstrap(ctx, otel.Config{ServiceName:
"origod", Version, Replica, Stdout})` with `Stdout` a JSON handler on
standard output, because `Bootstrap` defaults to standard error and the
node's log lines stay on standard output (spec 002), wraps the public handler in
`otel.Handler` with `WithRouteTemplate` returning the mux pattern and
`WithMetricsHook` feeding the two request metrics, and wraps the storage
transport in `otel.Transport`.

`internal/tracing` is the one package in the module that imports
`go.opentelemetry.io/otel` and `go.opentelemetry.io/otel/trace`. Every
other package takes the span helpers it needs from `internal/tracing`
and imports no OpenTelemetry package; `cmd/origod` reaches the SDK
through `latere.ai/x/pkg/otel` as well. The rule keeps one seam to
change when `pkg/otel` gains a tracer of its own, and it is what amends
spec 001's seventh invariant, whose direct dependencies are otherwise
the standard library and `latere.ai/x/pkg`; the `depcheck` gate of
`.lateregate.yaml` lists the whole build list of `./cmd/origod` with a
reason per upstream root, so a further direct dependency fails the
gate.

One trace per request; a push's spans are
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
`promtool check rules` in the `specindex` job of `verify.yml`, which
installs `promtool` and runs it beside the cross-reference test (spec
013 owns the job's other steps):

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

- Right after start-up, with nothing recorded, `GET /metrics` contains
  every name in the table with every listed label value at 0, because
  `internal/metrics/register.go` registered them; after a fixture load
  (create, push, clone, a refused push) no label value equals a
  repository id, subject, reference, or path used by the fixture
  (proposed: `cmd/origod`, `TestMetricsVocabulary`, the presence part
  needing no fixture; `internal/metrics`, `TestRegisterNamesEveryMetric`
  comparing the registered names to the table of this spec read from
  the file, whose path is a test-only constant resolved from the test's
  own source file with `runtime.Caller`, never from the working
  directory, so the `tempdir` gate of spec 002, which runs the suite
  from an empty directory, and the `hermetic` gate see the spec file;
  spec 021's code-table test walks the module the same way, and the
  builder of each is told here).
- A push against an in-memory OTLP receiver produces one trace whose
  spans are the five named, with the repository id as an attribute
  (proposed: `cmd/origod`, `TestPushTrace`).
- The request log line for a push carries the listed fields and no
  `Authorization` value when the request used basic auth (proposed:
  `cmd/origod`, `TestRequestLogRedactsCredentials`).
- `promtool check rules deploy/base/prometheusrule.yaml` passes in the
  `specindex` job of `verify.yml`, which installs `promtool` and runs
  it, and every alert's metric names are in the table (proposed:
  `tools/specindex` reads the rule file; `verify.yml`, the `specindex`
  job).

## Outcome

Built on 2026-09-08 in eleven commits: `internal/tracing`, the metric
table in `internal/metrics`, one commit per recording package for the
move of its registrations, the node's telemetry wiring with the three
tests, the depcheck decision, the alert rules with the `specindex`
check, and the documentation.

| Criterion | Test |
|---|---|
| every name at 0 on the first scrape, no fixture label is a repository, subject, reference, or path | `cmd/origod`, `TestMetricsVocabulary`; `internal/metrics`, `TestRegisterNamesEveryMetric` reading this file through `runtime.Caller`, and `TestEveryClosedVocabularyReadsZero` |
| one push produces one trace with the five spans and the repository id as an attribute | `cmd/origod`, `TestPushTrace` against an in-memory OTLP receiver |
| the request log line carries the listed fields and no credential | `cmd/origod`, `TestRequestLogRedactsCredentials` |
| `promtool check rules` passes and every alert's metric is in the table | `tools/specindex`, `TestAlertRulesNameDefinedMetrics` and `TestRulesReportsAnUndefinedMetricAndAMalformedFile`; the `specindex` job of `verify.yml`, which installs `promtool` by pinned version and checksum |

The `Set` `internal/metrics` returns is the handles, one field per row,
and a field whose row is missing or has another type panics at start-up
rather than reading 0 forever. Each recording package's `Metrics` option
is that `Set`; a package built without one registers a set of its own,
so a test that asserts on nothing needs no registry.

Divergences and interpretations, all kept:

- A labelled histogram carries its family and no series before its first
  observation. `latere.ai/x/pkg/metrics` has no way to create a
  histogram cell with zero observations, only `Observe`, which would
  record one; a counter is seeded with an `Add` of 0 per label
  combination as the design says, and the cross product where a metric
  has two vocabularies. The pkg item below is the gap.
- `origo_requests_total` and `origo_request_duration_seconds` carry no
  series before the first request: `route` is the mux pattern, which has
  no vocabulary to seed. `TestMetricsVocabulary` asserts presence by
  name for every metric and a 0 series for every closed vocabulary.
- The handler is wrapped without `WithRouteTemplate`.
  `latere.ai/x/pkg/otel` passes that function to the span name formatter,
  which runs before the mux has matched, so a template returning the
  pattern would name every root span `<METHOD> ` with an empty route.
  Without it the same pattern still reaches the metrics hook, which is
  what the two labels needed.
- The route the hook and the log line use comes from a details struct the
  outermost wrapper installs on the request's context and a middleware
  behind the verifier fills. The verifier and the application mux each
  hand the next layer a request of their own, so the pattern the
  outermost wrapper sees is the public mux's catch-all `/`; the same
  struct carries the repository, subject, and actor, which are known
  only there. A probe, which passes neither, keeps the hook's own route.
- The log line is written by a wrapper of `cmd/origod` inside
  `otel.Handler`, not by `otel.Handler`, which logs nothing.
- `promtool check rules` reads a Prometheus rules file, not a Kubernetes
  object: run on `deploy/base/prometheusrule.yaml` it fails on
  `apiVersion`, `kind`, `metadata`, and `spec`. `tools/specindex -rules`
  prints the rules document inside the object, and the job checks that.
  The same flag fails first when an alert names an Origo metric no spec
  defines; a metric another exporter publishes, which is the autoscaler
  row, does not start with the prefix and is not checked.
- `deploy/base/prometheusrule.yaml` is not a resource of the base's
  kustomization. A PrometheusRule needs the Prometheus operator's
  CustomResourceDefinition, which Origo does not require and the kind
  stack does not install, so a base that named it would fail to apply on
  every installation without that operator. An installation that runs
  the operator applies the file beside the base, which
  `docs/operations.md` says.
- A read's spans, `index.check`, `materialize`, and `git.<command>`, are
  not built. No criterion names them, and each is an edit in the two
  packages spec 006 is being built in; the five spans of the write path,
  which the criterion names, are there.
- Every object storage call is a child span through `otel.Transport` on
  the storage transport, named by its HTTP method. A span named by the
  operation belongs with the store adapter spec 015 builds, which is
  what owns `origo_storage_ops_total{op}`.
- The refused push of the vocabulary fixture is a push to a repository
  that does not exist, answered `repo_not_found`. A push the log refuses
  cannot be produced with the git client against a single node: git
  takes the old value of every update from the server's own
  advertisement, so a stale value never reaches the node; the test that
  produces one builds the request body itself
  (`internal/httpgit`, `TestReferenceMovedBetweenAdvertisementAndPush`).
- The bucket sets of `origo_compaction_seconds` and
  `origo_storage_seconds` are this spec's choice, which the table left
  open: 0.5 s to 10 minutes for a compaction, the shared duration
  buckets for a storage call.
- The outbound transport to the issuers and the authorizer is not
  wrapped; the Traces section names the storage transport only.

Both builder items are closed: spec 010's `request_id` is the trace id
of the request's span, a fresh UUID when nothing traced it, which is the
common case at the default sampling ratio; spec 005's registrations are
in the table here.

`latere.ai/x/pkg`, two items:

- `pkg/metrics` has no way to register a labelled histogram's series at
  zero. `Registry.Histogram` returns a family and `Histogram.Observe` is
  the only way to create a cell, so a histogram with a label vocabulary
  cannot read 0 per value before its first observation the way a counter
  can. An `Init(labels)` on `Histogram`, or a variant of `Histogram`
  taking the vocabulary, would close it.
- `pkg/otel` has no tracer. It bootstraps the exporters, wraps a handler,
  wraps a transport, and reads the ids off a context, but exposes no
  `Start`, so a consumer that needs a child span imports
  `go.opentelemetry.io/otel` and `otel/trace` itself and its own
  dependency gate has to admit them. Origo confines that to
  `internal/tracing`; a `Start(ctx, name, attrs...)` in `pkg/otel` would
  keep the SDK behind the library for every consumer.

The OpenTelemetry SDK is on the node's build list from this spec, which
the Current state above says it would be: `.lateregate.yaml` names it in
the `depcheck` decision beside `latere.ai/x/pkg`, with one allowance per
upstream root the OTLP exporters reach. Spec 001's seventh invariant is
amended to name it as the one further direct dependency, and the Traces
section above states which packages may import it: `internal/tracing`
alone, and `cmd/origod` through `latere.ai/x/pkg/otel`. The decisions
table of `specs/README.md` carries the rule.
