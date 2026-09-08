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
planned. `cmd/origod` has no subcommand dispatcher: it parses flags
and serves. This spec builds the dispatcher spec 002's subcommand
table describes, with `check` as its first subcommand; spec 014's
`migrate` joins it later, and spec 002's Outcome records the transfer.

## Design

### Requirements

| Requirement | Detail |
|---|---|
| Kubernetes | 1.29 or newer; a default storage class or nodes with local disk; an ingress controller; Pod Security admission at `restricted` on the namespace is supported and recommended |
| bucket | any S3 compatible endpoint that honours `If-None-Match: *` on `PUT` (spec 004), verified by `origod check`; MinIO, DigitalOcean Spaces, and AWS S3 known good; the bucket endpoint reachable by LFS clients or `ORIGO_S3_PUBLIC_ENDPOINT` set (spec 010) |
| identity | any OIDC issuer with discovery and JWKS over HTTPS (spec 007; plain HTTP only for the stub in the kind overlay); the operator registers one client for people and one for each service that will act on behalf of users |
| authorizer | an HTTP endpoint the operator runs (spec 007), which must deny the probe id spec 007's authorizer contract reserves; for a first installation the stub authorizer of spec 013 (`origo-stubs -allow <subjects>`, which allows a fixed list of subjects and denies the probe id) runs from the manifest the `kind` overlay carries, copied into the operator's overlay, with the image `ghcr.io/latere-ai/origo-stubs:<version>` of the same release as `origod` (spec 017's artifact table), which the archive pins |
| DNS and TLS | one hostname pointed at the ingress with a certificate the ingress holds |

### Manifests

`deploy/base` becomes provider-neutral and complete: Namespace,
ServiceAccount, Deployment with the cache volume, Service, the headless
gossip Service, the NetworkPolicy on the gossip port (spec 016),
Ingress without a class, an issuer annotation, or any
controller-specific annotation (the base's
`nginx.ingress.kubernetes.io/proxy-body-size: "0"` and
`nginx.ingress.kubernetes.io/proxy-read-timeout: "600"`, with the
send timeout and request buffering beside them, move to the `kind`
overlay, which adds them because a push body is a pack of any size and
a clone can take minutes; an operator's overlay adds the equivalent
for their controller, and the install document says so),
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
the stub issuer, authorizer, and sink of spec 013, owned by that spec
and applied in CI on every push; the source stub, its CA Secret and
ConfigMap, and the nodes' egress variables and CA mount are that
spec's kustomize component `test-source`, which `up.sh` includes and
the two install jobs below omit, so the overlay an operator copies
carries no test source), `digitalocean`, and `aws`. The overlay does
not carry `ORIGO_TOKEN_KEY`: the Secret `origod-token-key` that holds
it is generated by `up.sh` on the test stack and, on an installation,
by a block of the install document that runs `openssl ecparam -genkey
-name prime256v1` and writes the Secret, which is the one step an
operator takes that no manifest can, because the key must be theirs;
the overlay needs no other Secret. The two cloud overlays are validated
by `kustomize build` in CI only; nothing applies them there, because
CI has no cloud account, and the install document says so. Helm is not
offered; a kustomize overlay is a directory an operator can read.

Two jobs walk the install document's steps against the `kind` overlay
with what an operator would have: each runs the fenced `sh` blocks of
`docs/install.md` in order through `tools/docs/run-blocks.sh`, the
script spec 013 owns under "Documents as tests" and spec 014 uses for
its migration document, so the document is the test and a step that
drifts from the manifests fails the job. The `install` job in
`verify.yml` runs on every push with the candidate build of that
push, because there is no release for it: it creates a bare kind
cluster from `deploy/examples/kind/kind.yaml` with `kind create
cluster`, not with `up.sh`, because an operator has a cluster and no
script, and installs Cilium as the one step `up.sh` also takes, at the
chart version `deploy/examples/kind/versions.env` pins (spec 013's
overlay table), because `kind.yaml` disables the default CNI (spec 013)
and an operator's cluster comes with one; downloads the `candidate-images` artifact the `e2e` job of
spec 013 uploaded (`actions/upload-artifact` there,
`actions/download-artifact` here, the two `docker save` tarballs of
`origod` and `origo-stubs`) and loads both with `kind load
image-archive`; then walks the document, under a 20 minute budget,
which applies the `kind` overlay without the `test-source` component
and generates `ORIGO_TOKEN_KEY` through the document's own block.
The `install-release` job in `release.yml` runs on a
`v*` tag after the `publish` step of spec 017's pipeline, with the
release artifacts alone, the signed `origod` and `origo-stubs` images
and `deploy-<version>.tar.gz` downloaded from the published release,
so the tag proves the documented install works from what an operator
downloads; a failure there fails the workflow after the release
exists, which is the evidence the release notes link. The document
reads the image reference and the manifest path from two variables
the jobs set:

| Variable | Set by | Value |
|---|---|---|
| `ORIGO_INSTALL_IMAGE` | the `install` job of `verify.yml`, the `install-release` job of `release.yml` | the image reference the document's `apply` block pins: the candidate image the `install` job loaded on a push, `ghcr.io/latere-ai/origod:<version>` on a tag; the document names the release form as the default a reader copies; the stubs image is the same tag of `ghcr.io/latere-ai/origo-stubs`, pinned by the manifests the next row names, so the document carries no second variable |
| `ORIGO_INSTALL_MANIFESTS` | the same two jobs | the path of the manifests the document applies: `deploy/examples/kind` in the checkout on a push, the unpacked `deploy-<version>.tar.gz` on a tag, whose `kind` overlay pins both images; the document names the archive's path as the default |

Both runs end with `TestContract` (spec 021) against the installed
nodes. The `overlays` job of `verify.yml` runs `kustomize build` over
the three example overlays under a 5 minute budget.

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
spec 011, `docs/operations.md`). The apply step carries the block that
generates `ORIGO_TOKEN_KEY` into the Secret `origod-token-key` with
`openssl ecparam` before `kubectl apply -k`; the install jobs run that
block and an operator runs it the same way, so the key never sits in a
manifest and the document is what generates it.

## Not in this spec

A Helm chart. An operator. Installation outside Kubernetes beyond the
binary artifact of spec 017.

## Acceptance criteria

- The `kind` example overlay installs on a bare kind cluster from the
  candidate build on every push and from the release artifacts alone
  on a tag, and `TestContract` (spec 021) passes against it both ways
  (proposed: `.github/workflows/verify.yml`, the
  `install` job, 20 minutes, creating the cluster from `kind.yaml`,
  loading the `candidate-images` artifact of spec 013's `e2e` job, and
  walking `docs/install.md` with `ORIGO_INSTALL_IMAGE` and
  `ORIGO_INSTALL_MANIFESTS` set to the candidate build;
  `.github/workflows/release.yml`, the `install-release` job after
  `publish` with the two variables set to the release artifacts), and
  `kustomize build` succeeds on `deploy/examples/digitalocean` and
  `deploy/examples/aws` (proposed: `verify.yml`, the `overlays` job, 5
  minutes).
- `deploy/base/ingress.yaml` carries no annotation whose key starts
  with `nginx.ingress.kubernetes.io/` or `cert-manager.io/` and no
  `ingressClassName`, and `kustomize build deploy/examples/kind`
  renders the Ingress with `proxy-body-size: "0"` and
  `proxy-read-timeout: "600"` (proposed: `cmd/origod`,
  `TestBaseIngressIsControllerNeutral`, reading the two files through
  a test-only constant resolved from its own source file).
- `origod check` is dispatched by the subcommand table of spec 002,
  which this spec builds: `origod check` runs the check, `origod` and
  `origod serve` serve, `origod -version` prints the identity, and
  `origod nosuch` exits 2 with a usage line (proposed: `cmd/origod`,
  `TestSubcommandDispatch`).
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
