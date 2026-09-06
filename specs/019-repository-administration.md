---
title: "Repository administration: rename, transfer, freeze, delete, undelete, import, export, and garbage collection"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/004-write-ahead-log.md
  - specs/006-compaction.md
  - specs/007-authentication-and-delegation.md
affects: [internal/api/, internal/wal/, internal/repo/, docs/]
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

Spec 003 has create, get, patch, delete, and undelete. Nothing else.

## Design

All under `/v1/repos/{id}`, action `admin` unless noted.

| Method | Path | Behaviour |
|---|---|---|
| PATCH | `` | `owner` and `slug` change the clone URL at once; the old URL answers 404, never a redirect, because a redirect would let a stale URL keep working past a transfer between owners |
| POST | `transfer` | `{"owner": "<new>"}`: the same as a rename of the owner label, recorded as a separate event kind so a consumer can act on it; the id never changes, which is what makes transfer cheap |
| POST | `freeze`, `unfreeze` | writes refused with `forbidden` and reason `frozen` while reads continue; the state lives in the repository's metadata object and is reported by `GET` |
| DELETE | `` | marks deleted; every endpoint answers 404 except `undelete`; objects purged after the 7 day hold (spec 004) |
| POST | `undelete` | within the hold, restores; after it, 410 `gone` |
| POST | `import` | `{"source": "<https url>", "token": "<optional>"}`: the node runs `git clone --mirror` from the source into a fresh log, entry by entry in pack-sized batches, with a 30 minute budget and a 50 GiB cap; progress at `GET import`; the repository is `importing` and refuses pushes until done; the source is fetched with fsck on and no credential helper; only `https` sources |
| GET | `export.tar.zst` | a mirror bundle: `git bundle create` of every ref, streamed; the complete repository in one portable file; action `read` |
| GET | `stats` | size on the log, pack count, entry count since compaction, ref count, last push, last compaction; action `read` |
| POST | `gc` | runs compaction now (spec 006) and reports the sizes before and after; rate limited to once per hour per repository |

### Garbage collection guarantees

Storage per repository is bounded by compaction: after any compaction the
log holds the packs plus at most 64 entries, and unreferenced objects are
dropped by `git repack` on the primary. Deleted repositories are purged
after the hold. LFS objects not referenced by any manifest are purged 7
days after their last reference. A weekly sweep lists the bucket prefix
and reports objects no index names older than a day as
`origo_orphan_objects` and deletes them after 7 days. An operator can
therefore bound total storage as the sum of repository sizes reported by
`stats`, and `origo_storage_bytes_total` says what the bucket holds.

### Events

Each operation emits an event of its kind on the push event channel
(spec 008): `renamed`, `transferred`, `frozen`, `unfrozen`, `deleted`,
`undeleted`, `imported`, `compacted`, so a consumer keeps its own records
current without polling.

## Acceptance criteria

- Each operation has a conformance test in spec 013's suite covering the
  success path, the authorization failure, and the state conflict (freeze
  twice, undelete after the hold, import into a non-empty repository).
- An import of a public fixture repository with 5 000 commits and LFS
  objects completes within the budget and clones identically from Origo.
- An export re-imported into a fresh repository has identical
  `rev-list --all`.
- After 500 pushes and a `gc`, the repository's `stats` size is within
  10% of a fresh mirror clone's pack size, and the weekly sweep reports
  zero orphans.
