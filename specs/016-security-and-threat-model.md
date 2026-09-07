---
title: "Security and threat model: what Origo protects, against whom, and how"
status: drafted
track: infra
depends_on:
  - specs/001-architecture.md
  - specs/007-authentication-and-delegation.md
  - specs/012-limits-and-abuse.md
affects: [internal/httpgit/, internal/api/, internal/auth/, internal/repo/, internal/wal/, internal/placement/, internal/config/, deploy/, SECURITY.md]
effort: medium
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Security and threat model

## Overview

A git server holds source code and accepts bytes from anyone who can
reach it. This spec names the assets, the attackers, the trust
boundaries, and the control that answers each threat, so a reviewer
outside the project can check the design rather than take it on faith.
It also fixes the disclosure process and the hardening of the process
and the pod.

## Current state

Phase 1 verifies the static bearer of spec 002 in constant time, runs
every git subprocess through `internal/repo.Git` with the environment
below, `core.protectNTFS` and `receive.fsckObjects` on, and a 5 minute
deadline; the smart HTTP services run in their own process group that is
killed whole, the other subprocesses under the context's kill. The pod runs with
the security context below (`deploy/base/deployment.yaml`). `SECURITY.md`
exists at the root with the disclosure process. The gate runs `vuln` on
every push. Not yet: `transfer.fsckObjects`, `core.protectHFS`, a bill
of materials, a validator fuzz test, a test over the subprocess
environment.

## Design

### Assets

Repository contents and history; the bucket credentials; the token
signing key `ORIGO_TOKEN_KEY`; `ORIGO_AUTHORIZER_TOKEN`;
`ORIGO_EVENTS_SECRET`; `ORIGO_GOSSIP_SECRET`; the availability of the
service for every repository at once.

### Trust boundaries

```mermaid
flowchart LR
  subgraph untrusted
    C[git client / HTTP client]
  end
  subgraph node[origod, one pod]
    H[httpgit + api]
    G[git subprocesses]
    W[wal client]
  end
  subgraph trusted
    S[(bucket)]
    I[OIDC issuer]
    A[consumer authorizer]
    E[events sink]
  end
  subgraph external
    X[import / verify source]
  end
  subgraph peers[other origod pods]
    P[gossip]
  end
  C -->|TLS, bearer| H
  H -->|argv, stdin, GIT_DIR| G
  H --> W --> S
  H -->|JWKS| I
  H -->|service token| A
  H -->|HMAC| E
  G -->|https, egress allow-list| X
  P -.->|UDP, HMAC| H
```

Everything left of `origod` is hostile. The bucket, the issuer, the
authorizer, and the sink are trusted for what they say but not for
availability (spec 015). Gossip peers (spec 005) are the other pods of
the same Deployment and hold `ORIGO_GOSSIP_SECRET`; a datagram is
believed for membership only when its MAC verifies, and even then it
grants nothing: an announcement triggers a currency check, the log
decides what is served. The NetworkPolicy of `deploy/base` that admits
UDP 7946 from the Deployment's own pods only is defence in depth, not
the control, because a policy is enforced by the network plugin and a
MAC by the node.
The source of an `import` or a `verify` (specs 019, 014) is a host the
caller names, so it is hostile until the egress allow-list admits it,
and trusted only for the bytes git checks.
A git subprocess handles hostile bytes and is treated as semi-trusted:
it runs with the minimal environment below, no shell, no network, a
deadline, and under the pod's security context.

### Threats and controls

| Threat | Control | Spec |
|---|---|---|
| Reading a repository without permission | every request authenticated; authorization asked of the consumer per action and cached for at most 600 seconds; deny by default and never fail-open; repository-bound tokens scoped to one repository and one scope | 007 |
| Writing to a repository without permission | same; `write` is asked at `info/refs?service=git-receive-pack`, before the pack is accepted | 007 |
| A service acting as a user beyond its mandate | `act` is recorded on every entry and event; the authorizer sees both subject and actor and may refuse the pair | 007 |
| Malformed or malicious git objects | `receive.fsckObjects` (phase 1), `transfer.fsckObjects`, `core.protectNTFS` (phase 1), `core.protectHFS`; Origo never checks out a tree on the server except into the archive stream, which is `git archive` with no filesystem write | 004, 009 |
| Command injection through refs, owner, slug, or paths | reference names validated by `internal/wal.ValidRefName`; owner and slug by the grammar of spec 003; subprocess arguments never pass through a shell; `GIT_DIR` set explicitly; the only hook is Origo's own pre-receive, installed by the node and never from a push | 003, 004 |
| Resource exhaustion by one client | per-subject rate limit, per-node subprocess cap, body and repository size limits, subprocess deadlines | 012 |
| Server-side request forgery through `import` and `verify` | every server-side fetch goes to a host on the egress allow-list `ORIGO_EGRESS_ALLOW` (spec 002): comma separated exact hostnames or `*.` wildcards, matched with `latere.ai/x/pkg/hostmatch` after lower-casing and trailing-dot removal; the default, unset, refuses every source. Whatever the list says, the fetch runs through a `DialContext` that resolves the host once, refuses every resolved address in a refused range, and dials one of the remaining addresses by IP, so a name that rebinds between the check and the connection cannot redirect it; the refused ranges are RFC 1918, RFC 4193 (ULA), loopback, link-local, and unspecified, plus the cluster's service and pod ranges from `ORIGO_CLUSTER_CIDRS` (spec 002), default empty, meaning only the well-known ranges. Every redirect hop is resolved and dialed the same way. A refused source is 400 `invalid_request` with `details.reason: "egress"` and no connection is opened. The dialer is what git's `http.proxy` cannot give, so the fetch runs through a local forward proxy the node starts per import on a loopback port, which git is pointed at and which applies the dialer. Git is configured through the environment and nothing else, stated here once and referenced by specs 019 and 014: `GIT_CONFIG_COUNT=2`, `GIT_CONFIG_KEY_0=http.<source>.extraheader` with `GIT_CONFIG_VALUE_0=Authorization: Bearer <token>`, and `GIT_CONFIG_KEY_1=http.proxy` with `GIT_CONFIG_VALUE_1=http://127.0.0.1:<port>` of that proxy; the source URL, the token, and the proxy never appear on git's command line, and the proxy accepts connections from the one git process it was started for and closes with it | 019, 014 |
| Membership forgery through gossip | every datagram carries an HMAC-SHA256 under `ORIGO_GOSSIP_SECRET`; one without a valid MAC is dropped before it is parsed and counted on `origo_gossip_packets_total{direction="dropped"}`, so a sender without the secret cannot enter the live set, keep a dead node in it, or trigger a catch-up, and cannot change who a repository's compaction primary, import lease holder, or orphan sweeper is (specs 006, 019); the NetworkPolicy on the gossip port is defence in depth | 005 |
| Amplification through gossip | a valid datagram is at most one catch-up, and catch-ups triggered by gossip are rate-limited to one per repository per second, so a flood from a node that holds the secret costs one `HEAD` per named repository per second and nothing else; a datagram that names a repository the node does not hold is dropped; an invalid one costs one HMAC | 005 |
| Exhaustion through the bucket | breakers and per-operation deadlines so one slow client cannot hold a subprocess open against a slow bucket | 015 |
| Token theft | short-lived repository tokens; issuer tokens verified for audience; tokens never logged, never in URLs on the server side; the basic auth password is accepted for git and never written to a log line | 007, 011 |
| Secret exposure in logs or metrics | fixed-vocabulary labels; the redaction test of spec 011; the registers rule for messages | 011 |
| Tampering with the log | objects are immutable once written; entry lengths and digests are checked at materialization; a mismatch is an integrity error, never repaired from a local copy | 004, 015 |
| Cross-repository leakage on a node | one bare repository per id under `repos/`, `GIT_DIR` per request, no shared object store, no alternates | 004 |
| Supply chain | the image is built from a pinned Go toolchain and a pinned Debian base with git, with a bill of materials and provenance attached to the release; dependencies are the standard library and `latere.ai/x/pkg` | 002, 017 |
| A compromised node | the node holds the bucket credentials and the signing key; the blast radius is every repository the credentials reach, which is why one installation serves one trust domain and the bucket prefix is dedicated | 001 |

### Process and pod hardening

The pod in `deploy/base`: `runAsNonRoot` as user and group 65532,
`readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`,
capabilities `drop: [ALL]`, seccomp `RuntimeDefault`,
`automountServiceAccountToken: false`, the cache volume and `/tmp` the
only writable mounts, CPU request 250m, memory request 256Mi and limit
2Gi. Git subprocesses inherit exactly this environment
(`internal/repo.Git.Env`): `PATH`, `HOME` (an empty directory under
`ORIGO_DATA_DIR`), `GIT_DIR`, `GIT_CONFIG_NOSYSTEM=1`,
`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C`, plus
`GIT_PROTOCOL` on the smart HTTP services and `ORIGO_HOOK_DIR` on
`receive-pack`. No credential helper, no user hooks.

### Transport

TLS terminates at the ingress; the public listener may be plain HTTP
inside the cluster only. Bearer tokens are required on every request
including `info/refs`; the unauthenticated paths are
`GET /.well-known/jwks.json` (spec 007) and the probes `GET /readyz` and
`GET /version` (spec 002), which serve no repository state. There is no anonymous read in
v1; an operator who wants public repositories does so through the
authorizer answering allow for an anonymous subject, which is not in
this spec.

### Disclosure

`SECURITY.md` at the root: report to `security@latere.ai`, acknowledged
within 3 business days, fixed releases within 30 days for high
severity, credit in the release notes on request. Vulnerable
dependencies are caught by the gate's `vuln` check on every push.

## Not in this spec

Anonymous reads. Encryption of objects beyond what the bucket does.
Audit export beyond the log itself.

## Acceptance criteria

- A request without a token, with a token for another audience, or with
  an expired token is refused with 401 on every route the public
  listener serves except the unauthenticated three, the sweep walking
  the mux's patterns so a route added later is covered without a change
  to the test (proposed: `cmd/origod`, `TestEveryRouteRequiresAToken`,
  the route sweep spec 007 names beside its verifier tests).
- A pushed pack containing a `.git` tree entry, an NTFS-reserved name,
  or a broken object is rejected with git's message and no entry is
  written (proposed: `internal/httpgit`, `TestMaliciousPackWritesNothing`,
  packs built by `internal/gittest`).
- A reference name, owner, or slug containing a shell metacharacter,
  `..`, or a control character is refused by the validators and no
  subprocess starts (`internal/wal`, `TestValidRefName`), and
  `FuzzValidRefName` and `FuzzValidLabel` in `internal/wal` find no
  input the validator accepts that git refuses: each runs as a
  seed-corpus test in the suite on every push and for 40 seconds under
  `make fuzz` (spec 013) on the weekly schedule (proposed:
  `internal/wal`, `FuzzValidRefName`, `FuzzValidLabel`).
- A git subprocess observes exactly the documented environment
  (proposed: `internal/repo`, `TestSubprocessEnvironment`, running `env`
  through `Git.Command`).
- The egress dialer, tested on its own in `internal/api` with no
  operation around it: a host not on `ORIGO_EGRESS_ALLOW`, or one that
  resolves to `127.0.0.1`, `10.0.0.1`, `169.254.169.254`, `fd00::1`,
  or an address in `ORIGO_CLUSTER_CIDRS` though listed, or a redirect
  to one of those, is refused with the error the handlers map to 400
  `invalid_request` with `details.reason: "egress"` and opens no
  connection, asserted by a listener that counts connections; with the
  list unset every host is refused; and a host whose name resolves to
  a public address on the check and to `127.0.0.1` on the next lookup
  (a resolver the test controls) is dialed at the first address and
  never reaches the loopback listener (proposed: `internal/api`,
  `TestEgressDialerHonoursTheAllowList`,
  `TestEgressDialerPinsTheResolvedAddress`). That `import` and `verify`
  run through the dialer is asserted by specs 019 and 014 in their own
  criteria.
- A gossip datagram without a valid MAC is dropped and never enters
  the live set (spec 005, `TestGossipDropsABadMAC`), and 10 000 valid
  gossip datagrams for one repository in one second cause at most one
  catch-up (spec 005, `TestGossipCatchUpIsRateLimited`).
- The pod runs with the documented security context in the kind stack,
  and a test overlay that sets `allowPrivilegeEscalation: true` is refused
  by Pod Security admission at `restricted`, the label spec 013's
  overlay puts on the namespace (proposed: `test/e2e`,
  `TestClusterPodSecurityContext` on the stack of spec 013).
- `SECURITY.md` exists (in the tree) and the release carries a bill of
  materials and provenance (spec 017's artifact criterion).
