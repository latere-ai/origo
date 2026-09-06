# Security

Report a vulnerability to security@latere.ai. Do not open a public issue
for one. You will hear back within three business days, and a fix for a
high severity issue ships within thirty days. Credit in the release notes
on request.

The threat model, the trust boundaries, and the control for each threat
are in [`specs/016-security-and-threat-model.md`](specs/016-security-and-threat-model.md).
Every release carries a software bill of materials and build provenance,
and the quality gate checks dependencies for known vulnerabilities on
every push.
