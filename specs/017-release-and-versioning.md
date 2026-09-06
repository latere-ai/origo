---
title: "Release and versioning: images, binaries, compatibility, and what a version promises"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
  - specs/013-conformance-suite.md
affects: [.github/workflows/, Dockerfile, Dockerfile.ci, CHANGELOG.md, docs/]
effort: small
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Release and versioning

## Overview

An operator outside Latere installs Origo from a release, not from a
checkout, and upgrades on their own schedule. This spec says what a
release contains, what its version number promises, and what an upgrade
may assume.

## Current state

A tag `v*` runs the shared release pipeline, which builds the image,
deploys Latere's installation, runs the smoke, and publishes a release
with the smoke evidence. There is no compatibility statement and no
binary artifact.

## Design

### Artifacts per release

| Artifact | Where | Notes |
|---|---|---|
| `ghcr.io/latere-ai/origod:<version>` | GHCR, multi-arch `linux/amd64` and `linux/arm64` | signed with cosign keyless; software bill of materials and build provenance attached |
| `origod_<version>_<os>_<arch>.tar.gz` | the GitHub release | for operators running outside Kubernetes; `linux` and `darwin`, both architectures; checksums file |
| `deploy-<version>.tar.gz` | the GitHub release | `deploy/base` rendered for that version, so an operator's overlay pins one artifact |
| release notes | the GitHub release | the CHANGELOG section for the version, plus the conformance run's evidence |

### Versioning

Semantic versioning on the tag. The number promises three things:

| Change | Bump |
|---|---|
| a removal or semantic change in the protocol contract (spec 003), the log format (spec 004), or a configuration variable | major; the previous contract number stays served for twelve months |
| a new capability, endpoint, variable, or metric | minor |
| a fix with no visible change | patch |

The log format carries its own version field in every entry and index
(`v: 1`). A node reads every version it has ever written and writes the
newest; a major bump of the log format ships a migration that rewrites a
repository's log on first access and is covered by the conformance suite
against a fixture written by the previous release.

### Upgrade

Any release upgrades from any earlier release in the same major without
steps. A major upgrade documents its steps in `docs/upgrades/<major>.md`
and refuses to start against a log it cannot read, with the message
naming the document. Rolling back to the previous release is supported
within a minor series because the log format did not change; across a
major it is not.

### Cadence and support

Minor releases as features land; patch releases for fixes; the two most
recent minor series receive patches. A release is cut only from a green
main with the conformance suite passed against the release candidate
image in the kind stack.

## Acceptance criteria

- A tag produces every artifact in the table, the image verifies with
  cosign, and the release notes equal the CHANGELOG section.
- A fixture repository written by release N-1 materializes and serves on
  release N (conformance suite, run against the previous tag's fixture).
- Starting a node against a log with a higher major format version fails
  with the documented message.
