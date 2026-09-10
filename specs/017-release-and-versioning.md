---
title: "Release and versioning: images, binaries, compatibility, and what a version promises"
status: testing
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
  - specs/013-test-stubs-and-kind-overlay.md
  - specs/021-conformance-suite.md
affects: [.github/workflows/, Dockerfile, Dockerfile.ci, Dockerfile.stubs, Makefile, CHANGELOG.md, tools/release/, tools/smoke/, docs/upgrades/, internal/wal/, internal/repo/, internal/version/, cmd/origod/, test/conformance/]
effort: small
created: 2026-09-06
updated: 2026-09-09
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

One change to the tree, for the builder: the shared runtime stage of
`Dockerfile` and `Dockerfile.ci` is `debian:bookworm-slim`, whose
`git` is 2.39, and `origod check` (spec 018) requires 2.40 because
spec 020's merge family needs it. This spec owns the move: the base
becomes `debian:trixie-slim` pinned by digest, which ships git 2.47,
in both files in one change so the two stages stay byte for byte the
same, as the artifact table below and spec 002's Images section say.

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
| `ghcr.io/latere-ai/origod:<version>` | GHCR, `linux/amd64` and `linux/arm64` | `Dockerfile.ci` over the shared runtime stage of spec 002: `debian:trixie-slim` pinned by digest, which ships git 2.47, above the 2.40 floor `origod check` (spec 018) enforces; signed with cosign keyless; an SPDX bill of materials, which ships as a release asset and is attached to the image as a referrer only when the repository is public (see the attestation note below) |
| `ghcr.io/latere-ai/origo-stubs:<version>` | GHCR, `linux/amd64` and `linux/arm64` | `Dockerfile.stubs` of spec 013, the stub issuer, authorizer, sink, and source in one image, published beside `origod` under the same tag and signed the same way, with its own bill of materials under the same attestation rule; pinned in the `kind` overlay of `deploy-<version>.tar.gz` beside `origod`, so spec 018's `install-release` job and an operator's first installation run the stub authorizer from a released, signed image and not from a checkout |
| `origod_<version>_<os>_<arch>.tar.gz` | the GitHub release | `linux` and `darwin`, `amd64` and `arm64`; `checksums.txt` with SHA-256 sums, signed |
| `deploy-<version>.tar.gz` | the GitHub release | `deploy/base` and `deploy/examples` with the image pinned to the version, so an operator's overlay references one artifact |
| `fixture-<version>.tar.gz` | the GitHub release | the bucket prefix `origo/repos/<id>/` of a fixture repository the harness pushes through the candidate image in the `kind` stack before `TestContract` and keeps, so the next release can prove it reads what this one wrote |
| release notes | the GitHub release | the `CHANGELOG.md` section for the version, the smoke evidence, and the conformance run's timings (spec 021) |

`GET /version` on a released node serves the tag as `version`.

**The attestation rule.** `actions/attest-sbom` and
`actions/attest-build-provenance` call GitHub's attestation API, which
refuses a private repository on Latere's organization plan: "Feature
not available for the latere-ai organization. To enable this feature,
please upgrade the billing plan, or make this repository public." The
four steps are therefore conditional on
`!github.event.repository.private`, the field the push event's payload
carries, and `release-verify`'s `gh attestation verify` on the same
condition. No operator sets anything: the steps return by themselves
the day the repository is made public. The condition reads `private`,
not the plan, so upgrading the plan while the repository stays private
leaves them skipped and this condition is what changes then.

What is deferred is only the attachment of the two attestations to the
image and their verification. The three SPDX documents are still built
and still shipped as release assets, signed together with everything
else through `checksums.txt`; cosign keyless signing calls sigstore
rather than GitHub, so every image and `checksums.txt` still carries a
signature an outside operator verifies against the workflow identity.

### The pipeline

`release.yml` in this repository produces every artifact itself and
calls no shared workflow: `service-release.yml` of `latere-ai/ci` runs
its deploy and smoke as one job it does not expose as a separate
callable step, so the two steps are written inline here, the same
`kubectl` and `tools/smoke/release.sh` invocations the shared workflow
makes. On a `v*` tag:

1. `build`: `go build` for the four `os/arch` pairs with the `-ldflags`
   of spec 002 setting `internal/version`, the archives and
   `checksums.txt`; `docker buildx` of `Dockerfile.ci` and of
   `Dockerfile.stubs` for `linux/amd64` and `linux/arm64`, each pushed
   as one multi-arch image; `cosign sign` keyless with the workflow's
   OIDC identity on both images and on `checksums.txt`; an SPDX bill of
   materials from the module graph and each image, attached with
   `attest-sbom` and provenance with `attest-build-provenance` when
   the repository is public, by the attestation rule above; the deploy
   archive from `deploy/base` and `deploy/examples` with both images
   pinned; and the two `linux/amd64` images as `docker save` tarballs
   in the artifact `candidate-images`, the shape spec 013's `build`
   job uploads, so step 2 hands them to `up.sh` the way the `e2e` job
   does.
2. `conformance`: the `e2e` job of spec 013 against the candidate
   image. The fixture harness of this spec pushes the fixture
   repository through the stack first, under a slug outside the
   `conformance-` prefix, and keeps it; then `TestContract` of spec
   021 runs, whose `Run` deletes only the repositories it created and
   never by prefix (spec 021), so the fixture survives the run; then
   `TestPreviousReleaseFixture` below runs against the fixture of the
   previous release, which the job downloads from that release's
   assets with `gh release download` and names in
   `ORIGO_PREVIOUS_RELEASE_FIXTURE`; the harness then reads every
   object under the fixture repository's prefix from the stack's MinIO
   through `ORIGO_TEST_S3_ENDPOINT` and its sibling variables, which
   the job exports with the overlay's fixed values (spec 013, the MinIO
   row: `http://localhost:30900`, bucket `origo-test`), and packs them
   as `fixture-<version>.tar.gz`; a failure stops the release.
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
   six-entry skip list, after step 3 and skipped when the secret is
   unset; its report and timings are attached to the release.
5. `publish`: the GitHub release with every artifact, the `CHANGELOG.md`
   section as the body, the smoke evidence when step 3 ran, the
   conformance timings of step 2, and the live report of step 4 when
   it ran.
6. `install-release`: the job spec 018 owns, after `publish`: the
   blocks of `docs/install.md` run against a fresh kind cluster with
   `ORIGO_INSTALL_IMAGE` and `ORIGO_INSTALL_MANIFESTS` set to the
   published image and the unpacked `deploy-<version>.tar.gz`, and
   end with `TestContract`; a failure fails the workflow after the
   release exists, which is the evidence the release notes link.
7. `release-verify`: from a clean runner, `cosign verify` on the image
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

`TestPreviousReleaseFixture` in `test/conformance` carries the `e2e`
build tag, like every test that needs a stack. The `e2e` job of spec
013 downloads `fixture-<version>.tar.gz` of the latest release with
`gh release download` and passes its path in the variable below; the
test skips with the message `ORIGO_PREVIOUS_RELEASE_FIXTURE unset`
when the variable is unset, which is the case on a fork with no
release and on a developer's machine. Before it starts, the test
uploads every object of the fixture into the stack's bucket under a
fresh prefix, `origo/repos/<new id>/` for an id it draws, through the
S3 client of `latere.ai/x/pkg/s3` against the `ORIGO_TEST_S3_ENDPOINT`
family the job exported, so the stack's nodes materialize the
repository from the log the previous release wrote and the fixture
itself is never modified.

| Variable | Set by | Value |
|---|---|---|
| `ORIGO_PREVIOUS_RELEASE_FIXTURE` | the `e2e` job of spec 013's `verify.yml` and the `conformance` step of `release.yml`, both through `gh release download` of the latest release's `fixture-<version>.tar.gz` | the path of the downloaded fixture archive; unset skips `TestPreviousReleaseFixture`, the way `ORIGO_INSTALL_MANIFESTS` (spec 018) gates the install jobs |

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
  `cosign verify` accepts the images, `sha256sum -c checksums.txt`
  passes on the binaries, and the release body equals the
  `CHANGELOG.md` section (proposed: the `release-verify` job in
  `release.yml`). `gh attestation verify` accepts the images on a tag
  cut from a public repository; while the repository is private the
  attestation rule above skips both the attachment and this check, and
  this clause of the criterion is the one part a private release does
  not prove.
- A tag on a fork with `ORIGO_RELEASE_DEPLOY` unset publishes every
  artifact and skips the deploy and smoke step, and the same tag with
  the variable set runs it: a release checklist item above, done by a
  maintainer by tagging a fork and recorded in the release notes, not
  a CI test, because CI cannot tag a fork of itself (`release.yml`, the
  `deploy` job's `if` on the variable).
- The fixture repository attached to release N-1 materializes and
  serves on release N with identical `rev-list --all`, the fixture
  downloaded from that release's assets by the job with `gh release
  download` into the path `ORIGO_PREVIOUS_RELEASE_FIXTURE` names,
  uploaded by the test under a fresh prefix through the S3 client
  before it clones, and the test skipped when the variable is unset
  (proposed: `test/conformance`, `TestPreviousReleaseFixture`, under
  the `e2e` tag).
- A node reading an index object with `v: 2` answers 503
  `repository_unavailable` with `details.key` for that repository, logs
  the documented line, serves another repository, and stays ready
  (proposed: `internal/repo`, `TestNewerLogFormatIsRefused`).
- Both Dockerfiles name `debian:trixie-slim` by one digest and the
  built image answers `git --version` with 2.47 or newer, so `origod
  check` passes its `git` line inside the image (proposed: `verify.yml`,
  spec 013's `build` job running `git --version` in the candidate image
  it built, the one job with a `docker build` step, and step 1 of
  `release.yml` the same way; and a test in `cmd/origod`,
  `TestDockerfilesShareOneRuntimeStage`, comparing the two files'
  runtime stages byte for byte through a test-only constant resolved
  from its own source file).
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

## Outcome

Built on 2026-09-09; the first release ran on 2026-09-10. Every
criterion a checkout can prove has a passing test in the tree, and the
tag closed every row below that a tag can close. The spec stays at
`testing` on the three that a tag cannot: the attestations, which need
the repository to be public; the `live` job, which needs the two
secrets and an installation behind them; and the compatibility
assertion against a fixture an earlier release attached, which needs a
second tag.

The first tag, `v0.1.0` of 2026-09-10, published nothing and was
deleted. It is blocked twice over, on two limits outside this
repository that the table below states and neither of which is worked
around.

### Criterion to test

| Criterion | Test | State |
|---|---|---|
| the fixture of release N-1 materializes and serves on release N with identical `rev-list --all`, uploaded under a fresh prefix through the S3 client, skipped when the variable is unset | `test/conformance`, `TestPreviousReleaseFixture` (`e2e`), with `TestReleaseFixtureRoundTrip` and `TestReleaseFixtureRefusesABadArchive` on the harness itself and `TestReleaseFixturePush`/`TestReleaseFixturePack` as the pipeline's two halves | in the tree; the assertion against a real previous fixture waits for the second release |
| a node reading an index object with `v: 2` answers 503 `repository_unavailable` with `details.key`, logs the documented line, serves another repository, and stays ready | `internal/repo`, `TestNewerLogFormatIsRefused`; `internal/wal`, `TestNewerFormatIsNamed` | passing |
| both Dockerfiles name `debian:trixie-slim` by one digest and the built image answers `git --version` with 2.47 or newer | `cmd/origod`, `TestDockerfilesShareOneRuntimeStage`; the `build` job of `verify.yml` and the `build` job of `release.yml`, each running `git --version` in the image it built | passing in the tree; the image check runs on a tag or a dispatch |
| `tools/smoke/release.sh` passes against a stub whose `/readyz` answers `ok` and `/version` serves `TAG`, with standard input closed, and fails naming the mismatch | `tools/smoke/release_test.sh`, run by the `test` gate through `TestReleaseSmoke` | passing |
| `main.version` no longer exists and a binary linked with `internal/version.Version` serves and prints it | `cmd/origod`, `TestVersionHasOneSource` | passing |
| the deploy archive carries `deploy/base` and `deploy/examples` with every image at the version and no placeholder, and refuses a tree whose placeholder is gone | `tools/release`, `TestDeployArchive` over `deploy_archive_test.sh` | passing |
| a tag produces every artifact for both architectures, `cosign verify` accepts the images, `sha256sum -c checksums.txt` passes, and the release body equals the `CHANGELOG.md` section | the `release-verify` job of `release.yml` | passing: the `verify the published release` job of the tag run 34461460766, 50 s, verified both signatures against `^https://github.com/latere-ai/origo/\.github/workflows/release\.yml@refs/tags/` and the GitHub OIDC issuer, refused a foreign identity, checked `checksums.txt` and its cosign bundle, read the deploy archive, and matched the body against the section |
| `gh attestation verify` accepts the images | the `release-verify` job of `release.yml`, its `Verify the attestations` step | deferred while the repository is private: GitHub's attestation API refuses a private repository on this plan, so nothing is attached and nothing is verified. The repository going public, or the condition being changed after a plan upgrade, is what closes it |
| a tag on a fork with `ORIGO_RELEASE_DEPLOY` unset publishes every artifact and skips the deploy and smoke step | the release checklist, done by a maintainer and recorded in the release notes | passing on the repository itself rather than on a fork: no repository variable is set here, so the tag run 34461460766 skipped `deploy and smoke` and published all eleven assets and both images anyway. A fork adds nothing the run did not show, because the variable is what the condition reads |

### What blocked the first release, and what still holds

The budget limit was lifted by the user on 2026-09-10 and `v0.1.0` was
cut. Three limits outside this repository remain on the record, none
worked around, each for the user to decide on:

| Limit | What it blocks | Evidence |
|---|---|---|
| GitHub's attestation API refuses a private repository on the `latere-ai` organization plan | the SBOM and provenance attestations, and `release-verify`'s `gh attestation verify`; nothing else. The attestation rule above skips those steps, so a private release is otherwise complete: every artifact, the three SPDX documents as assets, and the cosign signatures | run 34416521585 of 2026-09-10, the `build` job, `actions/attest-sbom`: "Feature not available for the latere-ai organization. To enable this feature, please upgrade the billing plan, or make this repository public." |
| an organization budget on the `actions` product SKU, `budget_amount` 80 with `prevent_further_usage` true, reached at 17 787 minutes and $80.00 net in September 2026 | lifted. It blocked every job of every workflow, so no push run, no dispatched run, and no release run started at all | runs 34433190432, 34433196681 and 34433964953 of 2026-09-10, every job annotated "The job was not started because an Actions budget is preventing further use."; the organization's billing budgets endpoint. Run 34447226405 of the same day is the first green push run after it was restored |
| no installation for the `live` job to run against: the repository carries no `ORIGO_LIVE_URL` and no `ORIGO_LIVE_TOKEN` secret, no `ORIGO_RELEASE_DEPLOY` variable and no `production` environment, and `https://git.latere.ai` does not resolve | the `live` job runs and its `TestContract` skips, so the release publishes without a live conformance run; `deploy and smoke` is skipped with it. This is what holds specs 003, 019, 020, and 021 at `testing`, and the `live` row below | the tag run 34461460766, job `conformance against the live installation`, 40 s: `contract_test.go:136: nothing answers at ORIGO_TEST_URL (http://localhost:30080)` then `--- SKIP: TestContract (0.00s)`. `gh api /repos/latere-ai/origo/actions/secrets` and `.../variables` both answer `total_count: 0` |

`v0.1.0` was cut on 2026-09-10 from a green `main` at commit `058eb6d`
with `go tool lateregate release v0.1.0`, and the tagged commit is
`2b2468d`. Neither remaining limit blocks a release: the attestation
limit blocks the two rows that name it, and the live limit blocks the
`live` row and the four specs that wait on it. What closes each is in
the table above and in the pending table below; both need the user, not
this repository.

### What only a real release proves

The first release ran on 2026-09-10: tag `v0.1.0`, commit `2b2468d`,
Release run 34461460766, every job green or deliberately skipped. Beside
it the tag's `verify` run 34461461220 ran the cluster tiers, the
up-script check, and the mutation job, all green.

| Pending | Closed by | State |
|---|---|---|
| every artifact of the table for `linux/amd64` and `linux/arm64`, the signatures, the checksums, and the body against the changelog | the `release-verify` job of the first tag | closed by run 34461460766. Both images are OCI indexes carrying `linux/amd64` and `linux/arm64`; the four archives, `checksums.txt`, its cosign bundle, the deploy archive, the fixture, and the three SPDX documents are the eleven assets |
| the bill of materials as a release asset, spec 016's supply-chain row in its shipping half | the `build` job of the first tag: the three SPDX documents are uploaded with the other assets | closed by run 34461460766: `sbom-origod.spdx.json`, `sbom-origo-stubs.spdx.json`, and `sbom-source.spdx.json` are on the release |
| conformance against the image the tag published, on the kind stack | the `conformance` job of the first tag | closed by run 34461460766: `--- PASS: TestContract (65.84s)` over the groups 003, 007, 008, 009, 010, 015, 019, 020, and 012, with `TestSameAnswersOnStubAndStack` beside it |
| the bill of materials and the provenance *attached to* a published image and verified, the other half of spec 016's supply-chain row | the repository becoming public, which is what turns the four `attest-*` steps and `release-verify`'s `Verify the attestations` step back on; no tag closes it while the repository is private | open |
| the `live` job: `TestContract` against `ORIGO_LIVE_URL` with `ORIGO_LIVE_TOKEN` and spec 021's six-entry skip list | the first tag on a repository where the two secrets are set and something answers at the URL | open. Run 34461460766's `live` job ran and skipped: neither secret is set and `https://git.latere.ai` does not resolve. The job is green because a skipped test passes, which is why `publish`'s `needs.live.result == 'success'` did not stop the release and why nothing here claims the live run happened |
| the fork tag with `ORIGO_RELEASE_DEPLOY` unset | a maintainer, recorded in the release notes | closed by run 34461460766 itself, which ran with the variable unset |
| `TestPreviousReleaseFixture` against a fixture a release actually attached | the second tag | open. Run 34461460766 reports `--- SKIP: TestPreviousReleaseFixture (0.00s)`: no earlier release carried a fixture. `fixture-v0.1.0.tar.gz` is on this release, so the second tag closes it |
| `install-release` with `ORIGO_INSTALL_IMAGE` and `ORIGO_INSTALL_MANIFESTS` | spec 018, which owns step 6 of the pipeline | closed by run 34461460766: the job ran in 3 m 43 s against the published images and the published `deploy-v0.1.0.tar.gz` |

The spec stays at `testing`. Three rows are open, and each needs
something outside a tag: the repository becoming public or the plan
being upgraded, the two live secrets with an installation behind them,
and a second tag.

### The defect the first tag found

The first cut of `v0.1.0`, run 34450584556, published the release and
then skipped `install-release` and `release-verify`, so nothing verified
what had been published. The cause is not in either job: `deploy` is
skipped whenever `ORIGO_RELEASE_DEPLOY` is unset, and GitHub evaluates
the implicit `success()` gate of a job over its whole ancestor closure,
not over its direct `needs` alone, so the skip travelled through
`publish`, which runs under `always()`, into the two jobs below it,
which carried no condition. Both now carry
`if: ${{ always() && needs.publish.result == 'success' }}`, the same
form `live` and `publish` already used.
`TestReleaseSurvivesASkippedDeploy` in `tools/release` parses
`release.yml`, walks the graph, and fails on any job below `deploy` that
does not say `always()`;
`TestSilentSkipIsFound` proves the check on the shape the workflow had
when the release went out unverified. The tag was unwound, the release
deleted so the `conformance` job could not read `v0.1.0`'s own fixture
as a previous release's, and the version re-cut.

### Divergences

- Step 6 of the pipeline, `install-release`, was left out of
  `release.yml` at first: the Design says spec 018 owns it, 018 was
  `validated` and unbuilt, and a job that applies a document that does
  not exist would fail every tag. Spec 018 built `docs/install.md` and
  added the job on 2026-09-09, so `release.yml` now carries all seven
  steps and the gap comment is gone.

- The four `attest-*` steps and `release-verify`'s
  `gh attestation verify` are conditional on
  `!github.event.repository.private`, which the Design's attestation
  rule states. The v0.1.0 tag of 2026-09-10 failed in the `build` job
  at the first `attest-sbom` step with "Feature not available for the
  latere-ai organization", after both images were already pushed and
  before the signing step, so no later job ran and no release was
  published. The condition is the fix the user chose over making the
  repository public or upgrading the plan; both remaining options stay
  open and either one turns the steps back on, the first by itself.
  Each half logs a `::notice::` naming the reason, so a run of a
  private release says in its own log which check did not run.
- The images the `conformance` job runs are pulled back from the
  registry rather than built a second time, so the stack tests the
  published bytes. The spec's step 1 says the `build` job uploads the
  two `linux/amd64` images as `docker save` tarballs in
  `candidate-images`; it does, from the pushed manifest.
- The bill of materials is three SPDX documents, not one: the module
  graph from the tree, and the contents of each image, which carry the
  Debian packages beside the binary's own modules. Each image's own
  document is what `attest-sbom` attaches to it; all three are release
  assets. The spec asks for "the module graph and each image", which one
  document cannot be.
- `--provenance=false --sbom=false` on both `buildx` runs, with the
  attestations attached afterwards by `attest-sbom` and
  `attest-build-provenance`. An inline attestation carries the build
  time, so two builds of one commit produce different manifests; the
  fleet's `attest` job attaches them as referrers for the same reason.
- The release body is the changelog section followed by the evidence of
  the steps that ran, so `release-verify` checks the section is the
  body's prefix rather than the whole of it. A body that were only the
  section would carry no evidence, which the artifact table requires.
- `Dockerfile.ci` copies `out/release/bin/<os>_<arch>/origod`, the
  binary `make release-archives` cross-compiled, in place of
  `out/origod`. The image and the archives of one release then hold one
  binary, and the two-architecture row is reachable without compiling
  inside the image. Nothing on a push builds `Dockerfile.ci`: the
  `build` job of `verify.yml` builds `Dockerfile` and `Dockerfile.stubs`, both of which
  compile the binary themselves.
- The target that cross-compiles the archives is `make
  release-archives`, not `make release`. The shared gate reserves the
  target name `release` for `go tool lateregate release`, the command
  that cuts a tag, and fails any target of that name that does
  something else; the first form of this spec used the reserved name
  and turned the wiring check red on `748e19c`. Only the name changed:
  the recipe, `out/release/`, and the four archives are as the Design
  states.
- The `e2e` and `e2e-slow` jobs of `verify.yml` now run `go test -v`.
  They named no test in their logs, so a spec's stack proof had to be
  argued from the package's wall-clock time; a stack proof is now read
  from the run. This belongs here because 017 is what makes a release
  verifiable.

### Open

- `checksums.txt` covers the four binary archives alone, as the artifact
  table's row says. Whether `deploy-<version>.tar.gz` and
  `fixture-<version>.tar.gz` join it is not settled: the fixture is
  produced in a later job than the checksums, so covering it needs the
  signing moved after `conformance`.
- The `deploy` job takes its kubeconfig from `ORIGO_KUBECONFIG`, where
  the shared pipeline mints a short-lived one from a provider token. A
  long-lived kubeconfig in a repository secret is what spec 002's table
  fixes; whether the release should mint instead is for a later round.

### Coverage

`cmd/origod` 93.2%, `internal/repo` 94.4%, `internal/wal` 98.0%,
`internal/version` 100.0%, `test/conformance` 92.7%. `tools/release`
and `tools/smoke` hold a doc comment and a test that runs a shell
script, so they carry no statements and the gate does not measure them.

### What this closes elsewhere

- Spec 002's Images section and the decision row on the runtime base:
  both Dockerfiles and `Dockerfile.stubs` are on `debian:trixie-slim`
  pinned by one digest, git 2.47.
- Spec 016 stays at `testing`. Its supply-chain row reads "the release
  carries a bill of materials and provenance", and the pipeline that
  produces them exists while no release does; the first tag closes it.
- Specs 003 and 004 stay at `testing`. Nothing here touches their
  remaining criteria: the conformance suite and the code table (021),
  the cluster-job tests and the packs (013, 006), and the Spaces probe
  of the release checklist, which is a maintainer's step.
