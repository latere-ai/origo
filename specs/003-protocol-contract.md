---
title: "Protocol contract: what a consumer relies on"
status: in-progress
track: infra
depends_on:
  - specs/001-architecture.md
affects: [internal/httpgit/, internal/api/, internal/auth/, internal/events/, docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Protocol contract

## Overview

This is the document a platform integrating Origo reads. It names every
endpoint, header, and behaviour a consumer may rely on, and nothing else
is promised. A consumer that codes against this contract can run its
tests against the conformance stub (spec 013) and against a live Origo
and get the same answers.

## Current state

Latere's hosting product codes against this contract with its data plane
product as the interim provider, so the contract is fixed before Origo
ships its first release.

## Design

### Identity

Every request carries `Authorization: Bearer <token>`, a JWT from one of
the configured OIDC issuers, or git's basic auth with any username and the
token as the password. Origo verifies signature, issuer, expiry, and
audience `origo`. A service token may carry an `act` claim naming the
subject it acts for; the effective subject is then that one, and the
service is recorded as the actor (spec 007). Origo never decides who may
do what: it asks the consumer's authorizer with the effective subject,
the repository, and the action (`read`, `write`, `admin`), and caches
the answer for 60 seconds.

### Repository lifecycle

| Method | Path | Body | Result |
|---|---|---|---|
| POST | `/v1/repos` | `{"id": "<uuid>", "owner": "<opaque>", "slug": "<name>", "default_branch": "main"}` | 201; `id` chosen by the consumer, unique forever; `owner` and `slug` are labels Origo stores for URLs and never interprets |
| GET | `/v1/repos/{id}` | | `{id, owner, slug, default_branch, size_bytes, head, updated_at}` |
| PATCH | `/v1/repos/{id}` | `{"owner", "slug", "default_branch"}` any subset | renames take effect at once; the old URL answers 404 |
| DELETE | `/v1/repos/{id}` | | 202; objects are deleted from the log after a 7 day hold during which the consumer may undelete |
| POST | `/v1/repos/{id}/undelete` | | 200 within the hold |

Clone URLs are `<ORIGO_PUBLIC_URL>/<owner>/<slug>.git`; the id form
`<ORIGO_PUBLIC_URL>/r/<id>.git` always works and is what a consumer should
store.

### Smart HTTP

`GET .../info/refs?service=git-upload-pack|git-receive-pack`,
`POST .../git-upload-pack`, `POST .../git-receive-pack`, protocol v2
advertised, v0 accepted. Capabilities a consumer may rely on:

| Capability | Meaning |
|---|---|
| `allow-tip-sha1-in-want`, `allow-reachable-sha1-in-want` | shallow fetch of any reachable commit by hash, which is how a build fetches a deploy's commit |
| `filter` | partial clone (`blob:none`, `tree:0`) |
| `shallow`, `deepen-since`, `deepen-not` | shallow clones and deepening |
| `atomic` | a push with several reference updates lands entirely or not at all |
| `push-options` | `origo.event=off` suppresses the push event for that push |
| `report-status-v2` | per-reference results |

A push is acknowledged only when durable (spec 001). A push that races
another on the same reference gets git's standard non-fast-forward
rejection and can be retried after a fetch. There is no lock a consumer
can take; a consumer that needs a single writer serializes on its own side.

### Read operations

All under `/v1/repos/{id}` and detailed in spec 009: `refs`, `commits`
(log with paging), `commits/{sha}`, `compare/{base}...{head}` (diff, capped),
`tree/{sha}?path=`, `blob/{sha}` (raw bytes with content type), and
`archive/{sha}.tar.zst` streaming a tarball of the tree at that commit
with a stable entry order and no `.git`.

### Push events

When `ORIGO_EVENTS_URL` is set, every reference update sends one signed
`POST` per push (spec 008):

```json
{"id": "<event uuid>", "repo": "<uuid>", "owner": "…", "slug": "…",
 "pusher": {"sub": "…", "actor": "…"},
 "updates": [{"ref": "refs/heads/main", "before": "<sha>", "after": "<sha>", "forced": false}],
 "at": "2026-09-06T10:00:00Z"}
```

Header `Origo-Signature: sha256=<hmac>` over the body with the shared
secret; retried with backoff for 24 hours; delivered at least once, so the
consumer keys on `id`.

### Delegation

A consumer that commits or pushes on behalf of a user does so with its own
service token carrying `act`; the reflog and the push event carry both the
effective subject and the actor. A consumer that lets a build fetch mints a
short-lived read token through `POST /v1/repos/{id}/tokens {"scope": "read",
"ttl": 1200}`, which returns a token bound to that repository only.

### Errors

Family envelope `{"error": {"code", "message", "details"}}` with stable
codes: `unauthenticated`, `forbidden`, `repo_not_found`, `repo_exists`,
`ref_not_found`, `non_fast_forward`, `over_quota`, `rate_limited`,
`storage_unavailable`. Git protocol errors use git's own sideband
messages with the same codes as text.

### Compatibility

The contract is versioned by the `Origo-Contract: 1` response header.
Additive changes keep the number; a removal or a semantic change bumps it
and the previous number stays served for twelve months.

## Acceptance criteria

- The conformance suite (spec 013) exercises every row of every table
  above against a live node and against the stub, and both pass.
- A consumer's integration tests written against the stub pass unchanged
  against a live node.
- An unknown capability, endpoint, or field is not relied upon by any
  consumer in the organization, checked by grepping consumers for
  `/v1/repos` paths and comparing to this document.

## Outcome

Phase 1 shipped, on 2026-09-06, the part of the contract one node can
serve without identity: smart HTTP in both URL forms and the repository
lifecycle. Everything else in this document is promised, not yet served.

Served:

- `GET .../info/refs?service=`, `POST .../git-upload-pack`,
  `POST .../git-receive-pack` at `/r/<id>.git` and `/<owner>/<slug>.git`,
  protocol v2 advertised and v0 accepted. Capabilities verified with the
  real git client: `allow-tip-sha1-in-want`, `allow-reachable-sha1-in-want`,
  `filter` (`blob:none`), `shallow`, `deepen-since`, `deepen-not`,
  `atomic`, `push-options` (recorded in the entry header; `origo.event=off`
  is read by spec 008), `report-status-v2`.
- A push is acknowledged only after its entry and index object are
  durable; a push over a reference another writer moved gets git's
  rejection with the `non_fast_forward` code in the sideband.
- `POST /v1/repos` (201, `repo_exists` on a duplicate id or a taken
  name), `GET`, `PATCH` (rename takes effect at once and the old URL
  answers 404; `default_branch` moves HEAD through the log), `DELETE`
  (202, 7 day hold), `POST .../undelete`.
- The error envelope with the codes named here and the
  `Origo-Contract: 1` header on every response.

Not yet served (phase 2 and later): OIDC identity, the authorizer,
delegation and `act`, `POST .../tokens`, the read operations of spec 009,
push events of spec 008, and the conformance suite of spec 013, which is
what this spec's acceptance criteria require. In phase 1 every request
carries the one static bearer of `ORIGO_DEV_TOKEN` (spec 002, Outcome).

Divergences:

- `owner` and `slug` are restricted to `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`
  and the owners `r` and `v1` are reserved, because both appear as path
  segments of the public surface.
- `updated_at` in the repository representation is the metadata's update
  time (creation or rename), not the last push: the index objects carry
  no timestamp. Spec 009 decides whether that field moves with a push.
- The name is stored beside the log as `origo/names/<owner>/<slug>`,
  created by create-if-absent so a taken name is refused by the store.
  Spec 004's object table did not list it.
- The status codes for the lifecycle errors: 400 `invalid_request` for a
  malformed body, 404 `repo_not_found`, 409 `repo_exists`, 503
  `storage_unavailable`. `invalid_request` is an addition to the code
  list.
