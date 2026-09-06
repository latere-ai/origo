---
title: "Server-side git operations: merge, cherry-pick, revert, and file writes without a clone"
status: vague
track: infra
depends_on:
  - specs/009-read-api-and-archive.md
  - specs/004-write-ahead-log.md
affects: [internal/api/, internal/repo/]
effort: large
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Server-side git operations

## Overview

Placeholder, not scheduled. A platform that automates changes across many
repositories (the migration tooling Latere's hosting product plans, a
review flow, a bot fixing a dependency) wants to create a commit without
cloning: write files at a path on a branch, merge one branch into another
with a fast-forward or a merge commit, cherry-pick, revert. Every one of
these is a `git` plumbing sequence on the node's warm copy that produces
a pack and a reference transaction, which is exactly what a push produces,
so they commit through the same log path and appear as pushes with the
service as actor and the requesting subject in `act`. Fixed now so the
design space stays open: no operation runs user code; conflicts are
reported, never resolved automatically; every operation is one entry;
and the API shape is `POST /v1/repos/{id}/commits` with a body naming the
branch, the expected parent (so a stale caller gets `non_fast_forward`
rather than clobbering), and the changes.

## Design

To be written when a consumer needs it.

## Acceptance criteria

None until drafted.
