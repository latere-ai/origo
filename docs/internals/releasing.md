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

`.github/workflows/release.yml` builds, signs and publishes through the
shared image release stages of
[latere-ai/ci](https://github.com/latere-ai/ci) (`image-build.yml`,
`image-deploy.yml`, `image-publish.yml`, `release-verify.yml`), with
Origo's own jobs between and after them. It runs these jobs, in order, and publishes only
when the ones before `publish` pass:

| Job | What it does |
|---|---|
| `build` | the build stage. Refuses a tag whose commit is not on `main` or whose `verify.yml` run on the branch push did not pass, waiting up to an hour for a run still going, and a tag without a changelog section. Builds `origod` and `origo` for linux and darwin on amd64 and arm64 into eight archives, then the two multi-arch images (`origod`, `origo-stubs`), each pushed by digest under no tag, checks `origod` reports the tag's version, signs both keylessly, writes an SPDX bill of materials for each, and attaches SBOM and build provenance attestations to each image. Last, the deploy archive |
| `conformance` | pulls the pushed digests, checks git inside `origod`, brings up the kind stack from them and runs the conformance suite, the release fixture, and the previous release's fixture on this build, which is what proves the log format still reads, then packs this release's fixture |
| `deploy` | the deploy stage, only when the repository variable `ORIGO_RELEASE_DEPLOY` is set: sets `origod` in `deploy/prod` to the pushed digest (both containers of the Deployment run it), applies the overlay with the kubeconfig in the `ORIGO_KUBECONFIG` secret, waits for the `origod` rollout, then runs `tools/smoke/release.sh` against `https://code.latere.ai` every five seconds for up to two minutes until it serves the tag. `/` belongs to the browsing interface on that hostname, so the smoke leaves the landing check off. Only `origod` is passed: `origo-stubs` is pushed too, but `deploy/prod` does not run it |
| `live` | the conformance suite against `ORIGO_LIVE_URL` |
| `publish` | the publish stage. Tags both digests with the version, writes `checksums.txt` over every file the release attaches, the fixture included, signs it keylessly, and publishes the GitHub release with the changelog section and the evidence of the jobs above |
| `install-release` | checks that `checksums.txt` names the deploy archive and the fixture and that the archive carries the base, the page and `origod` at the tag, then walks `docs/install.md` on a bare kind cluster against the published images and the published deploy archive, with nothing from this workflow's own build |
| `release-verify` | the verify stage, on a clean runner with no checkout: the `checksums.txt` signature, refused for another repository, every asset against it, each image's tag resolving to a signed and attested digest, and the release body against the changelog section |

Nothing has a tag before `publish`, so `conformance` and `deploy` name
the images by the digests `build` outputs. The signatures name the
stages in latere-ai/ci and are bound to this repository by the
certificate's workflow repository; `docs/upgrades/README.md` gives the
commands.

The same tag also runs the tag-only jobs of `tiers.yml`: the cluster
tier, the kind up-script check, and the mutation job. See
[`testing.md`](testing.md).

## What a release carries

| Asset | Contents |
|---|---|
| `origod_<version>_<os>_<arch>.tar.gz`, `origo_<version>_<os>_<arch>.tar.gz` | the node and the agent client, statically linked |
| `checksums.txt`, `checksums.txt.sigstore.json` | SHA-256 of every other asset, the deploy archive, the fixture and the bills of materials included, and its keyless signature |
| `deploy-<version>.tar.gz` | `deploy/base` and `deploy/examples` with both images pinned to the version, and the `docs/install.md` of that version |
| `fixture-<version>.tar.gz` | a repository log written by this release, which the next release must read |
| `sbom-origod.spdx.json`, `sbom-origo-stubs.spdx.json` | the bills of materials, one per image |

`deploy-archive.sh` refuses to pack while any manifest still names a
placeholder tag (`unreleased`, `candidate`), so an archive cannot ship
an image nobody built.

## Publishing from a fork

The build stage derives the image namespace from the repository that
runs it, `ghcr.io/<owner>` with the owner lowered, so a fork's tag
publishes the fork's own images and its deploy archive names them. Set
the repository variable `RELEASE_IMAGE_NAMESPACE` to an owner path under
`ghcr.io` to publish elsewhere. Leave `ORIGO_RELEASE_DEPLOY` unset and a
tag deploys nothing.
