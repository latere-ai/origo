---
title: "Limits and abuse controls"
status: drafted
track: infra
depends_on:
  - specs/007-authentication-and-delegation.md
  - specs/004-write-ahead-log.md
affects: [internal/limits/, internal/httpgit/, internal/api/, internal/config/, cmd/origod/]
effort: small
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Limits and abuse controls

## Overview

Origo runs behind a consumer that owns quotas per user or organization;
Origo enforces per-repository and per-node bounds so one client cannot
exhaust a node, and exposes the one knob the consumer sets through the
authorizer, `quota_bytes`.

## Current state

Phase 1 bounds what its parsers accept: a JSON body of 64 KiB, at most
100 000 commands and 1 000 push options in a receive-pack request (both
answered 400 `invalid_request`), a 4 KiB entry header, a 64 MiB
transaction, a 64 MiB index object. Every git subprocess has a 5 minute
deadline. Object storage calls use `pkg/s3`'s `DefaultRetry`: 3 attempts
from 50 ms, capped at 2 s, on a 5xx, a 429, or a transport failure.
Nothing limits a push's size, a repository's size, a subject's request
rate, or the number of concurrent subprocesses. `internal/limits` does
not exist.

## Design

| Limit | Value | Enforced at | Answer |
|---|---|---|---|
| repository size | the authorizer's `quota_bytes`, default 50 GiB | receive-pack, before the entry is written: `size_bytes` of the held index plus the pack's bytes; LFS upload batch (spec 010) | sideband `over_quota` with `details.limit: "repository"`, `bytes`, `max` |
| single push | 2 GiB, one entry, one `PUT` (spec 004) | receive-pack, from `Content-Length` when present and while spooling | 413 `over_quota`, `details.limit: "push"` |
| references | 100 000 commands per push and 100 000 references in the map after it | receive-pack | `over_quota`, `details.limit: "refs"` (phase 1 answers 400 `invalid_request` for the command count) |
| push options | 1 000 per push | receive-pack | 400 `invalid_request` |
| requests per subject | 600 per minute, a token bucket per effective subject per node, burst 600 | every route of the public listener after authentication | 429 `rate_limited` with `Retry-After` in whole seconds and `details.limit: "subject"` |
| concurrent git subprocesses per node | `ORIGO_MAX_GIT_PROCS`, default 64, one semaphore shared by `internal/httpgit`, `internal/api`, and compaction | before a subprocess starts; a request waits at most 5 seconds for a slot | 429 `rate_limited`, `details.limit: "subprocesses"` |
| subprocess wall time | 5 minutes for `upload-pack`, `receive-pack`, `index-pack`, and repack; 30 seconds for a read API operation (spec 009) | `internal/repo.Git` and the handlers | the subprocess is killed with its process group; 503 `storage_unavailable` on the API, git's own error on the sideband |
| JSON body | 64 KiB | every `/v1/` route except the operation routes of spec 020, which carry their own 64 MiB limit | 400 `invalid_request` |
| entry header, transaction, index object | 4 KiB, 64 MiB, 64 MiB | the parsers of spec 004 | the entry or index is refused as corrupt (spec 015) |
| object storage retries | 3 attempts from 50 ms, capped at 2 s, under `ORIGO_STORAGE_TIMEOUT` per attempt (spec 015) | `pkg/s3` | the store's error |

`receive.fsckObjects` (on since phase 1) rejects malformed objects on
the way in; `transfer.fsckObjects` and `core.protectHFS` (spec 016) are
added to the repository configuration of spec 004. A frozen repository
(spec 019) accepts reads and refuses writes with `repo_frozen`. A deleted
repository is unreadable at once and purged after the hold (spec 004).
`origo_rate_limited_total{limit}` counts every refusal by `subject` or
`subprocesses`.

## Not in this spec

Per-organization quotas, which live in the consumer's authorizer.
Connection limits at the ingress. Bandwidth shaping.

## Acceptance criteria

- A push that would take `size_bytes` past `quota_bytes` is refused in
  the sideband with `over_quota`, no entry is written, and
  `details.bytes` and `max` name the sizes (proposed: `internal/httpgit`,
  `TestPushOverQuotaWritesNothing`).
- A push whose `Content-Length` or spooled body exceeds 2 GiB is 413
  `over_quota` with `details.limit: "push"`, with the spool stopped at
  the limit (proposed: `internal/httpgit`, `TestPushOverTwoGiBIsRefused`,
  with the limit lowered by an option in the test).
- A subject sending 601 requests in one minute sees 429 with
  `Retry-After` on the 601st and a second subject sees none (proposed:
  `internal/limits`, `TestPerSubjectTokenBucket`).
- With `ORIGO_MAX_GIT_PROCS=2`, a third concurrent clone waits up to 5
  seconds then answers 429 `rate_limited` with
  `details.limit: "subprocesses"` (proposed: `internal/limits`,
  `TestSubprocessCap`).
- A frozen repository accepts a clone and refuses a push with
  `repo_frozen` (spec 019's conformance case).
