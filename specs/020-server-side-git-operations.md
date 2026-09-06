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
| `expected_head` | the commit the caller believes the branch points at; `null` for a new branch; mismatch is 409 `non_fast_forward` with the details of spec 003: `ref`, `expected`, `actual` |
| `author` | required: `{"name", "email"}`, both non-empty, `email` with one `@`; a missing or empty field is 400 `invalid_request` with `details.field: "author"`; the committer is always `Origo <origo@<host of ORIGO_PUBLIC_URL>>` with the effective subject's identity in the message trailer `Origo-Subject:` and the actor in `Origo-Actor:` (spec 007) |
| `message` | the commit message, 1 to 64 KiB |
| `dry_run` | `true` computes the result and returns it without committing |

Response 201 `{"commit": "<sha>", "branch", "entry_seq", "tree": "<sha>"}`;
for `dry_run`, 200 with the same fields and `"committed": false`. One
operation is one entry, one commit, one event: a `push` with one
update whose `operation` field (spec 008) is the operation's name,
carried in the entry header as the push option `origo.operation=<name>`
so the repair sweep of spec 008 can rebuild the event. No operation
runs user code: no hooks, no filters, no smudge, no submodule fetch.

### Operations

| Operation | Path | Body beyond the common fields | Result |
|---|---|---|---|
| write files | `commits` | `changes: [{"path", "content" (base64, at most 10 MiB decoded per file) \| "content_ref" (a blob sha already in the repository) \| "delete": true, "mode": "100644"\|"100755"\|"120000"}]`, 1 to 1 000 changes in a body of at most 64 MiB, each `path` checked by the rules below | one commit with `expected_head` as parent |
| merge | `merge` | `source: <branch or sha>`, `strategy: "fast_forward_only"\|"merge_commit"\|"fast_forward_if_possible"` (default), `message` optional for a merge commit | fast-forward moves the branch with no new commit and answers the source's sha; a merge commit has two parents; a conflict is 409 `merge_conflict` with `details.paths` |
| cherry-pick | `cherry-pick` | `commits: [<sha>]`, 1 to 100, applied in order, `mainline` for a merge commit | one commit per picked commit, all in one entry and one transaction, so partial application never lands; a conflict is 409 `merge_conflict` naming the commit and paths |
| revert | `revert` | `commits: [<sha>]`, 1 to 100, `mainline` | one revert commit per input, same atomicity and conflict rule |

### Paths

A change's `path` is accepted when every rule holds, and otherwise
the request is 400 `invalid_change` with `details.index` and
`details.reason: "path"`: at most 4 096 bytes; no leading slash; no
empty component (no `//`, no trailing slash); no component `.` or
`..`; no NUL byte; no component that names the git directory,
case-insensitively, in any of the forms git's `core.protectNTFS` and
`core.protectHFS` refuse (`.git`, `.git` followed by dots or spaces,
`git~1` and the other 8.3 short names, and the HFS forms with ignorable
code points); and a `120000` change's content is a relative target
under the same rules. The rules are one function, `ValidChangePath`,
which `FuzzChangePath` covers and spec 009's `?path=` also uses.

### Mechanics

The node runs the operation on the warm copy, acquired for writing for
the duration (the write lock of spec 004, which a push also takes), in
a temporary index and without a worktree: `git read-tree` of
`expected_head` into the temporary index, `git update-index
--cacheinfo` or `git hash-object -w` per change, `git write-tree`,
`git commit-tree` with the author, the committer, and the trailers;
`git merge-tree --write-tree` for merges, cherry-picks, and reverts,
which produces a tree without a worktree and reports conflicts as
data. Every argument that came from the request follows
`--end-of-options` and a value starting with `-` is refused (spec 009).
The new objects are written into the copy's object store by those
commands; they are unreachable until the reference moves, so a failure
leaves nothing a reader can see.

Then, without a synthesized `receive-pack`: `git pack-objects --revs
--end-of-options` over `<expected_head>..<new>` writes the entry's
pack; `git index-pack --strict` over that pack and `git fsck
--connectivity-only --no-progress <new>` check the new objects the way
`receive.fsckObjects` would; the pack's size is checked against
`quota_bytes` and the push size limit (spec 012); and `Log.Commit` of
spec 004 commits the pack with the one-update transaction, the
subject and actor, and `push_options: ["origo.operation=<name>"]`,
after which `Cache.Advance` records the sequence and the branch is
moved with `git update-ref`. `commits` is bounded by the 30 second
budget of spec 009 and the merge family by 5 minutes; over budget is
504 `operation_timeout`, nothing is committed, and the loose objects
stay unreachable until the next compaction removes them.

Concurrent operations on one branch serialize on `expected_head`: the
second sees a mismatch and retries after reading the branch. Operations
on different branches of one repository serialize on the repository's
commit like any two pushes.

### Errors

`non_fast_forward` (spec 003) is answered with `details.ref`,
`details.expected` (the caller's `expected_head`), and `details.actual`
(the branch's current head) when the branch moved since the caller read
it.
Codes this spec defines:

| Code | Status | Message | Details |
|---|---|---|---|
| `merge_conflict` | 409 | The change conflicts with the branch. Resolve it in a clone and push. | `commit`, `paths` |
| `invalid_change` | 400 | A change in the request is not valid. | `index`, `reason` (`path`, `mode`, `content`, `too_many`, `too_large`) |
| `operation_timeout` | 504 | The operation took too long and nothing was changed. | `operation`, `budget_seconds` |

`ref_not_found`, `over_quota`, `forbidden`, and `repo_frozen` apply as
elsewhere.

### Limits

The `commits` operation has two size limits that both hold: at most 10
MiB of decoded content per file, and at most 64 MiB for the whole
request body, so a request of six 10 MiB files is refused by the body
limit and one 11 MiB file by the file limit.

| Limit | Value | Answer |
|---|---|---|
| changes per `commits` request | 1 000 | 400 `invalid_change`, `details.reason: "too_many"` |
| content per file | 10 MiB decoded, inline; larger content is uploaded through LFS (spec 010) and referenced by pointer, or pushed | 400 `invalid_change`, `details.reason: "too_large"`, `details.index` |
| request body | 64 MiB, every operation; spec 012's 64 KiB JSON limit does not apply to these routes | 400 `invalid_request` |
| commits per cherry-pick or revert | 100 | 400 `invalid_change`, `details.reason: "too_many"` |
| operations per repository per minute | 60, a token bucket per repository per node like spec 012's per-subject one | 429 `rate_limited` with `Retry-After`, `details.limit: "repository"`, `details.retry_after`, counted on `origo_rate_limited_total{limit="repository"}` |

`git merge-tree --write-tree` with conflict output as data needs git
2.40, which is why the `git` line of `origod check` (spec 018) requires
2.40.

## Not in this spec

Rebase. Squash merges (a merge commit or a `commits` request built by
the caller from a diff). Resolving conflicts on the server. Creating
tags and other lightweight references from a request, a small later
addition with its own spec. Server-side hooks or policy on the content of a commit;
the authorizer decides who may write, and content policy is the
consumer's.

## Acceptance criteria

- `commits` with three changes (add, modify with `content_ref`, delete)
  on a fixture branch produces one commit whose tree equals a
  client-side commit of the same changes, with the committer `Origo
  <origo@<host>>` and the author from the request, one entry whose
  header carries `origo.operation=commits`, and one `push` event with
  `operation: "commits"`; a second request with the stale
  `expected_head` is 409 `non_fast_forward`, and a request without
  `author` is 400 (proposed: `internal/api`,
  `TestCommitsWritesOneEntryAndRefusesStaleHead`).
- `merge` with `fast_forward_if_possible` fast-forwards when it can and
  creates a two-parent commit when it cannot; a conflicting merge is 409
  `merge_conflict` naming the paths and leaves the branch unchanged
  (proposed: `internal/api`, `TestMergeStrategiesAndConflict`).
- Cherry-picking three commits of which the second conflicts commits
  nothing and names the second commit (proposed: `internal/api`,
  `TestCherryPickIsAtomic`).
- A `commits` request whose pack would exceed `quota_bytes` is
  `over_quota` and commits nothing; a request with 1 001 changes, one
  with an 11 MiB file, and one with six 10 MiB files are refused with
  the documented code and reason; the 61st operation on a repository
  in one minute is 429 with `details.limit: "repository"` (proposed:
  `internal/api`, `TestServerSideOperationsHonourLimits`).
- Every path in a table of accepted and refused paths (`a/b`, `.git/x`,
  `a/.GIT/x`, `git~1/x`, `a//b`, `/a`, `a/../b`, a 4 097 byte path, a
  path with a NUL) is classified as the rules say and no refused path
  reaches a subprocess (proposed: `internal/api`, `TestChangePathRules`).
- Twenty concurrent `commits` requests on one branch with the same
  `expected_head` produce exactly one commit and nineteen
  `non_fast_forward` answers (proposed: `internal/api`,
  `TestConcurrentCommitsSerializeOnExpectedHead`).
- `FuzzChangePath` and `FuzzOperationBody` in `internal/api` find no
  panic and no path the validator accepts that `git update-index`
  refuses: each runs as a seed-corpus test in the suite on every push
  and for 40 seconds under `make fuzz` (spec 002) on the weekly
  schedule (proposed: `internal/api`, `FuzzChangePath`,
  `FuzzOperationBody`).
- The conformance suite (spec 013) gains one case per operation.
