---
title: "Server-side git operations: commits, merges, cherry-picks, and reverts without a clone"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
  - specs/008-push-events.md
  - specs/009-read-api-and-archive.md
affects: [internal/api/, internal/repo/, internal/httpgit/]
effort: large
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Server-side git operations

## Overview

A platform that changes many repositories at once, a migration tool that
rewrites a manifest across every project, a bot that bumps a dependency,
a review flow that merges on approval, should not have to clone each
repository to make one commit. This spec adds operations that create a
commit on the server from a description of the change: write and delete
files on a branch, merge one branch into another, cherry-pick, and
revert. Each is a sequence of git plumbing on the node's warm copy that
produces a pack and a reference transaction, which is exactly what a push
produces, so each commits through `Log.Commit` of spec 004 as a `push`
entry and looks like a push everywhere else: in the index, in events, in
history.

The consumer that needs this first is Latere's hosting product, whose
spec 026 states that a later tool walks every project's history and
applies a change by pushing a branch; this spec is what makes that a
request rather than a clone.

## Current state

Spec 009 serves refs, commits, trees, and blobs. Spec 004's receive path
turns a pack and a transaction into an entry. Nothing creates a commit
without a client.

## Design

### Common shape

Every operation is a `POST` under `/v1/repos/{id}` with action `write`,
authorized like a push (spec 007), that names the branch it writes and
the commit it expects to find there, so a stale caller is refused rather
than clobbering:

| Field | Meaning |
|---|---|
| `branch` | `refs/heads/<name>` or a short name; must exist except for `commits` with `create_branch: true` |
| `expected_head` | the commit the caller believes the branch points at; `null` for a new branch; mismatch is 409 `non_fast_forward` with `details.head` |
| `author` | `{"name", "email"}`; the committer is always Origo with the effective subject's identity in the message trailer `Origo-Subject:` and the actor in `Origo-Actor:` (spec 007) |
| `message` | the commit message, 1 to 64 KiB |
| `dry_run` | `true` computes the result and returns it without committing |

Response 201 `{"commit": "<sha>", "branch", "entry_seq", "tree": "<sha>"}`;
for `dry_run`, 200 with the same fields and `"committed": false`. One
operation is one entry, one commit, one event (`push` with one update,
`Origo-Operation: <name>` on the event). No operation runs user code:
no hooks, no filters, no smudge, no submodule fetch.

### Operations

| Operation | Path | Body beyond the common fields | Result |
|---|---|---|---|
| write files | `commits` | `changes: [{"path", "content" (base64, at most 10 MiB per file) \| "content_ref" (a blob sha already in the repository) \| "delete": true, "mode": "100644"\|"100755"\|"120000"}]`, 1 to 1 000 changes, paths validated as git does (`git check-ref-format --allow-onelevel` semantics for components, no `.git`, no `..`) | one commit with `expected_head` as parent |
| merge | `merge` | `source: <branch or sha>`, `strategy: "fast_forward_only"\|"merge_commit"\|"fast_forward_if_possible"` (default), `message` optional for a merge commit | fast-forward moves the branch with no new commit and answers the source's sha; a merge commit has two parents; a conflict is 409 `merge_conflict` with `details.paths` |
| cherry-pick | `cherry-pick` | `commits: [<sha>]`, 1 to 100, applied in order, `mainline` for a merge commit | one commit per picked commit, all in one entry and one transaction, so partial application never lands; a conflict is 409 `merge_conflict` naming the commit and paths |
| revert | `revert` | `commits: [<sha>]`, 1 to 100, `mainline` | one revert commit per input, same atomicity and conflict rule |

### Mechanics

The node runs the operation in a temporary index and worktree-free
sequence on the warm copy: `git read-tree` of `expected_head`,
`git update-index --cacheinfo` or `hash-object -w` per change,
`git write-tree`, `git commit-tree` with the author and the trailers;
`git merge-tree --write-tree` for merges, cherry-picks, and reverts,
which produces a tree without a worktree and reports conflicts as data.
The new objects are packed with `git pack-objects --revs` from the old
head to the new, and the pack plus the single reference update go through
the receive path of spec 004 as if a client had pushed them, so every
check a push gets (fsck, quota, size) applies. The warm copy is acquired
for writing for the duration, the same lock compaction takes (spec 006),
bounded by the 30 second budget of spec 009 for `commits` and 5 minutes
for the merge family; over budget is 504 `operation_timeout` and nothing
is committed.

Concurrent operations on one branch serialize on `expected_head`: the
second sees a mismatch and retries after reading the branch. Operations
on different branches of one repository serialize on the repository's
commit like any two pushes.

### Errors

`non_fast_forward` (spec 003) is answered with `details.head` and
`details.expected_head` when the branch moved since the caller read it.
Codes this spec defines:

| Code | Status | Message | Details |
|---|---|---|---|
| `merge_conflict` | 409 | The change conflicts with the branch. Resolve it in a clone and push. | `commit`, `paths` |
| `invalid_change` | 400 | A change in the request is not valid. | `index`, `reason` (`path`, `mode`, `content`, `too_many`, `too_large`) |
| `operation_timeout` | 504 | The operation took too long and nothing was changed. | `operation`, `budget_seconds` |

`ref_not_found`, `over_quota`, `forbidden`, and `repo_frozen` apply as
elsewhere.

### Limits

| Limit | Value |
|---|---|
| changes per `commits` request | 1 000 |
| content per file | 10 MiB inline; larger content is uploaded through LFS (spec 010) and referenced by pointer, or pushed |
| request body | 64 MiB |
| commits per cherry-pick or revert | 100 |
| operations per repository per minute | 60, then `rate_limited` |

## Not in this spec

Rebase. Squash merges (a merge commit or a `commits` request built by
the caller from a diff). Resolving conflicts on the server. Creating
tags and other lightweight references from a request, a small later
addition with its own spec. Server-side hooks or policy on the content of a commit;
the authorizer decides who may write, and content policy is the
consumer's.

## Acceptance criteria

- `commits` with three changes (add, modify with `content_ref`, delete)
  on a fixture branch produces one commit whose tree equals a client-side
  commit of the same changes, one entry, and one `push` event with
  `Origo-Operation: commits`; a second request with the stale
  `expected_head` is 409 `non_fast_forward` (proposed: `internal/api`,
  `TestCommitsWritesOneEntryAndRefusesStaleHead`).
- `merge` with `fast_forward_if_possible` fast-forwards when it can and
  creates a two-parent commit when it cannot; a conflicting merge is 409
  `merge_conflict` naming the paths and leaves the branch unchanged
  (proposed: `internal/api`, `TestMergeStrategiesAndConflict`).
- Cherry-picking three commits of which the second conflicts commits
  nothing and names the second commit (proposed: `internal/api`,
  `TestCherryPickIsAtomic`).
- A `commits` request whose pack would exceed `quota_bytes` is
  `over_quota` and commits nothing; a request exceeding the change or
  size limits is `invalid_change` with the index (proposed:
  `internal/api`, `TestServerSideOperationsHonourLimits`).
- Twenty concurrent `commits` requests on one branch with the same
  `expected_head` produce exactly one commit and nineteen
  `non_fast_forward` answers (proposed: `internal/api`,
  `TestConcurrentCommitsSerializeOnExpectedHead`).
- Fuzzing the change path validator and the operation bodies finds no
  panic and never reaches a subprocess with a path git would reject
  (proposed: `internal/api`, `FuzzChangePath`, `FuzzOperationBody`).
- The conformance suite (spec 013) gains one case per operation.
