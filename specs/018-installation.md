---
title: "Installation: running Origo on any Kubernetes with any S3 compatible bucket"
status: complete
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/005-placement-and-replication.md
  - specs/007-authentication-and-delegation.md
  - specs/011-observability.md
  - specs/013-test-stubs-and-kind-overlay.md
  - specs/017-release-and-versioning.md
  - specs/021-conformance-suite.md
affects: [deploy/, docs/install.md, docs/configuration.md, docs/api.md, docs/README.md, tools/apidoc/, tools/specindex/, cmd/origod/, internal/config/, tools/docs/, Makefile, .github/workflows/]
effort: medium
created: 2026-09-06
updated: 2026-09-11
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

Built on 2026-09-09 and complete since 2026-09-11; the Outcome records
the tests, the two install jobs, the walk of the page, and what
diverged. Before it, `deploy/base` held a Deployment, a Service, the
headless gossip Service, an Ingress, a PodDisruptionBudget, and a
ServiceAccount; `deploy/prod` set the namespace `origo`;
`deploy/bootstrap` held the Namespace and Secret templates. The Ingress
assumed the `nginx` class and the cert-manager issuer
`letsencrypt-prod` with host `code.latere.ai`; the Deployment set
`ORIGO_PUBLIC_URL` to `https://code.latere.ai` and read the bootstrap
Secrets `origod-s3` and `origod-auth` (spec 007) through `envFrom`.
There was no HPA, no PrometheusRule, no `deploy/examples`, no `origod
check`, no `docs/install.md`, and no `docs/configuration.md`; spec
002's table was the only configuration reference, and `docs/README.md`
listed both pages as planned. The subcommand dispatcher spec 002's
table describes was built by spec 014 with `serve` and `migrate`, and
this spec added `check` to it; spec 002's Outcome records both.

## Design

### Requirements

| Requirement | Detail |
|---|---|
| Kubernetes | 1.29 or newer; a default storage class or nodes with local disk; an ingress controller; Pod Security admission at `restricted` on the namespace is supported and recommended |
| bucket | any S3 compatible endpoint that honours `If-None-Match: *` on `PUT` (spec 004), verified by `origod check`; MinIO, DigitalOcean Spaces, and AWS S3 known good; the bucket endpoint reachable by LFS clients or `ORIGO_S3_PUBLIC_ENDPOINT` set (spec 010) |
| identity | any OIDC issuer with discovery and JWKS over HTTPS (spec 007; plain HTTP only for the stub in the kind overlay); the operator registers one client for people and one for each service that will act on behalf of users |
| authorizer | an HTTP endpoint the operator runs (spec 007), held to the five rules of that spec's authorization endpoint contract, of which the operator-facing consequence is that the endpoint learns of a repository before Origo does, so a registration precedes every `POST /v1/repos`; a single-tenant installation satisfies the contract with a static allow-list that denies the probe id; for a first installation the stub authorizer of spec 013 (`origo-stubs -allow <subjects>`, which allows a fixed list of subjects and denies the probe id) runs from the manifest the `kind` overlay carries, copied into the operator's overlay, with the image `ghcr.io/latere-ai/origo-stubs:<version>` of the same release as `origod` (spec 017's artifact table), which the archive pins |
| DNS and TLS | one hostname pointed at the ingress with a certificate the ingress holds |

### Manifests

`deploy/base` becomes provider-neutral and complete: ServiceAccount,
Deployment with the cache volume, Service, the headless gossip
Service, the NetworkPolicy on the gossip port (spec 016),
Ingress without a class, an issuer annotation, or any
controller-specific annotation (the base's
`nginx.ingress.kubernetes.io/proxy-body-size: "0"` and
`nginx.ingress.kubernetes.io/proxy-read-timeout: "600"`, with the
send timeout and request buffering beside them, move to the `kind`
overlay, which adds them because a push body is a pack of any size and
a clone can take minutes; an operator's overlay adds the equivalent
for their controller, and the install document says so),
HorizontalPodAutoscaler (spec 005), PodDisruptionBudget, and
PrometheusRule (spec 011). No Secret is in the base, because `kubectl
apply -k` of a base that carried one would overwrite the operator's on
every rollout: the Secret templates stay in `deploy/bootstrap`
(`secrets.example.yaml`, the two Secrets `origod-s3` and `origod-auth`
of spec 007, the second gaining `ORIGO_GOSSIP_SECRET` of spec 005;
`ORIGO_TOKEN_KEY` is in neither, because the Secret `origod-token-key`
below holds it), with every other required variable, and the base
Deployment reads both through `envFrom`. The Namespace is not in the
base either: it stays in `deploy/bootstrap`
with the Secret templates, applied by hand once (spec 002's layout),
because the identity that rolls out a release, the `deploy` job of spec
017 and an operator's day-to-day `kubectl apply -k`, never creates
namespaces, and a base that carried one would have every overlay
either create it or patch it away. The HorizontalPodAutoscaler scales on CPU only, in the base and
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
and an operator's cluster comes with one; downloads the `candidate-images` artifact the `build` job of
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
| `events` | when `ORIGO_EVENTS_URL` is set, a signed `ping` event (below) answers any status under 500; when it is unset the line is `ok events: not configured`, so the line count is seven either way |
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

### API reference

`docs/api.md`, the page `docs/README.md` lists for a consumer, is the
second output of `make docs` and this spec's: the endpoint table, the
header table, the code table with each code's status and sentence, and
the authorization endpoint, which is the one call Origo makes rather
than serves and therefore no definition table's row. That section is the
passage spec 007 writes under its own heading, carried verbatim through
`specs.Index.Section`, a reader of one spec heading this spec adds to
the shared parser, so the contract an operator's endpoint is held to has
one source and the page cannot drift from it. The rest is
rendered by `tools/apidoc`, a Go program in its own module beside
`tools/specindex`, from the same cross-reference data `specindex`
parses out of the specs (the tables whose first header is `Method` and
`Path`, `Header`, or `Code`), grouped by the spec that owns each row
with a link to it, so the page never carries a name the specs do not
define. The parser is shared, not copied: `tools/specindex` exports
the package `tools/specindex/specs`, the parser and the
cross-reference model its `main` uses today, and `tools/apidoc`
requires the `tools/specindex` module with a `replace ../specindex`
directive in its `go.mod`, so both tools read one parser and a table
shape one of them does not recognise is a finding in both. The export
is a builder item of this spec: `specindex` is a tool, not a package a
spec owns, so moving its parser under `specs/` changes no other spec's
status, and `make specindex` (`go test ./...` in that module) covers
the new package as it covers `main`. `make docs` runs `cd tools/apidoc && go run . -write` after
the configuration page, and the `specindex` job of `verify.yml` runs
`make docs` and then `git diff --exit-code docs/`, so a spec change
that moves a table shows up as a documentation diff on the same push. The page states the contract
number of spec 003 at its top and nothing a spec does not state.

### The install document

`docs/install.md`: requirements, the five steps (create the bucket,
register the issuer clients, write the overlay, apply, run the check),
the first clone and push, how to point a consumer at the authorizer, and
where to look when something fails. It serves two readers at once, the
`install` job walking it against the `kind` overlay and an operator
walking it against their own cluster, and it separates them by naming
the example stack at every point where it is what an unset variable
falls back to: the address, the token, and the repository the walkthrough
creates each read an `ORIGO_*` variable an operator sets, and the prose
beside each block says what the fallback is and that no real installation
has it. The token an operator's issuer mints and the registration their
authorization endpoint needs are sketches in unfenced-as-`sh` blocks,
which neither `run-blocks.sh` nor `TestInstallDocumentIsWellFormed`
executes, because their shape is the operator's provider's and not
Origo's (the check's output, the alerts of
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
  loading the `candidate-images` artifact of spec 013's `build` job, and
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
- `make docs` regenerates `docs/configuration.md` and `docs/api.md`
  byte-identical in the `specindex` job of `verify.yml`, and `docs/api.md` carries
  every endpoint, header, and code the cross-reference of
  `specs/README.md` lists and no other, and the authorization endpoint
  section is the passage spec 007 writes and not a restatement of it, so
  a deck that states no such contract renders no page (proposed:
  `internal/config`,
  `TestConfigurationDocIsCurrent`; `tools/apidoc`,
  `TestAPIDocIsCurrent` and `TestAuthorizationSectionComesFromTheSpec`,
  which read the specs and the page through a
  test-only constant resolved from its own source file;
  `tools/specindex/specs`, `TestSectionCarriesOneSpecPassage`). The
  configuration page is required to carry every variable of a spec that
  has started and is free of a variable a spec has only designed,
  because the page is generated from `internal/config` and a spec at
  `drafted` reaches no code, and a row whose spec file is not in the
  tree is skipped the same way, because the deck's own lint is what
  reports a missing file; the reverse direction is unscoped, so a row
  no spec defines still fails
  (`internal/config`, `TestUnstartedSpecsDoNotNeedAReferenceRow`).
- A maintainer following `docs/install.md` on a fresh kind cluster
  reaches a successful push without consulting any other document: a
  release checklist item of spec 017, done once per release by hand and
  recorded in the release notes, not a CI test, because the `install`
  job proves the commands and only a person proves the prose.

## Outcome

Built on 2026-09-09 and complete since 2026-09-11: every criterion a
checkout or a push can prove has a passing test in the tree, and the
rows that needed a published release closed on the tags the pending
table below names.

### Criterion to test

| Criterion | Test | State |
|---|---|---|
| the `kind` overlay installs on a bare cluster from the candidate build on every push and `TestContract` passes against it | the `install` job of `verify.yml`: `kind create cluster` from `kind.yaml`, Cilium at the version `versions.env` pins, the `candidate-images` artifact loaded, `tools/docs/run-blocks.sh docs/install.md` with `ORIGO_INSTALL_IMAGE` and `ORIGO_INSTALL_MANIFESTS`, then `TestContract` through `ORIGO_LIVE_URL`, in 20 minutes | passing on every push since; run 34634410315 of 2026-09-11 is one, its `install from the documentation` job `success` |
| the same from the release artifacts alone on a tag | the `install-release` job of `release.yml` after `publish`, with the published images pulled and loaded and the `kind` overlay of `deploy-<version>.tar.gz` | passing: the job ran in 3 m 43 s in the tag run 34461460766 of `v0.1.0`, at commit `2b2468d`, against `ghcr.io/latere-ai/origod:v0.1.0` and the published `deploy-v0.1.0.tar.gz`, and ended in `TestContract`. It had been skipped by the first cut of the tag, run 34450584556, which spec 017 records and fixed |
| `kustomize build` succeeds on `deploy/examples/digitalocean` and `deploy/examples/aws` | the `overlays` job of `verify.yml`, 5 minutes, over all three example overlays | passing |
| `deploy/base/ingress.yaml` carries no `nginx.ingress.kubernetes.io/` or `cert-manager.io/` annotation and no `ingressClassName`, and the `kind` overlay renders `proxy-body-size: "0"` and `proxy-read-timeout: "600"` | `cmd/origod`, `TestBaseIngressIsControllerNeutral` | passing |
| `origod check` runs the check, `origod` and `origod serve` serve, `origod -version` prints the identity, and `origod nosuch` exits 2 with a usage line | `cmd/origod`, `TestSubcommandDispatch` | passing |
| `origod check` prints a `fail` line naming the requirement for each of the eight failures and exits 1, and seven `ok` lines and 0 when everything is in place | `cmd/origod`, `TestCheckReportsEachRequirement`, with `TestGitVersionParsesTheThreeNumbers` on the version floor and `TestCheckInitContainerSharesTheNodesEnvironment` on the init container that runs it | passing |
| `make docs` regenerates `docs/configuration.md` and `docs/api.md` byte-identical, and `docs/api.md` carries every endpoint, header, and code the cross-reference lists and no other | `internal/config`, `TestConfigurationDocIsCurrent`, `TestReferenceRowsAreComplete`, `TestDefaultsOnThePageAreTheDefaultsInTheCode`; `tools/apidoc`, `TestAPIDocIsCurrent` and `TestPageStatesTheContractAndNothingElse`; the `specindex` job of `verify.yml` runs `make docs` and `git diff --exit-code docs/` | passing |
| `docs/api.md` carries the authorization endpoint as the passage spec 007 writes, and a deck that states none renders no page | `tools/apidoc`, `TestAuthorizationSectionComesFromTheSpec`; `tools/specindex/specs`, `TestSectionCarriesOneSpecPassage` on the reader it uses | passing |
| the document's blocks run with nothing set and with an operator's variables set, and the second path never reaches the example stack's issuer | walked by hand on 2026-09-10 against a local node with the stubs of spec 013, once with nothing set and once with the token, the repository id, the owner, and the slug set through the document's own variables, which are the page's and not the node's; the `install` job walks the first path on every push | passing by hand; the job is the standing proof of the first path |
| every `sh` block of `docs/install.md` parses and every link and `deploy/` path it names exists | `tools/docs`, `TestInstallDocumentIsWellFormed` | passing |
| `ORIGO_TOKEN_KEY` comes from the Secret `origod-token-key` and from nowhere else, in every workload that runs `origod` | `cmd/origod`, `TestSigningKeyHasOneSource` | passing |
| a maintainer reaches a successful push following `docs/install.md` on a fresh cluster without another document | spec 017's release checklist, done once per release by hand | walked on 2026-09-11 against `v0.1.1`. It reached a push and a clone that read it back over HTTPS, after the eight defects below were fixed and the page was walked a second time from a fresh cluster. The SSH half did not hold because the page and the archive did not travel together; the archive now carries the page, which closes it |

Coverage of the packages this spec touched: `cmd/origod` 92.6%,
`internal/config` 98.9%, `tools/configdoc` 93.8%, `tools/apidoc` 94.6%,
`tools/specindex` 95.7%, `tools/specindex/specs` 95.2%.

### Divergences

- `deploy/base/prometheusrule.yaml` is in the directory but is not a
  resource of `deploy/base/kustomization.yaml`. The Design lists it
  among what the base holds; a `PrometheusRule` needs the Prometheus
  operator's CustomResourceDefinition, which Origo does not require and
  the `kind` stack does not install, so a base that applied it would
  fail `kubectl apply -k` on every cluster without that operator. The
  file stays for an installation that runs the operator to apply beside
  the base, and the install document says where it is and why it is not
  applied. The decisions table carries the row.
- The `build` job of `verify.yml` now runs on every event but the weekly
  schedule, where it ran on a tag and a dispatch. The Design puts the
  `install` job on every push and the job downloads `candidate-images`
  and builds nothing, so the build had to move with it. The cluster
  tiers, the up-script check, and the mutation job keep the tag-only
  rule spec 013 set for the minutes they cost; one image build and one
  20 minute install job are what a push now carries that it did not.
- `docs/install.md` had read a `VERSION` variable that
  `ORIGO_INSTALL_IMAGE` defaulted from, which the Design's variable
  table does not name. The walk of 2026-09-11 removed it: a number
  baked into the settings block is stale the day after a tag, and step
  6's `set image` then overrode the archive's own pins with it. The
  page's `IMAGE` is empty unless `ORIGO_INSTALL_IMAGE` is set, step 6
  runs `set image` only when it is, and the version appears once in the
  download line a reader edits. The Design's table stands: both install
  jobs set the two variables and neither changed.
- `ORIGO_TOKEN_KEY` left `deploy/bootstrap/secrets.example.yaml`. The
  Manifests section says `origod-auth` carries it and the base reads
  both bootstrap Secrets through `envFrom`; the paragraph after it says
  the Secret `origod-token-key` holds the key and the install document
  generates it, which is what `up.sh` and the `kind` overlay already
  did. The two cannot both hold on an overlay derived from the base: an
  operator who filled the template and ran the document's generate step
  had two keys, one of them `replace-me`, and a node that refuses to
  start. The base now names `origod-token-key` in an explicit `env`
  entry in the node and in the check, the template no longer offers a
  field for the key, and the second paragraph is the one that stands,
  because it is the one written for this spec. The decisions table row
  is amended.

- The install document and `docs/api.md` gained the authorization
  contract on 2026-09-10, after a walk of the document against a real
  installation found two holes the `kind` overlay hides: the walkthrough
  minted its token at the example stack's stub issuer, which no real
  issuer serves, and it never said that an authorization endpoint has to
  know a repository before Origo creates one, so a reader with a real
  endpoint met a 403 the page did not explain. Both are now in the
  document, the five rules and the single-tenant case are in step 2, and
  spec 007 gained the contract section `docs/api.md` renders. The
  additions were asked for by a spec of another deck, which owns none of
  this tree and made no change in it.

- `tools/docs/TestInstallDocumentIsWellFormed` is not in the Acceptance
  criteria. The `install` job is the document's proof and needs a
  cluster; the test is the half that needs none, so a broken block or a
  dead link fails in the `test` gate in a second rather than 20 minutes
  later, and it runs on a machine with no container engine.

- Spec 013's `TestE2EJobsSelectByPrefix` grew a row. Its last rule is
  that no job outside its table runs the `e2e` tier, and the `install`
  job ends in `TestContract` against the installation, an `e2e`-tagged
  line. The job is in the table now, with its 20 minute budget, its
  `needs: build` and `candidate-images` download, and the one selection
  `./test/conformance/... -run 'TestContract'`; it runs no `e2e`
  package, so the rule that each job selects its own tests and nothing
  else is unweakened and the `install` job is held to it too.

### What only a real release proves

| Pending | Closed by | State |
|---|---|---|
| `install-release`: `docs/install.md` walked against the published images and the published `deploy-<version>.tar.gz` on a bare cluster, ending in `TestContract` | the first `v*` tag | closed by the tag run 34461460766 of `v0.1.0` |
| a maintainer walking the prose to a successful push on a fresh cluster | spec 017's release checklist at the first release, recorded in the release notes | closed for the HTTPS path by the walk of 2026-09-11 against `v0.1.1`, recorded below. The next release's notes carry the checklist entry; `v0.1.1`'s were already published |
| the same walk reaching an SSH clone | a release whose `deploy-<version>.tar.gz` carries the SSH overlay, or a change that ships the page and the manifests together | closed 2026-09-11 by the second: `tools/release/deploy-archive.sh` packs `docs/install.md` beside `deploy/`, and `deploy_archive_test.sh` fails without it, so the page a reader follows is the page of the release they hold |

### The walk of 2026-09-11

Walked against `v0.1.1`: `deploy-v0.1.1.tar.gz` and
`ghcr.io/latere-ai/origod:v0.1.1` with `origo-stubs:v0.1.1`, all three
downloaded from the release, with no checkout on the path of any
command the walk ran. The machine was macOS 27 on arm64 with podman
5.7.1 as the container engine and the kind node image at Kubernetes
1.36.1. Two deviations from what an operator would have, both forced by
that machine and neither touching what the page says: Cilium cannot
start under podman on macOS (`mount: /sys/fs/bpf: permission denied` in
its `mount-bpf-fs` init container), so the cluster ran kind's own
network plugin, which does not enforce the overlay's NetworkPolicies;
and the container VM has 2 GiB shared with other work, so the
StatefulSet ran one node where the overlay asks for three. The
`install` job runs three nodes behind Cilium on every push, which is
where that half is proved.

Eight defects, each what the page said, what happened, and the change:

1. **No cluster.** Every default address on the page is a host port of
   a kind cluster the page never tells you to make. A plain `kind
   create cluster` publishes the API server and nothing else (`podman
   port` prints one line, `6443/tcp`), so `curl
   http://localhost:30080/version` answers `curl: (7)` for good. New
   section, "A throwaway cluster, if you do not have one", which
   creates it from `deploy/examples/kind/kind.yaml`.
2. **No network plugin.** That `kind.yaml` sets `disableDefaultCNI:
   true`. A cluster made from it stays `NotReady` with `cni plugin not
   initialized`, `kubectl apply -k` still reports every object created,
   all six pods sit in `Pending`, and step 6's `rollout status` ends in
   `error: timed out waiting for the condition`, which no row of the
   failure table covered. The same new section installs Cilium at the
   version `versions.env` pins, and three rows were added to the table.
3. **No archive.** `MANIFESTS` defaulted to `deploy/examples/kind`, a
   relative path that resolves only if you unpacked the archive in the
   current directory, which the page never said to do or how. New
   section, "Get the manifests", with the download and the unpack.
4. **A stale version that overrode the archive.** The settings block
   read `VERSION="${ORIGO_VERSION:-v0.1.0}"` while `v0.1.1` was the
   newest release, so pasting it printed `installing
   ghcr.io/latere-ai/origod:v0.1.0` and step 6's `kubectl set image
   "*=$IMAGE"` would have installed `v0.1.0` over the `v0.1.1`
   manifests just unpacked. `IMAGE` is empty by default now and step 6
   skips `set image` when it is, so the archive's own pins stand; the
   two install jobs set `ORIGO_INSTALL_IMAGE` and are unaffected. The
   version number appears once, in the download line a reader edits.
5. **A file that is not in the archive.** Step 3 said to copy
   `deploy/bootstrap/secrets.example.yaml`. `tar tzf
   deploy-v0.1.1.tar.gz` lists `deploy/base` and `deploy/examples` and
   nothing else, because `tools/release/deploy-archive.sh` packs those
   two. The page now prints both Secrets in full as appliable objects
   and names no file. `TestInstallDocumentIsWellFormed` cannot catch
   this class: it resolves a `deploy/` path against the checkout, where
   the file does exist.
6. **Secrets before the namespace.** Step 3 applied them and step 4
   created the namespace, so a reader in order met `namespaces "origo"
   not found`. Step 3 says to apply them after step 4, and the failure
   table names the message.
7. **A global setting changed with no warning.** `kind create cluster`
   made its cluster the current context in the shared kubeconfig, and
   while this walk was running another session operating the production
   cluster failed with `namespaces latere not found`. The page runs
   bare `kubectl` throughout and never said which cluster it acts on.
   The cluster section exports `KUBECONFIG` to a file of its own and
   says why, and the settings section ends with `kubectl config
   current-context` before anything applies.
8. **A create that swallowed its own error.** `curl -sf ...
   >/dev/null` on `POST /v1/repos` exited 22 and printed nothing when
   the node was serving and the bucket was still waking: the node
   logged `503` and `wal: create origo/repos/<id>/meta: context
   deadline exceeded`, while the paragraph under the block promised a
   `details.reason` the block had thrown away. The block prints the
   status and the body on anything but a 201, the address wait now
   waits for `/readyz` as well as `/version`, and three failure rows
   name the 503, the `storage_unavailable` a first push can meet, and
   the SSH case below. Run 2 below is the change showing its work.

The page was then walked again from a second, fresh cluster, in three
runs:

1. `tools/docs/run-blocks.sh docs/install.md`, the whole page in order
   the way the install jobs run it, on three replicas. It reported the
   seven `ok` lines of `origod check`, `{"version":"v0.1.1",...}` from
   `/version`, and `created e6a7ef4f-cda2-4d51-9eb3-166798f34c8d`, and
   then the push was rejected: `remote: storage_unavailable: The
   repository is temporarily unavailable. Nothing was lost. Try again
   in a few minutes.` with `! [remote rejected] main -> main
   (pre-receive hook declined)`. That rejection is what the new failure
   row for `storage_unavailable` was written from.
2. The page's blocks 8 to 12, the first clone and push, after the
   StatefulSet was scaled to the one node the machine holds. The create
   printed `create answered 503` and the whole body,
   `{"error":{"code":"storage_unavailable",...,"details":{"error":"breaker
   open","key":"origo/repos/.../meta","op":"get"}}}`. The block as it
   stood before this walk would have exited 22 and printed nothing, so
   this run is defect 8's fix showing its work.
3. The same blocks once more, with the breaker closed: `created
   ab23e1d6-1201-4022-94b1-314916c27c0d` and `the installation serves a
   clone and a push`.

So the criterion holds with one retry, which the page now names and
tells the reader to make.

### What the software would have to do

Step 5 and the SSH half of the walkthrough cannot be walked from the
newest release, and no wording fixes it. `deploy-v0.1.1.tar.gz` maps no
host port 30022 and no 30086 in its `kind.yaml`, runs no key resolution
stub, and sets none of the four SSH variables step 5 lists, while the page
on `main` says "The example overlay carries all of it, so on a
throwaway cluster there is nothing to add" and defaults the SSH
walkthrough to `http://localhost:30086` and `ssh://git@127.0.0.1:30022`.
Run against `v0.1.1` the key registration answers `curl: (7)` and
`ssh-keyscan -T 10 -p 30022 127.0.0.1` answers `write (127.0.0.1):
Broken pipe`.

The cause is that the page and the manifests do not travel together.
`tools/release/deploy-archive.sh` packs `deploy/base` and
`deploy/examples`, so a reader holds the newest archive and reads the
page on `main`, which documents whatever landed since the tag. Every
feature that reaches the example overlay before a release does this
again; SSH is only the first.

**Closed 2026-09-11 by the first of the two changes it named.**
`tools/release/deploy-archive.sh` now copies `docs/install.md` into the
archive beside `deploy/`, so the page a reader follows is the page of
the release they unpacked, and the relative default the page names for
the manifests resolves where they stand. `deploy_archive_test.sh`
requires the page in the archive and fails without it, which is what
keeps the two travelling together for every feature after SSH rather
than only this one.

The page also says to check `kind.yaml` for the two ports before
relying on step 5, and a failure row names the two errors, which is
what prose can do on its own.

A review on 2026-09-11 read the Design against the tree and found the
seven lines of `origod check` with their conditions, the self-test
variable, the probe request with the empty subject, the `ping` with
spec 008's three headers, the 2.40 floor, and exit 1 on any failure,
which a run against a bucket with no issuer behind it showed; the init
container running `check`; the base Ingress with no annotation and no
class while the kind overlay's patch carries the four; the three
example overlays rendering; the fourteen shell blocks of the install
page with the key block and the two variables; the generated API page
and the parser shared through the `replace` directive; and every named
test present. Four passages were behind the tree: the Current state in
unbuilt tense with the dispatcher's history reversed, a Manifests
sentence keeping `ORIGO_TOKEN_KEY` in the auth Secret against the
divergence recorded below, the Outcome's header still at `testing`,
and the first table row still waiting on a first green run. Each reads
as the tree and the runs stand.
