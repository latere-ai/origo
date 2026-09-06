---
title: "Authentication and delegation: issuers, the authorizer, acting on behalf of a subject"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
affects: [internal/auth/, internal/config/, internal/httpgit/, internal/api/, cmd/origod/, deploy/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
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

## Design

### Verification

| Rule | Value |
|---|---|
| algorithms | `RS256`, `ES256`; anything else is `unauthenticated` with `details.reason: "signature"` |
| `iss` | one of `ORIGO_OIDC_ISSUERS`, or `ORIGO_PUBLIC_URL` for a repository-bound token |
| `aud` | contains `origo` |
| `exp`, `nbf` | `exp` in the future, `nbf` in the past, with 60 seconds of skew |
| `iat` | at most 24 hours old |
| keys | from each issuer's `<iss>/.well-known/openid-configuration` `jwks_uri`, cached, refreshed every hour and on an unknown `kid` at most once a minute per issuer |
| credential forms | `Authorization: Bearer <token>`; basic auth with any username and the token as the password; basic auth with the token as the username and an empty password |
| verified-token cache | keyed by the SHA-256 of the token for the shorter of its lifetime and 5 minutes, so a busy client costs one signature check per 5 minutes |
| size | a token over 8 KiB is `unauthenticated` |

`ORIGO_DEV_TOKEN` is removed with this spec: `internal/config` refuses a
start-up that sets it (`ORIGO_DEV_TOKEN is no longer read; remove it`),
and `ORIGO_OIDC_ISSUERS`, `ORIGO_AUTHORIZER_URL`, and
`ORIGO_AUTHORIZER_TOKEN` become required.

### Effective subject and actor

| Claim | Meaning |
|---|---|
| `sub` | the caller |
| `act` | optional; when present, the subject on whose behalf the call is made, and `sub` becomes the actor |

Both are recorded in every entry header (`subject`, `actor`) and every
event (`pusher.sub`, `pusher.actor`). A consumer that commits for a user
sends its service token with `act: <user sub>`.

### The authorizer

For every request that names a repository, after verification:

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
`(subject, repo id, action)` for `ttl`; a deny for 5 seconds. A deny is
403 `forbidden` with the reason in `details.reason`. Any other outcome
(a timeout, a connection failure, a non-200, a body that does not parse)
is a fresh code, never fail-open:

| Code | Status | Message | Details |
|---|---|---|---|
| `authorizer_unavailable` | 503 | Permissions cannot be checked right now. Nothing was lost. Try again in a few minutes. | `url`, `status`, `error` |

`origo_authorizer_seconds{result}` observes every call with `allow`,
`deny`, or `error`.

### Repository-bound tokens

| Method | Path | Behaviour |
|---|---|---|
| POST | `/v1/repos/{id}/tokens` | action `admin`; body `{"scope": "read"\|"write", "ttl": <seconds, 1 to 3600>}`; 201 `{"token": "<jwt>", "expires_at": "<RFC 3339>"}`; 400 `invalid_request` for another scope or ttl |
| GET | `/.well-known/jwks.json` | the public key set Origo signs with, no token required; the one unauthenticated path of the public listener |

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

- Tokens from two stub issuers verify and a token with `aud` lacking
  `origo`, an expired `exp`, an unknown `iss`, an unknown `kid` after
  one refresh, or an unsupported `alg` answers 401 `unauthenticated`
  with the matching `details.reason` on `info/refs`, `git-upload-pack`,
  and `GET /v1/repos/{id}` (proposed: `internal/auth`,
  `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure`).
- A service token with `act` sets the effective subject: the authorizer
  request carries both, the entry header carries `subject` and `actor`,
  and the push event carries `pusher.sub` and `pusher.actor` (proposed:
  `internal/httpgit`, `TestActClaimIsRecordedOnEntryAndAuthorizer`).
- With the authorizer answering 500 or not at all, every request is 503
  `authorizer_unavailable` within 5 seconds, and the first request after
  it recovers is served without a restart (proposed: `internal/auth`,
  `TestAuthorizerOutageDeniesAndRecovers`).
- A `read` token minted for repository A answers 403 `forbidden` on
  `git-receive-pack` of A and on `info/refs` of repository B, and 401
  `unauthenticated` with `details.reason: "expired"` one second after
  its `exp` with a fake clock (proposed: `internal/auth`,
  `TestRepositoryBoundTokenScope`).
- A start-up with `ORIGO_DEV_TOKEN` set fails with the one message
  (`internal/config`, `TestDevTokenIsRefused`).
- Fuzzing the token parser with random bytes and mutated valid tokens
  finds no panic in 40 seconds (proposed: `internal/auth`, `FuzzParseToken`).
