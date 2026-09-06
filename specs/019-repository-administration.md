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
updated: 2026-09-07
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

All under `/v1/repos/{id}`, action `admin` unless noted, and every one
of them asks the authorizer before it reads `meta` (spec 007,
"Authorization before lookup"). `meta` (spec 004) gains `frozen_at`,
`purged_at`, `importing_since`, `import_node`, `import_error`, and
`imported_at`; spec 014 adds `verified_at` and `verified_equal`.

The three operations spec 003 owns gain events and one answer:
`PATCH /v1/repos/{id}` changes the clone URL at once and the old URL
answers 404, never a redirect, because a redirect would let a stale URL
keep working past a transfer between owners; it emits `renamed`
whichever labels it changed, `owner` included. `DELETE /v1/repos/{id}`
emits `deleted`. `POST /v1/repos/{id}/undelete` restores within the
hold and emits `undeleted`; after the purge it answers 410 `gone`.

| Method | Path | Behaviour |
|---|---|---|
| POST | `/v1/repos/{id}/transfer` | `{"owner": "<new>"}`: the same operation as `PATCH` with `owner` alone, recorded as `transferred` instead of `renamed` so a consumer can act on a change of owner without inspecting a rename; the id never changes, which is what makes transfer cheap |
| POST | `/v1/repos/{id}/freeze` | sets `frozen_at`; writes refuse with `repo_frozen` while reads continue; `GET /v1/repos/{id}` reports `frozen_at`; a second freeze is 409 `repo_frozen`; emits `frozen` |
| POST | `/v1/repos/{id}/unfreeze` | clears `frozen_at`; 200 whether or not it was frozen; emits `unfrozen` when it was |
| POST | `/v1/repos/{id}/import` | `{"source": "<https URL>", "token": "<optional bearer for the source>"}`; 202 at once, the import running in the background on the receiving node under a 30 minute budget and the repository's `quota_bytes` as the cap; the procedure is below; only `https` sources on the egress allow-list of spec 016 (`ORIGO_EGRESS_ALLOW`, else 400 `invalid_request` with `details.reason: "egress"`), fetched with `transfer.fsckObjects` on and no credential helper; pushes answer 409 `repo_importing` while `importing_since` is set; 409 `repo_not_empty` when the newest index names any entry; a second `POST` while one runs is 409 `repo_importing`; emits `imported` when done |
| GET | `/v1/repos/{id}/import` | `{"state": "running"\|"done"\|"failed", "refs", "bytes", "started_at", "finished_at", "error"}` from `meta`: `running` while `importing_since` is set, `failed` when `import_error` is set, `done` when `imported_at` is set, and 404 `ref_not_found` with `details.ref: "import"` when none of them is; `refs` and `bytes` are the transaction length and `pack_bytes` of the import entry; action `read` |
| GET | `/v1/repos/{id}/export.bundle` | `git bundle create - --all` streamed as `application/x-git-bundle`: the complete repository in one portable file; action `read` |
| GET | `/v1/repos/{id}/stats` | `{"size_bytes", "lfs_bytes", "packs", "entries_since_compaction", "refs", "pushed_at", "compacted_at"}`; action `read` |
| POST | `/v1/repos/{id}/gc` | on the repository's compaction primary (spec 005), runs compaction now (spec 006) and answers 200 `{"before": {"packs", "entries", "size_bytes"}, "after": {…}}`; on any other node, forwards nothing and compacts nothing: it creates the request object of spec 006 and answers 202 `{"status": "scheduled", "details": {"primary": "<node name>", "within_seconds": 600}}`, and the primary's sweep compacts within 10 minutes; 429 `rate_limited` with `Retry-After` and `details.limit: "repository"`, `details.retry_after` when a `gc` ran on the repository within the last hour, counted on `origo_rate_limited_total{limit="repository"}`; emits `compacted` from the node that compacted |

| Code | Status | Message | Details |
|---|---|---|---|
| `gone` | 410 | This repository was deleted and its hold has passed. It cannot be restored. | `id`, `purged_at` |
| `repo_frozen` | 403 on a write, 409 on a second freeze | This repository is frozen and does not accept pushes. | `frozen_at` |
| `repo_importing` | 409 | This repository is importing and does not accept pushes until the import finishes. | `started_at` |
| `repo_not_empty` | 409 | This repository already has history; import into an empty repository. | `seq` |

### Import

One import is one log entry. The node clones the source with `git
clone --mirror --end-of-options <source>` into a scratch directory
under `ORIGO_DATA_DIR/spool/`, with the source token passed through the
environment and never on the command line: `GIT_CONFIG_COUNT=1`,
`GIT_CONFIG_KEY_0=http.<source>.extraheader`, and
`GIT_CONFIG_VALUE_0=Authorization: Bearer <token>`, so the token is
in no process listing and no log line. It then runs `git repack -a -d`
and `git fsck --connectivity-only`, uploads every pack under
`objects/pack/` as `packs/<hash>.pack` and `.idx` the way compaction
does (spec 006, step 4), and commits one entry through `Log.Commit` in
the shape of a compaction: kind `compact`, no pack in the entry,
`Packs` = the uploaded packs, `CompactedThrough` = the sequence before
its own (the index format of spec 004 lists entries in
`(compacted_through, seq]`, so an entry cannot fold itself), and a full
reference transaction creating every reference from zeros; `HEAD` is in
the transaction with `old` the symbolic value `index/0` holds and `new`
the source's, and is omitted when the two are equal, because a
transaction never names a reference that does not change. The index
object after it lists the packs and one entry, and any node materializes
it by step 2 of spec 004. There is no
batching and no ordering to get right: the mirror is the state, and
the history arrives as packs. The whole run is bounded by 30 minutes
and by `quota_bytes` over the pack bytes; over either, nothing is
committed, the scratch directory is removed, and `import_error` says
which.

Import state lives in `meta` so any node can answer for it: the node
reads `meta`, answers 409 `repo_importing` when `importing_since` is
set, otherwise writes `importing_since` and `import_node` and starts;
at the end it rewrites `meta` with them cleared and `imported_at` or
`import_error` set. `meta` is rewritten unconditionally (spec 004), so
two imports that start within one read-write window both run; the
second one's `Log.Commit` loses its round to the first, its callback
refuses like a compaction's (spec 006), and it ends with
`import_error: "not empty"`, so the log never holds two imports. A node that reads an `importing_since` older than 45 minutes
whose `import_node` is not in the live set of spec 005 clears both
and sets `import_error: "import node lost"`, so a node killed
mid-import leaves a repository that reports `failed` and accepts a new
import within 45 minutes; the scratch directory on the dead node is
removed by its next start-up.

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
purged after the hold. LFS objects with no `lfs/verified/<oid>` marker (spec 010)
are deleted 7 days after upload. A weekly sweep lists the bucket prefix,
reports objects no index, marker, or metadata names and older than a day
as `origo_orphan_objects`, deletes them after 7 days, and reports the
bytes under the prefix as `origo_storage_bytes`. An operator can bound
total storage as the sum of `size_bytes` and `lfs_bytes` over `stats`.

### Events

Each operation emits an event of its kind on the channel of spec 008,
with the shared fields `id`, `kind`, `repo`, `owner`, `slug`, `at`, and
`pusher` (`{"sub", "actor"}` of the caller, the same field and shape
as the `push` event, so a consumer decodes one identity for every
kind):

| Event | Extra fields |
|---|---|
| `renamed` | `from: {"owner", "slug"}`, `to: {"owner", "slug"}` |
| `transferred` | `from: {"owner"}`, `to: {"owner"}` |
| `frozen`, `unfrozen` | none |
| `deleted` | `purge_after` |
| `undeleted` | none |
| `imported` | `source` (the host and path, never the token), `refs`, `bytes` |
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
- An import of a fixture of 5 000 commits built by `internal/gittest`
  and served by the stub source (`internal/gittest.ServeHTTP`, `git
  http-backend` behind an `httptest` server that requires the bearer)
  completes within the budget as one `compact` entry, `import` reports
  `done` with the reference count, the bearer appears in no process
  argument list and no log line, and a clone from Origo has the same
  `rev-list --all` as a clone of the source (proposed: `test/e2e`,
  `TestImportFixture`).
- A node killed during an import leaves `importing_since` set; another
  node reports `running` for 45 minutes with a fake clock, then
  `failed` with `import node lost`, and accepts a new import
  (proposed: `internal/api`, `TestImportLeaseExpires`).
- A `gc` on a node that is not the primary answers 202 naming the
  primary and compacts nothing there, and a second `gc` on the primary
  within an hour is 429 with `details.limit: "repository"` (proposed:
  `internal/api`, `TestGcRoutesToThePrimary`).
- `PATCH` with `owner` emits `renamed` and `transfer` emits
  `transferred`, both with `pusher` (proposed: `internal/events`,
  `TestRenameAndTransferEvents`).
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
