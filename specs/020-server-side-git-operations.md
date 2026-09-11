---
title: "Server-side git operations: commits, merges, cherry-picks, and reverts without a clone"
status: complete
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
  - specs/008-push-events.md
  - specs/009-read-api-and-archive.md
  - specs/012-limits-and-abuse.md
  - specs/019-repository-administration.md
affects: [internal/api/, internal/repo/, internal/httpgit/, internal/contract/, internal/limits/, internal/auth/, cmd/origod/, test/conformance/]
effort: large
created: 2026-09-06
updated: 2026-09-12
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

The consumer that needs this first is a hosting product that walks every
project's history and applies a change by pushing a branch; this spec is
what makes that a request rather than a clone.

## Current state

Built on 2026-09-09 and complete since 2026-09-11; the Outcome records
the tests, the divergences, and the runs. Before it, spec 009 served
refs, commits, trees, and blobs, spec 004's receive path turned a pack
and a transaction into an entry, and nothing created a commit without a
client.

One item the builder took from spec 012: a subject that drives many
repositories through these routes needs a request rate of its own. Spec
012's per-subject bucket was one figure for the node,
`ORIGO_REQUESTS_PER_MINUTE`, 600 by default, which is 300 back-to-back
pushes a minute under one token; a tool that walks a fleet of
repositories crosses it while every human client stays far below. The
authorizer answers the figure: spec 007's response carries an optional
`requests_per_minute`, absent meaning the variable's value, and this
spec's builder made the bucket read it, so a consumer raises the rate
for its own tooling without raising it for every caller of the node.

## Design

### Common shape

Every operation is a `POST` under `/v1/repos/{id}` with action `write`,
authorized like a push (spec 007), that names the branch it writes and
the commit it expects to find there, so a stale caller is refused rather
than clobbering:

| Field | Meaning |
|---|---|
| `branch` | `refs/heads/<name>` or a short name; must exist except for `commits` with `create_branch: true` |
| `create_branch`, `from` | `commits` only: `create_branch: true` creates `branch`, which must not exist (409 `non_fast_forward` with `details.expected: null` and `details.actual` the existing head when it does), and requires `from`, a commit sha or a branch name in the repository whose tree the new branch's first commit starts from and whose commit is its parent; `from` without `create_branch` and `create_branch` without `from` are 400 `invalid_request` with `details.field`; a repository with no commit takes `from: null` and the first commit has no parent |
| `expected_head` | the commit the caller believes the branch points at; `null` with `create_branch: true` and refused otherwise; mismatch is 409 `non_fast_forward` with the details of spec 003: `ref`, `expected`, `actual` |
| `author` | required: `{"name", "email"}`, both non-empty, `email` with one `@`; a missing or empty field is 400 `invalid_request` with `details.field: "author"`; the committer is always `Origo <origo@<host of ORIGO_PUBLIC_URL>>` with the effective subject's identity in the message trailer `Origo-Subject:` and the actor in `Origo-Actor:` (spec 007) |
| `message` | the commit message, 1 to 64 KiB |
| `dry_run` | `true` computes the result and returns it without committing; the objects it writes go into a temporary object directory under `<ORIGO_DATA_DIR>/spool/`, set as `GIT_OBJECT_DIRECTORY` with the repository's `objects/` in `GIT_ALTERNATE_OBJECT_DIRECTORIES`, and the directory is removed after the response, so a dry run never writes into the repository's objects and a loop of dry runs leaves nothing for compaction to clear |

Response 201 `{"commit": "<sha>", "branch", "entry_seq", "tree": "<sha>"}`;
for `dry_run`, 200 with the same fields, `entry_seq` null because no
entry was committed, and `"committed": false`. One
operation is one entry, one commit, one event: a `push` with one
update whose `operation` field (spec 008) is the operation's name,
carried in the entry header as the push option `origo.operation=<name>`
so the repair sweep of spec 008 can rebuild the event. No operation
runs user code: no hooks, no filters, no smudge, no submodule fetch.

### Operations

| Method | Path | Body beyond the common fields | Result |
|---|---|---|---|
| POST | `/v1/repos/{id}/commits` | `changes: [{"path", "content" (base64, at most 10 MiB decoded per file) \| "content_ref" (a blob sha already in the repository) \| "delete": true, "mode": "100644"\|"100755"\|"120000"}]`, 1 to 1 000 changes in a body of at most 64 MiB, each `path` checked by the rules below | one commit with `expected_head` as parent |
| POST | `/v1/repos/{id}/merge` | `source: <branch or sha>`, `strategy: "fast_forward_only"\|"merge_commit"\|"fast_forward_if_possible"` (default), `message` optional for a merge commit, defaulting to `Merge <source> into <branch>` with both names as the request gave them | fast-forward moves the branch with no new commit and answers the source's sha; a merge commit has two parents; a conflict is 409 `merge_conflict` with `details.paths` |
| POST | `/v1/repos/{id}/cherry-pick` | `commits: [<sha>]`, 1 to 100, applied in order, `mainline` for a merge commit | one commit per picked commit, all in one entry and one transaction, so partial application never lands; a conflict is 409 `merge_conflict` naming the commit and paths |
| POST | `/v1/repos/{id}/revert` | `commits: [<sha>]`, 1 to 100, `mainline` | one revert commit per input, same atomicity and conflict rule |

### Paths

A change's `path` is checked by the path rules of spec 009
(`internal/api.ValidPath`, the one function both specs use); a path
they refuse, or an empty one, is 400 `invalid_change` with
`details.index` and `details.reason: "path"`. A `120000` change's
content is a relative target checked by the same rules.

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
commands (into the temporary object directory for a dry run); they
are unreachable until the reference moves, so a failure leaves nothing
a reader can see. With `create_branch`, the temporary index is read
from `from` and `from` is the parent.

Then, without a synthesized `receive-pack`: `git pack-objects --revs
--end-of-options` over `<expected_head>..<new>` writes the entry's
pack; `git index-pack --strict` over that pack and `git fsck
--connectivity-only --no-progress <new>` check the new objects the way
`receive.fsckObjects` would; the pack's size is checked against
`quota_bytes` and the push size limit (spec 012); and `Log.Commit` of
spec 004 commits the pack with the one-update transaction, the
subject and actor, and `push_options: ["origo.operation=<name>"]`,
after which `Cache.Advance` records the sequence, the branch is
moved with `git update-ref`, and `events.Enqueue` (spec 008) writes the
event. `commits` is bounded by the 30 second budget of spec 009 and the
merge family by 5 minutes; over budget is 504 `operation_timeout`
(spec 009) with `details.budget_seconds` 30 or 300, nothing is
committed, and the loose objects stay unreachable in the copy's object
store until the copy is evicted (spec 005) or rebuilt (spec 004);
compaction does not prune loose objects (spec 006). A dry run leaves
nothing at all: its objects go into the temporary object directory,
which is removed after the response, so no loose object of a dry run
ever enters the repository's `objects/`.

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

`operation_timeout` (spec 009), `ref_not_found`, `over_quota`,
`forbidden`, and `repo_frozen` apply as elsewhere.

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
- `commits` with `create_branch: true` and `from: main` creates the
  branch with `main`'s head as the parent, the same request on an
  existing branch is 409, one without `from` is 400 naming the field,
  and a `dry_run` of it answers 200 with `committed: false` and
  `entry_seq` null, leaves `objects/` of the repository unchanged, and
  leaves no directory under `spool/` (proposed: `internal/api`,
  `TestCreateBranchFromAndDryRunWritesNothing`).
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
- A change whose `path` the rules of spec 009 refuse, and one whose
  path is empty, is 400 `invalid_change` with `details.index` and
  `details.reason: "path"` and no subprocess starts (proposed:
  `internal/api`, `TestChangePathsUseTheReadRules`; the table of
  accepted and refused paths and `FuzzValidPath` are spec 009's).
- Twenty concurrent `commits` requests on one branch with the same
  `expected_head` produce exactly one commit and nineteen
  `non_fast_forward` answers (proposed: `internal/api`,
  `TestConcurrentCommitsSerializeOnExpectedHead`).
- `FuzzOperationBody` in `internal/api` finds no panic over random and
  mutated request bodies: it runs as a seed-corpus test in the suite
  on every push and for 40 seconds under `make fuzz` (spec 013) on the
  weekly schedule (proposed: `internal/api`, `FuzzOperationBody`).
- The conformance suite (spec 021) gains one case per row of the
  Operations table, each running the row's success path against the
  fixture the suite pushes (proposed: `test/conformance`,
  `TestContract/020/commits`, `TestContract/020/merge`,
  `TestContract/020/cherry-pick`, `TestContract/020/revert`).

## Outcome

Built on 2026-09-09 as `internal/api/operations.go` (the routes, the
request shapes, and every check a request is held to before a
subprocess starts) and `internal/api/operations_git.go` (the plumbing,
the pack, and the commit), with the two codes in `internal/contract`
and the per-subject rate in `internal/limits` and `internal/auth`.

| Criterion | Test |
|---|---|
| `commits` with three changes produces one commit whose tree equals a client-side commit, the committer `Origo <origo@<host>>`, one entry carrying `origo.operation=commits`, one `push` event with `operation`; a stale `expected_head` is 409, a request without `author` is 400 | `internal/api`, `TestCommitsWritesOneEntryAndRefusesStaleHead` |
| `create_branch` with `from`, 409 on an existing branch, 400 without `from`, and a dry run that answers `committed: false` and leaves `objects/` and `spool/` untouched | `internal/api`, `TestCreateBranchFromAndDryRunWritesNothing` |
| `merge` fast-forwards when it can, writes a two-parent commit when it cannot, and a conflict is 409 `merge_conflict` naming the paths with the branch unchanged | `internal/api`, `TestMergeStrategiesAndConflict` |
| Cherry-picking three commits of which the second conflicts commits nothing and names the second | `internal/api`, `TestCherryPickIsAtomic` |
| `over_quota` on a pack past `quota_bytes`, `too_many` at 1 001 changes, `too_large` at 11 MiB, the body limit at six 10 MiB files, and 429 `details.limit: "repository"` on the 61st operation | `internal/api`, `TestServerSideOperationsHonourLimits` |
| A path the read rules refuse and an empty one are 400 `invalid_change` with `index` and `reason: "path"`, and no subprocess starts | `internal/api`, `TestChangePathsUseTheReadRules` |
| Twenty concurrent requests with one `expected_head` make one commit and nineteen `non_fast_forward` answers | `internal/api`, `TestConcurrentCommitsSerializeOnExpectedHead` |
| No request body panics a handler | `internal/api`, `FuzzOperationBody` |
| The conformance suite gains one case per row of the Operations table | `test/conformance`, `TestContract/020/commits`, `/merge`, `/cherry-pick`, `/revert`, spec 021's to own and to run |

Four more tests carry what the criteria do not name:
`TestOperationRefusalsAndBudget` (every `invalid_request` and
`invalid_change` shape, `ref_not_found` on an unknown branch, source,
or picked commit, and `repo_frozen` on a frozen repository),
`TestOperationBudgetIsAnswered` (504 `operation_timeout` with
`details.budget_seconds`), `TestOperationsSurviveGitFailures` (a failed
step before the commit writes nothing; a branch the copy could not move
after the entry landed still answers 201 and the next currency check
reconciles the reference), and `TestRevertOfAMergeTakesTheMainline`.
`internal/api` is at 90.9% and `internal/limits` at 98.1%.

Spec 012's builder item is closed: `auth.Decision.RequestsPerMinute`
carries spec 007's optional figure with no default,
`Buckets.SetRate(subject, perMinute)` sets that subject's rate and
burst, and `Limits.SetSubjectRate` is called wherever an allow arrives,
in `api.admit`, `httpgit.decide`, and the LFS decision. Spec 012's
Outcome records it.

### Divergences

- **The pack excludes what the log already holds, not only
  `expected_head`.** The Mechanics say `git pack-objects --revs` over
  `<expected_head>..<new>`. That is the minimal pack for `commits`, a
  cherry-pick, and a revert, whose new commits all have
  `expected_head` as their parent, but a merge commit's second parent
  is the source, so the range would pack the source's whole history a
  second time although the log already holds it. The rule built is the
  same range with one `^` per pre-existing commit the operation started
  from: `expected_head` (or `from`), and the source as well for a merge
  commit. It is identical to the spec's range for the other three
  operations and is what a push of the same change would send, which
  is what the Overview asks for.
- **A fast-forward merge commits an entry with no pack at all.** The
  source is already in the log, so there is nothing new to write; the
  entry carries the one-update transaction alone, the shape the
  `default_branch` patch of spec 003 already uses.
- **`git update-ref` runs before `Cache.Advance`, not after.** The
  Mechanics name the other order. Advancing first would leave the copy
  claiming a sequence whose reference it does not hold, and no currency
  check would repair it; moving the reference first means a failure
  leaves the copy behind the log and the next check reconciles it from
  the index. The response is 201 either way: the entry is committed and
  the log is the source of truth.
- **`--end-of-options` is not passed to `git index-pack` or `git
  pack-objects`.** The Mechanics name it on `pack-objects`;
  `index-pack` refuses the option outright and neither command is
  handed a value that came from the request, the pack's path being the
  node's own. Every argument a request supplies still follows
  `--end-of-options`, and a value starting with `-` is refused before
  any subprocess starts.
- **A pick names its base through the sides, not through
  `--merge-base`.** The Mechanics say `git merge-tree --write-tree` for
  a cherry-pick and a revert, whose three-way base is the picked
  commit's parent (its own commit for a revert) and not the merge base
  of the two sides. `--merge-base=<commit>` says exactly that and git
  learned it after the git of the runtime image, so the first build was
  503 on the stack while it passed against the toolchain's git. Each
  side is re-parented onto the base as a commit of its own instead:
  the merge base of that pair is the base the pick needs, and the merge
  asks git for nothing the image does not have. The two commits are
  unreachable, are outside the entry's pack, and cost two objects per
  picked commit. `TestPicksNameNoMergeBaseOption` reads every
  invocation an operation made and fails on the option.
- **`git update-index` is given the operation's own empty directory as
  `GIT_WORK_TREE`.** The command insists on a work tree whatever it is
  asked to do and the warm copy is bare. Nothing is read from the
  directory, the index is the only thing the call changes, and the
  directory is removed with the rest of the workspace.

### The conformance criterion and the stack proof

The four cases are in the tree. Spec 021 landed beside this spec and
its builder wrote them on the suite's own harness, which is the right
side of the line: the harness, `Target`, and the skip groups are that
spec's, and `test/conformance/cases020.go` holds one case per row of
the Operations table, each running the row's success path against a
repository the case pushes, with `invalid_change`, `merge_conflict`,
and the `non_fast_forward` of a stale `expected_head` beside it. Spec
021's code table carries the two rows and their statuses, and the four
call sites pass `contract.Code*` constants through `contract.Write`
and `contract.Refuse`, which the walk of
`TestEveryCodeHasOneSentence` reads.

The stack proof is the dispatched run 34353736553 on `ae3d036`, in
which the four `TestContract/020` cases passed against the kind stack
and every job but one was green: the gate, the integration tier, the
mutation job, the `e2e-slow` cluster tier, the up-script check, and the
`e2e` job's own cluster scenarios. The `e2e` job's conformance step
failed on `019/gc` and `012/rate_limited`, two cases of other specs,
and on the `TestSameAnswersOnStubAndStack` that follows them; none of
the three is this spec's and none touches its routes.

Two dispatched runs came before it. The run 34344691822, on `fabe944`,
was green in every job with this spec's own tests in the tree and
before the four conformance cases existed. The run 34347920915, the
first with the cases, failed the `e2e` job on `020/cherry-pick` and
`020/revert`, which is how the `--merge-base` defect below was found:
the cases caught a difference between the toolchain's git and the
runtime image's that no test running against the local git could
have.

This spec stayed at `testing` until spec 021 reached `complete`, the
way specs 003 and 004 waited on the suite: what remained was the run of
the four cases against the installation `ORIGO_LIVE_URL` names, which
spec 021 owns. The four cases are green against the image the first
release published: the `conformance` job of the tag run 34461460766 of
`v0.1.0` names `--- PASS: TestContract/020 (3.16s)`, and the `cluster
e2e tier` job of the tag's `verify` run 34461461220 names its four
subtests, `commits`, `merge`, `cherry-pick`, and `revert`.

Unlike specs 003 and 019, this spec loses nothing to the group rule
spec 021's Outcome states: none of the four cases carries a group, so
a live target supplies everything they need and one live run closes
all four. What this spec waits on is that run and nothing else.

The first release ran on 2026-09-10 and did not produce the live run.
The `live` job of the tag run 34461460766 of `v0.1.0` executed and its
`TestContract` skipped, because the repository carries neither
`ORIGO_LIVE_URL` nor `ORIGO_LIVE_TOKEN`. With the URL empty the test
takes its stack branch instead of its live branch, finds nothing at
`ORIGO_TEST_URL`, and skips there: the job's log reads
`contract_test.go:136: nothing answers at ORIGO_TEST_URL
(http://localhost:30080)` then `--- SKIP: TestContract (0.00s)`. The
job never dialled an installation. A skipped test passes, so the job is
green, and this spec does not read that green as the run.

The `install from the release artifacts` job of the same run did run
`TestContract` in live mode, and it passed: `contract_test.go:133: live
run against http://localhost:30180: 51 passed`, with exactly the six
groups skipped. Its target is a kind cluster the job had just built
from the published artifacts, not an installation, so that run closes
spec 018's job row and no criterion here.

Spec 017's Outcome records that limit and its lifting. The v0.1.3
release run 34546335576 of 2026-09-11 is the run: its `live` job, id
103120952813, dialled `https://code.latere.ai` and passed,
`contract_test.go:133: live run against ***: 51 passed` and
`--- PASS: TestContract (173.69s)`. This spec's four cases are among
them, `--- PASS: TestContract/020/commits`, `/merge`, `/cherry-pick`,
and `/revert`, one per row of the Operations table. That was the last
item and the spec is `complete`.

### Open

The spec leaves these unsaid and the build chose the smallest answer
that keeps its own rules; each is a candidate for a later round.

- `message` on a cherry-pick or a revert. The common shape makes it the
  commit message; a pick writes one commit per input. Built: when the
  request names one it is the message of every commit the operation
  writes, and when it does not, a cherry-pick keeps the picked commit's
  message and a revert takes git's own `Revert "<subject>"` with the
  reverted id.
- `strategy: "fast_forward_only"` over a diverged branch. No code is
  named for it. Built: 409 `non_fast_forward` with `details.ref` the
  branch, `details.expected` the source, and `details.actual` the
  branch's head, which is what git calls the same refusal on a push.
- A `content_ref` naming a blob the repository does not hold, and a
  change `git update-index` refuses at its path. Built: 400
  `invalid_change` with the change's `index` and the reasons `content`
  and `path`, so the closed set of five reasons stays closed.
- An empty `changes` or `commits` list, and `from: null` on a
  repository that has history. Built: 400 `invalid_request` naming the
  field, because the Limits table's `too_many` is the answer to a list
  that is too long and not to one that is empty.
- `Origo-Actor:` is written only when the request carries an actor, so
  a commit made without delegation has one trailer rather than an
  empty one.
- One subprocess slot (spec 012) covers a whole operation, the way one
  covers a whole read. The Mechanics do not mention the semaphore.
- An operation does not call the compaction trigger a push calls
  (`compact.Manager.After`); the primary's sweep picks the repository
  up on its next round. The Mechanics do not name the trigger and the
  handler's `Compactor` interface does not carry it.
- An operation on a repository an import holds is refused by
  `Log.Commit`'s transaction check rather than by a state code: the
  import requires an empty repository and commits its own entry, so
  whichever writer lands first refuses the other. The Errors section
  names `repo_frozen` and not `repo_importing`, and the build added no
  code.

### Spec defects

- The Limits section says `git merge-tree --write-tree` with conflict
  output as data needs git 2.40, which is the floor `origod check` will
  require. That is true of the form this spec uses, and the divergence
  above is what keeps it true: `--merge-base` needs 2.41, above the
  2.39 of the `bookworm-slim` runtime image the first build ran on;
  spec 017 has since moved the image to `trixie-slim` and git 2.47, and
  the build keeps the form that needs 2.40 alone. A
  reader of the Mechanics should not have to work out which options of
  `merge-tree` the floor admits.
- A reference name that collides with an existing one answered 503
  `storage_unavailable`, which told a caller to wait for something
  that would never change. Creating `refs/heads/topic` where
  `refs/heads/topic/x` exists is git's directory-file conflict: git
  cannot hold a file and a directory at one path. The Errors section
  named no code for it, so the build fell through to the storage
  refusal. Spec 025's Open section reported it, having met it while
  writing its end-to-end test. Closed on 2026-09-12 by the user's
  decision: `create_branch` answers 400 `invalid_request` with
  `details.field: "branch"`, `details.ref` the reference in the way,
  and a reason naming both, before anything is written, in either
  direction of the conflict (`refs/heads/topic` beside
  `refs/heads/topic/x`, and `refs/heads/topic/x/y` beside it too).
  `refDirectoryConflict` in `internal/api/operations.go` finds the
  smallest such reference so the answer is stable, and
  `TestCreateBranchRefusesADirectoryFileConflict` holds both
  directions and asserts the references are unchanged after the
  refusals. A 409 of its own was not added: the name is wrong for this
  repository the way a malformed name is, and nothing the caller can
  wait for changes that.

- The frontmatter's `affects` named neither `internal/limits/` nor
  `internal/auth/`, which the builder item from spec 012 changes, nor
  `cmd/origod/`, which the route sweep of spec 016 obliges every new
  route to add a line to. All three are in the list now.

A review on 2026-09-11 read the Design against `internal/api/operations.go`
and `operations_git.go` and found the four routes, every field of the
common shape and of each operation's body, the limits (1 000 changes,
10 MiB per file, 64 MiB per body, 100 picked commits, 64 KiB message,
60 operations a minute per repository, 5 minutes for the merge family),
the three strategy names, the five `invalid_change` reasons, the
response fields with `committed` on a dry run, the committer
`Origo <origo@<host>>` with the two trailers, the `origo.operation=`
push option on the entry, the dry run's temporary object directory with
the copy's as alternate, the plumbing as the Mechanics and the
divergences state it, the two codes at their statuses, and every named
test present with the four conformance cases in `cases020.go`. Three
passages were behind the tree: the Current state in unbuilt tense with
spec 012's item still to take, a sentence saying the spec stays at
`testing` two paragraphs before the one that closes it, and a
spec-defect bullet saying spec 017 moves the image in a later phase.
Each reads as the tree stands.
