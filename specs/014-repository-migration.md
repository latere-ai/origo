---
title: "Migration of existing repositories from a prior host (cross-repo)"
status: vague
track: infra
depends_on:
  - specs/013-conformance-suite.md
  - specs/019-repository-administration.md
affects: [docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Migration of existing repositories

## Overview

Placeholder for the cross-repo plan. An operator adopting Origo has
repositories on a prior host: at Latere, a data plane product that holds
each repository as a workspace with a `.git` directory in its file plane,
served over smart HTTP with a single-writer lock shared with sandbox
mounts. After Origo ships, such a repository becomes a pointer to an
Origo repository; the prior host keeps ownership, sharing, quota
accounting, and any mounting by clone and checkout; its existing clone
URLs proxy or redirect to Origo so no remote breaks; its lock becomes
Origo's linearized reference transaction; and each repository is
migrated once through `POST /v1/repos/{id}/import` of spec 019, verified
by comparing `git rev-list --all` on both sides.

## Design

To be written with the prior host's own spec, which owns its workspace
and mount semantics. Fixed here: repository ids are minted by the prior
host and stored on its own record; its authorizer endpoint (spec 007)
answers for its repositories; migration is per repository, resumable
through the import status of spec 019, and the old copy is kept
read-only for 30 days.

## Not in this spec

Anything about the prior host's data model. A migration tool inside
Origo beyond the import of spec 019.

## Acceptance criteria

- Every migrated repository has identical `git rev-list --all` on both
  sides (the import criterion of spec 019).
- No clone URL in use before the migration stops working after it.
- The prior host's mounts of a migrated repository keep working during
  and after the migration.
