---
title: "Release and versioning: images, binaries, compatibility, and what a version promises"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
  - specs/013-conformance-suite.md
affects: [.github/workflows/, Dockerfile, Dockerfile.ci, CHANGELOG.md, tools/smoke/, docs/upgrades/, internal/wal/, cmd/origod/]
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

`.github/workflows/release.yml` runs `service-release.yml` of
`latere-ai/ci` on a `v*` tag: build, package with `Dockerfile.ci`, push
`ghcr.io/latere-ai/origod:<tag>`, apply `deploy/prod/`, wait for the
rollout, run `tools/smoke/release.sh`, publish the GitHub release with
the smoke evidence and the `CHANGELOG.md` section. `CHANGELOG.md` has an
`Unreleased` section. No tag has been cut, so the pipeline has never run
for Origo. `internal/wal` writes `v: 1` in every header and index and
refuses any other version. `docs/upgrades/` is empty. There is no
compatibility statement, no binary artifact, no signature, no bill of
materials. Known defect the first tag will hit: after `GET /readyz` answers 200,
`tools/smoke/release.sh` runs `grep -qx "ok"` with no file, which reads
standard input and exits 1 under `set -e` when nothing is piped in; the
smoke must grep the saved body or check the status code alone.

## Design

### Artifacts per release

| Artifact | Where | Notes |
|---|---|---|
| `ghcr.io/latere-ai/origod:<version>` | GHCR, `linux/amd64` and `linux/arm64` | signed with cosign keyless; an SPDX bill of materials and SLSA provenance attached as referrers |
| `origod_<version>_<os>_<arch>.tar.gz` | the GitHub release | `linux` and `darwin`, `amd64` and `arm64`; `checksums.txt` with SHA-256 sums, signed |
| `deploy-<version>.tar.gz` | the GitHub release | `deploy/base` and `deploy/examples` with the image pinned to the version, so an operator's overlay references one artifact |
| release notes | the GitHub release | the `CHANGELOG.md` section for the version, the smoke evidence, and the conformance run's timings (spec 013) |

`GET /version` on a released node serves the tag as `version`.

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
conformance suite against a fixture written by the previous release.

### Upgrade

Any release upgrades from any earlier release in the same major without
steps: the Deployment rolls one pod at a time with none unavailable, a
replaced pod starts cold and warms, and there is no schema. A major
upgrade documents its steps in `docs/upgrades/<major>.md`; a node that
finds an index object or entry header with a `v` above what it reads
refuses to serve that repository with the log line
`log format v<n> is newer than this release reads; see docs/upgrades/<major>.md`
and 503 `storage_unavailable`, and `/readyz` stays 200 so the rest of
the installation serves. Rolling back to the previous release is
supported within a minor series; across a major it is not.

### Cadence and support

Minor releases as features land; patch releases for fixes; the two most
recent minor series receive patches. A release is cut only from a green
`main` with the conformance suite passed against the candidate image in
the kind stack (spec 013).

## Not in this spec

A Helm chart. Release automation beyond `latere-ai/ci`. A public
container registry other than GHCR.

## Acceptance criteria

- A tag produces every artifact in the table, `cosign verify` accepts
  the image, `sha256sum -c checksums.txt` passes on the binaries, and
  the release body equals the `CHANGELOG.md` section (proposed: a
  `release-verify` job in `release.yml` that runs after publish).
- A tag on a fork builds and publishes the image and the smoke passes
  against it (moved from spec 002; the first tag verifies it).
- A fixture repository written by release N-1 materializes and serves
  on release N (proposed: `test/conformance`, `TestPreviousReleaseFixture`
  over a fixture the release pipeline stores under `test/conformance/fixtures/<tag>/`).
- A node reading an index object with `v: 2` answers 503
  `storage_unavailable` for that repository, logs the documented line,
  and stays ready (proposed: `internal/repo`, `TestNewerLogFormatIsRefused`).
- `tools/smoke/release.sh` passes against a node whose `GET /readyz`
  answers `ok` (proposed: `tools/smoke`, a shell test over a stub server).
