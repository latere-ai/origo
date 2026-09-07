---
title: "Protocol contract: what a consumer relies on"
status: in-progress
track: infra
depends_on:
  - specs/001-architecture.md
affects: [internal/contract/, internal/httpgit/, internal/api/, internal/auth/, internal/events/, docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-08
author: changkun
---

# Protocol contract

## Overview

This is the document a platform integrating Origo reads. It names every
endpoint, header, capability, and error a consumer may rely on, and
nothing else is promised. A consumer that codes against this contract can
run its tests against the contract stub (spec 013) and against a live
Origo and get the same answers, which the conformance suite (spec 021)
proves. Specs 007, 008, 009, 010, 014, 019, and 020 add surfaces to the
contract; each owns its own table, and this document points at them so
the contract is one document with its appendices.

## Current state

Phase 1 serves the part of this contract one node can serve without
identity: smart HTTP in both URL forms from `internal/httpgit`, the
repository lifecycle from `internal/api`, the error envelope and the
`Origo-Contract` header from `internal/contract` over
`latere.ai/x/pkg/httpjson`. Identity is the static bearer of spec 002's
Outcome. The Outcome below lists what is served and what is promised.

Defects against the contract found by review, for the builder:

- `GET /readyz` and `GET /version` on the public listener are mounted in
  `cmd/origod/node.go` outside `contract.Middleware`, so those two
  responses carry no `Origo-Contract` header; the table below says every
  response of the public listener carries it.
- `unauthenticated` is sent with no `details.reason`: `internal/auth`
  answers every refusal alike. The reasons are the list in the table
  below, produced by spec 007's verifier; the first draft's `scope` was
  never produced by any path and is dropped, because a token with the
  wrong scope is 403 `forbidden` (spec 007).

## Design

### Identity

Every request carries `Authorization: Bearer <token>` or git's basic auth
with any username and the token as the password. From spec 007 on, the
token is a JWT from one of the configured OIDC issuers with audience
`origo`, or a repository-bound token Origo minted; a service token may
carry an `act` claim naming the subject it acts for, and the service is
then recorded as the actor. Origo never decides who may do what: it asks
the consumer's authorizer with the effective subject, the repository, and
the action (`read`, `write`, `admin`), and caches the answer for the
`ttl` the authorizer returns, 60 seconds by default (spec 007). In phase
1 the token is the one value of `ORIGO_DEV_TOKEN` and every request
carries the subject `dev`.

A request without a valid token answers 401 `unauthenticated` with
`WWW-Authenticate: Basic realm="origo"`, on every path including
`info/refs`, so git prompts for credentials. For every request that
names a repository the authorizer is asked before the repository's
metadata is read (spec 007, "Authorization before lookup"): a deny is
403 `forbidden` whether or not the repository exists, and 404
`repo_not_found` is answered only to a caller the authorizer allowed.

### Repository lifecycle

A repository is identified by a lower-case UUID the consumer chooses,
matching `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
unique forever: an id is never reused, even after deletion and purge.
`owner` and `slug` are labels Origo stores for URLs and never interprets,
matching `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, not `.` or `..`; the owners
`r` and `v1` are reserved because both are path segments of the public
surface. Bodies are JSON, at most 64 KiB, unknown fields refused.

| Method | Path | Request | Response |
|---|---|---|---|
| POST | `/v1/repos` | `{"id", "owner", "slug", "default_branch"}`; `default_branch` defaults to `main` and must be a valid branch name | 201 with the representation; 409 `repo_exists` on a duplicate id or a taken `owner/slug`; 400 `invalid_request` |
| GET | `/v1/repos/{id}` | | 200 `{"id", "owner", "slug", "default_branch", "size_bytes", "head", "updated_at"}`; 404 `repo_not_found` for an unknown, malformed, or deleted id; 410 `gone` for a purged id (spec 019). Other specs add fields to the representation and own them: `pushed_at` (spec 009), `frozen_at` (spec 019), `verified_at` and `verified_equal` (spec 014); each is null until its operation ran |
| PATCH | `/v1/repos/{id}` | any subset of `{"owner", "slug", "default_branch"}` | 200 with the representation; a rename takes effect at once and the old URL answers 404; `default_branch` moves `HEAD` through the log; 409 `repo_exists` when the name is taken |
| DELETE | `/v1/repos/{id}` | | 202 `{"id", "deleted_at", "purge_after"}`; every other endpoint answers 404 from then on and 410 `gone` once the objects are purged after the 7 day hold (spec 019); repeated on a deleted repository, 202 with the original times |
| POST | `/v1/repos/{id}/undelete` | | 200 with the representation within the hold; 410 `gone` after the purge (spec 019) |

`size_bytes` is the sum of the pack bytes in the log since creation;
`head` is the object id of the default branch, empty when the branch does
not exist; `updated_at` is the time of the last create or rename of the
metadata, not the last push (spec 009 adds `pushed_at`, read from the
index object's `pushed_at` of spec 004).

Clone URLs are `<ORIGO_PUBLIC_URL>/<owner>/<slug>.git`; the id form
`<ORIGO_PUBLIC_URL>/r/<id>.git` always works and is what a consumer
should store. In the tables below `{repo}` stands for either
`r/{id}.git` or `{owner}/{slug}.git`.

### Smart HTTP

| Method | Path | Behaviour |
|---|---|---|
| GET | `/{repo}/info/refs` | `?service=git-upload-pack` or `git-receive-pack`; any other value 400 `invalid_request`; protocol v2 advertised when the client sends `Git-Protocol: version=2`, v0 otherwise |
| POST | `/{repo}/git-upload-pack` | a fetch or clone; `Content-Encoding: gzip` accepted |
| POST | `/{repo}/git-receive-pack` | a push; `Content-Encoding: gzip` accepted; the body is spooled to disk before git runs |

Capabilities a consumer may rely on:

| Capability | Meaning |
|---|---|
| `allow-tip-sha1-in-want`, `allow-reachable-sha1-in-want` | fetch of any reachable commit by hash, which is how a build fetches a deploy's commit |
| `filter` | partial clone (`blob:none`, `tree:0`) |
| `shallow`, `deepen-since`, `deepen-not` | shallow clones and deepening |
| `atomic` | a push with several reference updates lands entirely or not at all; every push through Origo is atomic whether or not the client asks, because a push is one log entry |
| `push-options` | options are recorded in the entry; `origo.event=off` suppresses the push event for that push (spec 008) |
| `report-status-v2` | per-reference results |

A push is acknowledged only when durable (spec 001, invariant 1). A push
that races another on the same reference gets git's rejection with
`non_fast_forward` in the sideband and can be retried after a fetch. A
push to `HEAD` is refused. There is no lock a consumer can take; a
consumer that needs a single writer serializes on its own side.

### Read operations

All under `/v1/repos/{id}` with action `read` and defined in spec 009:
`refs`, `commits`, `commits/{sha}`, `compare/{base}...{head}`,
`tree/{sha}`, `blob/{sha}`, and `archive/{sha}.tar.gz`.

### Push events

When `ORIGO_EVENTS_URL` is set, every acknowledged push sends one signed
`POST` (spec 008 owns delivery, signing, retries, and the `kind` values):

```json
{"id": "<event uuid>", "kind": "push", "repo": "<uuid>", "seq": 1044, "owner": "…", "slug": "…",
 "pusher": {"sub": "…", "actor": "…"},
 "updates": [{"ref": "refs/heads/main", "before": "<sha>", "after": "<sha>", "forced": false}],
 "at": "2026-09-06T10:00:00Z"}
```

Delivery is at least once, so a consumer keys on `id`, which spec 008
derives from `repo` and `seq` so a redelivery carries the same id. Spec
008 defines two optional fields: `kind_detail` on a push that changes
no branch or tag, and `operation` on a push made by a server-side
operation (spec 020).

### Delegation and tokens

A consumer that commits or pushes on behalf of a user does so with its
own service token carrying `act`; the entry and the event carry both the
effective subject and the actor. A consumer that lets a build fetch mints
a repository-bound token through `POST /v1/repos/{id}/tokens` (spec 007).

### Errors

The envelope is `latere.ai/x/pkg/httpjson`:
`{"error": {"code": "<code>", "message": "<one user sentence>", "details": {…}}}`.
`code` is stable, `message` is the one sentence fixed here for the code
and never built from the underlying error, `details` is an object of the
developer fields named below, present only when there is one. Git
protocol errors use the sideband as `<code>: <message>`. Codes other
specs add: `authorizer_unavailable` (007), `blob_too_large` (009),
`operation_timeout` (009), `repository_unavailable` (015), `gone`,
`repo_frozen`, `repo_importing`, `repo_not_empty`, `import_not_found`
(019), `merge_conflict`, `invalid_change` (020).

| Code | Status | Message | Details |
|---|---|---|---|
| `invalid_request` | 400 | The request is malformed. | `reason`: the validation failure in the developer register; `field` when one field is at fault |
| `unauthenticated` | 401 | A bearer token is required. | `reason`: `missing`, `malformed`, `size`, `signature`, `issuer`, `issuer_unavailable`, `audience`, `expired`, `nbf`, `iat`, `unknown_key`; spec 007 says which check produces each |
| `forbidden` | 403 | You do not have permission to do this. | `action`, `subject`, `reason` from the authorizer |
| `repo_not_found` | 404 | Repository not found. | `id`, or `owner` and `slug` |
| `ref_not_found` | 404 | The reference or object does not exist in this repository. | `ref` |
| `repo_exists` | 409 | A repository with this id or name already exists. | `field`: `id` or `name`; `id`, `owner`, `slug` |
| `non_fast_forward` | sideband; 409 on the JSON API | The reference moved since you fetched. Fetch, then push again. | `ref`, `expected`, `actual` |
| `over_quota` | 413 | The push exceeds the repository's limit. | `limit`: `repository`, `push`, or `refs`; `bytes`; `max` |
| `rate_limited` | 429 with `Retry-After` | Too many requests. Wait and try again. | `limit`, `retry_after` |
| `storage_unavailable` | 503 | The repository is temporarily unavailable. Nothing was lost. Try again in a few minutes. | `op`, `key`, `error` |

Every response, success or error, carries `Origo-Contract`.

| Header | Meaning |
|---|---|
| `Origo-Contract` | the contract version, `1`; on every response of the public listener |

Headers other specs add, listed here so a consumer reads them from the
contract; each is defined by the spec named:

| Spec | Header | Meaning |
|---|---|---|
| this spec | `Origo-Contract` | the contract version, above |
| 005 | `Origo-Prefer` | on every response that names a repository: the nodes that hold it warm, highest score first; a hint for routing, never a redirect; absent on a 401 or 403 for a name that did not resolve, so a refused caller learns nothing about where a repository lives |
| 015 | `Origo-Stale` | on a response served from the local copy without a currency check while the bucket is unreachable: the whole seconds since the last check that answered; absent on every consistent response, so a consumer that must not read stale refuses the response by this header |

The read API's `Origo-Commit` and `Origo-Truncated` are spec 009's; the
event delivery headers are spec 008's.

### Compatibility

Additive changes (a new endpoint, field, capability, header, or code)
keep the number. A removal or a semantic change bumps it, and the
previous number stays served for twelve months, selected by a request
header `Origo-Contract: <n>` from the next major version on (spec 017).

## Acceptance criteria

- The conformance suite (spec 021) exercises every row of every table
  above against a live node and against the stub, and both pass
  (`test/conformance`, `TestContract`).
- A consumer's integration tests written against the stub pass unchanged
  against a live node (spec 013, the stub criterion).
- Every code in the table above and in the tables of specs 007, 009,
  015, 019, and 020 has exactly one `message`, asserted by a test over
  `internal/contract` that lists the codes and their sentences and by the
  conformance suite comparing responses to it (proposed:
  `internal/contract`, `TestEveryCodeHasOneSentence`).
- A repository created with an id, renamed, deleted, and undeleted goes
  through every status of the lifecycle table, the old URL answers 404
  after the rename, and a duplicate id or a taken name answers 409
  (`internal/api`, `TestRepositoryLifecycle`).
- Clone, fetch, push, partial clone with `blob:none`, shallow clone with
  deepening, fetch by reachable hash, an atomic multi-reference push, and
  push options work over both URL forms with the real git
  (`internal/httpgit`, `TestCloneFetchPushOverSmartHTTP`).
- A push over a moved reference is refused with `non_fast_forward` in the
  sideband and a concurrent push to another branch lands
  (`internal/httpgit`, `TestStalePushIsRefusedAndConcurrentBranchesLand`,
  `TestReferenceMovedBetweenAdvertisementAndPush`).

## Outcome

Phase 1 shipped, on 2026-09-06, the part of the contract one node can
serve without identity. Served: both URL forms of smart HTTP with every
capability in the table, verified with the real git client; a push
acknowledged only after its entry and index object are durable; the
lifecycle table; the envelope with the codes named here; the
`Origo-Contract: 1` header on every response.

Not yet served (phase 2 and later): OIDC identity, the authorizer,
delegation and `act`, `POST /v1/repos/{id}/tokens`, the read operations
of spec 009, push events of spec 008, and the conformance suite of spec
021, which is what the first two acceptance criteria require. In phase 1
every request carries the one static bearer of `ORIGO_DEV_TOKEN`.

Divergences recorded against the first draft, all kept:

- `owner` and `slug` are restricted to the grammar above and `r` and
  `v1` are reserved.
- `updated_at` is the metadata's update time, not the last push: the
  phase 1 index objects carry no timestamp. Spec 004 gives the index
  object `pushed_at` and spec 009 serves it.
- The name is stored beside the log as `origo/names/<owner>/<slug>`,
  created by create-if-absent, so a taken name is refused by the store.
- `invalid_request` was added for a malformed body, id, label, branch
  name, or service parameter.
- `POST /v1/repos/{id}/undelete` after the purge answers 404
  `repo_not_found` today, because the purge removes `meta`; 410 `gone`
  needs the tombstone spec 019 defines.

Divergences to fix, owned by spec 021's code-table test:

- Phase 1 sends `message` in lower case without a period (`a bearer
  token is required`, `repository not found`, `repository unavailable`),
  two sentences for `repo_exists` (`a repository with this id exists`,
  `a repository with this owner and slug exists`), and the validation
  reason inside `message` for `invalid_request`. The table above is the
  contract; the code moves the reason into `details.reason` and sends the
  fixed sentences.
- The unknown-route handler in `cmd/origod` answers 404 `repo_not_found`
  with `no such route`; it moves to `invalid_request` with
  `details.reason: "no such route"`.
- The sideband for a refused commit is `storage_unavailable: the push
  was not recorded, retry`, and for a moved reference
  `non_fast_forward: <ref> moved to <sha> since you fetched; fetch
  first`; both become the table's sentences, the reference and the
  hashes moving to the developer detail.
