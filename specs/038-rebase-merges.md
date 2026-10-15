---
title: "Rebase merges: the merge endpoint replays a branch's commits onto the target's tip"
status: drafted
track: infra
depends_on:
  - specs/008-push-events.md
  - specs/012-limits-and-abuse.md
  - specs/018-installation.md
  - specs/020-server-side-git-operations.md
  - specs/021-conformance-suite.md
  - specs/025-agent-client.md
affects: [internal/api/, internal/limits/, internal/origoclient/, internal/origocli/, docs/api.md, docs/cli.md, api/openapi.yaml, test/conformance/, specs/012-limits-and-abuse.md, specs/020-server-side-git-operations.md, specs/021-conformance-suite.md, specs/025-agent-client.md, specs/README.md]
effort: medium
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Rebase merges

## Overview

`POST /v1/repos/{id}/merge` (spec 020) lands a branch in one of two
shapes: a fast-forward, or a two-parent merge commit. When many agents
land work on one branch, every landing that cannot fast-forward adds a
merge commit, so a hundred landings a day put a hundred merge commits
into the history, and `git log --first-parent` is the only linear view
left. A rebase lands the same work as a straight line: each commit of
the branch is replayed onto the target's tip, keeping its author and
message, and the target moves forward to the last one.

The other cost at volume is the round trip. Every operation names
`expected_head`, and a landing that loses the race to another answers
409 `non_fast_forward`, after which the agent reads the head and asks
again. A merge commit has to name the head it merges into, but a replay
is recomputed from whatever the tip is, so for a rebase the node can
absorb the race instead of the caller.

This spec adds `strategy: "rebase"` to the merge endpoint: the range,
the replay, what happens to merge commits, empty commits and signatures
in the range, the identity each replayed commit carries, the answer, and
an optional `expected_head` that lets the node retry a lost race itself.

## Current state

Not built. `opMerge` in `internal/api/operations.go` accepts the three
strategies `StrategyFastForwardOnly`, `StrategyMergeCommit` and
`StrategyFastForwardIfPossible`, and refuses a null `expected_head`.
`operation.run` in `internal/api/operations_git.go` takes the write
lease, refuses a branch whose head is not `expected_head` with 409
`non_fast_forward`, and maps a `wal.ConflictError` from `Log.Commit` (a
writer on another node moved the branch first) to the same 409.

The replay this spec needs already exists as the cherry-pick:
`operation.pick` applies each commit with `operation.mergeTree`, which
re-parents both sides onto the picked commit's parent with
`operation.reparent` and runs `git merge-tree --write-tree -z
--name-only`, so the three-way base is the commit's parent and a
conflict comes back as data, answered 409 `merge_conflict` with
`details.commit` and `details.paths`. The form needs git 2.40, the floor
`gitFloor` in `cmd/origod/check.go` enforces; the runtime image carries
2.47 (`Dockerfile`, Debian trixie). `operation.commitTree` writes every
commit with the request's `author`, the committer `Origo <origo@<host>>`
from `Handler.committer`, and `operation.message`, which adds the
trailer `Origo-Subject:` with the request's subject. When the message
ends in a trailer block (`trailerBlock`), the trailer joins that block,
so git still parses the message's own trailers, and a block that
already names the same subject gets no second line.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| `git replay --onto` | one subprocess replays the range in the bare copy with no worktree and no index, which works on both versions checked | on 2.54.0 and on 2.47.3: a conflict exits 1 with nothing on stdout or stderr, so `merge_conflict` could name neither the commit nor the paths; a merge commit in the range is `fatal: replaying merge commits is not supported yet!`; filtering merges with `--no-merges` silently drops the merged side's changes (two files vanished from the replayed tip); a commit whose change the target already holds is kept on 2.47.3 and dropped on 2.54.0; an empty range exits 1 on 2.47.3, the conflict status, and 0 on 2.54.0; `--ref-action` does not exist on 2.47.3 and its default changed to updating the references by 2.54.0; the manual calls the command experimental. The 2.40 floor would also have to rise to a release that has the command |
| `git rebase` in a worktree | the porcelain, with its own conflict handling | a checkout per operation, and the porcelain runs hooks; spec 020 runs no user code and keeps no worktree |
| Linearize merge commits in the range | replay the non-merge commits and drop the merges | a conflict resolution, or any change made in a merge commit itself, exists only in that commit, so dropping it changes the result silently |
| Always rewrite, even when a fast-forward is possible | new ids for every landed commit | the ids an agent recorded stop matching, which breaks the containment question spec 037 answers by id |
| **The pick loop over the range, chosen** | `operation.pick` with each commit's own parent as base, the original author kept, empty commits classified, merges refused | runs on the 2.40 floor, reports a conflict with its commit and paths, and reuses code the conformance suite already drives |

## Design

### The request

The merge body of spec 020 with `strategy: "rebase"`:

| Field | Rule under `rebase` |
|---|---|
| `branch` | the target; must exist, else 404 `ref_not_found` |
| `source` | a branch name or a commit id, resolved with `^{commit}` under the write lease; a caller that read the source passes its id |
| `expected_head` | the target's head as today, or `null`: replay onto whatever the target holds when the operation runs (below) |
| `author` | must be absent: every replayed commit keeps its own author, so a value would change nothing. Present is 400 `invalid_request` with `details.field: "author"` |
| `message` | must be absent, for the same reason: each commit keeps its message. Present is 400 with `details.field: "message"` |
| `dry_run` | as for every operation |

### The range and the replay

Under the write lease, with `H` the target's head and `S` the source:

1. When `H` is a proper ancestor of `S` (`git merge-base --is-ancestor`,
   `operation.ancestor`), the target fast-forwards to `S` exactly as
   `fast_forward_if_possible` does: no new commit, no pack, every id
   kept.
2. Otherwise the range is `git rev-list --reverse --topo-order --parents
   --end-of-options ^H S`, oldest first: the commits `S` reaches and `H`
   does not.
3. A commit with more than one parent anywhere in the range refuses the
   whole operation before any replay: 400 `invalid_request` with
   `details.field: "strategy"`, `details.commit` the oldest such commit,
   and a reason naming `merge_commit` as the strategy that lands it.
   Nothing the caller can wait for changes the history, which is the
   rule spec 020 used for a directory-file conflict.
4. A range longer than `api.MaxPickCommits` is 400 `invalid_change` with
   `details.reason: "too_many"` and `details.index` the bound, the
   answer a cherry-pick of too many commits gets: a rebase is that loop
   over a range.
5. Each commit `C` with parent `P` is replayed onto the current tip `T`
   (first `H`): `operation.mergeTree(C, P, T, C)`, the cherry-pick's
   call. A conflict ends the operation with 409 `merge_conflict`,
   `details.commit` `C` and `details.paths` the conflicted paths, and
   nothing is committed.
6. The result is classified before a commit is written:

| Case | Test | Outcome |
|---|---|---|
| started empty | `C`'s tree equals `P`'s tree | replayed as an empty commit on `T` |
| became empty | the merged tree equals `T`'s tree and `C` did not start empty | dropped and listed in `dropped`: the target already holds the change |
| otherwise | | a new commit with the merged tree and parent `T`, which becomes the tip |

`git replay` keeps a started-empty commit on both versions and drops a
became-empty one on 2.54.0 alone; the loop's answer does not depend on
the git version.

### The identity of a replayed commit

| Part | Value |
|---|---|
| author | `C`'s author name, email and date, read with the range in one `rev-list --format` call and passed to `commit-tree` |
| committer | `Origo <origo@<host>>` from `Handler.committer`, at the operation's time, as every operation writes |
| message | `C`'s message through `operation.message`, so the trailer `Origo-Subject:` with the acting subject joins the message's own trailer block and a commit rebased twice by one subject carries the trailer once |
| signature | none. A signature covers the parent and the committer, both of which change, so a signed commit is replayed unsigned; checked on both git versions, `git replay` drops the `gpgsig` header the same way. A caller that must keep a signature lands with a fast-forward or `merge_commit` |

`operation.commitTree` gains an author argument; the other operations
pass the request's `author` as today.

### `expected_head` and the retry

With `expected_head` set, a head that differs is 409 `non_fast_forward`
as today, and so is a `wal.ConflictError` from `Log.Commit`.

With `expected_head: null`, the head the lease holds is the base. When
`Log.Commit` answers a `wal.ConflictError` on the target, its catch-up
has already applied the newer index to the copy, so the node reads the
new head and runs the whole replay again from step 1. It does this at
most `limits.MaxRebaseAttempts` times in all, inside one `api.MergeBudget`;
past either it answers 409 `non_fast_forward` with `details.expected`
the head of the last attempt and `details.actual` the head that beat it.
A conflict on a later attempt is 409 `merge_conflict` as on the first.

The rule spec 025 states for its own command, that a write is a
statement about a branch the caller has read, stays the command's: `origo
merge -strategy rebase` still requires `-expect`. A caller of the API
that wants the node to absorb the race says so with `null`.

### The answer

The answer of spec 020, with two fields under `rebase`:

```json
{"commit": "<new tip>", "branch": "refs/heads/main", "entry_seq": 1044,
 "tree": "<sha>",
 "replayed": [{"original": "<sha in source>", "commit": "<new sha>"}],
 "dropped": ["<sha in source>"]}
```

`replayed` lists every replayed commit in order, empty on a
fast-forward. When nothing is replayed and nothing fast-forwards, because
`S` is `H` or an ancestor of it, or because every commit of the range
was dropped, the answer is 200
with `commit` the head, `entry_seq` null, `committed: false`, and no
entry or event is written.

One landing is one entry with one update of the target, `old` the head
the replay started from, and one `push` event with `operation: "merge"`
(spec 008). The new tip descends from the old head, so the event's
`forced` is false. The pack is `operation.pack` with the old head and
`S` among the haves, so it carries the new commits and trees and no
blob the source already brought.

### Cost and bounds

Per replayed commit the node runs four subprocesses: two `commit-tree`
for the re-parented sides, `merge-tree`, and the `commit-tree` of the
new commit; the range, the authors and the messages are one
`rev-list`. At `api.MaxPickCommits` that is about four hundred short
subprocesses under one subprocess slot and `api.MergeBudget`. The
per-repository bucket of spec 020, `api.OperationsPerMinute` per node,
still bounds how many landings a repository takes a minute.

### Names the build adds

Each moves, with the build, into the table of the spec that owns its
kind: the strategy, the two fields and the null rule into spec 020's
Operations table, the constant into spec 012's limits, the client
constant into spec 025.

| Name | Kind | Meaning |
|---|---|---|
| `rebase` | value of `strategy` | the replay above; `api.StrategyRebase` in the code |
| `replayed`, `dropped` | response fields of the merge route | the mapping from source commits to new ones, and the commits that became empty |
| `commit` | key of `details` on `invalid_request` | the merge commit that refuses a rebase, beside `field` and `reason` |
| `limits.MaxRebaseAttempts` | Go constant | 3: how many times a rebase with a null `expected_head` replays after losing the commit race |
| `origoclient.Rebase` | Go identifier | the strategy value the client accepts beside the three of today |

No error code, header, route or metric is added.

## Not in this spec

Rebasing the source branch onto the target and moving the source, which
is a second update and a forced one. Replaying merge commits
(`--rebase-merges`). Squashing the range into one commit. Re-signing
replayed commits. A strategy field on the `push` event. Raising
`api.OperationsPerMinute` or the repository's push ceiling of spec 005.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A rebase of a three-commit branch onto a target that moved replays each commit with its author name, email and date, the committer `Origo <origo@<host>>`, one `Origo-Subject:` trailer inside the message's trailer block, and the target at the last one, in one entry and one `push` event with `forced` false | `internal/api`, `TestRebaseReplaysWithTheOriginalAuthor` | not built |
| A range holding a started-empty commit and a commit whose change the target already holds keeps the first and lists the second in `dropped` | `internal/api`, `TestRebaseClassifiesEmptyCommits` | not built |
| A conflict on the second of three commits is 409 `merge_conflict` naming that commit and its paths, and the target is unchanged | `internal/api`, `TestRebaseConflictCommitsNothing` | not built |
| A range with a merge commit is 400 `invalid_request` naming the commit with no replay started; one of `api.MaxPickCommits` + 1 commits is 400 `invalid_change` `too_many`; `author` or `message` present is 400 on that field | `internal/api`, `TestRebaseRefusals` | not built |
| A target that is an ancestor of the source fast-forwards with every id kept and no pack; a range the target already holds answers 200 with `committed: false` and writes no entry | `internal/api`, `TestRebaseFastForwardsAndNoOps` | not built |
| Ten concurrent rebases of ten branches onto one target with `expected_head` null all land, linear, and with one shared `expected_head` nine of ten answer 409 `non_fast_forward`; a rebase whose `Log.Commit` loses to an entry the test commits directly through the log while the operation holds the lease replays and lands on its second attempt, and one that loses `limits.MaxRebaseAttempts` times answers 409; no failpoint is added | `internal/api`, `TestRebaseWithoutExpectedHeadAbsorbsTheRace` | not built |
| A dry run answers `replayed` and `dropped` and leaves `objects/` and `spool/` unchanged | `internal/api`, `TestRebaseDryRunWritesNothing` | not built |
| `origo merge -strategy rebase -expect <sha> <source>` sends no `author` and no `message` and prints the receipt, whose state is `up to date` rather than `dry-run` when the node answers `committed: false` to a request sent without `-dry-run` | `internal/origocli`, `TestMergeRebaseSendsNoAuthor` | not built |
| The suite lands a two-commit branch on a moved `main` with `rebase` and reads a linear history back | `test/conformance`, `TestContract/020/merge-rebase` | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 012 | `limits.MaxRebaseAttempts` joins the limits |
| 020 | the merge row gains `rebase`, `replayed`, `dropped`, the null `expected_head` and the absent `author` and `message`; the Limits row reads "commits per cherry-pick, revert or rebase"; Not in this spec drops "Rebase" |
| 021 | the case `020/merge-rebase` |
| 025 | `origo merge` accepts `-strategy rebase`, omits `author` and refuses `-m` for it; the receipt prints `up to date` when `committed` is false and the command sent no `-dry-run` |
