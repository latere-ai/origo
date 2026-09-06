---
title: "Migration of Drive's hosted repositories (cross-repo)"
status: vague
track: infra
depends_on:
  - specs/013-conformance-suite.md
affects: [docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Migration of Drive's hosted repositories

## Overview

Placeholder for the cross-repo plan. Latere's data plane product hosts
repositories today as workspaces holding a `.git` directory in its file
plane, served over smart HTTP with a single-writer lock shared with
sandbox mounts. After Origo ships, a repository-kind workspace becomes a
pointer to an Origo repository; the product keeps ownership, sharing,
quota accounting, and mounting by clone and checkout; its existing
`/git/...` URLs proxy or redirect to Origo so no remote breaks; the lock
becomes Origo's linearized reference transaction; and each existing
repository is migrated once by pushing its objects and refs into the log,
verified by comparing `git rev-list --all` on both sides.

## Design

To be written with the product's own spec, which owns the workspace and
mount semantics. Fixed here: repository ids are minted by the product and
stored on the workspace row; the product's authorizer endpoint answers for
its repositories; migration is per repository, resumable, and the old
copy is kept read-only for 30 days.

## Acceptance criteria

- Every migrated repository has identical `rev-list --all` on both sides.
- No clone URL in use before the migration stops working after it.
- The product's sandbox mounts of a repository-kind workspace keep
  working during and after the migration.
