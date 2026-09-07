---
title: "Release and versioning: images, binaries, compatibility, and what a version promises"
status: validated
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
  - specs/013-test-stubs-and-kind-overlay.md
  - specs/021-conformance-suite.md
affects: [.github/workflows/, Dockerfile, Dockerfile.ci, CHANGELOG.md, tools/smoke/, docs/upgrades/, internal/wal/, internal/repo/, internal/version/, cmd/origod/, test/conformance/]
effort: small
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Release and versioning

## Overview

An operator outside Latere installs Origo from a release, not from a
checkout, and upgrades on their own schedule. This spec says what a
release contains, what its version number promises, and what an upgrade
may assume.

## Current state

`.github/workflows/release.yml` is a thin caller of `service-release.yml`
in `latere-ai/ci` on a `v*` tag: that pipeline builds one `linux/amd64`
binary, packages it with `Dockerfile.ci`, pushes
`ghcr.io/latere-ai/origod:<tag>`, applies `deploy/prod/`, waits for the
rollout, runs `tools/smoke/release.sh`, and publishes the GitHub
release with the smoke evidence and the `CHANGELOG.md` section. It
produces none of the other artifacts in the table below: no second
architecture, no binary archives, no signature, no bill of materials,
no provenance, no deploy archive. `CHANGELOG.md` has an `Unreleased`
section. No tag has been cut, so the pipeline has never run for Origo.
`internal/wal` writes `v: 1` in every header and index and refuses any
other version, and nothing maps that refusal to a response code.
`docs/upgrades/` is empty. There is no compatibility statement.

Two version mechanisms exist, for the builder to reduce to one:
`internal/version.Version`, set by the `-ldflags` of spec 002's
`Makefile`, and `main.version` in `cmd/origod/main.go`, set by the
shared pipeline with `-X main.version=<tag>` and copied over the first
at start-up when non-empty. `main.version` is removed under this spec;
`release.yml` below sets `internal/version.Version`, `Commit`, and
`Date` with the same `-ldflags` the `Makefile` uses, so a binary from
the pipeline and one from `make build` carry their identity the same
way and `GET /version` has one source.

Known defect the first tag will hit, which the builder fixes under this
spec with the shell test the criteria propose: after `GET /readyz`
answers 200, `tools/smoke/release.sh` runs `grep -qx "ok"` with no
file operand, which reads standard input; under `set -e` it exits 1
whenever nothing is piped in, which is every pipeline run. The fix
greps the saved body, `grep -qx ok "$tmp/readyz"`. The script is not
changed by this spec's text.

## Design

### Artifacts per release

| Artifact | Where | Notes |
|---|---|---|
| `ghcr.io/latere-ai/origod:<version>` | GHCR, `linux/amd64` and `linux/arm64` | `Dockerfile.ci` over the shared runtime stage of spec 002: `debian:trixie-slim` pinned by digest, which ships git 2.47, above the 2.40 floor `origod check` (spec 018) enforces; signed with cosign keyless; an SPDX bill of materials and SLSA provenance attached as referrers |
| `origod_<version>_<os>_<arch>.tar.gz` | the GitHub release | `linux` and `darwin`, `amd64` and `arm64`; `checksums.txt` with SHA-256 sums, signed |
| `deploy-<version>.tar.gz` | the GitHub release | `deploy/base` and `deploy/examples` with the image pinned to the version, so an operator's overlay references one artifact |
| `fixture-<version>.tar.gz` | the GitHub release | the bucket prefix `origo/repos/<id>/` of a fixture repository pushed through the candidate image in the `kind` stack, so the next release can prove it reads what this one wrote |
| release notes | the GitHub release | the `CHANGELOG.md` section for the version, the smoke evidence, and the conformance run's timings (spec 021) |

`GET /version` on a released node serves the tag as `version`.

### The pipeline

`release.yml` in this repository produces every artifact itself and
calls no shared workflow: `service-release.yml` of `latere-ai/ci` runs
its deploy and smoke as one job it does not expose as a separate
callable step, so the two steps are written inline here, the same
`kubectl` and `tools/smoke/release.sh` invocations the shared workflow
makes. On a `v*` tag:

1. `build`: `go build` for the four `os/arch` pairs with the `-ldflags`
   of spec 002 setting `internal/version`, the archives and
   `checksums.txt`; `docker buildx` of `Dockerfile.ci` for
   `linux/amd64` and `linux/arm64` pushed as one multi-arch image;
   `cosign sign` keyless with the workflow's OIDC identity on the image
   and on `checksums.txt`; an SPDX bill of materials from the module
   graph and the image, attached with `attest-sbom`; provenance with
   `attest-build-provenance`; the deploy archive from `deploy/base` and
   `deploy/examples` with the image pinned.
2. `conformance`: the `e2e` job of spec 013 against the candidate
   image, running `TestContract` of spec 021, which also pushes the
   fixture repository, and `TestPreviousReleaseFixture` below against
   the fixture of the previous release; the harness then reads every
   object under that repository's prefix from the stack's MinIO through
   `ORIGO_S3_PUBLIC_ENDPOINT` (the host port of spec 013's overlay
   table) and packs them as `fixture-<version>.tar.gz`; a failure stops
   the release.
3. `deploy`: runs only when the repository variable
   `ORIGO_RELEASE_DEPLOY` (spec 002) is set: `kubectl`, with the
   kubeconfig held in the repository secret `ORIGO_KUBECONFIG` (spec
   002), runs `apply -k deploy/prod/` with the image pinned to the tag
   and `rollout status` with a 10 minute wait, then
   `tools/smoke/release.sh` runs against the public URL, whose markdown
   output is the evidence. Latere sets the variable and the secret on
   its own repository; a fork does not, so a tag on a fork publishes
   every artifact and skips this step.
4. `live`: the live run of spec 021, `TestContract` against
   `ORIGO_LIVE_URL` with `ORIGO_LIVE_TOKEN` (spec 002) and that spec's
   four-entry skip list, after step 3 and skipped when the secret is
   unset; its report and timings are attached to the release.
5. `publish`: the GitHub release with every artifact, the `CHANGELOG.md`
   section as the body, the smoke evidence when step 3 ran, the
   conformance timings of step 2, and the live report of step 4 when
   it ran.
6. `release-verify`: from a clean runner, `cosign verify` on the image
   and the checksums with the workflow identity, `sha256sum -c
   checksums.txt` over the downloaded archives, `gh attestation verify`
   on the image, and the release body compared with the changelog
   section.

### Versioning

Semantic versioning on the tag. The number promises:

| Change | Bump |
|---|---|
| a removal or semantic change in the protocol contract (spec 003), the log format (spec 004), a configuration variable, or an event payload | major; the previous contract number stays served for twelve months, selected by a request header `Origo-Contract: <n>` |
| a new capability, endpoint, field, variable, metric, or event kind | minor |
| a fix with no visible change | patch |

The log format carries its version in every entry header and index
object (`v: 1`). A node reads every version it has ever written and
writes the newest; a major bump of the log format ships a migration
that rewrites a repository's log on first access and is covered by the
conformance suite against the fixture the previous release attached.
The fixture is downloaded from that release's assets when the test
runs and is never committed to the tree.

### Upgrade

Any release upgrades from any earlier release in the same major without
steps: the Deployment rolls one pod at a time with none unavailable, a
replaced pod starts cold and warms, and there is no schema. A major
upgrade documents its steps in `docs/upgrades/<major>.md`; a node that
finds an index object or entry header with a `v` above what it reads
refuses to serve that repository with the log line
`log format v<n> is newer than this release reads; see docs/upgrades/<major>.md`
and 503 `repository_unavailable` (spec 015) with `details.key` naming
the object, because the bucket is healthy and one repository is what
cannot be served; `storage_unavailable` would tell the client to retry
in a few minutes, which does not help. `/readyz` stays 200 so the rest
of the installation serves. Rolling back to the previous release is
supported within a minor series; across a major it is not.

### Cadence and support

Minor releases as features land; patch releases for fixes; the two most
recent minor series receive patches. A release is cut only from a green
`main` with the conformance suite (spec 021) passed against the
candidate image in the kind stack (spec 013).

### Release checklist

What no job proves and a maintainer does by hand before the tag, each
recorded in the release notes as done or as not applicable:

| Item | Spec |
|---|---|
| a tag on a fork with `ORIGO_RELEASE_DEPLOY` unset publishes every artifact and skips the deploy and smoke step; done once for the first release and again when `release.yml` changes | this spec |
| the create race, `HEAD` 404, and `GET` 304 rows of `tools/spike/condwrite` pass on DigitalOcean Spaces with the current build | 004 |
| `docs/install.md` walked on a fresh kind cluster from the release artifacts alone, reaching a push without another document | 018 |

## Not in this spec

A Helm chart. A public container registry other than GHCR. Signing
with a key held by Latere; keyless signing binds the artifacts to the
workflow identity, which is what an outside operator can verify.

## Acceptance criteria

- A tag produces every artifact in the table for both architectures,
  `cosign verify` and `gh attestation verify` accept the image,
  `sha256sum -c checksums.txt` passes on the binaries, and the release
  body equals the `CHANGELOG.md` section (proposed: the
  `release-verify` job in `release.yml`).
- A tag on a fork with `ORIGO_RELEASE_DEPLOY` unset publishes every
  artifact and skips the deploy and smoke step, and the same tag with
  the variable set runs it: a release checklist item above, done by a
  maintainer by tagging a fork and recorded in the release notes, not
  a CI test, because CI cannot tag a fork of itself (`release.yml`, the
  `deploy` job's `if` on the variable).
- The fixture repository attached to release N-1 materializes and
  serves on release N with identical `rev-list --all`, the fixture
  downloaded from that release's assets at test time and skipped when
  no previous release exists (proposed: `test/conformance`,
  `TestPreviousReleaseFixture`).
- A node reading an index object with `v: 2` answers 503
  `repository_unavailable` with `details.key` for that repository, logs
  the documented line, serves another repository, and stays ready
  (proposed: `internal/repo`, `TestNewerLogFormatIsRefused`).
- `tools/smoke/release.sh` passes against a stub server whose
  `GET /readyz` answers `ok` and `GET /version` serves `TAG`, with
  standard input closed, and fails naming the mismatch when the version
  differs (proposed: `tools/smoke/release_test.sh`, run by the `test`
  gate through a Go test in `tools/smoke` that executes it).
- `main.version` no longer exists: a binary built with
  `-X github.com/latere-ai/origo/internal/version.Version=v1.2.3`
  serves `v1.2.3` on `GET /version` and prints it for `-version`, and
  `grep -r 'main.version' cmd/` finds nothing (proposed: `cmd/origod`,
  `TestVersionHasOneSource`).
