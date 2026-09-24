# Releasing

For maintainers: how a version is cut, what the release workflow builds
and proves before it publishes, and how a fork publishes its own. What
a version number promises to users is in
[`upgrades/`](../upgrades/README.md).

## Notes first

Every tag has a section in `CHANGELOG.md`, and that section is the body
of the GitHub release. Write under `## Unreleased` as work lands, in
terms of what changed for whoever runs or calls Origo, not what was
committed. A tag without a section is refused twice: by the pre-push
hook before it leaves your machine, and by the release workflow before
it creates anything.

## Cutting a release

```sh
go tool lateregate release v0.11.0
```

refuses while CI on the current commit is red (`-force-red` overrides,
for a red that is known and unrelated), then in one commit:

1. moves the notes under `## Unreleased` into a `## v0.11.0 - <date>`
   section;
2. rewrites the version inside each `release.stamp` pattern of
   `.lateregate.yaml`: the current release named in `SECURITY.md`, the
   image tag of `deploy/prod/kustomization.yaml`, and the `VERSION=`
   line of `docs/install.md`. A pattern that does not match its file
   exactly once refuses the release before anything is written;
3. tags the commit and pushes the commit and the tag together, so the
   workflow runs once.

Tests in `tools/docs` hold each stamped file to the newest changelog
section, so a stamp that stopped matching fails the build on `main`
rather than shipping a stale version.

## What a tag runs

`.github/workflows/release.yml` runs these jobs, in order, and publishes
only when the ones before `publish` pass:

| Job | What it does |
|---|---|
| `build` | builds `origod` and `origo` for linux and darwin on amd64 and arm64 into eight archives, `checksums.txt` over them, the two multi-arch images (`origod`, `origo-stubs`), the deploy archive, and the release fixture. Signs both images and `checksums.txt` keylessly with cosign, writes three SPDX bills of materials (one per image, one for the module graph), and attaches SBOM and build provenance attestations to each image |
| `conformance` | brings up the kind stack from the published images and runs the conformance suite, the release fixture, and the previous release's fixture on this build, which is what proves the log format still reads |
| `deploy` | only when the repository variable `ORIGO_RELEASE_DEPLOY` is set: applies `deploy/prod`, waits for the rollout, and runs `tools/smoke/release.sh`, which waits for the served version to reach the tag |
| `live` | the conformance suite against `ORIGO_LIVE_URL` |
| `publish` | creates the GitHub release with the changelog section and the evidence of the jobs above, and uploads every artifact |
| `install-release` | walks `docs/install.md` on a bare kind cluster against the published images and the published deploy archive, with nothing from this workflow's own build |
| `release-verify` | on a clean runner, verifies the image signatures, the attestations, the `checksums.txt` bundle, every archive against it, and that verification refuses a foreign identity |

The same tag also runs the tag-only jobs of `verify.yml`: the cluster
tier, the kind up-script check, and the mutation job. See
[`testing.md`](testing.md).

## What a release carries

| Asset | Contents |
|---|---|
| `origod_<version>_<os>_<arch>.tar.gz`, `origo_<version>_<os>_<arch>.tar.gz` | the node and the agent client, statically linked |
| `checksums.txt`, `checksums.txt.cosign.bundle` | SHA-256 of every archive, and its keyless signature |
| `deploy-<version>.tar.gz` | `deploy/base` and `deploy/examples` with both images pinned to the version, and the `docs/install.md` of that version |
| `fixture-<version>.tar.gz` | a repository log written by this release, which the next release must read |
| `sbom-origod.spdx.json`, `sbom-origo-stubs.spdx.json`, `sbom-source.spdx.json` | the bills of materials |

`deploy-archive.sh` refuses to pack while any manifest still names a
placeholder tag (`unreleased`, `candidate`), so an archive cannot ship
an image nobody built.

## Publishing from a fork

The workflow derives the image namespace from the repository that runs
it, `ghcr.io/<owner>`, so a fork's tag publishes the fork's own images
and its deploy archive names them. Set the repository variable
`ORIGO_IMAGE_NAMESPACE` to publish elsewhere; an owner whose name has
capital letters must set it, because an image reference is lower case.
Leave `ORIGO_RELEASE_DEPLOY` unset and a tag deploys nothing.
