# Security

Report a vulnerability to security@latere.ai. Do not open a public issue
for one. You will hear back within three business days, and a fix for a
high severity issue ships within thirty days. Credit in the release notes
on request.

Fixes go to the two most recent minor release series. v0.16.7 is the
current release.

What Origo protects, against whom, and how each threat is answered is
written down in the
[threat model](specs/016-security-and-threat-model.md), so a reviewer
can check the design rather than take it on faith. Every request that
names a repository is authorized before the repository is looked up,
so a refused caller cannot tell a private repository from a missing
one. A request without a token is admitted only on the read routes of
an installation that turns anonymous reads on, and is authorized the
same way. A
server-side fetch of another host reaches only a host the operator
listed, and never a private, cluster, or loopback address. Git runs
with a minimal environment, no shell, and its object checks on. The
pod runs as a non-root user on a read-only root file system with every
capability dropped.

Dependencies are checked for known vulnerabilities on every push. A
release is built, signed and published by the shared release stages in
[latere-ai/ci](https://github.com/latere-ai/ci), run for this
repository. It carries an SPDX bill of materials for each image, and
cosign signatures over the images and the checksums that name those
stages and this repository. Each image also carries an SBOM attestation
and a build provenance attestation, so `gh attestation verify` answers
for the image you are about to run.
[`docs/upgrades/README.md`](docs/upgrades/README.md) says what to verify
and with which command, for a current release and for the releases
signed before the stages.
