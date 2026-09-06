---
title: "Installation: running Origo on any Kubernetes with any S3 compatible bucket"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/007-authentication-and-delegation.md
  - specs/017-release-and-versioning.md
affects: [deploy/, docs/install.md, docs/configuration.md, cmd/origod/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Installation

## Overview

An operator with a cluster and a bucket installs Origo in under an hour
from the documentation alone, without reading a spec or asking Latere.
This spec fixes what they need, the manifests they apply, the one command
that tells them whether the installation is sound, and the configuration
reference that lists every variable with its default.

## Current state

`deploy/base` and `deploy/prod` exist for Latere's installation and assume
its ingress class, its issuer, and its bucket. There is no install
document and no configuration reference beyond spec 002's table.

## Design

### Requirements

| Requirement | Detail |
|---|---|
| Kubernetes | 1.29 or newer; a default storage class or nodes with local disk; an ingress controller; Pod Security admission at `restricted` on the namespace is supported and recommended |
| bucket | any S3 compatible endpoint that honours `If-None-Match: *` on `PUT` (spec 004); verified by `origod check` below; MinIO, DigitalOcean Spaces, AWS S3 known good; Azure Blob through an S3 gateway |
| identity | any OIDC issuer with discovery and JWKS; the operator registers one client for people and one for each service that will act on behalf of users |
| authorizer | an HTTP endpoint the operator runs (spec 007); a reference authorizer that allows everything for a fixed list of subjects ships in `deploy/examples/authorizer/` for a first installation |
| DNS and TLS | one hostname pointed at the ingress with a certificate the ingress holds |

### Manifests

`deploy/base` is provider-neutral and complete: Namespace, ServiceAccount,
Deployment with the cache volume, Service, Ingress, HorizontalPodAutoscaler,
PodDisruptionBudget, PrometheusRule, and a Secret template. An operator
writes an overlay with their hostname, ingress class, storage class or
local-volume choice, replica bounds, and the Secret with their bucket and
issuer values, and applies it with `kubectl apply -k`. `deploy/examples/`
carries three overlays that are tested in CI: `kind` (MinIO in-cluster,
the stub issuer), `digitalocean`, and `aws`. Helm is not offered; a
kustomize overlay is a directory an operator can read.

### The check

`origod check` runs against the configuration and reports one line per
requirement: bucket reachable and conditional create honoured, issuer
discovery and JWKS reachable, authorizer answering, events sink
answering when configured, disk writable with the configured ceiling,
git binary present at a supported version. Exit non-zero on any failure.
It runs as an init container in the Deployment so a misconfigured pod
never reports ready, and the install document tells the operator to run
it first.

### Configuration reference

`docs/configuration.md` is generated from `internal/config` by
`make docs` and checked in CI for drift: every variable, its default, its
constraints, and which listener or subsystem reads it. The install
document links to it and never restates a value.

### The install document

`docs/install.md`: requirements, the five steps (create the bucket,
register the issuer clients, write the overlay, apply, run the check),
the first clone and push, how to point a consumer at the authorizer, and
where to look when something fails (the check's output, the alerts of
spec 011, `docs/operations.md`).

## Acceptance criteria

- The `kind` example overlay installs in the CI stack from the release
  artifacts alone and the conformance suite passes against it.
- `origod check` fails with a named line for each of: an unreachable
  bucket, a bucket that ignores conditional create (the fake with the
  option off), an unreachable issuer, an authorizer answering 500, an
  unwritable data directory, a missing git binary.
- `docs/configuration.md` regenerates byte-identical in CI.
- A person following `docs/install.md` on a fresh kind cluster reaches a
  successful push without consulting any other document; this is
  exercised once per release by a maintainer and recorded in the notes.
