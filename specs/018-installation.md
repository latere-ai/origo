---
title: "Installation: running Origo on any Kubernetes with any S3 compatible bucket"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/007-authentication-and-delegation.md
  - specs/017-release-and-versioning.md
affects: [deploy/, docs/install.md, docs/configuration.md, cmd/origod/, internal/config/, Makefile]
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

`deploy/base` holds a Deployment, a Service, the headless gossip
Service, an Ingress, a PodDisruptionBudget, and a ServiceAccount.
`deploy/prod` sets the namespace `origo`; `deploy/bootstrap` holds the
Namespace and Secret templates. The Ingress assumes the `nginx` class
and the cert-manager issuer `letsencrypt-prod` with host `git.latere.ai`;
the Deployment sets `ORIGO_PUBLIC_URL` to `https://git.latere.ai` and
reads `origod-s3` and `origod-dev-token`. There is no HPA, no
PrometheusRule, no `deploy/examples`, no `origod check`, no
`docs/install.md`, and no `docs/configuration.md`; spec 002's table is
the only configuration reference. `docs/README.md` lists both pages as
planned.

## Design

### Requirements

| Requirement | Detail |
|---|---|
| Kubernetes | 1.29 or newer; a default storage class or nodes with local disk; an ingress controller; Pod Security admission at `restricted` on the namespace is supported and recommended |
| bucket | any S3 compatible endpoint that honours `If-None-Match: *` on `PUT` (spec 004), verified by `origod check`; MinIO, DigitalOcean Spaces, and AWS S3 known good; the bucket endpoint reachable by LFS clients or `ORIGO_S3_PUBLIC_ENDPOINT` set (spec 010) |
| identity | any OIDC issuer with discovery and JWKS; the operator registers one client for people and one for each service that will act on behalf of users |
| authorizer | an HTTP endpoint the operator runs (spec 007); a reference authorizer that allows everything for a fixed list of subjects ships in `deploy/examples/authorizer/` for a first installation |
| DNS and TLS | one hostname pointed at the ingress with a certificate the ingress holds |

### Manifests

`deploy/base` becomes provider-neutral and complete: Namespace,
ServiceAccount, Deployment with the cache volume, Service, the headless
gossip Service, Ingress without a class or an issuer annotation,
HorizontalPodAutoscaler (spec 005), PodDisruptionBudget, PrometheusRule
(spec 011), and a Secret template with every required variable. The
Latere values move out of the base into `deploy/prod`. An operator
writes an overlay with their hostname, ingress class, storage class or
local-volume choice, replica bounds, and the Secret with their bucket
and issuer values, and applies it with `kubectl apply -k`.
`deploy/examples/` carries three overlays tested in CI: `kind` (MinIO
in-cluster, the stub issuer and authorizer of spec 013),
`digitalocean`, and `aws`. Helm is not offered; a kustomize overlay is a
directory an operator can read.

### The check

`origod check` runs against the same configuration as `origod` and
prints one line per requirement, `ok <name>` or `fail <name>: <detail>`,
exiting 1 on any failure:

| Line | What passes |
|---|---|
| `bucket` | a listing under the prefix answers |
| `conditional-create` | a `PUT If-None-Match: *` on `origo/check/<uuid>` answers 200 and a second one 412; the key is deleted afterwards |
| `issuer` | each issuer's discovery document and JWKS are fetched |
| `authorizer` | a `POST` with `action: "read"` for a fixed probe repository id answers 200 with `allow` present |
| `events` | when `ORIGO_EVENTS_URL` is set, a `POST` with `Origo-Event: check` answers any status under 500 |
| `disk` | a file is created and removed under `ORIGO_DATA_DIR` and the file system holds at least `ORIGO_CACHE_BYTES` |
| `git` | `git --version` runs and reports 2.39 or newer |

It runs as an init container in the Deployment so a misconfigured pod
never reports ready, and the install document tells the operator to run
it first.

### Configuration reference

`docs/configuration.md` is generated from `internal/config` by
`make docs` and compared in the verify workflow: every variable of spec
002's table, its default, its constraints, and which subsystem reads it.
The install document links to it and never restates a value.

### The install document

`docs/install.md`: requirements, the five steps (create the bucket,
register the issuer clients, write the overlay, apply, run the check),
the first clone and push, how to point a consumer at the authorizer, and
where to look when something fails (the check's output, the alerts of
spec 011, `docs/operations.md`).

## Not in this spec

A Helm chart. An operator. Installation outside Kubernetes beyond the
binary artifact of spec 017.

## Acceptance criteria

- The `kind` example overlay installs in the CI stack from the release
  artifacts alone and `TestContract` passes against it (spec 013's
  stack; proposed: `.github/workflows/verify.yml`, the `install` job).
- `origod check` prints a `fail` line naming the requirement for each of:
  an unreachable bucket, a bucket that ignores conditional create (the
  `pkg/s3` fake with the option off), an unreachable issuer, an
  authorizer answering 500, an unwritable data directory, a missing git
  binary, and exits 1; with everything in place it prints seven `ok`
  lines and exits 0 (proposed: `cmd/origod`, `TestCheckReportsEachRequirement`).
- `make docs` regenerates `docs/configuration.md` byte-identical in the
  verify workflow (proposed: `internal/config`, `TestConfigurationDocIsCurrent`).
- A maintainer following `docs/install.md` on a fresh kind cluster
  reaches a successful push without consulting any other document, once
  per release, recorded in the release notes (spec 017).
