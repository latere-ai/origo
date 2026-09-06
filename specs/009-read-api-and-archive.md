---
title: "Read API and archive: refs, log, diff, tree, blob, tarball"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
affects: [internal/api/, internal/repo/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Read API and archive

## Overview

A platform shows a project's history, the diff between two versions, and a
file at a commit without cloning anything. A build wants the tree at one
commit as a tarball. This spec is the JSON and tarball surface over a
materialized repository, every operation served from the local copy after
the consistency check of spec 005.

## Current state

Spec 003 names the endpoints. `git` provides every operation; the work is
bounding and shaping them.

## Design

All paths under `/v1/repos/{id}`, action `read`.

| Method | Path | Result and bounds |
|---|---|---|
| GET | `refs?prefix=refs/heads/` | `[{"name", "sha", "peeled"}]`, 10 000 max |
| GET | `commits?ref=<ref\|sha>&path=&since=&until=&limit=&cursor=` | log newest first, `limit` ≤ 200, cursor is the last sha; each `{sha, parents, author, committer, message, trailers}` |
| GET | `commits/{sha}` | the commit plus `stats: {files, additions, deletions}` |
| GET | `compare/{base}...{head}?path=` | unified diff as `text/x-diff`, 1 MiB cap with `Origo-Truncated: true`, binary files listed not shown, rename detection on |
| GET | `tree/{sha}?path=&recursive=0` | `[{"path", "mode", "type", "sha", "size"}]`, 5 000 entries per page with cursor |
| GET | `blob/{sha}` | raw bytes, `Content-Type` sniffed, 50 MiB cap, `Range` supported |
| GET | `archive/{sha}.tar.zst` | `git archive` piped through zstd level 3, streamed, entries in git's order, mtime fixed to the commit time so the digest is reproducible, no `.git`, `Origo-Commit` header with the resolved sha |

Every operation runs a git subprocess with a 30 second budget, `--no-pager`,
and `GIT_DIR` set; concurrency is bounded at 32 per node with 429
`rate_limited` beyond. `sha` may be any reachable object or a ref name;
an unknown one is 404 `ref_not_found`. Responses carry `ETag` from the
index ETag plus the request so a consumer's cache revalidates cheaply.

The archive is what a build system should fetch for a single commit: one
request, no negotiation, reproducible bytes.

## Acceptance criteria

- Every endpoint has a golden test against a fixture repository with
  merges, renames, binaries, and a 60 MiB blob; the archive's sha256 is
  stable across two nodes.
- `compare` over a 5 MiB change returns 1 MiB with the truncation header
  in under one second.
- The archive of a 1 GiB tree streams with first bytes in under 200 ms.
- Paging on `commits` and `tree` returns every item exactly once.
