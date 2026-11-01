# Upgrades

What a version number promises, what an upgrade needs from you, and what
to do when a node refuses a repository it cannot read. One file per log
format sits beside this one: `2.md` when a release moves the log format
to 2, and so on, which is the file a node's log line names when it meets
a format it cannot read. There is no such file yet, because the log
format has not changed.

## What the number promises

Origo uses semantic versioning on the release tag.

| Change | Bump |
|---|---|
| a removal or a changed meaning in the API contract, the log format, a configuration variable, or an event payload | major |
| a new capability, endpoint, field, variable, metric, or event kind | minor |
| a fix with no visible change | patch |

After a major bump the previous contract stays served for twelve
months. A client selects it with the request header `Origo-Contract`
and the contract number it was written against.

## Upgrading inside one major

Any release upgrades from any earlier release of the same major with no
steps. Change the image tag in your overlay and apply it, or set it on
the running workload, with `VERSION` the tag you are moving to:

```sh
kubectl -n origo set image deployment/origod "*=ghcr.io/latere-ai/origod:$VERSION"
kubectl -n origo rollout status deployment/origod --timeout=600s
```

`*=` sets both containers of the pod, the node and the check that runs
before it, which are one image. Name only the node and the check keeps
running the old release. The Deployment replaces one pod at a time with
none unavailable. A
replaced pod starts with an empty cache and fills it from the log on the
first request for each repository, so the first request for a
repository after a rollout is slower than the ones after it. There is no
schema and no migration step.

The one exception is an authorization endpoint of your own. A minor
release can add an action to the vocabulary without asking it, and a
later minor release starts asking it for an operation. An endpoint that
answers an action it does not know with an error has to decide the new
action before the nodes reach the release that asks it, or that
operation answers 503 `authorizer_unavailable`. The changelog of the
release that adds an action says which release asks it;
[`authorizer.md`](../authorizer.md) describes each one.

Rolling back to an earlier release is supported inside one minor series.
Across a major it is not: the newer release may have written objects the
older one refuses, which is the refusal below.

## Support

Minor releases ship as features land, patch releases as fixes land. The
two most recent minor series receive patches.

## When a node refuses a repository

A node reads every log format version it has ever written and writes the
newest. A node that finds an object written by a newer release logs

```
log format v2 is newer than this release reads; see docs/upgrades/2.md
```

and answers that one repository with 503 and the code
`repository_unavailable`, naming the object in `details.key`. Every
other repository goes on serving and `/readyz` stays 200, so the
installation keeps working.

You see this after a rollback across a major, or while a fleet runs two
majors at once. The fix is to run the newer release again, then follow
the upgrade document the log line names.

## Verifying what you install

Every release after v0.16.5 is built, signed and published by the
shared release stages in [latere-ai/ci](https://github.com/latere-ai/ci),
run for this repository. The signatures are keyless, so no key held by
anyone signs them: the images are signed by the build stage's identity,
and `checksums.txt` by the publish stage's. Set `VERSION` to the tag you
are installing, then check the image, the archives, and the manifests:

```sh
ISSUER=https://token.actions.githubusercontent.com

cosign verify "ghcr.io/latere-ai/origod:$VERSION" \
  --certificate-oidc-issuer "$ISSUER" \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-build\.yml@' \
  --certificate-github-workflow-repository latere-ai/origo

gh release download "$VERSION" --repo latere-ai/origo \
  --pattern 'checksums.txt*' --pattern 'deploy-*.tar.gz' --pattern 'origo*_*.tar.gz'
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer "$ISSUER" \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-publish\.yml@' \
  --certificate-github-workflow-repository latere-ai/origo checksums.txt
grep -q " deploy-$VERSION.tar.gz$" checksums.txt
sha256sum -c --ignore-missing checksums.txt
```

The first command proves the image was built and signed by the build
stage, run for this repository. Any repository can call a public
workflow, so the identity names the stage and
`--certificate-github-workflow-repository` is what ties the signature to
Origo. The second pair proves the checksum file was signed by the
publish stage run for this repository, and `sha256sum -c` then proves
every archive you downloaded, the deploy manifests among them, matches
it; the file also lists files you did not download, which
`--ignore-missing` skips. `--ignore-missing` would skip the deploy
archive the same way if the file did not list it, so the `grep` before
it stops on a release whose sums leave the manifests out.

The bill of materials is a release asset, one per image:
`sbom-origod.spdx.json` and `sbom-origo-stubs.spdx.json`.

Each image also carries an SBOM attestation and a build provenance
attestation in the registry, which name the build stage, the repository
and the commit the image was built from:

```sh
gh attestation verify "oci://ghcr.io/latere-ai/origod:$VERSION" --repo latere-ai/origo \
  --signer-workflow latere-ai/ci/.github/workflows/image-build.yml
```

### Releases up to v0.16.5

Releases up to v0.16.5 were signed by this repository's own release
workflow, and their checksum signature is `checksums.txt.cosign.bundle`.
Check one of those with the identity naming that workflow and no
`--signer-workflow`:

```sh
IDENTITY='^https://github.com/latere-ai/origo/\.github/workflows/release\.yml@refs/tags/'
ISSUER=https://token.actions.githubusercontent.com

cosign verify --certificate-identity-regexp "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" "ghcr.io/latere-ai/origod:$VERSION"

gh release download "$VERSION" --repo latere-ai/origo \
  --pattern 'checksums.txt*' --pattern 'deploy-*.tar.gz' --pattern 'origo*_*.tar.gz'
cosign verify-blob --bundle checksums.txt.cosign.bundle \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" checksums.txt
grep -q " deploy-$VERSION.tar.gz$" checksums.txt
sha256sum -c --ignore-missing checksums.txt

gh attestation verify "oci://ghcr.io/latere-ai/origod:$VERSION" --repo latere-ai/origo
```

Those releases carry a third bill of materials, `sbom-source.spdx.json`,
for the module graph. v0.1.0 predates the attestations and carries
neither. Its signature, checksums, and bills of materials are
unaffected.
