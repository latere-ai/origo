---
title: "Authentication and delegation: issuers, the authorizer, acting on behalf of a subject"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
affects: [internal/auth/, internal/config/, internal/httpgit/, internal/api/, cmd/origod/, deploy/, Makefile, test/e2e/, test/stubs/]
effort: medium
created: 2026-09-06
updated: 2026-09-07
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

`internal/auth.StaticBearer` admits the one value of `ORIGO_DEV_TOKEN`
in the three forms spec 002's Outcome lists and stamps every admitted
request with the subject `dev`; `internal/httpgit` records that subject
in the entry header. `ORIGO_OIDC_ISSUERS`, `ORIGO_AUTHORIZER_URL`, and
`ORIGO_AUTHORIZER_TOKEN` are read by `internal/config` and unused.
`ORIGO_TOKEN_KEY` is not read. No request is authorized: every admitted
request may do everything.

Removing `ORIGO_DEV_TOKEN` breaks three things that set it today:
`make dev` (`DEV_SERVICE_ENV` in the `Makefile`), the end-to-end harness
(`test/e2e/harness_test.go`, `start`), and the bootstrap Secret
`origod-dev-token` (`deploy/bootstrap/secrets.example.yaml`, read by
`deploy/base/deployment.yaml`). The replacement is the stub issuer and
the stub authorizer of spec 013 under `test/stubs/`, built in the same
phase as this spec: `make dev` runs `test/stubs/cmd/origo-stubs` beside
MinIO and prints a clone line with a token the stub minted (spec 002,
Local stack); the harness starts the issuer and the authorizer
in-process and points `ORIGO_OIDC_ISSUERS` and `ORIGO_AUTHORIZER_URL` at
them; the bootstrap Secret becomes `origod-auth` with
`ORIGO_OIDC_ISSUERS`, `ORIGO_AUTHORIZER_URL`, `ORIGO_AUTHORIZER_TOKEN`,
and `ORIGO_TOKEN_KEY`, and the kind overlay (spec 013) runs the stubs as
pods. `cmd/origod/main_test.go` and `internal/config/config_test.go`
drop the variable from their fixtures.

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
| `kid` | names a key of the issuer's set, after one refresh when unknown | `kid` |
| signature | verifies with that key | `signature` |
| `aud` | contains `origo` | `audience` |
| `exp` | in the future, with 60 seconds of skew | `expired` |
| `nbf` | absent or in the past, with 60 seconds of skew | `nbf` |
| `iat` | present and at most 24 hours old | `iat` |
| keys | from each issuer's `<iss>/.well-known/openid-configuration` `jwks_uri`, cached, refreshed every hour and on an unknown `kid` at most once a minute per issuer | |
| credential forms | `Authorization: Bearer <token>`; basic auth with any username and the token as the password; basic auth with the token as the username and an empty password | |
| verified-token cache | keyed by the SHA-256 of the token for the shorter of its lifetime and 5 minutes, so a busy client costs one signature check per 5 minutes; at most 65 536 entries, least recently used evicted | |

`ORIGO_DEV_TOKEN` is removed with this spec: `internal/config` refuses a
start-up that sets it (`ORIGO_DEV_TOKEN is no longer read; remove it`),
and `ORIGO_OIDC_ISSUERS`, `ORIGO_AUTHORIZER_URL`, and
`ORIGO_AUTHORIZER_TOKEN` become required. Development and the test
tiers run the stub issuer and the stub authorizer of spec 013 in its
place (Current state).

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

### The authorizer

After verification, per request that names a repository:

`POST <ORIGO_AUTHORIZER_URL>` with `Authorization: Bearer <ORIGO_AUTHORIZER_TOKEN>`,
`Content-Type: application/json`, a 5 second timeout:

```json
{"subject": "…", "actor": "…", "repo": {"id": "…", "owner": "…", "slug": "…"}, "action": "read"}
```

`action` is `read` for `info/refs?service=git-upload-pack`,
`git-upload-pack`, the read API, LFS download, and `GET /v1/repos/{id}`;
`write` for `info/refs?service=git-receive-pack`, `git-receive-pack`, and
LFS upload; `admin` for `POST /v1/repos`, `PATCH`, `DELETE`, `undelete`,
`POST /v1/repos/{id}/tokens`, and the operations of spec 019. For
`POST /v1/repos` the `repo` object carries the id, owner, and slug the
body names.

Response 200 `{"allow": true, "ttl": 60, "replicas": 1, "quota_bytes": 53687091200}`
or 200 `{"allow": false, "reason": "…"}`. `ttl` defaults to 60 seconds
and is capped at 600; `replicas` (spec 005) defaults to 1; `quota_bytes`
(spec 012) defaults to 53687091200. An allow is cached per
`(subject, actor, repo id, action)` for `ttl`; a deny for 5 seconds; an
answer for an unresolved name (empty id) is not cached. Each cache
holds at most 65 536 entries and evicts the least recently used. The
call is made once and retried once when the connection failed before a
response line arrived (a refused or reset connection, a dial timeout);
a 5xx, a timeout after the request was sent, and a body that does not
parse are never retried. A deny is 403 `forbidden` with the reason in
`details.reason`. Any other outcome (a timeout, a connection failure
after the retry, a non-200, a body that does not parse) is a fresh
code, never fail-open:

| Code | Status | Message | Details |
|---|---|---|---|
| `authorizer_unavailable` | 503 | Permissions cannot be checked right now. Nothing was lost. Try again in a few minutes. | `url`, `status`, `error` |

`origo_authorizer_seconds{result}` observes every call with `allow`,
`deny`, or `error`.

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
Per-request rate limits (spec 012). Issuer discovery over anything but
HTTPS.

## Acceptance criteria

- Tokens from two stub issuers (`test/stubs/issuer`, spec 013) verify,
  and a token that fails each row of the verification table answers 401
  `unauthenticated` with that row's `details.reason` (`missing`,
  `size`, `malformed`, `signature` for an unsupported `alg` and for a
  bad signature, `issuer`, `kid` after one refresh, `audience`,
  `expired`, `nbf`, `iat`) on `info/refs`, `git-upload-pack`, and
  `GET /v1/repos/{id}` (proposed: `internal/auth`,
  `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure`).
- A service token with `act` sets the effective subject: the authorizer
  request carries both, the entry header carries `subject` and `actor`,
  and the push event carries `pusher.sub` and `pusher.actor` (proposed:
  `internal/httpgit`, `TestActClaimIsRecordedOnEntryAndAuthorizer`).
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
  on every push and for 40 seconds under `make fuzz` (spec 002) on the
  weekly schedule (proposed: `internal/auth`, `FuzzParseToken`).
