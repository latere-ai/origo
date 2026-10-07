---
title: "Authorizer contract 2: the envelope the three open cores share, issuer-qualified subjects, the owner policy"
status: complete
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/007-authentication-and-delegation.md
  - specs/026-repository-directory.md
  - specs/027-anonymous-read.md
affects: [authorizer/, internal/auth/, internal/contract/, internal/httpgit/, internal/api/, internal/sshd/, internal/config/, cmd/origod/, test/stubs/authorizer/, test/conformance/, docs/api.md, docs/install.md, specs/003-protocol-contract.md, specs/007-authentication-and-delegation.md]
effort: medium
created: 2026-09-13
updated: 2026-10-07
author: changkun
---

# Authorizer contract 2

## Overview

Origo asks one endpoint whether a subject may do one thing to one
repository. Two sibling open cores, Cella and Lux, ask the same
question of the same kind of endpoint, with a different envelope. The
family decided on 2026-09-13 that the three cores share one contract,
so that one authorizer serves all three and an operator writes one
endpoint. This spec is Origo's side of that decision: the shared envelope as
Origo's authorizer contract 2, subjects qualified by their issuer, a
built-in owner policy, a configurable audience, and the shared
verifier. Contract 2 replaces contract 1 in one release: the family
runs no compatibility windows between its own components, a rule it
decided the same day, and the one authorizer the hosted installation
runs for Origo changes in the same batch.

The client protocol contract of [[003-protocol-contract]], the one a
git client and an API caller code against, is unchanged and stays
contract 1; `Origo-Contract` does not move. What changes is the
contract between a node and the operator's authorizer.

## Current state

Spec 007's authorizer is contract 1 and is what the hosted installation
runs.
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
| `repo.list` answer | `{repos, next_cursor}`, `{allow: false}`, `{directory: false}` | unchanged, and `next_cursor` stays the authorizer's value passed through, which the contract tells the endpoint every caller who can list sees: spec 031 seals it on the node, so the caller sees only a cursor the node wrote (issue #1) |

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
007, which becomes the package's. The behavior of every row of spec
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
and the conformance suite speak contract 2 alone. The hosted
installation's authorizer for Origo changes to contract 2 in the same
coordinated batch, so the installation's authorizer and node roll
together.

## Not in this spec

Moving the authorizer, the registry, the grants and the SSH keys from
the identity service to the platform control plane, which is the
family's own work; the `act`
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
| The family's shared audience suite passes against the verifier the middleware installs: `origo` is admitted, a token addressed to the issuer itself, to another service, or to nobody is refused, a token that names no subject is refused, the issuer is called for its key set alone, and the retired `is_superadmin` flag grants no platform role | `TestConformance` in `internal/auth`, `latere.ai/x/pkg/authkit/conformance`'s `Run`, with `TestAudienceAtTheDoor` on a protected route | built |
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
  Origo keeps `internal/auth`'s verifier; its behavior is unchanged and
  `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure` is the proof. The
  audience became configurable in place. This is the one waiver.

- **Release is coupled and not yet cut.** Contract 1 is removed in the
  same release that ships contract 2, and the hosted installation's
  authorizer for Origo changes to contract 2 in the same coordinated
  batch, the move of the git plane's decisions to the platform control
  plane. The node code and its
  overlay audience variable are ready; the tag waits for that batch.
  Moving to `complete` waits for the release.

## Outcome

Built and shipped in `v0.4.1`, released on 2026-09-15 by the tag run
34905955936 at commit `7988ecb`, the `changelog: v0.4.1` commit. Every
job of that run passed: `artifacts, images, signatures, attestations`,
`conformance against the published image`, `deploy and smoke`,
`conformance against the live installation`, `publish the release`,
`verify the published release`, `install from the release artifacts`.
The hosted installation serves it: `GET /version` answers
`{"version":"v0.4.1","commit":"7988ecb","build_time":"2026-09-14T22:49:32Z"}`.
`Current state` above describes the contract 1 installation of
2026-09-13; `v0.4.1` is what the hosted installation ran from that
release on.

`v0.4.0` carries the same node changes and published nothing. It was
gate-green, and its release run 34898933307 failed in `conformance
against the published image`: the stack came up, and six
create-then-clone subtests of `TestContract` (`007/tokens`,
`007/authorizer_unavailable`, `008/push`, `008/event-off`,
`009/blob_too_large`, `010/lfs_locks_unsupported`) read `Connection
reset by peer` from origod in the kind cluster. That job runs before
deploy, so nothing reached the installation. The node's own side was
cleared by reproducing the clone single-node and across a two-node
cluster; `v0.4.1` added a step that dumps the stack's logs when a
conformance step fails, and passed. The cause was never proven and is
assumed a transient of the kind stack.

Every criterion of the table has a passing test in the tree. Read back
on 2026-09-15 with `go test -v`, each named test reports `--- PASS`,
the conformance row excepted:

| Test | Package |
|---|---|
| `TestAuthorizerEnvelope`, `TestContractOneIsGone`, `TestFiguresReachTheirConsumers`, `TestSubjectsAreIssuerQualified`, `TestAudienceIsConfigurable`, `TestCheckProbesTheAuthorizer`, `TestOwnerPolicy`, `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure` | `internal/auth` |
| `TestWalObjectsBacksTheOwnerPolicy`, `TestOwnerPolicyNodeMode` | `cmd/origod` |
| `TestAuthorizerIsOptional` | `internal/config` |
| `TestContract` | `test/conformance` |

The conformance row is the exception: `TestContract` carries the `e2e`
build tag and does not run in an ordinary `go test`. It ran in the
release run's two conformance jobs, against the published image and
against the live installation, and the cases it carries speak contract 2
through `test/stubs/authorizer`.

One divergence from the Design stands, the waiver above: the verifier is
Origo's own in `internal/auth` and not `latere.ai/x/pkg/authkit/jwt`,
because the family's C5 has not shipped the options Origo's rules need.
The audience became configurable in place and the behavior of every row
of spec 007's verification table is unchanged. The second departure of
`State on 2026-09-14`, that the release was coupled and not yet cut, is
what `v0.4.1` closes: contract 1 went in the same release, and the
production overlay points `ORIGO_AUTHORIZER_URL` and `ORIGO_SSH_KEYS_URL`
at the platform control plane (`deploy/prod/authorizer.yaml`,
`deploy/prod/ssh.yaml`),
with the authorizer URL moved out of the `origod-auth` Secret into the
manifest, because a URL is not a secret and only the bearer is.
## State on 2026-09-16: the vocabulary is an importable package

The four action strings and the resource kind of the envelope above have
one home in the shipped source, `latere.ai/x/origo/authorizer`,
a package at the module root that anything may import. `Vocabulary()` is
the table as `latere.ai/x/pkg/authz`'s own type, built once through
`NewVocabulary`, and `Actions`, `Kind` and `Known` read that same value.
`PageActions()` names `repo.list` alone: the shared vocabulary carries
the actions and not the shape of their answers, so `authz/server` takes
the page actions as an option and this package is where the name lives.
`internal/contract` keeps its constants and reads them from there, so
`details.action` on a 403 and the agent client's branch are the
published strings. A walk over the shipped Go source holds the one-home
claim; `test/` is outside it on purpose, because the stub of spec 013
answers as an operator's endpoint would and the suite of spec 021 drives
an installation it did not build, so both speak the wire rather than the
node's constants.

Nothing on the wire moves. The envelope, the four names, the kind, the
answer shapes and the five rules are this spec's, unchanged; what
changes is who can say them in Go. `origod`'s own client now carries the
vocabulary, so an action outside the table is an `authz.UnknownAction`
in the node rather than a round trip to an operator's endpoint
(`TestAnUnknownActionCostsNoRoundTrip`), and `repo.list` still travels,
because a page is asked through `Ask`, which the shared client does not
validate. Spec 007's endpoint section was rewritten to contract 2 in the
same change, since `make docs` renders it into `docs/api.md` and it
still described contract 1.

The work is Origo's part of the family's one authorizer library, whose
acceptance row is that no repository re-declares another repository's
action strings. The platform control plane re-declared these four in its
own source because Origo exported nothing importable; that declaration
becomes `authorizer.Vocabulary()` and `authorizer.PageActions()` on
Origo's next tag, the way its Lux section already reads
`latere.ai/x/lux/authorizer`. Lux made the same promotion first, and the
package mirrors its shape.

## State on 2026-09-16: the verifier stays, and its waiver is a test

`Audience and verifier` above says the verifier becomes
`latere.ai/x/pkg/authkit/jwt` with the options the family's C5 adds. C5
shipped in pkg v0.71.0 and grew in v0.72.0, and the options are there:
ES256, a token size bound, an `iat` age, `RequireIssuedAt`, a local
issuer with its own keys, a clock skew, an issuer comparison that trims a
trailing slash from both sides, a reason table whose values are this
spec's wire words read through `jwt.ReasonOf`, and in v0.72.0 a list of
issuers each discovering its own key set, weighed before the signature,
which is this spec's order. The verifier still does not move, because
the options are not composable into spec 007's row.

Spec 007's verification table asks two things of one token: its `kid`
names a key of the issuer's set, else `unknown_key`, and its `exp` and
`nbf` carry 60 seconds of skew. authkit/jwt v0.72.0 offers one path with
each and neither with both. v0.72.0's `Issuers` and `LocalKeys` are
real and Origo's tripwire is written against them, but neither is one of
these two rows.

| Path | names the key strictly | carries `ClockSkew` |
|---|---|---|
| `LocalIssuer` + `LocalKey` + `LocalKeyID` | yes | no: `Validate` zeroes the skew for a local token, whatever `Config.ClockSkew` says |
| `Issuer` + `JWKSURL` | no: `verifySignature` falls back to every key of the set when the `kid` names none | yes |

Read as Origo's answers, each path loses a row of the table. The JWKS
path verifies a token this spec refuses; the local path, the only one
that can be handed a key the node resolved itself, refuses a token this
spec reads:

```
an issuer's token, kid "nope", signed by the issuer
    spec 007: unknown_key          JWKS path: verified
an issuer's token 59 seconds past exp
    spec 007: verified, in skew    local path: expired
```

The two paths cannot be composed, either. Handing the package one
resolved key means `Config.LocalKeys`, which is the path that zeroes the
skew; and picking the key a `kid` names needs the JOSE header, which the
package decodes for itself and does not hand back. `DecodePayload` reads
the payload alone. So a node that keeps spec 007's key rules cannot hand
the shared verifier a key, and a node that hands it the issuer's set
loses the `kid` rule.

What a move would leave behind is most of the verifier anyway. Of spec
007's table, authkit/jwt would carry the shape, the algorithms, the
signature, `exp`, `nbf` and `iat`. Origo would keep `missing`, `size`,
the `iss` routing over `ORIGO_OIDC_ISSUERS`, `issuer_unavailable`,
`unknown_key`, `subject` (the package reads an empty `sub` as
`malformed`), `delegation`, `aud` (the package checks it after `exp`,
where the table checks it before), the rendered `<iss>|<sub>` subject,
the verified-token cache, discovery with the OIDC 4.3 issuer check, and
the one-second-doubling retry ladder that spec 013's kind stack needed.

So the waiver of `State on 2026-09-14` stands, and stops being a date.
`TestTheSharedVerifierCannotCarrySpec007` in `internal/auth` holds both
rows of the table above against pkg v0.72.0 and reds when either closes;
`.lateregate.yaml`'s `verifier` waiver names it. Either of two changes
to the package closes it on its own: a `kid` that names no key of the
set refused rather than tried against every key, which makes the JWKS
path whole; or `Config.ClockSkew` honored for a token of
`Config.LocalIssuer`, which makes the local path whole. An exported
reader of the JOSE header would close it a third way, by letting a
caller that keeps its own key sets hand the verifier the one key a `kid`
names.

The spec's `authorizer` half needed no such move and was already done:
`internal/auth`'s client is `latere.ai/x/pkg/authz`. The identity gate's
`authorizer` rule read three files of `test/conformance` as second
clients, on the import path of the spec 013 stub rather than on any
request they build, and those three are skipped for the reason the
section above gives for keeping `test/` outside the one-home walk.

## State on 2026-09-17: one row closed, three remain

pkg v0.73.0 closed the row the section above named. authkit/jwt now
decides which key verifies a token by one rule on every path: the `kid`
names the key, a key declaring no `kid` answers whatever `kid` a token
names, a token carrying no `kid` is answered only by a set holding
exactly one key, and anything else is `jwt.ErrUnknownKey`, reason
`unknown_key`. The JWKS fallback that tried every key of the set in turn
is gone, so the token spec 007 refuses with `unknown_key` is refused with
`unknown_key`; the local path, which called the same miss a signature,
uses the same word. A `kid` miss still forces one refresh of the set
first, which is this table's "after one refresh".

The second row of that section did not close and does not need to:
`Validate` still zeroes the skew for a token of `Config.LocalIssuer`
whatever `Config.ClockSkew` says, and spec 007 gives a repository-bound
token no skew for the same reason the package does, so the two agree.
Read against v0.73.0 the old tripwire reds, correctly, on the `kid` row.

The verifier still does not move. Three rows of spec 007's table have no
home in the package, and none is composable away:

| Row | What the package does |
|---|---|
| `exp`, `nbf`, `iat` | `Validate` calls `time.Now` and `jwt.Config` takes no clock, so a token minted on the clock Origo's verifier runs on reads `expired` |
| keys, OIDC Discovery 4.3 | discovery follows the `jwks_uri` of whatever document answers; it never checks the document's `issuer` against the URL it was fetched under |
| `issuer_unavailable` | a fetch failure is wrapped unclassified, so `jwt.ReasonOf` reads the empty string and an unreachable issuer is not a row of the table |

The middle row is the one with teeth. A document served under one URL and
naming another issuer publishes the key set the package then verifies
that URL's tokens with, which is the fetch spec 007 fails on purpose.
`TestTheSharedVerifierCannotCarrySpec007` builds exactly that stack and
reads the token back as verified.

There is no seam below `Validate` to take less of it. `verifyAgainst` is
unexported, and `DecodePayload` and `ParseUnverified` read the payload
alone, so there is still no exported reader of the JOSE header: a caller
cannot let the package choose the key and keep the claims window on its
own clock. It is `Validate` or nothing, and `Validate` brings its own
clock and its own discovery. Handing the package a key set the node
resolved itself means `Config.LocalKeys`, which is the path that zeroes
the skew an issuer's token carries, so that composition is closed too.

So `.lateregate.yaml`'s `verifier` waiver stands, with its reason rewritten
to these three rows, and the tripwire rewritten to probe them: it asserts
the `kid` row that closed, so a regression is caught, and reds when the
clock, the 4.3 check, or the word for an unreachable issuer arrives. The
three findings it covers are one thing and clear together: `go.mod`
imports no shared verifier, and `internal/auth/token.go` takes a token
apart at two lines. Nothing on the wire moved: no refusal reason changed,
no configuration variable changed, and `internal/auth` is untouched.

Of the three closures that section offered, the package took the first.
What would close the rest is a clock on `jwt.Config`, the discovery
document's `issuer` checked against the URL it was fetched under, and a
reason word for a fetch that failed.


## State on 2026-09-17: the verifier moved

pkg v0.74.0 closed the three rows the section above named, and
`internal/auth` verifies through `latere.ai/x/pkg/authkit/jwt`.

| Row that blocked | What v0.74.0 carries |
|---|---|
| `exp`, `nbf`, `iat` | `jwt.Config.Now` is the one clock: the three windows, the key-set cache's TTL and the refresh back-off all read it, so the whole validator runs on the clock Origo hands it and nil is `time.Now` |
| keys, OIDC Discovery 4.3 | discovery on the `Issuers` path checks the document's `issuer` against the URL it was fetched from before its `jwks_uri` is read; a document naming another issuer, or naming none, is `jwt.ErrBadDiscovery` |
| `issuer_unavailable` | `jwt.ErrIssuerUnavailable`, reason `issuer_unavailable`, when discovery or the key set cannot be read and no cached set answers; a cached set, however stale, is still an answer and is still served |

The tripwire was run against v0.74.0 before anything moved. It reddened on
all three and stayed green on the `kid` row it asserts, which is what said
the move was due.

### What the node hands the shared verifier

`Issuers` from `ORIGO_OIDC_ISSUERS` with the trailing slash trimmed;
`LocalIssuer` `ORIGO_PUBLIC_URL` with `LocalKey` the public half of
`ORIGO_TOKEN_KEY` under `LocalKeyID`, the node's own thumbprint;
`ClockSkew` 60 s; `MaxTokenBytes` 8192; `MaxTokenAge` 24 h;
`RequireIssuedAt`; `CacheTTL` the hour of `RefreshInterval`; `Now` the
node's clock; and the node's instrumented client with the 5 second fetch
budget as its timeout, which is the only bound that package puts on a
fetch.

`Audiences` is deliberately not among them. Spec 007's table checks `aud`
between `signature` and `exp`; `Validate` checks it after `iat`. Leaving it
unset makes the row Origo's, weighed in its own place, rather than forking
the verifier to move it.

### What stays on Origo's side, and why

| Shim | Why it is not the shared package's |
|---|---|
| `missing` | nothing arrived; `jwt.ErrNoToken` carries no reason by design |
| `subject` | an empty `sub` is `malformed` to the package, which is the shape row, not the row spec 007 gives it |
| `delegation` | the family's D5 is Origo's rule, and the package reads no `act` |
| `aud` before `exp` | spec 007's table pins the order; see above |
| `issuer_unavailable` for a bad discovery document | spec 007 fails such a fetch "like an unreachable issuer"; the package calls the issuer itself bad, reason `issuer`. One `errors.Is` keeps the word an operator has always read |
| `<iss>\|<sub>` with the trailing slash trimmed | the authorizer envelope's subject (`authz.Subject`), which no verifier renders |
| the verified-token cache | the package caches key sets, not verdicts: without it every request re-runs the signature and the whole window. `TokenCacheTTL` and `CacheEntries` are Origo's |
| the discovery back-off | measured, not assumed; see below |

### The back-off, measured

Against v0.74.0, with the clock standing still, five requests naming an
issuer that is down made five discovery fetches: the package has no
back-off to evaluate, only the absence of one. Spec 007 bounds the same
attempts, and `TestFirstFetchFailureBacksOffFromASecond` names the
incident the bound was written for, so the ladder stays: until an issuer
answers once, an attempt is made after one second, doubled per consecutive
failure and capped at the minute, and a token arriving before the pause
elapses is refused `issuer_unavailable` with no fetch. It is now a pacing
gate in front of `Validate` and holds no keys.

Once an issuer has answered, the package paces itself and the gate steps
aside: `CacheTTL` refreshes the set on the hour and a `kid` naming no key
of it forces one refresh, at most one every fifteen seconds. That window
is the one behavior of spec 007 the move changed: the spec bounded the
same refresh to one a minute. It is strictly more responsive to a rotation
and strictly more fetches, it changes no refusal reason and nothing an
operator sets, and it is the package's figure rather than a value Origo
can configure.

`Run` keeps the loop, with the keys taken out of it. The package exposes
no way to warm its key set, so the loop probes each issuer through
`FetchKeys`, the same two documents `origod check` reads, and keeps what
it learned rather than what it read: the line an operator sees at start-up
when an issuer does not answer, and the back-off above. It costs one
discovery and one key-set fetch per issuer per hour beside the package's
own, which is the price of that package having no warm-up.

### On the wire

One refusal moved by an instant. The package refuses a token from past
`exp` plus the skew where this node refused at it, so a token is read for
one instant longer at the boundary. No refusal reason changed, no
configuration variable changed, and `issuer_unavailable` reads exactly as
it did, for an unreachable issuer and for a discovery document naming
another issuer alike.

The rows keep their order under each other, which is what the shim walks
rather than approximates: a token that names nobody and is expired besides
reads `expired`, and one that names nobody and carries another audience
reads `audience`, because the package refuses an empty `sub` last and the
table puts that row last too.

One residual, worth writing down, and it is the only one: a token whose
signature segment is not base64 and whose `sub` is empty reads `subject`
where the table says `malformed`, because both are `jwt.ErrMalformedToken`
and only the segment tells them apart. Both are 401 `unauthenticated` on a
token that could never verify, and reading the segment here again is the
duplication this move removed.

`internal/auth/token.go` holds no parser, no signature and no claims
window. `sharedverifier_test.go`, which was the waiver written as a red
test, is its inverse: `go list -deps` shows `authkit/jwt` in the build
list, and `token.go` and `verifier.go` call into no base64 and no
signature of their own. `.lateregate.yaml` carries no `waive` block.

## State on 2026-09-17: a personal access token carries what it may do

A person can push to Origo over HTTPS with a personal access token,
which the family's identity work added. The key mints a short token
whose `token_use` is `pat`, and since the family scoped those keys that
token also carries what its holder narrowed the credential to: a set of
grants, one action of a published vocabulary paired with a resource
selector, as RFC 9396's `authorization_details`.

**Nothing on the wire moves.** The envelope is this spec's, field for
field. `claims` already carries every verified claim of the token
verbatim, so the two claims a decision point intersects with travel
inside contract 2 rather than as a change to it, and an authorizer that
reads neither is unaffected. The node reads them nowhere it decides.

| What travels | Where |
|---|---|
| `token_use` | `claims.token_use`, verbatim |
| `authorization_details` | `claims.authorization_details`, verbatim, entry for entry |

### The rule, and where it is applied

The authorizer a node names (`ORIGO_AUTHORIZER_URL`) is its decision
point, and the hosted installation's, the platform control plane,
intersects its answer with the grants.
The rule is `latere.ai/x/pkg/authz`'s and is written once there:

```
allow(req) = decide(req) AND ( token_use(req) != pat
                               OR EXISTS g in G(req) : covers(g, req) )

covers(g, req) = qualified(req.action) in g.actions
                 AND ( g.identifier = "" OR g.identifier = req.resource.id )
```

With `ORIGO_AUTHORIZER_URL` unset the owner policy of this spec is the
node's own decision point, so it applies the same intersection or a key
narrowed to one repository reaches every repository its holder owns.
That amends **The owner policy** above: the policy still decides from
`meta` alone, and it now narrows what it decided by the two claims. The
three properties hold as tests (`TestTheOwnerPolicyNarrowsAScopedToken`):
the policy's answer is the ceiling, so a grant on a repository the
person does not own still reads `not_owner`; the intersection only ever
turns an allow into a deny; and a request no grant covers is denied
without reading any of the node's tables. `repo.list` names no
repository, so it is covered by a kind-wide selector on `origo:repo.list`
and by nothing else: a key that grants a read on one repository cannot
read the names of the rest.

A claim nobody can parse is no verdict. It fails closed as an
`*Unavailable`, the way a lookup that did not answer does.

A repository-bound token the node minted carries no `token_use`
(`internal/auth/token.go`), so nothing narrows it: its scope decided it
at minting, and the call spec 012 makes for its writes asks a figure
rather than an access.

### The refusal

The reason is `grant`, the shared package's word, and Origo names it the
way it names every other reason a decision point writes. No word is
added and no row of spec 003's code table moves.

| Surface | What a person sees |
|---|---|
| HTTPS | 403 `forbidden` with `details.reason: "grant"`, beside `action` and `subject`, exactly as every authorizer deny is rendered |
| SSH | the operator's line carries `reason=grant`; the session's stderr carries the forbidden line byte for byte, because a git client reads that line and spec 021 fixed its form |

### The validator reads the claim, and says so

`jwt.Config.ReadsGrants` is the verifier's promise that whoever holds
the identity applies the grants. Without it the package refuses a token
carrying grants with reason `grants_unread`: the claim is a restriction,
so a service that reads it and applies none grants more than the person
asked for, and silently. The node sets it, and applies the restriction
at both its decision points, so `grants_unread` is a word no client of
Origo reads. Every other bearer, an operator's and an environment's
alike, carries no such claim and verifies exactly as before.

### The warm-up, and the residual it closes

**State on 2026-09-17: the verifier moved** ends by saying the loop
costs one discovery and one key-set fetch per issuer per hour beside the
package's own, "which is the price of that package having no warm-up".
That price is paid no longer: pkg v0.75.0 carries `jwt.Validator.Warm`,
the probe is that warm, and the keys it reads are kept. An issuer is
read once per refresh interval rather than twice, and the first token of
an issuer pays for no fetch (`TestStartUpWarmsTheSharedVerifier`).

Two things follow, and the second is the only behavior that changed.

- A warm reaches no network for a set inside the package's `CacheTTL`,
  which is `RefreshInterval` on this node's clock, so a pass fetches
  exactly what the loop's stale rule asks for.
- A warm that did not answer for every issuer names them only in the
  text of its report, and this node's back-off is per issuer, so the
  node then asks each due issuer itself. An issuer already out of reach
  costs one more discovery attempt per pass while it is out of reach;
  the ladder, the request-path gate and `issuer_unavailable` are
  unchanged (`TestFirstFetchFailureBacksOffFromASecond`, whose start-up
  count is two for that reason).

### Acceptance

| Criterion | Test that proves it | State |
|---|---|---|
| A token whose `token_use` is `pat` verifies, and the identity behind it carries the credential class and the grants | `internal/auth`, `TestAPersonalAccessTokenVerifiesWithItsGrants` | built |
| The envelope carries `token_use` and `authorization_details` verbatim for a key holder's push, and neither for a token that carries neither | `internal/auth`, `TestTheEnvelopeCarriesTheTwoClaims` | built |
| With no authorizer configured the owner policy narrows its answer by the grants, and never widens it | `internal/auth`, `TestTheOwnerPolicyNarrowsAScopedToken`, table-driven | built |
| A deny whose reason is `grant` reaches the person in `details.reason` over HTTPS and the operator's line over SSH, with the sideband unchanged | `internal/auth`, `TestTheGrantRefusalReachesTheClient`; `internal/sshd`, `TestSSHNamesAGrantRefusal` | built |
| An issuer's key set is read once at start-up, not twice | `internal/auth`, `TestStartUpWarmsTheSharedVerifier` | built |

## State on 2026-09-20: the audience is a list, primary first

Spec 029 widens `ORIGO_OIDC_AUDIENCE` from one value to a comma
separated list, following Cella. Three places in this document read
differently from this date, and none of them is rewritten in place,
because two of them sit inside dated records and a record that is edited
is no longer one:

- the configuration row "Audience accepted by the token verifier" is the
  audiences accepted by the token verifier, in the order the variable
  names them, the first of them the primary;
- the Design sentence "`ORIGO_OIDC_AUDIENCE`, default `origo`, replaces
  the constant" holds exactly as written for a single value, and gains a
  clause for a list: every entry is accepted, and the first is what the
  node's signer mints with;
- the line of `State on 2026-09-14` that records `ORIGO_OIDC_AUDIENCE`
  (default `origo`) among what shipped is the record of that day and
  stays as it is.

Nothing else moves. Unset is still `origo`, a single value means exactly
what it meant here, and the widening is refused where it would be a
typo: an empty or repeated entry fails the start-up rather than becoming
a shorter set (`internal/config`, `TestConfiguredAudienceSet`). The fifth
acceptance row's test, `TestAudienceIsConfigurable`, gains the list case:
a verifier holding two names admits each of them on its own and refuses a
third.

## State on 2026-09-26: deletion is its own action

The table gains two rows, `repo.delete` and `repo.undelete`, both of kind
`Repository`, after `repo.list`:

| Action | Operation |
|---|---|
| `repo.delete` | `DELETE /v1/repos/{id}`, which starts spec 019's hold |
| `repo.undelete` | `POST /v1/repos/{id}/undelete`, which ends it inside the hold |

### Why an authorizer needs them

A contract 2 request carries the action and the resource, never the
route. While `DELETE /v1/repos/{id}` asks `repo.admin`, so do `PATCH`,
`undelete`, `tokens`, `transfer`, `freeze`, `unfreeze`, `import`,
`verify` and `gc`, and an endpoint cannot refuse one without refusing the
rest. That matters to an endpoint that keeps a registry of repositories
beside Origo and is the only writer of it: it deletes at Origo itself,
inside the change that removes its row, and has to refuse a deletion by
anyone else, the repository's administrators included, or the registry
keeps a row for a repository Origo has deleted. With `repo.admin`
answering both, the choice is between leaving that direct deletion open
and taking away the administrators' rename, token minting and freeze.

`repo.undelete` is apart for the same reason in the other direction. A
repository brought back after such a registry dropped its row is a
repository no row names, reachable by no name the registry resolves and
owned by nobody.

Creation stays `repo.admin`. A create names an id no registry holds yet,
and rule 4 of spec 007 already makes an endpoint decide an unknown id
without the repository, from the owner and slug the resource carries, so
an endpoint that registers repositories itself refuses a create by anyone
else today.

### The order, and why it is two releases

An endpoint that validates the action against the vocabulary it was
built with, which is every endpoint on `latere.ai/x/pkg/authz/server`,
answers an action outside that table with a 400. Origo reads a 400 as no
answer and renders it `authorizer_unavailable`, a 503. A node that asked
`repo.delete` before its endpoint knew the action would therefore refuse
every deletion and every undelete of the installation.

So the rows arrive a release before any route asks them:

1. The release that carries this section publishes the two rows and asks
   neither. Every route asks exactly what spec 007's table says, and
   `DELETE` and `undelete` still ask `repo.admin`.
2. An endpoint built against this module reads the two rows from
   `Vocabulary()`, decides them, and is deployed.
3. The next minor release moves `DELETE` to `repo.delete` and `undelete`
   to `repo.undelete`, and spec 007's table with it.

This is not the compatibility window the family's decision of 2026-09-13
rules out. No release serves two contracts: each has one table and one
action per route, and nothing selects between them. What the first
release adds is table rows that are not yet sent, which is the promise
the `authorizer` package already makes, a new action being a new row
first.

### What does not change

- The owner policy allows an owner every action on its repository and an
  admin subject every action on every repository, so both keep deleting
  and undeleting with no rule added.
- The envelope, the resource of kind `Repository` with the id alone on
  both routes, the answer shape and the five rules.
- A personal access token is narrowed by the qualified action (**State on
  2026-09-17: a personal access token carries what it may do**). From the
  release that moves the two routes, a key granted `origo:repo.admin`
  alone no longer covers a deletion; one that deletes carries
  `origo:repo.delete`.

### Acceptance

| Criterion | Test that proves it | State |
|---|---|---|
| The table carries `repo.delete` and `repo.undelete` after `repo.list`, and `Actions`, `Kind` and `Known` read them | `authorizer`, `TestTheVocabularyIsSpec028sTable` | built |
| The node's and the client's constants are the published strings | `authorizer`, `TestTheNodeAndTheClientReadTheSameStrings` | built |
| The shared conformance suite drives the two rows against an endpoint told this table | `authorizer`, `TestConformanceAgainstTheStub` | built |
| `DELETE` asks `repo.delete` and `undelete` asks `repo.undelete`, and a deny of either reaches the caller as a 403 naming it in `details.action` while the same caller still renames and mints | `internal/api`, `TestDeleteAndUndeleteAskTheirOwnActions`; `test/conformance`, `007/forbidden`, which denies the two on a repository of its own | built |

The routes moved in the release after the one that published the rows,
as step 3 above says, and spec 007's action table moves with them.

## State on 2026-10-07: a name is its own action

The table gains one row, `repo.rename`, of kind `Repository`, after
`repo.undelete`:

| Action | Operation |
|---|---|
| `repo.rename` | `PATCH /v1/repos/{id}` that sets `owner` or `slug` to another label, and `POST /v1/repos/{id}/transfer` |

`repo.admin` keeps creating a repository, a `PATCH` of
`default_branch`, minting a repository-bound token, freeze, unfreeze,
starting an import, verify, and `gc`.

### Why an authorizer needs it

The section of 2026-09-26 kept deleting apart from administering so an
endpoint that keeps a registry of repositories beside Origo, and is the
only writer of it, can hold a repository's existence to itself. A
repository's name belongs to that registry as much as its existence
does: the registry resolves a name to a repository, and Origo's name
index answers the same question at the git host. While a rename asks
`repo.admin`, so do a change of the default branch, a token, a freeze
and a transfer, and the endpoint cannot refuse one without refusing the
rest. The choice is between leaving a change of name open to every
administrator, so the name at the git host and the name in the registry
can part, and taking away the administrators' default branch, token
minting and freeze. With `repo.rename` the endpoint refuses a change of
name to everyone but the registry's writer, which renames at Origo
inside the change that renames its row, and the administrators keep the
rest.

The resource carries the repository id alone, as on every route that
names the id (spec 007), and not the name the request asks for. An
endpoint decides who may change a repository's name, which is all an
endpoint holding names to one writer needs; which names a writer may
choose is that writer's own rule.

### What it reverses

The section of 2026-09-26 says, under its "Why an authorizer needs
them", that the repository's administrators keep renaming it, minting
its tokens and freezing it under `repo.admin`, and spec 007 and the
`authorizer` package said the same. That described the table of that
day, and the dated section stays as written. From this section a name
is held as existence is: the administrators keep the default branch,
the tokens and the freeze, and a change of name is `repo.rename`.

### Which questions a PATCH asks

`PATCH /v1/repos/{id}` changes the labels, the default branch, or both,
and asks for what its body names:

| Body names | Before the lookup | After the lookup |
|---|---|---|
| `owner` or `slug`, and no `default_branch` | `repo.rename` | nothing |
| `default_branch`, and no `owner` or `slug` | `repo.admin` | nothing |
| `default_branch` and `owner` or `slug` | `repo.admin` | `repo.rename`, when `owner` or `slug` differs from the repository's |
| none of the three | `repo.admin` | nothing |

Spec 007 asks the authorizer before the repository is read, so the
first question is decided by what the body names, and a caller it
refuses learns nothing about the repository. Only the lookup tells a new
label from the current one, so the second question waits for it: a body
that repeats the repository's `owner` and `slug` beside a new
`default_branch`, which is what a client that sends the whole
representation does, is no change of name and asks `repo.admin` alone.
The second question goes only to a caller the first allowed, to whom
spec 007 already answers 404 for a repository that does not exist, so
it discloses nothing the first did not. Nothing is written until every
question asked is allowed, and a refusal is the 403 `forbidden` of the
action refused. A body whose `owner` and `slug` equal the repository's
writes no name and emits no event, whichever question it asked.

`POST /v1/repos/{id}/transfer` names a new owner by what it is, and asks
`repo.rename` before the lookup, alone.

### The order

The row arrives before any route asks it, as on 2026-09-26 and for the
same reason: an endpoint that validates the action against the
vocabulary it was built with answers an action outside it with a 400,
which Origo renders as 503 `authorizer_unavailable`.

1. The release that carries this section publishes the row and asks it
   nowhere. A rename and a transfer ask `repo.admin`.
2. An endpoint built against this module reads the row from
   `Vocabulary()` and decides it. One with no rule of its own for it
   decides it as it decides `repo.admin`, which keeps every answer it
   gave before.
3. The next minor release moves `PATCH` and `transfer` as the table
   above says, and spec 007's table with it.

### What does not change

- The owner policy decides `repo.rename` on an existing repository as it
  decides `repo.admin`: the owner and an admin subject are allowed, so a
  node with no endpoint renames as before. On an id that names nothing,
  `repo.admin` is a create and `repo.rename` is refused like every other
  action.
- The stub authorizer of spec 013 decides `repo.rename` by the rule that
  decides the same request as `repo.admin`, unless the rule that matches
  it names `repo.rename` itself, so a rule table written for
  administration keeps deciding a change of name.
- A repository-bound token's scope allows neither action.
- The envelope, the answer shape, the five rules, and the `renamed` and
  `transferred` events.
- A personal access token is narrowed by the qualified action (**State
  on 2026-09-17: a personal access token carries what it may do**).
  From the release that moves the two routes, a key granted
  `origo:repo.admin` alone no longer covers a change of name; one that
  renames or transfers carries `origo:repo.rename`.

### Acceptance

| Criterion | Test that proves it | State |
|---|---|---|
| The table carries `repo.rename` after `repo.undelete`, and `Actions`, `Kind` and `Known` read it | `authorizer`, `TestTheVocabularyIsSpec028sTable` | built |
| The node's and the client's constants are the published strings | `authorizer`, `TestTheNodeAndTheClientReadTheSameStrings` | built |
| The shared conformance suite drives the row against an endpoint told this table | `authorizer`, `TestConformanceAgainstTheStub` | built |
| The owner policy and the stub authorizer answer `repo.rename` on an existing repository as they answer `repo.admin`, and the stub decides it alone for a rule that names it | `internal/auth`, `TestTheOwnerPolicyDecidesARenameAsAdministration`; `test/stubs/authorizer`, `TestARenameIsDecidedAsAdministration` | built |
| A `PATCH` that changes `owner` or `slug` and a `transfer` ask `repo.rename`, a `PATCH` of `default_branch` asks `repo.admin`, one of both asks both, and a deny reaches the caller as a 403 naming the action in `details.action` with nothing changed | `internal/api`, `TestAChangeOfNameAsksRepoRename`; `test/conformance`, `007/forbidden`, which denies `repo.rename` on a repository of its own | built |

The routes move in the release after the one that publishes the row, as
step 3 above says, and spec 007's action table moves with them.
