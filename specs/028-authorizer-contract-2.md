---
title: "Authorizer contract 2: the envelope the three open cores share, issuer-qualified subjects, the owner policy"
status: in-progress
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/007-authentication-and-delegation.md
  - specs/026-repository-directory.md
  - specs/027-anonymous-read.md
affects: [internal/auth/, internal/httpgit/, internal/api/, internal/sshd/, internal/config/, cmd/origod/, test/stubs/authorizer/, test/conformance/, docs/api.md, docs/install.md, specs/003-protocol-contract.md, specs/007-authentication-and-delegation.md]
effort: medium
created: 2026-09-13
updated: 2026-09-14
author: changkun
---

# Authorizer contract 2

## Overview

Origo asks one endpoint whether a subject may do one thing to one
repository. Two sibling open cores, Cella and Lux, ask the same
question of the same kind of endpoint, with a different envelope. The
family decided on 2026-09-13 that the three cores share one contract,
so that one authorizer serves all three and an operator writes one
endpoint (latere-ai/specs, `decisions/2026-09-13-one-platform-open-cores.md`).
This spec is Origo's side of that decision: the shared envelope as
Origo's authorizer contract 2, subjects qualified by their issuer, a
built-in owner policy, a configurable audience, and the shared
verifier. Contract 2 replaces contract 1 in one release: the family
runs no compatibility windows between its own components
(latere-ai/specs, `decisions/2026-09-13-no-compatibility-windows.md`),
and the one authorizer Origo has, auth today, changes in the same
batch.

The client protocol contract of [[003-protocol-contract]], the one a
git client and an API caller code against, is unchanged and stays
contract 1; `Origo-Contract` does not move. What changes is the
contract between a node and the operator's authorizer.

## Current state

Spec 007's authorizer is contract 1 and is what code.latere.ai runs.
It sends `{subject, actor, repo: {id, owner, slug}, action}` and reads
`{allow, reason, ttl, replicas, quota_bytes, requests_per_minute}`,
with spec 026's `list` action in a body of its own. Subjects are the
bare `sub` of the token although `ORIGO_OIDC_ISSUERS` is a list, so
two issuers that agree on a `sub` are one subject. There is no owner
policy: an installation must run an endpoint, and the install document
carries a thirty-line one. The audience is the constant `origo`. The
verifier is Origo's own, in `internal/auth`, with rules the sibling
cores' shared verifier does not have yet: an 8 KiB token bound, an
`iat` age, and a local issuer for the repository-bound tokens the node
mints.

The cache and retry rules of contract 1 are the ones the shared
contract adopted: an allow cached for the answer's `ttl` with a 600
second cap, a deny for 5 seconds, unavailability never, one retry when
the connection failed before a response line. They do not change.

## Design

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `ORIGO_ADMIN_SUBJECTS` | empty | Comma-separated issuer-qualified subjects allowed every action by the built-in owner policy; unused when an external authorizer is configured. |
| `ORIGO_OIDC_AUDIENCE` | `origo` | Audience accepted by the token verifier. |

### The envelope

```
POST <ORIGO_AUTHORIZER_URL>
Authorization: Bearer <ORIGO_AUTHORIZER_TOKEN>
Content-Type: application/json

{
  "subject":  "https://auth.example.com|0f5c1d2e-…",
  "issuer":   "https://auth.example.com",
  "sub":      "0f5c1d2e-…",
  "claims":   { …every verified claim of the token, verbatim… },
  "action":   "repo.write",
  "resource": {"kind": "Repository", "id": "…", "owner": "…", "slug": "…"},
  "request":  {"id": "…", "ip": "203.0.113.4", "user_agent": "git/2.47"}
}

200 {"allow": true, "reason": "", "ttl": 60, "limits": {"replicas": 1, "quota_bytes": 53687091200, "requests_per_minute": 600}}
200 {"allow": false, "reason": "…"}
```

| Field | Contract 1 | Contract 2 |
|---|---|---|
| `subject` | the token's `sub`, or its `act` | `<iss>\|<sub>`; empty for an anonymous request ([[027-anonymous-read]]) and for the probe |
| `issuer`, `sub` | absent | the two halves apart |
| `claims` | absent | every verified claim, verbatim; the node reads none |
| `actor` | always empty since 2026-09-13, when the family's D5 removed the `act` claim | absent |
| `action` | `read`, `write`, `admin`, `list` | `repo.read`, `repo.write`, `repo.admin`, `repo.list` |
| `repo` | `{id, owner, slug}` | `resource: {kind: "Repository", id, owner, slug}`; `repo.list` carries `{kind: "Repository"}` and no id |
| `request` | absent | `id`, `ip`, `user_agent` |
| figures | `ttl`, `replicas`, `quota_bytes`, `requests_per_minute` at the top level | `ttl` at the top level; the three figures under `limits` |
| `repo.list` answer | `{repos, next_cursor}`, `{allow: false}`, `{directory: false}` | unchanged, and `next_cursor` stays the authorizer's value passed through: issue #1 stays open until `filter` over the node's own name index replaces the directory answer, which the family record dates to the registry's move to `platformd` |

The five rules of spec 007 hold word for word. The probe id
`00000000-0000-0000-0000-000000000001` is unchanged and is the
shared contract's probe for Origo.

### Subjects

A subject is `<iss>|<sub>`, the issuer URL with its trailing slash
removed, a pipe, and the `sub` claim. It is what the authorizer
receives, what every entry header's `subject` and every event's
`pusher.sub` record from the release that ships contract 2 on, and
what `origo` prints. A repository-bound token's `sub` is the minter's
rendered subject and its `iss` is `ORIGO_PUBLIC_URL`, so its rendered
subject would nest; the node renders a local token's subject as the
`sub` it carries, which is already rendered. Entries written before
that release carry the bare `sub` and are history.

### The owner policy

With `ORIGO_AUTHORIZER_URL` unset, `ORIGO_ADMIN_SUBJECTS` is read and
the log says `owner policy` at start: a subject may create a
repository, and may read, write and administer a repository whose
`owner` label is its rendered subject; `repo.list` returns its own; a
subject in `ORIGO_ADMIN_SUBJECTS` may do everything on every
repository; the probe id is denied; anonymous is denied. The `owner`
is recorded in `meta` at creation from the creating subject. With an
authorizer set, `ORIGO_ADMIN_SUBJECTS` is read and unused. The install
document's thirty-line endpoint becomes an example, not a requirement.

### Audience and verifier

`ORIGO_OIDC_AUDIENCE`, default `origo`, replaces the constant. The
verifier becomes `latere.ai/x/pkg/authkit/jwt` with the options the
family's C5 adds for Origo's rules: the token size bound, the `iat`
age, the local issuer with a fixed key, and the reason table of spec
007, which becomes the package's. The behaviour of every row of spec
007's verification table is unchanged; `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure`
is the proof.

### The shared package

The envelope, the client with its cache and retry, the owner policy's
frame, the stub authorizer, and the conformance test an authorizer
passes are `latere.ai/x/pkg/authz`; Origo adds its action vocabulary
and its `resource` shape. `test/stubs/authorizer` becomes that
package's stub with Origo's rule table.

### One contract, one release

Contract 1's envelope, its `actor` field, its bare-`sub` subjects and
the constant audience are removed in the release that ships contract
2; nothing selects between them. The stub authorizer, `origod check`
and the conformance suite speak contract 2 alone. auth's authorizer
for Origo changes to contract 2 in the same coordinated batch, so the
installation's authorizer and node roll together.

## Not in this spec

Moving the authorizer, the registry, the grants and the SSH keys from
auth to the platform control plane (the family's id-06); the `act`
claim, which the family's D5 removed from the verifier and the minter on
2026-09-13 ahead of this spec; the `filter`-based `repo.list`, which waits for the
registry to sit beside the node's name index.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every operation reaches the authorizer with the row's `action`, a `resource` of kind `Repository`, `issuer` and `sub` apart, every claim of the token in `claims`, and a `request` block; no code path builds the contract 1 body | `TestAuthorizerEnvelope`, table-driven over every handler; `TestContractOneIsGone`, which finds no `"repo":` or `"actor":` key in the client | built |
| The three figures are read from `limits` and reach the consumers spec 007 names | `TestFiguresReachTheirConsumers` | built |
| A subject is `<iss>\|<sub>` in the authorizer request, the entry header, the event, and `origo`'s output; two issuers agreeing on a `sub` are two subjects | `TestSubjectsAreIssuerQualified` | built |
| With no authorizer configured the owner policy holds every rule of its list, denies the probe and anonymous, and `ORIGO_ADMIN_SUBJECTS` acts on everything | `TestOwnerPolicy`, table-driven | built |
| `ORIGO_OIDC_AUDIENCE` changes the accepted audience and defaults to `origo` | `TestAudienceIsConfigurable` | built |
| Every row of spec 007's verification table holds through the shared verifier with the same reason | `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure` unchanged | built |
| `origod check` sends the probe in contract 2 and reads an allow as an endpoint that does not read the request | `TestCheckProbesTheAuthorizer` | built |
| The conformance suite's authorizer group passes against the shared stub in contract 2 | `test/conformance`, the authorizer group | built |

## State on 2026-09-14

The node's side of contract 2 is built and its tests are green. What
shipped: the shared envelope (`latere.ai/x/pkg/authz`), issuer-qualified
subjects rendered in the verifier and carried through entry headers and
events, the three figures decoded out of `limits` in one seam, the
built-in owner policy over `wal.Meta.Creator` for a node with no
authorizer, `ORIGO_OIDC_AUDIENCE` (default `origo`) and
`ORIGO_ADMIN_SUBJECTS`, an optional `ORIGO_AUTHORIZER_URL`, and
`test/stubs/authorizer` as a thin wrapper over `authz/stub`. All eight
acceptance rows pass.

Two departures from the design above, both deliberate:

- **The verifier stays Origo's own.** The design says it becomes
  `latere.ai/x/pkg/authkit/jwt` with the family's C5 options. C5 has not
  shipped: authkit/jwt on `pkg` main is RS256-only, one issuer by exact
  string, with no ES256, no token-size bound, no `iat` age, no local
  issuer with a fixed key, and no reason table. Until C5 adds those,
  Origo keeps `internal/auth`'s verifier; its behaviour is unchanged and
  `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure` is the proof. The
  audience became configurable in place. This is the one waiver.

- **Release is coupled and not yet cut.** Contract 1 is removed in the
  same release that ships contract 2, and auth's authorizer for Origo
  changes to contract 2 in the same coordinated batch (the identity
  epic's id-06 git-plane move to `platformd`). The node code and its
  overlay audience variable are ready; the tag waits for that batch.
  Moving to `complete` waits for the release.
