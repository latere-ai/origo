---
title: "Read API and archive: refs, log, diff, tree, blob, tarball"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
affects: [internal/api/, internal/repo/, internal/gittest/, test/e2e/]
effort: medium
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Read API and archive

## Overview

A platform shows a project's history, the diff between two versions, and
a file at a commit without cloning anything. A build wants the tree at one
commit as an archive. This spec is the JSON and archive surface over a
materialized repository, every operation served from the local copy after
the currency check of spec 004.

## Current state

Spec 003 names the endpoints and points here. `internal/api` serves the
lifecycle only; `internal/repo.Git` runs git with a hermetic environment
and a deadline, and `Cache.Acquire(ctx, id, false)` gives a handler a
current copy under a read lock. `internal/gittest` builds fixtures with the real
git. None of the endpoints below exists.

## Design

All paths are under `/v1/repos/{id}`, action `read`, and begin with
`Cache.Acquire(ctx, id, false)`.

### Path grammar

`{sha}`, `{base}`, and `{head}` are one path segment each, so they take
a full object id (40 or 64 hexadecimal characters) or a short name
without a slash matching `^[A-Za-z0-9][A-Za-z0-9._-]*$` (`main`,
`v1.2`, `HEAD`), resolved with `git rev-parse --verify --end-of-options
<name>` and git's own short-name rules. A full reference name, which
has slashes (`refs/heads/feature/x`), is passed as a query parameter
with the segment set to the placeholder `-`, which no short name can
be: `?ref=` for `commits/{sha}`, `tree/{sha}`, `blob/{sha}`, and
`archive/{sha}.tar.gz`, and `?base=` and `?head=` for
`compare/-...-`. A query parameter given with a segment other than `-`
is 400 `invalid_request` with `details.reason: "ref"`. The `commits`
list has no segment and takes `?ref=` for both forms. An unresolvable
name is 404 `ref_not_found` with `details.ref`.

Every value that reaches a git subprocess from a request (`{sha}`,
`?ref=`, `?base=`, `?head=`, `?path=`, `?cursor=`) is placed after
`--end-of-options`, or after `--` for a path, and a value that starts
with `-` is refused as 400 `invalid_request` with `details.reason:
"option"` before any subprocess starts; a `?path=` is further checked
by the path rules below, and a refused one is 400 `invalid_request`
with `details.reason: "path"`. Every response carries `Origo-Commit`
with the resolved object id and `ETag: "<seq>"` where `<seq>` is the
index sequence the copy holds, so `If-None-Match` answers 304 without
running git.

### Path rules

A path from a request, here in `?path=` and in spec 020's changes, is
accepted when every rule holds: at most 4 096 bytes; no leading slash;
no empty component (no `//`, no trailing slash); no component `.` or
`..`; no NUL byte; no component that names the git directory,
case-insensitively, in any of the forms git's `core.protectNTFS` and
`core.protectHFS` refuse (`.git`, `.git` followed by dots or spaces,
`git~1` and the other 8.3 short names, and the HFS forms with ignorable
code points). The rules are one function, `internal/api.ValidPath`,
which `FuzzValidPath` covers against `git update-index` and which spec
020 also applies to a symbolic link's target. The empty path is
accepted here, meaning the whole tree, and refused by spec 020.

| Method | Path | Response and bounds |
|---|---|---|
| GET | `/v1/repos/{id}/refs` | `?prefix=refs/heads/` (default `refs/`); `[{"name", "sha", "peeled"}]` from `git for-each-ref`, `peeled` the tag's target or null; at most 10 000 entries, `Origo-Truncated: true` past that |
| GET | `/v1/repos/{id}/commits` | `?ref=<sha or name>` (default `HEAD`) `&path=&since=&until=&limit=&cursor=`; newest first in `git rev-list` order; `limit` default 50, at most 200; `since` and `until` RFC 3339; `cursor` is the sha of the last commit of the previous page: the node walks `git rev-list --end-of-options <ref>` from the start, discards output up to and including the cursor, and returns the next `limit` commits, so a page is exact whatever the graph and `--skip` is never used; a cursor not in the walk is 400 `invalid_request` with `details.reason: "cursor"`; `{"commits": [{"sha", "parents", "author": {"name", "email", "at"}, "committer": {…}, "message", "trailers": [{"key", "value"}]}], "next_cursor"}`, `trailers` from `git interpret-trailers --parse` in the order they appear with duplicate keys kept, empty when there are none; a repository whose `ref` does not exist because it has no commit yet (the default `HEAD` of an empty repository) answers 200 with `commits: []` and `next_cursor: null`, while a named `ref` that does not exist in a repository with history is 404 `ref_not_found` |
| GET | `/v1/repos/{id}/commits/{sha}` | the commit as above plus `"stats": {"files", "additions", "deletions"}` from `git show --numstat`; binary files count as a file with 0 lines |
| GET | `/v1/repos/{id}/compare/{base}...{head}` | `?path=&base=&head=`; `text/x-diff` from `git diff -M --no-color --end-of-options <base> <head> -- <path>`; at most 1 MiB, cut at a file boundary with `Origo-Truncated: true`; binary files listed as `Binary files differ` |
| GET | `/v1/repos/{id}/tree/{sha}` | `?path=&recursive=0&cursor=`; `{"entries": [{"path", "mode", "type", "sha", "size"}], "next_cursor"}` from `git ls-tree -l`; 5 000 entries per page, `cursor` the last path |
| GET | `/v1/repos/{id}/blob/{sha}` | raw bytes of a blob with `Content-Type` from `http.DetectContentType` over the first 512 bytes and `Content-Length`; `Range` honoured; a blob over 50 MiB without a `Range` of at most 50 MiB is 413 `blob_too_large` |
| GET | `/v1/repos/{id}/archive/{sha}.tar.gz` | `git archive --format=tar.gz --prefix=<slug>-<7 hex>/ <sha>` streamed; entries in git's tree order, mtime the commit time, no `.git`; reproducible for one git version |

The first draft named `.tar.zst`; zstd is not in the standard library
and spec 001 forbids a dependency beyond `latere.ai/x/pkg`, so the
format is git's own `tar.gz`.

| Header | Meaning |
|---|---|
| `Origo-Commit` | the object id `{sha}` resolved to, on every read response |
| `Origo-Truncated` | `true` when `refs` hit its cap or `compare` was cut at 1 MiB |

| Code | Status | Message | Details |
|---|---|---|---|
| `blob_too_large` | 413 | This file is larger than 50 MiB. Request it in ranges of at most 50 MiB. | `size`, `max` |
| `operation_timeout` | 504 | The operation took too long and nothing was changed. | `operation`, `budget_seconds` |

`GET /v1/repos/{id}` gains `pushed_at`, the `pushed_at` of the index
object the copy holds (spec 004: the `at` of the newest `push` entry,
null for a repository with no push, and the rule for an index object
written before the field existed); spec 019's `stats` reports the same
value.

Every operation runs one git subprocess with a 30 second deadline,
`--no-pager`, and the environment of spec 016, under the per-node
subprocess cap `ORIGO_MAX_GIT_PROCS` (spec 012), beyond which the answer
is 429 `rate_limited`. A subprocess that reaches the deadline is
killed and the answer is 504 `operation_timeout`, defined in the table
above and answered wherever a subprocess of the JSON API reaches its
budget (specs 012, 020), with `details.operation` the endpoint's last
path segment and `details.budget_seconds` the budget that ran out, 30
here; a streamed response (`compare`, `blob`, the archive) whose
subprocess is killed after the first byte is cut short, which the
client sees as a truncated body and not as a status.

The archive is what a build system should fetch for a single commit: one
request, no negotiation, reproducible bytes.

## Not in this spec

Search. Blame. Rendering of any kind. Paging on `refs` beyond the cap.

## Acceptance criteria

- Every endpoint has a golden test against a fixture with merges,
  renames, a binary file, and a 60 MiB blob built by `internal/gittest`,
  comparing each JSON and diff body to a checked-in expectation and the
  blob's body to its checked-in SHA-256, because a 60 MiB expectation
  does not belong in the tree (proposed: `internal/api`,
  `TestReadEndpointsGolden`).
- The archive of the fixture at one commit has the same SHA-256 on two
  nodes and after a rebuild from the log (proposed: `internal/api`,
  `TestArchiveIsReproducible`).
- `compare` over a 5 MiB change returns at most 1 MiB, cut at a file
  boundary, with `Origo-Truncated: true`, in under one second (proposed:
  `internal/api`, `TestCompareTruncates`).
- The archive of a 200 MiB tree sends its first byte within 200 ms,
  asserted on every push to `main` (proposed: `test/e2e`,
  `TestE2EArchiveStreams`, a plain test of the one-node run; the fixture is
  sized to the job budget of spec 013, and `TestMeasure` under
  `ORIGO_E2E_MEASURE=1` prints the same figure for a 1 GiB tree without
  asserting it).
- Paging `commits` with `limit=7` over 100 commits including a merge
  whose parents interleave in rev-list order, and `tree` with a 5 001
  entry directory, returns every item exactly once, a cursor not in
  the walk is 400, and `commits` on a repository with no commit answers
  an empty page (proposed: `internal/api`, `TestPagingIsExact`,
  `TestCommitsOnAnEmptyRepository`).
- A commit with two `Signed-off-by` trailers and one `Co-authored-by`
  answers `trailers` as three pairs in that order (proposed:
  `internal/api`, `TestTrailersKeepOrderAndDuplicates`).
- A `{sha}` of `-x`, a `?ref=--output=/tmp/x`, and a `?path=-` are 400
  `invalid_request` with no subprocess started, `tree/-?ref=refs/heads/feature/x`
  resolves the full name, and `tree/main?ref=refs/heads/x` is 400
  (proposed: `internal/api`, `TestPathGrammarRefusesOptions`).
- Every path in a table of accepted and refused paths (`a/b`, `.git/x`,
  `a/.GIT/x`, `git~1/x`, `a//b`, `/a`, `a/../b`, a 4 097 byte path, a
  path with a NUL) is classified as the path rules say and no refused
  path reaches a subprocess; `FuzzValidPath` finds no panic and no path
  the validator accepts that `git update-index` refuses, as a
  seed-corpus test on every push and for 40 seconds under `make fuzz`
  (spec 013) on the weekly schedule (proposed: `internal/api`,
  `TestPathRules`, `FuzzValidPath`).
- A `compare` whose subprocess is held past 30 seconds by a fixture
  answers 504 `operation_timeout` with `details.budget_seconds: 30`
  and no subprocess is left running (proposed: `internal/api`,
  `TestReadDeadlineIsOperationTimeout`, with the deadline lowered by an
  option in the test).
- A 60 MiB blob answers 413 `blob_too_large` without `Range` and 206
  with `Range: bytes=0-1023` (proposed: `internal/api`, `TestBlobRange`).
- A second request with `If-None-Match` equal to the `ETag` answers 304
  and runs no git subprocess (proposed: `internal/api`, `TestETagRevalidates`).
