---
title: "Limits and abuse controls"
status: drafted
track: infra
depends_on:
  - specs/007-authentication-and-delegation.md
  - specs/004-write-ahead-log.md
affects: [internal/limits/, internal/httpgit/, internal/api/]
effort: small
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Limits and abuse controls

## Overview

Origo runs behind a consumer that owns quotas per user or organization;
Origo enforces per-repository and per-node bounds so one client cannot
exhaust a node, and exposes the knobs the consumer sets through the
authorizer.

## Design

| Limit | Default | Enforced at | Source |
|---|---|---|---|
| repository size | 50 GiB | receive-pack, LFS verify | authorizer `quota_bytes` |
| single push | 10 GiB | receive-pack | fixed |
| reference count | 100 000 | receive-pack | fixed |
| requests per subject | 600 per minute | every listener | fixed, `rate_limited` |
| concurrent git subprocesses per node | 64 | httpgit, api | `ORIGO_MAX_GIT_PROCS` |
| subprocess wall time | 10 minutes for transfer, 30 seconds for reads | httpgit, api | fixed |
| request body | 10 GiB streamed, 1 MiB for JSON | listeners | fixed |
| object storage retries | 5 with backoff, 30 seconds total | wal | fixed |

`receive.fsckObjects` rejects malformed objects; `transfer.fsckObjects`
protects reads. A repository the authorizer marks `frozen` accepts reads
and rejects writes with `forbidden` and a reason, which is how a consumer
pauses or holds a repository. Deleted repositories are unreadable at once
and purged after the hold.

## Acceptance criteria

- A push that would exceed `quota_bytes` is rejected before the entry is
  written, with the size in `details`.
- A client over the per-subject rate sees 429 with `Retry-After` and no
  other client is affected.
- A frozen repository accepts a clone and rejects a push.
