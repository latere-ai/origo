---
title: "Blame: which commit last changed each line of a file, on the read API"
status: drafted
track: infra
depends_on:
  - specs/009-read-api-and-archive.md
  - specs/012-limits-and-abuse.md
  - specs/035-commit-graph-and-bitmaps.md
affects: [internal/api/, internal/auth/, internal/limits/, internal/contract/, api/openapi.yaml, docs/api.md, test/conformance/, test/e2e/, specs/003-protocol-contract.md, specs/009-read-api-and-archive.md, specs/012-limits-and-abuse.md, specs/025-agent-client.md, specs/027-anonymous-read.md, specs/README.md]
effort: medium
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Blame

## Overview

"Who last changed this line, and why" is the question behind most reads of
a file's history, and the read API cannot answer it: spec 009 lists blame
under Not in this spec, and spec 025 refuses to synthesize it in the client,
because a client would have to fetch every version of the file. An agent or
a platform that wants it clones the repository.

`git blame` answers it in one subprocess. Its cost has two parts: the walk
over the history to find the commits that touched the file, and one diff
per version of the file. Measured with git 2.54.0 on synthetic
repositories, median of three runs on one machine:

| File | History | No commit-graph | Commit-graph | With changed paths |
|---|---|---|---|---|
| 4 lines, changed by 5 of 20 000 commits | 20 000 commits | 121.0 ms | 76.1 ms | 35.0 ms |
| 1 449 lines, changed by 4 000 of 20 000 commits | 20 000 commits | 271.7 ms | 236.1 ms | 209.8 ms |
| 842 lines, changed by 1 667 of 5 000 commits | 5 000 commits | 97.3 ms | 88.3 ms | 90.5 ms |
| 856 KiB, 1 000 versions | 1 000 commits | not measured | not measured | 731.4 ms |

The commit-graph of spec 035 removes most of the walk: blame reads its
changed-path filters (`bloom/queries` 19 999 and `bloom/response-no`
18 409 under `GIT_TRACE2_PERF` for the first row). What remains is the
diffs, which grow with the file's size times its number of versions, so a
bound on the size of the file is what makes blame affordable inside the
read API's budget.

This spec adds one read route, GET /v1/repos/{id}/blame/{sha}, that
answers the line ranges of one file at one commit with the commit that last
changed each range, its original path and line, and that commit's author,
committer, and summary.

## Current state

Not built. `internal/api/read.go` registers seven read routes in
`Handler.registerRead` and runs each through `Handler.open`, which
authorizes `read`, validates the request, takes the lease, answers
`If-None-Match` from `ETag: "<seq>"`, and takes one subprocess slot
(`limits.Slot`). The handlers bound their subprocesses with
`api.DefaultReadTimeout` and answer an expired budget with 504
`operation_timeout` (`readRequest.fail`). Names resolve through
`readRequest.resolve` (`git rev-parse --verify -q --end-of-options`),
paths through `pathValue` and `api.ValidPath`, the `{sha}` segment through
`target`. Commit identities are the `identity` type, `{"name", "email",
"at"}`, which `Handler.commits` fills from `%aI`.

No handler runs `git blame`. `internal/origocli` has no blame command, and
spec 025's Not in this spec names blame among the features the client may
not synthesize.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| `git blame --porcelain` | one header line per line of the file, then the line itself | the same attribution as `--incremental` and the same time (90.5 ms against 85.2 ms on the 842 line file), plus every line of the file to read past; the content is the blob route's to serve |
| Line content in the response | each range carries its lines | doubles the bytes for a caller that has the file already, which is every caller of spec 025's client; a caller without it reads `GET /v1/repos/{id}/blob/{sha}` once |
| The path as trailing segments | a route ending in `{path...}` | spec 009 puts every path in `?path=`, `tree` and `compare` included; one grammar for paths keeps one validator and one client rule |
| Rename and copy detection inside files | `-M`, `-C` | each multiplies the diff work that dominates a long history; whole-file renames are followed without them |
| `git blame --incremental` over a resolved id, ranges and a commit table | this spec | chosen: one record per range, each commit's identities once, no file content |

## Design

### The route

GET /v1/repos/{id}/blame/{sha}, action `read`, with spec 009's grammar:
`{sha}` is a full object id or a short name, or `-` with the name in
`?ref=`; the name resolves with `^{commit}` peeled, as `commits` does.

| Parameter | Rule |
|---|---|
| `path` | required; spec 009's path rules through `api.ValidPath`; the empty path is 400 `invalid_request` with `details.reason: "path"` |
| `start` | the first line, counted from 1; default 1 |
| `end` | the last line, inclusive; default `start` plus `limits.MaxBlameLines` minus 1, cut to the file's last line; a value past the last line is cut to it, as git cuts `-L` |

`start` or `end` that is not a positive integer, an `end` below `start`,
a window of more than `limits.MaxBlameLines` lines, and a `start` past the
file's last line are each 400 `invalid_request` with `details.reason:
"range"`.

### What the node runs

After `Handler.open`, under one subprocess slot and one budget of
`api.DefaultReadTimeout`, three subprocesses in sequence:

1. `git rev-parse --verify -q --end-of-options <name>^{commit}`, as every
   read resolves; unresolvable is 404 `ref_not_found` with `details.ref`.
2. `git cat-file -Z --batch` with `<oid>:<path>` and a NUL on its standard
   input, NUL-delimited because `api.ValidPath` admits a newline in a
   path (checked with git 2.54.0 on a path holding one). A `missing`
   answer, or an object that is not a blob (a directory or a
   submodule), is 404 `ref_not_found` with `details.ref` the path. A blob
   larger than `limits.MaxBlameBytes` is 413 `blame_too_large` and its
   content is not read. Otherwise the node reads the content and counts
   its lines the way git does, the newlines plus one for a last line
   without one; an empty file answers an empty page without step 3.
3. `git blame --incremental -L <start>,<end> <oid> -- <path>`.

The object id in step 3 is the one step 1 printed, which cannot begin with
`-`, and the path follows `--`, so no request value reaches blame as an
option. `--end-of-options` is not used here: with git 2.54.0, `git blame
--end-of-options <oid> -- <path>` fails with `bad revision '<path>'`,
while `git blame <oid> -- <path>` and `git blame --end-of-options <oid>
<path>` both answer.

`--incremental` prints, for each range, `<commit> <orig-line>
<final-line> <count>`, then the commit's `author`, `author-mail`,
`author-time`, `author-tz`, `committer` lines and `summary` the first time
that commit appears, then `filename`, in the order blame finds them. The
node sorts the ranges by final line. Whole-file renames are followed:
checked with git 2.54.0, lines that predate a `git mv` carry the old name
in `filename`. Git quotes `filename` in C style when the path holds a
character it quotes, a newline or a byte above 0x7f among them (checked:
`"sp ace \303\251.txt"`), and the node unquotes it.

### The response

`200` with `Origo-Commit` the resolved commit, as every read answers:

```json
{"path": "src/hot.go", "lines": 842,
 "ranges": [
   {"start": 1, "count": 3, "commit": "fb50b82…", "orig_path": "src/hot.go", "orig_start": 1},
   {"start": 4, "count": 1, "commit": "5283a22…", "orig_path": "src/old.go", "orig_start": 9}],
 "commits": {
   "fb50b82…": {"author": {"name": "Ada", "email": "ada@example.com", "at": "2021-04-04T14:05:44Z"},
                "committer": {"name": "Ada", "email": "ada@example.com", "at": "2021-04-04T14:05:44Z"},
                "summary": "change 4866"}},
 "next_start": null}
```

| Field | Meaning |
|---|---|
| `path` | the requested path |
| `lines` | the number of lines of the file at the commit |
| `ranges` | the window `start` to `end`, in line order, every line in exactly one range |
| `ranges[].start`, `ranges[].count` | the first line of the range in the file at the requested commit, and how many lines it holds |
| `ranges[].commit` | the commit that last changed those lines |
| `ranges[].orig_path`, `ranges[].orig_start` | the file's path in that commit and the first line's number there |
| `commits` | each commit the ranges name, once: `author` and `committer` in spec 009's identity shape, `at` in RFC 3339 with the commit's own offset from `author-time` and `author-tz` (`committer-time`, `committer-tz`), and `summary`, the first line of the message |
| `next_start` | the first line after `end` when the file has more, else null |

The identities and summary sit in one table rather than on each range
because one commit usually owns many ranges, and it is the shape git's own
output has. A caller pages a long file by passing `next_start` as `start`.

### Limits and errors

| Bound | Value | Answer |
|---|---|---|
| file size | `limits.MaxBlameBytes` | 413 `blame_too_large` with `details.size` and `details.max` |
| lines per response | `limits.MaxBlameLines` | the window above; a larger explicit window is 400 `invalid_request`, `details.reason: "range"` |
| budget | `api.DefaultReadTimeout` for the three subprocesses together | 504 `operation_timeout` with `details.operation: "blame"` and `details.budget_seconds` |
| subprocesses | one slot of `ORIGO_MAX_GIT_PROCS` for the request | 429 `rate_limited`, `details.limit: "subprocesses"` |
| requests | the subject's bucket of spec 012 | 429 `rate_limited`, `details.limit: "subject"` |

The size bound is set by the measurement above: blame of an 856 KiB file
spent about 0.73 ms per version on the machine measured, 0.87 ms scaled to
1 MiB, so a file at the bound with 10 000 versions takes about 9 seconds
there, inside the budget with room for a slower node, and a longer history answers 504 rather than
holding a slot. A window does not reduce that work much: the same file
with `-L 100,200` took 707.5 ms, because the lines that came from the first
commit keep the walk going to the root.

`ETag`, `If-None-Match`, and `Origo-Stale` behave as on every route of
spec 009. The route joins spec 027's anonymous route set beside the other
reads, where the one anonymous bucket (`limits.AnonymousRequestsPerMinute`)
bounds it as it bounds the archive. When spec 042 is built, a blame named
by a full object id follows its rule for immutable reads.

### Names the build adds

| Name | Kind | Meaning |
|---|---|---|
| GET /v1/repos/{id}/blame/{sha} | route | the route above |
| `blame_too_large` | error code | 413; message "This file is too large to blame."; details `size`, `max` |
| `limits.MaxBlameBytes` | Go constant | 1 MiB: the largest blob blamed |
| `limits.MaxBlameLines` | Go constant | 5 000: the most lines one response covers, the size of a page of `tree` (`api.TreePageSize`) |
| `blame` | value of `details.operation` | on 504 `operation_timeout` from this route |

The build moves the route into spec 009's route table and the code into a
code table of spec 009, beside `blob_too_large`, and the two limits into
spec 012's table; `tools/apidoc` then renders both into
`api/openapi.yaml`.

## Not in this spec

An `origo blame` command: spec 025 decides its own surface, and the route
makes one possible. Line move and copy detection (`-M`, `-C`). Ignoring
whitespace or revisions (`-w`, `--ignore-rev`). Blame of a directory. A
reverse blame. Streaming ranges as git finds them; the response needs them
in line order.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Blame of spec 009's fixture file at a commit, by short name, by full id, and by `-` with `?ref=`, answers ranges that cover every line once in order, with the commit, `orig_path`, and `orig_start` git's own blame gives, compared to a checked-in golden body | `internal/api`, `TestBlameGolden` | not built |
| A file renamed with `git mv` and edited after answers `orig_path` with the old name for the lines older than the rename | `internal/api`, `TestBlameFollowsRenames` | not built |
| `start` and `end` select a window, `end` past the last line is cut to it, `next_start` pages a 12 000 line file in three requests with no line twice or missing, and a window over `limits.MaxBlameLines`, an `end` below `start`, and a `start` past the end are 400 `invalid_request` with `details.reason: "range"` | `internal/api`, `TestBlamePaging` | not built |
| An empty path is 400 with `details.reason: "path"`, a missing path or a directory is 404 `ref_not_found` naming the path, a `{sha}` of `-x` starts no subprocess, and an empty file answers `lines: 0` and no ranges | `internal/api`, `TestBlameRefusals` | not built |
| A blob one byte over `limits.MaxBlameBytes` is 413 `blame_too_large` with `size` and `max`, and `git blame` is never started | `internal/api`, `TestBlameTooLarge` | not built |
| A blame held past a one second `ReadTimeout` answers 504 `operation_timeout` with `details.operation: "blame"`, and no subprocess is left running | `internal/api`, `TestBlameDeadline` | not built |
| Identities carry `at` with the commit's own offset, equal to what the commits route answers for the same commit | `internal/api`, `TestBlameIdentitiesMatchCommits` | not built |
| A path holding a newline and a path with bytes above 0x7f are blamed, and `orig_path` answers each unquoted, as the request named it | `internal/api`, `TestBlameUnusualPaths` | not built |
| On a copy holding spec 035's commit-graph, blame of a file changed by 5 of 20 000 commits reads changed-path filters under `GIT_TRACE2_PERF`; `TestMeasure` prints the time with and without the graph | `internal/api`, `TestBlameReadsChangedPaths` | not built |
| The suite: `039/blame` blames a pushed file and checks the ranges against the pushed history | `test/conformance`, `TestContract/039/blame` | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 003 | Read operations lists `blame/{sha}` |
| 009 | the route table gains the blame route, a code table gains `blame_too_large`, `details.operation` gains `blame`, and Not in this spec drops blame |
| 012 | the table gains `limits.MaxBlameBytes` and `limits.MaxBlameLines`, and the wall time row names blame under the read budget |
| 025 | Not in this spec no longer lists blame among what Origo lacks |
| 027 | the route set gains the blame route |
