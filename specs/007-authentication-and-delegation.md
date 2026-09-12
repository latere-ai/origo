---
title: "Authentication and delegation: issuers, the authorizer, acting on behalf of a subject"
status: complete
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
affects: [internal/auth/, internal/config/, internal/httpgit/, internal/api/, cmd/origod/, deploy/, Makefile, test/e2e/, test/stubs/issuer/, test/stubs/authorizer/]
effort: medium
created: 2026-09-06
updated: 2026-09-12
author: changkun
---

# Authentication and delegation

## Overview

Origo knows who is calling and asks the consumer whether they may. It
verifies tokens from configured OIDC issuers, lets a service act on
behalf of a subject with an auditable claim, mints narrow read tokens
for builds, and delegates every authorization decision to an endpoint the
consumer runs. Origo stores no user and no permission.

## Current state

Built on 2026-09-08 and in the tree. `internal/auth` holds the
verifier over the configured issuers and the node's own key
(`verifier.go`, `keys.go`, `token.go`), the credential forms and the
401 (`middleware.go`), the authorizer client with its caches and its
retry (`authorizer.go`), the guard that decides a request from a
repository-bound token's scope or the authorizer (`guard.go`), and the
signer of repository-bound tokens with the key set it serves
(`mint.go`). `internal/config` refuses `ORIGO_DEV_TOKEN`, requires
`ORIGO_OIDC_ISSUERS`, `ORIGO_AUTHORIZER_URL`, `ORIGO_AUTHORIZER_TOKEN`,
and `ORIGO_TOKEN_KEY`, and applies the issuer scheme rule.
`internal/httpgit` and `internal/api` ask the guard before any read of
the repository and record the subject and the actor in every entry;
`internal/api` serves `POST /v1/repos/{id}/tokens`. `cmd/origod` runs
the verifier in front of the public listener, its key refresh loop
beside the sweeper, and `GET /.well-known/jwks.json` beside `/readyz`
and `/version`. `internal/contract` carries the code table every
envelope is rendered from. The stubs this spec builds are
`test/stubs/issuer` and `test/stubs/authorizer`, to spec 013's table;
the end-to-end harness and the unit suites run them in-process.

`make dev` was out of service between this spec and spec 013, which
restored it: the phase 1 bearer is gone and the node needs an issuer,
an authorizer, and a signing key, which the stub binary
`test/stubs/cmd/origo-stubs` of spec 013 provides beside MinIO, with
`ORIGO_TOKEN_KEY` generated at start with `openssl ecparam -genkey
-name prime256v1` into a file under `out/` (spec 002, Local stack).
Nothing in this spec's criteria needs it. The bootstrap Secret
`origod-auth` carries `ORIGO_OIDC_ISSUERS`, `ORIGO_AUTHORIZER_URL`, and
`ORIGO_AUTHORIZER_TOKEN`, with spec 005's gossip secret beside them; a
third Secret, `origod-token-key`, carries `ORIGO_TOKEN_KEY` and is not
a template, because the operator generates the key (spec 018,
`docs/install.md`, and `deploy/bootstrap/README.md`), and the kind
overlay's `up.sh` generates it into that Secret the way `make dev`
writes its file. `deploy/base/deployment.yaml` reads all three.

## Design

### Verification

Checks run in the order of the table; the first failure is 401
`unauthenticated` with `details.reason` from the last column, the list
spec 003's table carries.

| Rule | Value | `reason` |
|---|---|---|
| presence | a credential in one of the forms below | `missing` |
| size | a token over 8 KiB is refused before it is parsed | `size` |
| shape | three base64url segments whose header and claims parse as JSON objects | `malformed` |
| algorithms | `RS256`, `ES256`; anything else | `signature` |
| `iss` | one of `ORIGO_OIDC_ISSUERS`, or `ORIGO_PUBLIC_URL` for a repository-bound token | `issuer` |
| local issuer | a token whose `iss` equals `ORIGO_PUBLIC_URL` is verified against the public key of `ORIGO_TOKEN_KEY` directly, with no discovery and no JWKS fetch, because the node holds the key: its `kid` must equal the node's own `kid` below, else `unknown_key`, and the rows from `signature` on apply as to any token; the issuer rows above and below are skipped for it | `unknown_key` |
| issuer keys | the issuer's key set has been fetched at least once; until discovery of that issuer succeeds every token naming it is refused | `issuer_unavailable` |
| `kid` | names a key of the issuer's set, after one refresh when unknown | `unknown_key` |
| signature | verifies with that key | `signature` |
| `aud` | contains `origo` | `audience` |
| `exp` | in the future, with 60 seconds of skew | `expired` |
| `nbf` | absent or in the past, with 60 seconds of skew | `nbf` |
| `iat` | present and at most 24 hours old | `iat` |
| `sub` | present and not empty; a token that names nobody is not a caller, and would otherwise verify to the empty subject that spec 027 admits as anonymous | `subject` |
| keys | from each issuer's `<iss>/.well-known/openid-configuration` `jwks_uri`, cached, refreshed every hour and on an unknown `kid` at most once a minute per issuer; the discovery fetch and the JWKS fetch each have a 5 second timeout; an issuer unreachable at start-up does not fail the start-up: it is logged, retried every minute by the loop and from one second on by a request, doubling per failure up to the minute, and its tokens are refused with `issuer_unavailable` until a fetch succeeds, while tokens of the other issuers verify | |
| issuer scheme | an issuer URL is `https://`; `http://` is accepted only when the URL's host is a loopback address or the URL is listed in `ORIGO_OIDC_INSECURE_ISSUERS` (spec 002), which the kind overlay of spec 013 sets for the stub issuer and a production deployment never sets; any other `http://` issuer is a start-up failure naming it | |
| credential forms | `Authorization: Bearer <token>`; basic auth with any username and the token as the password; basic auth with the token as the username and an empty password | |
| verified-token cache | keyed by the SHA-256 of the token for the shorter of its lifetime and 5 minutes, so a busy client costs one signature check per 5 minutes; at most 65 536 entries, least recently used evicted | |

`ORIGO_DEV_TOKEN` is removed with this spec: `internal/config` refuses a
start-up that sets it (`ORIGO_DEV_TOKEN is no longer read; remove it`),
and `ORIGO_OIDC_ISSUERS`, `ORIGO_AUTHORIZER_URL`,
`ORIGO_AUTHORIZER_TOKEN`, and `ORIGO_TOKEN_KEY` become required, the
key in every mode: a node never starts without one, so the token
endpoint below never runs without a key to sign with. Development and
the test tiers run the stub issuer and the stub authorizer of spec 013
in its place and generate the key at start (Current state).

### Effective subject and actor

| Claim | Meaning |
|---|---|
| `sub` | the caller |
| `act` | optional; when present, the subject on whose behalf the call is made, and `sub` becomes the actor |

Both are recorded in every entry header (`subject`, `actor`) and every
event (`pusher.sub`, `pusher.actor`). A consumer that commits for a user
sends its service token with `act: <user sub>`.

### Authorization before lookup

For every request that names a repository, the authorizer is asked
before the repository's metadata is read, with what the path names. The
id form (`/r/{id}.git`, `/v1/repos/{id}`) sends the id from the path
with `owner` and `slug` empty. The name form (`/{owner}/{slug}.git`)
first resolves `origo/names/<owner>/<slug>` to an id, because the id is
what an authorizer keys on; when the name resolves, the request carries
the id with the owner and slug from the path, and when it does not, the
id is empty and the owner and slug are sent alone. Only after an allow
does the node read `meta` or the index. So a deny is 403 `forbidden`
whether or not the repository exists, a caller the authorizer denies
learns nothing about the id or the name, and 404 `repo_not_found` is
answered only to a caller the authorizer allowed. `POST /v1/repos`
sends the id, owner, and slug the body names. Spec 003 states the rule
for consumers and spec 019 applies it to every operation it adds.

### The authorization endpoint

Origo holds no permission. Before every operation that names a
repository it calls one endpoint the operator runs and asks whether a
subject may do one thing to one repository. That endpoint is the whole
permission model, and Origo adds nothing to it and caches its answer.
Nothing here is any one operator's: it is what an endpoint must do to
serve any installation, whatever holds the permissions behind it.

The call, after verification, per request that names a repository:

```
POST <ORIGO_AUTHORIZER_URL>
Authorization: Bearer <ORIGO_AUTHORIZER_TOKEN>
Content-Type: application/json

{"subject": "…", "actor": "…", "repo": {"id": "…", "owner": "…", "slug": "…"}, "action": "read"}
```

| Field | Value |
|---|---|
| `subject` | the effective subject: the token's `sub`, or its `act` when the token carried one. It is empty only for the probe below |
| `actor` | the delegating service when the token carried `act`, else empty |
| `repo.id` | the repository id, a lower-case UUID. Empty for a name Origo could not resolve |
| `repo.owner`, `repo.slug` | set on the name form and on a creation, empty on the id form |
| `action` | `read`, `write`, or `admin` |

The answer is 200 either way, `{"allow": true}` with the optional
figures below, or `{"allow": false, "reason": "…"}` whose `reason`
reaches the client as `details.reason` on Origo's 403 `forbidden`:

```
200 {"allow": true, "ttl": 60, "replicas": 1, "quota_bytes": 53687091200, "requests_per_minute": 600}
200 {"allow": false, "reason": "…"}
```

The action Origo sends per operation:

| Action | Operations |
|---|---|
| `read` | `info/refs?service=git-upload-pack`, `git-upload-pack`, LFS download, `GET /v1/repos/{id}`, the read API and archive of spec 009, and the three reads of spec 019: import state, `export.bundle`, and `stats` |
| `write` | `info/refs?service=git-receive-pack`, `git-receive-pack`, LFS upload, and the server-side git operations of spec 020 |
| `admin` | `POST /v1/repos`, `PATCH`, `DELETE`, `undelete`, minting a repository-bound token, and the rest of spec 019: transfer, freeze, unfreeze, starting an import, and `gc` |

Spec 026 adds a fourth action, `list`, which names no repository and
asks which repositories a subject may see. It is that spec's to state
and it changes nothing here: the three actions above, the five rules,
the answer shape, and the caches are unchanged, and an endpoint built to
this spec alone stays correct.

Spec 019 marks three of its own operations `read`, and the per-operation
row wins over the sentence that calls its operations `admin`: a reader
who may clone may also read the size of what they cloned. On
`POST /v1/repos` the `repo` object carries the id, owner, and slug the
body names, so the endpoint decides a creation from the name the caller
chose.

**The five rules.** An endpoint that keeps them serves any installation.

1. **Answer 200 for both verdicts.** Anything else, a 500, a timeout, a
   body that does not parse, is a refusal, and Origo renders it as
   `authorizer_unavailable`. There is no fail-open.
2. **Deny `00000000-0000-0000-0000-000000000001` for every subject and
   every action**, the empty subject included. That repository id is
   reserved as a probe: `origod check` (spec 018) sends it with an empty
   subject and reads an allow as an endpoint that does not read the
   request. The stub of spec 013 denies it.
3. **Key on the repository id when the answer varies by repository.**
   The id form sends the id alone, with no owner and no slug, and it is
   what every clone by id and every API call uses.
4. **Decide without the repository.** An empty or unknown id is the
   ordinary case, for a creation and for a name that did not resolve.
   Answer it without revealing which, because Origo asks before it reads
   any metadata, so that a deny and a repository that does not exist
   look alike.
5. **Treat the endpoint's availability as Origo's.** It is called on the
   request path of every repository operation, so it sits near the nodes,
   answers from memory, and keeps nothing slow in front of the answer.

**A single-tenant installation needs no service.** Nothing above asks
for a database, a permission model, or an answer that varies by
repository. An endpoint that answers `{"allow": true}` for a list of
subjects, `{"allow": false}` for everyone else, and always denies the
probe id keeps the contract in full: rule 3 does not apply when the
answer is the same everywhere, and every figure may be omitted for
Origo's defaults. That is a few dozen lines behind the same bearer, and
it is where a team hosting its own repositories starts.

**The figures, and when to send them.** Each is optional and each has a
default, so an endpoint that sends `allow` alone is complete.

| Field | Omit it when | Send it when |
|---|---|---|
| `ttl` | 60 seconds of revocation lag suits you | you want fewer calls; the cap is 600 |
| `replicas` | always, unless you run Origo's placement policy (spec 005); absent is 1 | a repository needs more than one warm node |
| `quota_bytes` | 50 GiB per repository suits you (spec 012); absent is 53687091200 | you sell plans or cap by tenant |
| `requests_per_minute` | your subjects are people; absent buckets the subject at `ORIGO_REQUESTS_PER_MINUTE` (spec 012), and absent is not zero | one subject drives many repositories, a build fleet under one token, which spec 020 names as its case |

**What Origo does with the answer.** An allow is cached per
`(subject, actor, repo id, action)` for `ttl`; a deny for 5 seconds; an
answer for an unresolved name (an empty id) is not cached. So the call
rate an endpoint sees is set by the cache and not by the traffic, and a
higher `ttl` divides it. The call is made once and retried once when the
connection failed before a response line arrived (a refused or reset
connection, a dial timeout); a 5xx, a timeout after the request was
sent, and a body that does not parse are never retried.

### The authorizer client

Each of the two caches holds at most 65 536 entries and evicts the least
recently used. A deny is 403 `forbidden` with the reason in
`details.reason`. Any other outcome (a timeout, a connection failure
after the retry, a non-200, a body that does not parse) is a fresh code,
never fail-open:

| Code | Status | Message | Details |
|---|---|---|---|
| `authorizer_unavailable` | 503 | Permissions cannot be checked right now. Nothing was lost. Try again in a few minutes. | `url`, `status`, `error` |

`origo_authorizer_seconds{result}` observes every call with `allow`,
`deny`, or `error`. `requests_per_minute` was built by spec 020's
builder on 2026-09-09: every handler that reads an allow hands the
figure to the bucket table, so a subject is bucketed at its own rate
from the request after its first.

### Repository-bound tokens

| Method | Path | Behaviour |
|---|---|---|
| POST | `/v1/repos/{id}/tokens` | action `admin`; body `{"scope": "read"\|"write", "ttl": <seconds, 1 to 3600>}`; 201 `{"token": "<jwt>", "expires_at": "<RFC 3339>"}`; 400 `invalid_request` for another scope or ttl |
| GET | `/.well-known/jwks.json` | the public key set Origo signs with, no token required; unauthenticated like `GET /readyz` and `GET /version` (spec 002), and the only unauthenticated path that is part of the contract |

The token is an ES256 JWT signed with `ORIGO_TOKEN_KEY`, a PEM-encoded
ECDSA P-256 private key; `kid` is the first 16 hex characters of the
SHA-256 of the public key's DER encoding. Claims: `iss` =
`ORIGO_PUBLIC_URL`, `aud: ["origo"]`, `sub` = the minter's effective
subject, `act` copied from the minter when present, `repo: <id>`,
`scope`, `iat`, `exp`, `jti` (a UUID). A repository-bound token skips
the authorizer because the decision was made at minting: `read` allows
`read` on that repository, `write` allows `read` and `write`, neither
allows `admin`, and any other repository is 403 `forbidden` with
`details.reason: "token bound to another repository"`. Rotating the key
invalidates outstanding tokens; their `ttl` bounds the damage.

## Not in this spec

Anonymous reads (spec 016 names the option). Token revocation lists.
Per-request rate limits (spec 012). Issuer discovery over plain HTTP
beyond the loopback and `ORIGO_OIDC_INSECURE_ISSUERS` exceptions above.

## Acceptance criteria

- Tokens from two stub issuers (`test/stubs/issuer`, built by this spec
  to spec 013's table) verify,
  a token with `iss` equal to `ORIGO_PUBLIC_URL` verifies against the
  node's key with no fetch, and a token that fails each row of the
  verification table answers 401 `unauthenticated` with that row's
  `details.reason` (`missing`, `size`, `malformed`, `signature` for an
  unsupported `alg` and for a bad signature, `issuer`,
  `issuer_unavailable`, `unknown_key` after one refresh and for a local
  token with another `kid`, `audience`, `expired`, `nbf`, `iat`),
  asserted on the verifier alone (proposed: `internal/auth`,
  `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure`); that every
  route of the public listener runs the verifier, `info/refs`,
  `git-upload-pack`, `git-receive-pack`, `GET /v1/repos/{id}`, and the
  rest, is the route sweep in `cmd/origod`, `TestEveryRouteRequiresAToken`,
  which spec 016 names as its criterion and which asserts `missing`,
  `audience`, and `expired` on every route.
- With one of two issuers unreachable at start-up, the node starts,
  serves the other issuer's tokens, refuses the first's with
  `issuer_unavailable`, and serves them without a restart once the
  issuer answers and the minute retry ran with a fake clock; a
  discovery fetch that hangs is abandoned after 5 seconds (proposed:
  `internal/auth`, `TestIssuerUnavailableIsRetried`).
- An `http://` issuer on a loopback host starts, one on another host
  fails the start-up naming it, and the same URL listed in
  `ORIGO_OIDC_INSECURE_ISSUERS` starts (proposed: `internal/config`,
  `TestInsecureIssuersNeedTheList`).
- A service token with `act` sets the effective subject: the authorizer
  request carries both and the entry header carries `subject` and
  `actor` (proposed: `internal/httpgit`,
  `TestActClaimIsRecordedOnEntryAndAuthorizer`; the push event's
  `pusher` is spec 008's criterion).
- With the authorizer answering 500 or not at all, every request is 503
  `authorizer_unavailable` within 5 seconds, a 500 is sent one request
  and a refused connection two, and the first request after it recovers
  is served without a restart (proposed: `internal/auth`,
  `TestAuthorizerOutageDeniesAndRecovers`).
- A denied caller gets 403 `forbidden` on an unknown id and on an
  unknown name, and an allowed caller gets 404 `repo_not_found` on the
  same paths, with the authorizer stub recording one request carrying
  the id or the name before any store read (proposed: `internal/api`,
  `TestDenyBeforeLookup`).
- 65 537 distinct `(subject, actor, repo id, action)` allows leave the
  authorizer cache at 65 536 entries with the first one evicted, and the
  same holds for the verified-token cache (proposed: `internal/auth`,
  `TestCachesAreBounded`).
- Two service tokens that differ only in `act` are two authorizer calls
  and two cache entries (proposed: `internal/auth`,
  `TestCacheKeyIncludesActor`).
- A `read` token minted for repository A answers 403 `forbidden` on
  `git-receive-pack` of A and on `info/refs` of repository B, and 401
  `unauthenticated` with `details.reason: "expired"` one second after
  its `exp` with a fake clock (proposed: `internal/auth`,
  `TestRepositoryBoundTokenScope`).
- A start-up with `ORIGO_DEV_TOKEN` set fails with the one message
  (proposed: `internal/config`, `TestDevTokenIsRefused`).
- `FuzzParseToken` in `internal/auth` finds no panic over random bytes
  and mutated valid tokens: it runs as a seed-corpus test in the suite
  on every push and for 40 seconds under `make fuzz` (spec 013) on the
  weekly schedule (proposed: `internal/auth`, `FuzzParseToken`).

## Outcome

Built on 2026-09-08 as the Design describes, in three steps: the two
stub packages, `internal/auth` with the configuration, the handlers,
and the node, then the harness, `make dev`, and the bootstrap Secret.
Every criterion has a passing test in the tree:

| Criterion | Test |
|---|---|
| two issuers verify, the local token verifies with no fetch, each row's reason | `internal/auth`, `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure`; the route sweep `cmd/origod`, `TestEveryRouteRequiresAToken` asserts `missing`, `audience`, and `expired` on every route of the public listener and that `/readyz`, `/version`, and `/.well-known/jwks.json` answer without a token |
| an unreachable issuer at start-up, the minute retry on a fake clock, the hung discovery abandoned | `internal/auth`, `TestIssuerUnavailableIsRetried` |
| the issuer scheme rule | `internal/config`, `TestInsecureIssuersNeedTheList` |
| `act` on the authorizer request and the entry header | `internal/httpgit`, `TestActClaimIsRecordedOnEntryAndAuthorizer` |
| the authorizer outage: a 500 one request, a refused connection two, `authorizer_unavailable` within the timeout, recovery without a restart | `internal/auth`, `TestAuthorizerOutageDeniesAndRecovers` |
| deny before lookup on an unknown id and an unknown name, 404 only to an allowed caller, one authorizer request before any store read | `internal/api`, `TestDenyBeforeLookup`, over the id form and the name form of both handlers |
| both caches bounded at 65 536 with the first entry evicted | `internal/auth`, `TestCachesAreBounded` |
| two tokens differing in `act` are two calls and two entries | `internal/auth`, `TestCacheKeyIncludesActor` |
| a `read` token refused by scope on `git-receive-pack` of A and on `info/refs` of B, expired one second after `exp` | `internal/auth`, `TestRepositoryBoundTokenScope` |
| `ORIGO_DEV_TOKEN` refused with the one message | `internal/config`, `TestDevTokenIsRefused` |
| `FuzzParseToken` | `internal/auth`, as a seed-corpus test on every push; the 40 second run is `make fuzz` of spec 013 on the weekly schedule |

Spec 013 built `make fuzz`, which runs `FuzzParseToken` for 40
seconds, and the `fuzz` job of `verify.yml` that calls it on the
weekly schedule, so the spec is complete.

Divergences and interpretations, all kept:

- The exp and nbf skew of 60 seconds applies to an issuer's token, for
  the difference between the issuer's clock and the node's. A
  repository-bound token was minted on the node's own clock and gets
  no skew, which is what lets it be `expired` one second after its
  `exp` as the criterion says; with the skew the two sentences of the
  Design could not both hold.
- A minted token carries the minter's `sub` and `act` as they were on
  the minter's own token (`sub` the minter's actor when there was one,
  `act` the minter's subject), so the verifier derives from it the
  same effective subject and actor the minter had and the entry a
  build pushes names both. Read literally, "`sub` = the minter's
  effective subject, `act` copied" would put one value in both claims
  and make the subject its own actor.
- The discovery and JWKS timeout and the authorizer's timeout are
  constructor options with the Design's values as their defaults
  (`auth.DefaultFetchTimeout`, `auth.AuthorizerTimeout`, both 5
  seconds); the tests inject shorter ones and assert the defaults, so
  a hung fetch does not cost the suite five seconds per run.
- The key refresh has two paths: a loop that fetches every issuer at
  start, retries an unfetched issuer once a minute, and refreshes a set
  older than an hour; and a fetch from the request path when a token
  names an issuer with no keys or an unknown `kid`, at most once a
  minute per issuer once the set has been fetched (below for the
  first fetch). The criterion's minute retry on a fake clock drives
  the second.
- The verified-token bound of `TestCachesAreBounded` writes 65 536
  entries the way `Verify` writes them and then verifies one real
  token, because 65 537 signatures do not fit the suite's budget; the
  authorizer half makes 65 537 real calls through the stub's handler
  in-process.
- The retry accepts net/http's closed idle connection (`http: server
  closed idle connection`, the peer's FIN seen before the request was
  registered on the connection, which net/http does not retry for a
  POST): the call failed before any response byte, so it is the one
  retry the Design allows; `TestClosedIdleConnectionIsRetried` forces
  the order, and without it `TestAuthorizerOutageDeniesAndRecovers`
  was flaky (CI run 34202697213).
- Both caches are `latere.ai/x/pkg/cache.TTLCache` with the bound and
  the least-recently-used eviction; each entry carries its own expiry
  (the token's lifetime capped at 5 minutes, the allow's `ttl`, the 5
  seconds of a deny) beside the value.
- `POST /v1/repos/{id}/tokens` answers 404 `repo_not_found` for an
  unknown or deleted repository after the `admin` allow, by spec 003's
  general rule; the table here names only 201 and 400.
- The reason of a repository-bound token refused by scope is `token
  scope does not allow this action`; the Design fixes only the other
  repository's.
- The stub issuer has one control path beyond spec 013's table, a POST
  to /resume, and both stubs a `Resume` method, so a test ends an
  outage without restarting the stub. The `-fail`, `-hang`,
  `-allow`, `-authorizer-token`, and `-key` flags of the table are the binary's,
  which spec 013 builds; the packages expose them as options
  (`WithToken`, `WithAllow`, `WithKey`, `WithRS256`, `WithIssuer`,
  `WithClock`) and methods (`Fail`, `Hang`).
- The two defects spec 003's Current state records are fixed here and
  its Outcome updated: `unauthenticated` carries `details.reason`, and
  `/readyz` and `/version` on the public listener carry
  `Origo-Contract`. With them, every envelope of `internal/api`,
  `internal/httpgit`, and `internal/auth` is rendered from the code
  table in `internal/contract` (`contract.Sentence`, `contract.Write`)
  with the developer reason in `details`, and the unknown-route
  handler answers `invalid_request`; the sideband strings of a refused
  push are unchanged and stay with spec 021's code-table test.
- The `issuer_unavailable` the kind stack met at start (spec 013's
  Outcome, the `up-script` job, CI run 34207615785), fixed at its root
  on 2026-09-08: the start-up fetch of a node that came up before its
  issuer failed, and the request path then held the fetch a token could
  trigger for the whole minute, so every token of that issuer was
  refused for a minute after the issuer answered. Until an issuer's
  first fetch succeeds, a failed attempt is retried after one second,
  doubled per consecutive failure and capped at the minute
  (`FirstRetryInterval`), on the request path and by the loop; a
  fetched set keeps the minute cap
  (`TestFirstFetchFailureBacksOffFromASecond`,
  `TestIssuerUnavailableIsRetried`). Readiness stays independent of the
  issuers: an unreachable issuer does not stop the node, and a readiness
  that waited for its keys would hold a rollout for an outage the node
  is built to ride out.

A review on 2026-09-11 read the Design against `internal/auth` and the
handlers and found every rule of the verification table in the stated
order with the stated reason, every constant at its value (the 8 KiB
token bound, the 60 second skew, the 24 hour `iat`, the 5 minute and
65 536 entry caches, the 600 second `ttl` cap, the 5 second deny, the
hour and minute refreshes, the second-to-minute first retry), the
retry on a failure before any response byte and on nothing else, the
minted token's claims and `kid`, the scope rules with both reasons,
the action each handler sends, the stubs' options and control paths,
and every named test present. Two sentences were behind the tree: the
Current state said `make dev` was out of service and that one Secret
carried all four variables, when the key has its own Secret,
`origod-token-key`, that the install document and the bootstrap README
describe; and a row of the Outcome table still said the spec stays at
`testing` on the fuzz run, which spec 013 closed. Both read as the
tree stands.

One row was added on 2026-09-12 after a review of the three
repositories' authentication seams. A token with no `sub`, or an empty
one, verified to the empty subject, which is the principal spec 027
admits as anonymous, so an issuer's token naming nobody reached the
authorizer as an anonymous request whether or not
`ORIGO_ANONYMOUS_READ` was set; it is refused with `subject` now
(`TestVerifierAcceptsTwoIssuersAndRefusesEachFailure`,
`TestBadCredentialIsNotAnonymous`).
