---
title: "Repository scaffold: module, binary, configuration, quality gate, release"
status: testing
track: infra
depends_on:
  - specs/001-architecture.md
affects: [cmd/origod/, internal/config/, Makefile, .lateregate.yaml, Dockerfile, Dockerfile.ci, deploy/, .github/workflows/]
effort: small
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Repository scaffold

## Overview

A compiling, testable, releasable repository before any protocol code
exists: the Go module, the binary, typed configuration, the quality gate,
the container image, the release pipeline, and the deploy manifests. The
shape is chosen so the repository reads as an ordinary open source Go
service to a newcomer and passes the same bar as every Latere service.

## Current state

The repository holds a README, a license, the spec deck, a `go.mod` with
the gate as a tool dependency, and one workflow that runs the gate.

## Design

### Layout

```
cmd/origod/             main: configuration, listeners, run group
internal/config/        typed configuration from environment; missing keys fail startup with one message
internal/wal/           write-ahead log client: entries, index, compare-and-swap (spec 004)
internal/repo/          local repository cache: materialize, evict, git subprocess wrapper (spec 004, 005)
internal/placement/     rendezvous hashing, gossip (spec 005)
internal/compact/       compaction (spec 006)
internal/auth/          token verification, authorizer, delegation (spec 007)
internal/httpgit/       smart HTTP: info/refs, upload-pack, receive-pack
internal/api/           JSON API: repositories, refs, log, diff, tree, blob, archive (spec 009)
internal/lfs/           batch API and presigned transfer (spec 010)
internal/events/        push events (spec 008)
internal/limits/        quotas and rate limits (spec 012)
deploy/base|prod|bootstrap/  manifests: Deployment with a local cache volume, Service, Ingress,
                        HorizontalPodAutoscaler and PodDisruptionBudget (spec 005), PrometheusRule (spec 011)
test/e2e/               conformance suite (spec 013)
test/conformance/       the suite as an importable package for consumers' stubs
Makefile, make/         the gate: go tool lateregate from .lateregate.yaml
Dockerfile, Dockerfile.ci
```

### Binary and listeners

| Listener | Port | Serves |
|---|---|---|
| public | `:8080` | `/<owner>/<repo>.git/*` smart HTTP, `/v1/*` JSON API, LFS |
| internal | `:8081` | `/livez`, `/readyz`, `/metrics` |
| gossip | `:7946/udp` | node to node sequence announcements (spec 005) |

Readiness requires object storage reachable and the local disk writable.

### Configuration

| Variable | Required | Purpose |
|---|---|---|
| `ORIGO_S3_ENDPOINT`, `ORIGO_S3_REGION`, `ORIGO_S3_BUCKET`, `ORIGO_S3_KEY`, `ORIGO_S3_SECRET` | yes | the bucket; prefix `origo/` fixed; path-style when `ORIGO_S3_PATH_STYLE=1` |
| `ORIGO_DATA_DIR` | no | `/var/lib/origo` default; must be a local disk, not a network file system |
| `ORIGO_CACHE_BYTES` | no | eviction ceiling for the repository cache, default 80% of the disk |
| `ORIGO_PUBLIC_URL` | yes | `https://git.example.com`, used in clone URLs and event payloads |
| `ORIGO_OIDC_ISSUERS` | yes | comma separated issuer URLs whose tokens are accepted (spec 007) |
| `ORIGO_AUTHORIZER_URL`, `ORIGO_AUTHORIZER_TOKEN` | yes | the consumer's authorization endpoint (spec 007) |
| `ORIGO_EVENTS_URL`, `ORIGO_EVENTS_SECRET` | no | push event sink and HMAC key (spec 008) |
| `ORIGO_NODE_NAME` | no | pod name by default; the identity used in gossip and placement |
| `ORIGO_GOSSIP_PEERS` | no | a DNS name that resolves to all nodes; the headless Service in Kubernetes |
| `OTEL_*` | no | standard exporter configuration |
| `ORIGO_STORAGE_TIMEOUT` | no | per object operation, default 10 seconds; the circuit breaker of spec 015 opens on repeated timeouts |

### Quality bar

Bare `make` runs `go tool lateregate`: format, vet, lint, modernize, per
package coverage floor 90%, the suite with only the toolchain on `PATH`,
the suite against an empty `TMPDIR`, the spec tree. `git` is the one
binary the suite may require beyond the toolchain, declared in the
hermetic allow list with the reason. Fuzz tests cover every parser that
reads bytes from a client: pkt-line, the index, entry headers.

### Release

A tag `v*` runs the reusable release pipeline: build the binary, package
it with `Dockerfile.ci`, push `ghcr.io/latere-ai/origod:<tag>`, apply
`deploy/prod/`, wait for rollout, run the conformance suite against the
live service, publish the release with the evidence attached. Direct
pushes to `main` run verify only. Both callers are thin files under
`.github/workflows/`.

## Acceptance criteria

- `make` passes on a clean checkout with no protocol code.
- `origod` starts against MinIO and an empty disk, serves `/readyz` 200,
  and refuses to start without the bucket variables with one message that
  names every missing key.
- Coverage of `internal/config` is 100%.
- A tag on a fork builds and publishes the image.

## Outcome

Phase 1 shipped the scaffold on 2026-09-06. What runs: `cmd/origod`
with typed configuration from `internal/config` (100% covered, one
start-up message names every missing key), the public listener on
`:8080`, the internal listener on `:8081` with `/livez`, `/readyz`,
`/version`, and `/metrics`, and the gossip UDP port `:7946` that reads
and discards datagrams until spec 005 gives them a meaning. Readiness
reports object storage (one listing) and the disk. The Makefile is the
gate binary's entry point plus `build`, `fmt`, `hooks`, `dev`,
`test-integration`, and `clean`; `docker-compose.yml`, `Dockerfile`, and
`Dockerfile.ci` follow the Latere service template; `deploy/base`,
`deploy/prod`, and `deploy/bootstrap` carry the manifests; a `v*` tag
runs the shared release pipeline and `tools/smoke/release.sh` checks
`/readyz` and `/version`. No tag was cut.

Acceptance: `make` passes on the checkout; `origod` starts against
MinIO with an empty disk and serves `/readyz` 200; without the bucket
variables it refuses with one message naming every missing key;
`internal/config` is at 100%. "A tag on a fork builds and publishes the
image" is unverified: no tag was cut in phase 1.

Divergences from this spec:

- Authentication is a phase 1 stand-in: the public listener accepts one
  static bearer from `ORIGO_DEV_TOKEN` (as a Bearer header or as git's
  basic auth password) and refuses everything else. `ORIGO_DEV_TOKEN` is
  required until spec 007 lands; `ORIGO_OIDC_ISSUERS`,
  `ORIGO_AUTHORIZER_URL`, and `ORIGO_AUTHORIZER_TOKEN` are read but
  optional. The code is marked in `internal/auth`.
- Variables added beyond the table: `ORIGO_PUBLIC_ADDR`,
  `ORIGO_INTERNAL_ADDR`, `ORIGO_GOSSIP_ADDR` (the spec's ports as
  defaults, so a test binds an ephemeral port), `ORIGO_SWEEP_INTERVAL`
  and `ORIGO_SWEEP_MIN_AGE` (spec 004's values as defaults), and
  `ORIGO_FAILPOINT` (empty in every deployment; the end-to-end suite
  kills a node with it). `ORIGO_CACHE_BYTES` is read and resolved but
  eviction is spec 005.
- `/readyz` and `/version` are also served on the public listener so
  the release smoke reaches them through the ingress; `/livez` and
  `/metrics` stay internal.
- The runtime image is Debian slim with git, not the static distroless
  base of the template: origod runs git as a subprocess. The hermetic
  allow list admits `/usr/bin` for the same reason.
- Layout additions beyond the table: `internal/contract` (the error
  envelope and `Origo-Contract` header of spec 003), `internal/metrics`
  (counters and histograms in the Prometheus text format, no client
  library), and `internal/gittest` (test support over the real git).
- Moved to `latere.ai/x/pkg` once a second consumer existed: the error
  envelope is `httpjson.Error` (the codes and the `Origo-Contract`
  header stay in `internal/contract`), the metrics registry is
  `pkg/metrics`, the probes on the internal listener are `pkg/health`
  (`/livez`, `/readyz`, `/version`, `/metrics`, text bodies), and the
  cancellable sleep and the sweep ticker are `pkg/wait`;
  `internal/metrics` is gone.
  `internal/placement`, `compact`, `lfs`, `events`, `limits`, and
  `test/conformance` do not exist yet; they land with their specs.
- `make/` fragments were not adopted: latere-ai/ci-gate's own Makefile is
  the reference shape, and every gate-named target lives in the gate.
- The integration tiers (`make test-integration`: the store suite on
  MinIO and the end-to-end suite) are not run by CI: latere-ai/ci's
  `lateregate.yml` has no services step. CI runs the unit tiers, which
  cover every package at 90% or more on an in-process store.
