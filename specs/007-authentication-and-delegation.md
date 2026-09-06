---
title: "Authentication and delegation: issuers, the authorizer, acting on behalf of a subject"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
affects: [internal/auth/, internal/httpgit/, internal/api/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Authentication and delegation

## Overview

Origo knows who is calling and asks the consumer whether they may. It
verifies tokens from configured OIDC issuers, lets a service act on behalf
of a subject with an auditable claim, mints narrow read tokens for builds,
and delegates every authorization decision to an endpoint the consumer
runs. Origo stores no user and no permission.

## Current state

Spec 003 names the credential forms. Latere's identity service issues
RS256 tokens with an `act` claim for delegation; any OIDC issuer works.

## Design

### Verification

Tokens are RS256 or ES256 JWTs with `iss` in `ORIGO_OIDC_ISSUERS`, `aud`
containing `origo`, `exp` in the future, `iat` at most 24 hours old. JWKS
are fetched from each issuer's discovery document, cached, refreshed
hourly and on an unknown `kid`. Basic auth on the git endpoints takes the
password as the token; the username is ignored. A token bound to one
repository (below) is accepted only for that repository.

### Effective subject and actor

| Claim | Meaning |
|---|---|
| `sub` | the caller |
| `act` | optional; when present, the subject on whose behalf the call is made, and `sub` becomes the actor |

Both are recorded on every push entry and every event. A consumer that
commits for a user sends its service token with `act: <user sub>`; the
authorizer receives the effective subject and may also receive the actor.

### The authorizer

`POST <ORIGO_AUTHORIZER_URL>` with `Authorization: Bearer <ORIGO_AUTHORIZER_TOKEN>`:

```json
{"subject": "…", "actor": "…", "repo": {"id": "…", "owner": "…", "slug": "…"}, "action": "read"|"write"|"admin"}
```

Response `{"allow": true, "ttl": 60, "replicas": 1, "quota_bytes": 53687091200}`
or `{"allow": false, "reason": "…"}`. Cached per `(subject, repo, action)`
for `ttl` seconds, default 60, deny cached 5 seconds. Unreachable
authorizer denies everything with `storage_unavailable` replaced by
`authorizer_unavailable`; Origo never fails open. The consumer's endpoint
is the single place ownership and membership are decided, which is what
keeps Origo free of a user model.

### Repository-bound tokens

`POST /v1/repos/{id}/tokens {"scope": "read"|"write", "ttl": <seconds ≤ 3600>}`
by a caller with `admin` returns an Origo-signed token with `aud: origo`,
`repo: <id>`, `scope`, and the requested `exp`. It skips the authorizer
because the decision was made at minting; a build fetches with it and can
do nothing else. Origo signs with a key from `ORIGO_TOKEN_KEY` and serves
its JWKS at `/.well-known/jwks.json` so consumers can verify.

### Rate of verification

Verified tokens are cached by hash for their lifetime up to 5 minutes, so
a busy client costs one signature check per 5 minutes.

## Acceptance criteria

- Tokens from two issuers verify; a token with the wrong audience,
  expired, or from an unknown issuer is 401 with `unauthenticated`.
- `act` sets the effective subject and both appear on the entry and the
  event; the authorizer is called with both.
- An authorizer outage denies every request and recovers without restart.
- A read token cannot push (403 on `receive-pack`), expires at `exp`, and
  is rejected for another repository.
- A fuzz test over the token parser finds no panic.
