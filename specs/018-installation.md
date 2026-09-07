---
title: "Installation: running Origo on any Kubernetes with any S3 compatible bucket"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/005-placement-and-replication.md
  - specs/007-authentication-and-delegation.md
  - specs/011-observability.md
  - specs/013-test-stubs-and-kind-overlay.md
  - specs/017-release-and-versioning.md
  - specs/021-conformance-suite.md
affects: [deploy/, docs/install.md, docs/configuration.md, cmd/origod/, internal/config/, Makefile, .github/workflows/]
effort: medium
created: 2026-09-06
updated: 2026-09-08
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
| identity | any OIDC issuer with discovery and JWKS over HTTPS (spec 007; plain HTTP only for the stub in the kind overlay); the operator registers one client for people and one for each service that will act on behalf of users |
| authorizer | an HTTP endpoint the operator runs (spec 007), which must deny the probe id spec 007's authorizer contract reserves; for a first installation the stub authorizer of spec 013 (`origo-stubs -allow <subjects>`, which allows a fixed list of subjects and denies the probe id) runs from the manifest the `kind` overlay carries, copied into the operator's overlay |
| DNS and TLS | one hostname pointed at the ingress with a certificate the ingress holds |

### Manifests

`deploy/base` becomes provider-neutral and complete: Namespace,
ServiceAccount, Deployment with the cache volume, Service, the headless
gossip Service, the NetworkPolicy on the gossip port (spec 016),
Ingress without a class or an issuer annotation,
HorizontalPodAutoscaler (spec 005), PodDisruptionBudget, PrometheusRule
(spec 011), and a Secret template with every required variable,
`ORIGO_GOSSIP_SECRET` (spec 005) and `ORIGO_TOKEN_KEY` (spec 007) among
them. The HorizontalPodAutoscaler scales on CPU only, in the base and
in every example overlay; no overlay installs a metrics adapter, and
`origo_requests_in_flight` (spec 011) is a dashboard signal, not an
autoscaler input (spec 005). The
Latere values move out of the base into `deploy/prod`. An operator
writes an overlay with their hostname, ingress class, storage class or
local-volume choice, replica bounds, and the Secret with their bucket
and issuer values, and applies it with `kubectl apply -k`.
`deploy/examples/` carries three overlays: `kind` (MinIO in-cluster,
the stubs of spec 013, owned by that spec and applied in CI on every
push), `digitalocean`, and `aws`. The two cloud overlays are validated
by `kustomize build` in CI only; nothing applies them there, because
CI has no cloud account, and the install document says so. Helm is not
offered; a kustomize overlay is a directory an operator can read.

The `install` job in `verify.yml` walks the install document's steps
against the `kind` overlay with what an operator would have: it runs
the fenced `sh` blocks of `docs/install.md` in order through
`tools/docs/run-blocks.sh`, the script spec 013 owns under "Documents
as tests" and spec 014 uses for its migration document, so the
document is the test and a step that drifts from the manifests fails
the job. On every push it uses the candidate build of
that push, the image the `e2e` job of spec 013 built, because there is
no release for it; on a `v*` tag it uses the release artifacts of spec
017, the signed image and `deploy-<version>.tar.gz`, downloaded from
the release, so the tag proves the documented install works from the
artifacts alone; the document reads the image reference and the
manifest path from two variables the job sets, and names the release
values as their defaults. Both runs end with `TestContract` (spec 021)
against the installed nodes.

### The check

`origod check` runs against the same configuration as `origod` and
prints one line per requirement, `ok <name>` or `fail <name>: <detail>`,
exiting 1 on any failure:

| Line | What passes |
|---|---|
| `bucket` | a listing under the prefix answers |
| `conditional-create` | a `PUT If-None-Match: *` on `origo/check/<uuid>` answers 200 and a second one 412; the key is deleted afterwards. With `ORIGO_CHECK_SELFTEST=1` (spec 002) the check runs against an in-process HTTP server inside `origod check` that accepts every `PUT` and ignores the header, so the line must read `fail conditional-create: second create answered 200`; that is how the check's own detection is tested, since `pkg/s3/s3test` always honours the header |
| `issuer` | each issuer's discovery document and JWKS are fetched |
| `authorizer` | a `POST` with `action: "read"`, an empty subject, and the probe repository id `00000000-0000-0000-0000-000000000001` answers 200 with `allow: false`; spec 007's authorizer contract reserves that id and requires the deny, so an allow is `fail authorizer: probe id allowed`, and the stub of spec 013 denies it |
| `events` | when `ORIGO_EVENTS_URL` is set, a signed `ping` event (below) answers any status under 500 |
| `disk` | a file is created and removed under `ORIGO_DATA_DIR` and the file system holds at least `ORIGO_CACHE_BYTES` |
| `git` | `git --version` runs and the version parsed from its output (`git version 2.47.1`, the first three dot-separated numbers after the second word) is 2.40 or newer, the floor spec 020's merge family needs; the released image carries 2.47 (spec 002, Images), and the floor stays at what the feature needs, not at what the image ships |

| Event | Payload |
|---|---|
| `ping` | `{"id", "kind": "ping", "at"}`, sent by `origod check` with the headers of spec 008 (`Origo-Event: ping`, `Origo-Signature` over the body, `Origo-Delivery` equal to `id`); a sink treats it as a delivery to acknowledge and nothing else |

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

- The `kind` example overlay installs in the CI stack from the
  candidate build on every push and from the release artifacts alone
  on a tag, and `TestContract` (spec 021) passes against it both ways
  (spec 013's stack; proposed: `.github/workflows/verify.yml`, the
  `install` job with its source chosen by the trigger), and
  `kustomize build` succeeds on `deploy/examples/digitalocean` and
  `deploy/examples/aws` (proposed: `verify.yml`, the `overlays` job).
- `origod check` prints a `fail` line naming the requirement for each
  of: an unreachable bucket, a store that ignores conditional create
  (`ORIGO_CHECK_SELFTEST=1`), an unreachable issuer, an authorizer
  answering 500 and one that allows the probe id, a sink answering 500
  to the `ping`, an unwritable data directory, a missing git binary,
  and a git older than 2.40 (a stub `git` on `PATH`), and exits 1; with
  everything in place against `pkg/s3/s3test` and the stubs of spec 013
  it prints seven `ok` lines and exits 0 (proposed: `cmd/origod`,
  `TestCheckReportsEachRequirement`).
- `make docs` regenerates `docs/configuration.md` byte-identical in the
  verify workflow (proposed: `internal/config`, `TestConfigurationDocIsCurrent`).
- A maintainer following `docs/install.md` on a fresh kind cluster
  reaches a successful push without consulting any other document: a
  release checklist item of spec 017, done once per release by hand and
  recorded in the release notes, not a CI test, because the `install`
  job proves the commands and only a person proves the prose.
