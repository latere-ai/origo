---
title: "Repository administration: rename, transfer, freeze, delete, undelete, import, export, and garbage collection"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/004-write-ahead-log.md
  - specs/006-compaction.md
  - specs/007-authentication-and-delegation.md
  - specs/008-push-events.md
affects: [internal/api/, internal/wal/, internal/repo/, internal/events/, docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Repository administration

## Overview

Beyond clone, fetch, and push, a consumer needs the operations that make
a repository something it can manage over years: change its labels, move
it to another owner, freeze it, delete it and change its mind, bring an
existing repository in with its history, take a full copy out, and trust
that storage does not grow without bound. This spec fixes those
operations on the JSON API.

## Current state

`internal/api` serves create, get, rename (`PATCH`), delete, and
undelete (spec 003). The sweeper's purge (`internal/wal`) removes every
object under the repository prefix including `meta`, so an undeleted-too-late
repository answers 404. Nothing else below exists.

## Design

All under `/v1/repos/{id}`, action `admin` unless noted. `meta` (spec
004) gains `frozen_at`, `importing`, and `purged_at`.

The three operations spec 003 owns gain events and one answer:
`PATCH /v1/repos/{id}` changes the clone URL at once and the old URL
answers 404, never a redirect, because a redirect would let a stale URL
keep working past a transfer between owners; it emits `renamed`.
`DELETE /v1/repos/{id}` emits `deleted`. `POST /v1/repos/{id}/undelete`
restores within the hold and emits `undeleted`; after the purge it
answers 410 `gone`.

| Method | Path | Behaviour |
|---|---|---|
| POST | `/v1/repos/{id}/transfer` | `{"owner": "<new>"}`: a rename of the owner label recorded as `transferred` so a consumer can act on it; the id never changes, which is what makes transfer cheap |
| POST | `/v1/repos/{id}/freeze` | sets `frozen_at`; writes refuse with `repo_frozen` while reads continue; `GET /v1/repos/{id}` reports `frozen_at`; a second freeze is 409 `repo_frozen`; emits `frozen` |
| POST | `/v1/repos/{id}/unfreeze` | clears `frozen_at`; 200 whether or not it was frozen; emits `unfrozen` when it was |
| POST | `/v1/repos/{id}/import` | `{"source": "<https URL>", "token": "<optional bearer for the source>"}`; 202; the node runs `git clone --mirror` from the source into the local copy and commits it as `push` entries in batches of at most 256 MiB of pack, with a 30 minute budget and the repository's `quota_bytes` as the cap; `meta.importing` is set for the duration and pushes answer 409 `repo_importing`; only `https` sources, fetched with `transfer.fsckObjects` on, no credential helper, and the token as basic auth; 409 `repo_not_empty` when the newest index names any entry; emits `imported` when done |
| GET | `/v1/repos/{id}/import` | `{"state": "running"\|"done"\|"failed", "refs", "bytes", "started_at", "finished_at", "error"}`; action `read` |
| GET | `/v1/repos/{id}/export.bundle` | `git bundle create - --all` streamed as `application/x-git-bundle`: the complete repository in one portable file; action `read` |
| GET | `/v1/repos/{id}/stats` | `{"size_bytes", "lfs_bytes", "packs", "entries_since_compaction", "refs", "pushed_at", "compacted_at"}`; action `read` |
| POST | `/v1/repos/{id}/gc` | runs compaction now (spec 006) on the receiving node and answers 200 `{"before": {"packs", "entries", "size_bytes"}, "after": {…}}`; 429 `rate_limited` with `Retry-After` when run within the last hour on the repository; emits `compacted` |

| Code | Status | Message | Details |
|---|---|---|---|
| `gone` | 410 | This repository was deleted and its hold has passed. It cannot be restored. | `id`, `purged_at` |
| `repo_frozen` | 403 on a write, 409 on a second freeze | This repository is frozen and does not accept pushes. | `frozen_at` |
| `repo_importing` | 409 | This repository is importing and does not accept pushes until the import finishes. | `started_at` |
| `repo_not_empty` | 409 | This repository already has history; import into an empty repository. | `seq` |

### Purge and the tombstone

The purge of spec 004 changes: it deletes every object under the prefix
except `meta`, which is rewritten with `purged_at`, and deletes the name
so it can be reused. `meta` is the tombstone that makes `gone` possible
and keeps the id unique forever (spec 003): `POST /v1/repos` with a
purged id is 409 `repo_exists`.

### Garbage collection guarantees

Storage per repository is bounded by compaction: after any compaction
the log holds the packs plus at most 64 entries, and unreachable objects
are dropped by `git repack` on the primary. Deleted repositories are
purged after the hold. LFS objects with no `verified` marker (spec 010)
are deleted 7 days after upload. A weekly sweep lists the bucket prefix,
reports objects no index, marker, or metadata names and older than a day
as `origo_orphan_objects`, deletes them after 7 days, and reports the
bytes under the prefix as `origo_storage_bytes`. An operator can bound
total storage as the sum of `size_bytes` and `lfs_bytes` over `stats`.

### Events

Each operation emits an event of its kind on the channel of spec 008,
with the shared fields `id`, `kind`, `repo`, `owner`, `slug`, `at`, and
`actor` (`{"sub", "actor"}` of the caller):

| Event | Extra fields |
|---|---|
| `renamed` | `from: {"owner", "slug"}`, `to: {"owner", "slug"}` |
| `transferred` | `from: {"owner"}`, `to: {"owner"}` |
| `frozen`, `unfrozen` | none |
| `deleted` | `purge_after` |
| `undeleted` | none |
| `imported` | `source`, `refs`, `bytes` |
| `compacted` | `before`, `after` as in the `gc` response |

## Not in this spec

Import from a non-`https` source. Export of LFS objects. A sweep faster
than weekly.

## Acceptance criteria

- Each operation has a conformance case in `TestContract` (spec 013)
  covering the success path, the 403 on a caller without `admin`, and
  the state conflict: freeze twice (409 `repo_frozen`), undelete after
  the purge (410 `gone`), import into a repository with an entry (409
  `repo_not_empty`), a push during an import (409 `repo_importing`)
  (proposed: `internal/api`, `TestAdministrationOperations`).
- An import of a public fixture repository with 5 000 commits completes
  within the budget, `import` reports `done`, and a clone from Origo has
  the same `rev-list --all` as a clone of the source (proposed:
  `test/e2e`, `TestImportFixture`).
- An export re-imported into a fresh repository has identical
  `rev-list --all` (proposed: `internal/api`, `TestExportRoundTrip`).
- After 500 pushes and a `gc`, `stats.size_bytes` is within 10% of the
  pack size of a fresh `git clone --mirror`, and the weekly sweep run
  once reports `origo_orphan_objects` 0 (proposed: `test/e2e`,
  `TestGcBoundsStorage`).
- A purged repository answers 410 `gone` on every endpoint, its id is
  refused by `POST /v1/repos` with 409, and its name is accepted
  (proposed: `internal/wal`, `TestPurgeLeavesATombstone`).
- Every operation delivers one event of its kind with the listed fields
  (proposed: `internal/events`, `TestAdministrationEvents`).
