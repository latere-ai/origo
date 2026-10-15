---
title: "Compare status and stat: the merge base, ahead and behind, and per-file counts as JSON"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/009-read-api-and-archive.md
  - specs/012-limits-and-abuse.md
  - specs/021-conformance-suite.md
  - specs/025-agent-client.md
affects: [internal/api/, internal/limits/, internal/origoclient/, internal/origocli/, skills/origo/SKILL.md, docs/cli.md, docs/api.md, api/openapi.yaml, test/conformance/, specs/009-read-api-and-archive.md, specs/012-limits-and-abuse.md, specs/021-conformance-suite.md, specs/025-agent-client.md, specs/README.md]
effort: medium
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Compare status and stat

## Overview

`GET /v1/repos/{id}/compare/{base}...{head}` answers one thing, the
patch, cut at a file boundary past `api.MaxCompareBytes` (1 MiB). Two
questions an agent asks before it reads any patch have no cheap answer:

- **What changed, file by file?** `origo diff` (spec 025) fetches the
  patch and counts `+` and `-` lines on the client. On this repository's
  own history from the `v0.1.0` tag to `1bdbd37`, the patch is 2 684 758
  bytes over 360 files. The node sends the first 1 MiB, which holds 178
  whole files, and the client's stat covers those 178 and says the rest
  are not counted. `git diff --numstat` of the same comparison is 12 036
  bytes and covers all 360.
- **Is my commit already in main, and how far behind is my base?** No
  route answers either. The patch of `main...<sha>` is empty when the two
  trees are equal, which is not containment. An agent pages
  `GET /v1/repos/{id}/commits` at 200 commits a page looking for its id,
  50 requests for a commit 10 000 commits back, or clones.

This spec adds a second answer on the same route, selected by a query
parameter: JSON with the merge base, how many commits head is ahead of
and behind base, a status, exact totals, and one row per file with its
path, its previous path on a rename, its change kind, and its added and
deleted lines. It is the stat mode filed as issue #8 and the status a
caller of common hosting APIs reads from a compare call, under the field
names those callers already know. The patch stays the default, byte for
byte.

## Current state

Not built. The compare route exists:
`Handler.compare` in `internal/api/read.go` takes `{base}...{head}`, or
`-...-` with `?base=` and `?head=`, through `target`, and `?path=`
through `pathValue`. It resolves both sides with `rev-parse --verify`
and the peel `^{}`, so a tree id stays a tree, runs
`git diff -M --no-color --end-of-options <base> <head> [-- <path>]`,
reads at most `api.MaxCompareBytes` + 1 bytes, and cuts with
`cutAtFileBoundary`. It reads no other query parameter and no `Range`
header. `Handler.open` sets `ETag: "<seq>"` from the copy's index
sequence and answers `If-None-Match` with 304 before any subprocess.
The diff is two-point: base against head, not against their merge base.

`origoclient.Client.Compare` (`internal/origoclient/read.go`) always
sends the placeholder form and returns the raw body as `Comparison`.
`session.comparison` in `internal/origocli/read.go` calls it once per
`-path`, counts with `parseDiff` (`internal/origocli/format.go`), and
prints `statLines`; past the node's cut it prints the
`[truncated: the installation cut the comparison at its 1 MiB cap ...]`
line. `parseDiff` counts a type change (a file replaced by a symbolic
link) as two files, because the patch carries two `diff --git` headers
for it.

`GET /v1/repos/{id}/commits/{sha}` already answers
`"stats": {"files", "additions", "deletions"}` from `git show --numstat`,
a binary file counting as a file with 0 lines (`Handler.commit`).

## Decision record

| Option | Shape | Why not |
|---|---|---|
| Content negotiation | `Accept: application/json` on the same URL | `Handler.open` sets one `ETag`, the index sequence, for every answer of a URL and answers 304 from it; two representations under one URL and one tag let a cache that ignores `Vary` hand the patch to a JSON caller. A query parameter gives each answer its own URL and keeps the tag correct with no change to `open` |
| A new route | GET /v1/repos/{id}/compare-stat/{base}...{head} | a second route for one resource, a second row in the route sweep and the OpenAPI document, and a second path grammar to keep equal to the first |
| Rows against the merge base | rows of `diff <merge_base> <head>`, as some hosting APIs answer | the patch of this route is two-point; rows that disagree with the patch on the same URL would make `origo diff` and `origo diff -p` disagree. A caller that wants the branch's own changes passes the answered `merge_base_commit.sha` as base, one more request |
| **`?format=json` on the existing route, chosen** | the patch stays the default; `format=json` selects the JSON answer | one URL per answer, the existing grammar, and a node without this spec ignores the parameter and answers the patch, which the client detects from `Content-Type` |

## Design

### The parameter

`format` is read beside `path`, `base` and `head`, under the option rule
of spec 009 (a value starting with `-` is refused):

| `format` | Answer |
|---|---|
| absent or `patch` | today's answer, unchanged: `text/x-diff`, cut past `api.MaxCompareBytes` |
| `json` | the answer below, `application/json` |
| anything else | 400 `invalid_request` with `details.reason: "format"`, before any subprocess |

`If-None-Match` and `ETag` work as on every read: the tag is the index
sequence and a match is 304 with no subprocess. Neither answer honors
`Range`; the compare route never read it, and the JSON answer is bounded
by its row cap instead.

### The answer

```json
{"base_commit": {"sha": "<40 or 64 hex>"},
 "head_commit": {"sha": "<hex>"},
 "merge_base_commit": {"sha": "<hex>"},
 "status": "diverged", "ahead_by": 2, "behind_by": 1,
 "stats": {"files": 7, "additions": 4, "deletions": 3},
 "files": [
  {"filename": "renamed.txt", "previous_filename": "moved.txt",
   "status": "renamed", "additions": 1, "deletions": 0, "changes": 1,
   "binary": false, "sha": "<blob id at head>"}]}
```

| Field | Meaning |
|---|---|
| `base_commit`, `head_commit` | `{"sha"}`: each side resolved with the peel `^{commit}`. A side that names a tree or a blob is 404 `ref_not_found`, the rule spec 009 gives `commits/{sha}` |
| `merge_base_commit` | `{"sha"}` from `git merge-base --end-of-options <base> <head>`, or `null` when the histories share no commit (the command exits 1 with no output) |
| `ahead_by`, `behind_by` | from `git rev-list --left-right --count --end-of-options <base>...<head>`, which prints `<left>\t<right>`: left is the commits only base reaches, which is `behind_by`; right is the commits only head reaches, which is `ahead_by`. Counted over the whole history whatever `?path=` says |
| `status` | `identical` when both counts are 0, `ahead` when only `ahead_by` is above 0, `behind` when only `behind_by` is, `diverged` when both are, and an unrelated history is `diverged` |
| `stats` | `files`, `additions`, `deletions` over every changed file, the shape and the binary rule of `commits/{sha}`; exact even when `files` is cut |
| `files` | one row per changed file in git's diff order, at most `limits.MaxCompareFiles` |
| `filename`, `previous_filename` | the path at head, and the path at base on a rename, `null` otherwise |
| `status` of a row | from the letter of the raw record, in the table below |
| `additions`, `deletions`, `changes` | lines added and deleted, and their sum; 0, 0, 0 for a binary file |
| `binary` | `true` when numstat prints `-` for both counts |
| `sha` | the object id at head, `null` on a removed file, so a caller reads the file with `GET /v1/repos/{id}/blob/{sha}` and no tree call |

| Raw letter | Row `status` |
|---|---|
| `A` | `added` |
| `D` | `removed` |
| `M` | `modified`, including a change of mode alone, which numstat counts as 0 and 0 |
| `R<score>` | `renamed`, with `previous_filename` |
| `T` | `changed`: a type change, counted as one file, where `parseDiff` counts two |

`git diff -M` detects renames up to git's default `diff.renameLimit` of
1 000 files; the node's environment sets no other (`repo.Git.Env` sets
`GIT_CONFIG_NOSYSTEM` and `GIT_CONFIG_GLOBAL=/dev/null`). Past it git
warns on stderr, exits 0, and reports each rename as `removed` and
`added`, exactly as the patch of the same URL does.

`?path=` narrows `files` and `stats` the way it narrows the patch. A row
past `limits.MaxCompareFiles` is not written; the node sets
`Origo-Truncated: true`, the header `refs` and the patch already use, and
keeps counting, so `stats` and the row count it implies are exact.

### The subprocesses

Five, in sequence, under one `api.DefaultReadTimeout` budget and one
subprocess slot as every read takes, each value after
`--end-of-options`:

1. `rev-parse --verify -q` of base with `^{commit}`, then of head.
2. `merge-base <base> <head>`; exit 1 with empty output is `null`, any
   other failure is the read's failure.
3. `rev-list --left-right --count <base>...<head>`.
4. `diff --raw --numstat -z -M --no-abbrev --no-color <base> <head>
   [-- <path>]`, read whole as a stream. With `-z` the raw section comes
   first, `:<old mode> <new mode> <old id> <new id> <letter>` then the
   path, and a second path on a rename, each NUL terminated; the numstat
   section follows in the same order, `<added>\t<deleted>\t<path>` and,
   on a rename, `<added>\t<deleted>\t` then the two paths. The parser
   pairs the two sections by position and fails the read when the counts
   or the paths disagree.

A budget that runs out is 504 `operation_timeout` with
`details.operation: "compare"`, as for the patch. The JSON answer is
written after the stream ends, so it is never cut short mid-body.

### Verified against git

Checked with git 2.54.0 and with 2.47.3, the version of the runtime
image (`Dockerfile`, Debian trixie), with identical output on both:
`rev-list --left-right --count main...feature` printed `1\t2` for a
feature branch two commits ahead and one behind, and `0\t0` for
`main...main`; for an unrelated history `merge-base` exited 1 with no
output and `rev-list` still printed `2\t1`; `rev-list` refused a tree
side (`main^{tree}...feature`) with `Invalid symmetric difference
expression`, which is why both sides peel to a commit; the combined
`--raw --numstat -z -M` call printed seven raw records then seven
numstat records for a change holding a binary file (`-\t-`), a delete, a
type change (`T`), a rename (`R097`, numstat `1\t0\t` then both paths),
a mode change (`M`, numstat `0\t0`) and a path with a space, and
`--shortstat` agreed: 7 files, 4 insertions, 3 deletions. `-l1` printed
`warning: exhaustive rename detection was skipped due to too many files`
and exited 0.

### The client

`origoclient` gains `CompareFiles(ctx, id, base, head, path)`, which
sends the placeholder form with `format=json` and decodes the answer
into `CompareSummary`, the fields above. When the node answers
`text/x-diff` instead, because it predates this spec and ignored the
parameter, `CompareFiles` returns that body in a `Patch` field and
leaves the decoded fields empty; no version probe is made. `origocli` keeps `parseDiff` for that
case alone.

`origo diff` and `origo show` print their stat lines from `files` and
`diff`'s totals line from `stats`, one request per `-path` as today, and
fetch the patch only for `-p`. `origo diff` writes one header line on
standard error, `<base 7 hex>...<head 7 hex>  ahead <n>  behind <n>
merge base <7 hex or none>`, the way `origo cat` writes its header.
Past the row cap the bracketed line names the cut and `-path`; the
1 MiB stat wall disappears with the JSON answer, because the totals are
exact. `origo diff --json` prints the answer as the node sent it, the
rule spec 025 gives every `--json`. The skill gains one pipeline:

| Question | The pipeline |
|---|---|
| is this commit already in main, and how far behind is my branch | `origo diff --json main <sha> \| jq .ahead_by` is 0 when main holds the commit; `origo diff --json main <branch> \| jq .behind_by` is how far the branch is behind |

### Names the build adds

Each moves, with the build, into the table of the spec that owns its
kind: the parameter and the fields into spec 009's compare row, the
constant into spec 012's limits, the client names into spec 025.

| Name | Kind | Meaning |
|---|---|---|
| `format` | query parameter of the compare route | `patch` (default) or `json` |
| `base_commit`, `head_commit`, `merge_base_commit` | response fields | each `{"sha"}`; the last may be `null` |
| `status`, `ahead_by`, `behind_by` | response fields | the relation of the two commits |
| `filename`, `previous_filename`, `changes`, `binary`, `sha` | row fields | beside `status`, `additions` and `deletions` |
| `limits.MaxCompareFiles` | Go constant | 5 000 rows, the count `api.TreePageSize` holds on one tree page, about 550 KB at 110 bytes a row |
| `origoclient.CompareFiles`, `origoclient.CompareSummary` | Go identifiers | the client call and its answer |

No error code, header, route, variable or metric is added.

## Not in this spec

A list of the commits between base and head; `GET /v1/repos/{id}/commits`
pages them. Rows against the merge base rather than base. Paging past
`limits.MaxCompareFiles`. Copy detection (`-C`), so no row is
`copied`. Per-file patches inside the JSON. An `ETag` that outlives a
push for a comparison of two full object ids, which is the read API's
cache rule and not this route's.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `format=json` over a fixture with a rename, a binary file, a mode change, a type change, an add and a delete answers one row each with the status of the letter table, `previous_filename` on the rename alone, `sha` null on the delete, and `stats` equal to `git diff --shortstat` of the same pair | `internal/api`, `TestCompareJSONRows` | not built |
| Four pairs over one fixture answer `ahead`, `behind`, `diverged` and `identical` with the counts `rev-list` gives, and an unrelated history answers `diverged` with `merge_base_commit` null | `internal/api`, `TestCompareJSONStatus` | not built |
| A comparison of `limits.MaxCompareFiles` + 1 files answers `limits.MaxCompareFiles` rows, `Origo-Truncated: true`, and `stats.files` of every file | `internal/api`, `TestCompareJSONCapsRowsNotTotals` | not built |
| Without `format`, and with `format=patch`, the body is byte for byte today's golden patch; `format=xml` and `format=-x` are 400 with no subprocess; a tree side is 404 `ref_not_found`; a repeated request with the `ETag` is 304 with no subprocess | `internal/api`, `TestCompareFormatKeepsThePatch` | not built |
| For a fixture of 300 files whose patch is over 2 MiB, the JSON answer is under 64 KiB and counts all 300, where the patch's 1 MiB cut does not | `internal/api`, `TestCompareJSONIsSmallerThanThePatch` | not built |
| `origo diff` against a node with this spec makes one request per `-path`, prints its rows and totals from the JSON and the ahead and behind line on standard error, and against a stand-in that answers `text/x-diff` to `format=json` prints what it prints today | `internal/origocli`, `TestDiffReadsTheStatFromJSON` | not built |
| The suite reads the JSON answer from the fixture's first commit to its last: `ahead` by 2, `behind_by` 0, a binary row for `bin.dat` and an `added` row for `sub/b.txt` | `test/conformance`, `TestContract/009/compare-json` | not built |
| The skill teaches the containment pipeline | `internal/origocli`, `TestTheSkillsResidentCostIsItsFrontmatter` with `jq .ahead_by` among its marks | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 009 | the compare row states `format`, the JSON answer and its peel, and points here for the fields |
| 012 | `limits.MaxCompareFiles` joins the limits |
| 021 | the case `009/compare-json` |
| 025 | `origo diff` and `origo show` read the stat from JSON with `parseDiff` as the fallback; `origo diff --json`; the header line; the skill's pipeline; the paragraph that left a stat mode to a later spec |
| README | the Later item for issue #8 leaves the list |
