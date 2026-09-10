# Upgrades

What a version number promises, what an upgrade needs from you, and what
to do when a node refuses a repository it cannot read. One file per
major upgrade sits beside this one: `1.md` when a release moves the log
format to 2, and so on. There is no such file yet, because there has
been no major upgrade.

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
steps. Change the image tag and apply:

```sh
kubectl -n origo set image deployment/origod origod=ghcr.io/latere-ai/origod:v1.2.3
kubectl -n origo rollout status deployment/origod --timeout=600s
```

The Deployment replaces one pod at a time with none unavailable. A
replaced pod starts with an empty cache and fills it from the log on the
first request for each repository, so the first request for a
repository after a rollout is slower than the ones after it. There is no
schema and no migration step.

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

Every release is signed with the release workflow's own identity, with
no key held by Latere, and carries a bill of materials.

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/latere-ai/origo/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/latere-ai/origod:v1.2.3

sha256sum -c checksums.txt
```

The bill of materials is a release asset: `sbom-origod.spdx.json`,
`sbom-origo-stubs.spdx.json`, and `sbom-source.spdx.json`, one per
image and one for the module graph.

Each image also carries an SBOM attestation and a build provenance
attestation, attached to the image in the registry, so the fourth check
is

```
gh attestation verify oci://ghcr.io/latere-ai/origod:v1.2.3 --repo latere-ai/origo
```

which names the workflow and the commit the image was built from.
v0.1.0 was built while the repository was private and carries neither
attestation, because GitHub's attestation API refuses a private
repository on this organization's plan. Its signature, checksums, and
bills of materials are unaffected.
