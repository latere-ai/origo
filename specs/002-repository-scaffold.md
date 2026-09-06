---
title: "Repository scaffold: module, binary, configuration, quality gate, release"
status: validated
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
deploy/base|prod|bootstrap/  manifests
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
