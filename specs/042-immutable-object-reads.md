---
title: "Immutable object reads: a read named by a full object id keeps its ETag and may be cached"
status: drafted
track: infra
depends_on:
  - specs/009-read-api-and-archive.md
  - specs/019-repository-administration.md
  - specs/025-agent-client.md
affects: [internal/api/, internal/origoclient/, internal/origocli/, docs/api.md, docs/cli.md, skills/origo/SKILL.md, test/conformance/, specs/009-read-api-and-archive.md, specs/025-agent-client.md, specs/README.md]
effort: small
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Immutable object reads

## Overview

Every read answers `ETag: "<seq>"`, the sequence of the index the copy
holds (spec 009). A blob, a tree, or a commit named by its full object id
cannot change, yet any push to the repository gives it a new tag, so a
client that revalidates after a push receives the whole body again. A
repository that takes ten pushes a second (spec 005's ceiling) changes the
tag of every such read ten times a second.

This spec makes a read that names a full object id answer that id as its
`ETag`, with `Cache-Control: private, immutable, max-age=31536000` (one
year, `api.ImmutableMaxAge`), and a 304 to `If-None-Match` after the same
authorization and lease as today. It gives the agent client of spec 025 an
opt-in cache on disk that serves such reads across the processes of an
agent's turns. The gain is modest: one request and one blob's bytes per
repeated file read, nothing for a first read, and no change to the bytes a
model reads.

## Current state

Not built. `Handler.open` (`internal/api/read.go`) authorizes, validates,
takes the lease, sets `ETag` to `"<seq>"` from the copy's sequence, and
answers a matching `If-None-Match` with 304 before it takes a subprocess
slot; no read route sets `Cache-Control`. `target` reads a name from the
`{sha}` segment or, beside `-`, from `?ref=`, `?base=`, or `?head=`.

`origoclient` sends every name in the query beside `-`. `origo cat`
(`runCat`, `internal/origocli/read.go`) is two requests: `Client.File`
lists the parent directory at the named revision, then `Client.Blob`
fetches the entry by its full id. Spec 025 cut revalidation because a
command is one process (its validation finding 13) and promises twice that
the command keeps no cache on disk.

No repository can use SHA-256: `wal.ParseIndex` accepts a reference value
only as 40 lower-case hexadecimal digits (`shaRe`, `internal/wal/format.go`)
and `Cache.initBare` runs `git init --bare` with git's default format.
Spec 009's grammar admits 64 digits, which no repository reaches.

The archive's prefix is `<slug>-<7 hex>` from `meta` (`Handler.archive`),
and a rename rewrites `meta` without committing an index object
(`wal.Log.Rename`), so today a revalidation after a rename answers 304 for
an archive whose bytes changed.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| The sequence for every read | today | a push changes the tag of content that cannot change |
| A 304 before the lease | answer from the request alone | a deleted repository answers 404 and a purged one 410 (spec 019), which the lease decides |
| `public` | shared caches may store it | authorization is per caller; a shared cache would serve one caller's read to another |
| The client cache on by default | the user cache directory | spec 025 promises the command writes no file, and an agent's sandbox may have a read-only or shared home; the operator who starts the agent opts in |
| Full-id tags, `private, immutable`, and an opt-in client cache | this spec | chosen |

## Design

### Which reads are immutable

A read names a full object id when its name, from the segment or the
query, matches `^[0-9a-f]{40}$`. Checked with git 2.54.0: such a name
resolves to that object even when a branch of the same name exists (git
warns that the reference name is ambiguous and ignores it), so no
reference can redirect it. An upper-case id resolves too, and keeps the
sequence tag under this rule.

| Route | Immutable when | `ETag` |
|---|---|---|
| `GET /v1/repos/{id}/blob/{sha}` | the name is a full id | `"<id>"`: the bytes are the object's, `Range` included |
| `GET /v1/repos/{id}/tree/{sha}` | the name is a full id | `"<id>"`: `path`, `recursive`, and `cursor` are part of the URL |
| `GET /v1/repos/{id}/commits/{sha}` | the name is a full id | `W/"<id>"`: git computes the stats rather than storing them, and a strong tag would promise the same bytes from nodes running two git versions during a rolling upgrade |
| `GET /v1/repos/{id}/compare/{base}...{head}` | both names are full ids | `W/"<base>...<head>"`, for the same reason |
| `GET /v1/repos/{id}/commits` | `ref` is a full id | `W/"<id>"`: the walk from one commit is fixed, its other parameters in the URL |
| GET /v1/repos/{id}/blame/{sha} of spec 039, once built | the name is a full id | `W/"<id>"` |
| `GET /v1/repos/{id}/archive/{sha}.tar.gz` | never | `W/"<id>-<slug>"` for a full id, `"<seq>-<slug>"` otherwise, with `Cache-Control: private, no-cache`: the prefix follows a rename and the gzip bytes are reproducible for one git version only |
| `GET /v1/repos/{id}/refs`, and any read by name | never | `"<seq>"` as today |

An immutable read carries `Cache-Control: private, immutable, max-age=<n>`,
`<n>` being `api.ImmutableMaxAge` in seconds, on its 200, its 206, and its
304. The promise holds within one contract number: a client keys what it
stores by `Origo-Contract`, which spec 003 sends on every response.

### Order of the checks

1. Authorization, as today: a caller without `read` gets 401, 403, or
   404, never 304, whatever `If-None-Match` says.
2. Validation of the request, as today.
3. The lease: deleted is 404, purged 410, unavailable 503; a cold node
   materializes first, as for today's 304.
4. `If-None-Match` against the tag above, spec 009's comparison: 304, no
   subprocess, no slot. The archive reads `meta` for its slug first.
5. Otherwise the read runs as today. A full id the repository does not
   hold is 404 `ref_not_found`: every route resolves with a peel, and
   `git rev-parse --verify` of an absent id fails with a peel (exit 1)
   while it prints the id without one (exit 0).

The 304 does not check that the object is in the repository: a client
holds a response for that URL only if the node served one, and a 304 hands
it nothing it does not have.

### The agent client's cache

`origo` keeps a cache when ORIGO_CACHE_DIR names a directory, and writes no
file when it is unset, which keeps spec 025's promise by default.
`origocli` reads the variable and hands `origoclient` a cache through
`Config.Cache`, because `origoclient` reads no environment variable (spec
025's package table).

| Rule | Value |
|---|---|
| what is stored | a 200 to a `GET` without `Range` whose `Cache-Control` carries `immutable` and a `max-age`, with a body of at most `origoclient.CacheEntryBytes` |
| with it | the body, `ETag`, `Content-Type`, `Origo-Commit`, `Origo-Truncated`, `Origo-Contract`, the time stored, the `max-age`, and the SHA-256 of the bearer |
| key | SHA-256 of the installation URL, a newline, and the path with its query; one file per key under a directory of its first two hex digits, mode 0600 under a 0700 root |
| a hit | an entry younger than its `max-age` whose `Origo-Contract` is the client's: served with no request when the bearer's digest matches; otherwise sent as `If-None-Match`, served on 304 with the digest rewritten, replaced on 200, so a second credential on a shared machine gets the bytes only once the node has authorized it |
| bound | `origoclient.CacheBytes` in total; a write past it deletes the files least recently used, by modification time, which a hit sets, until three quarters of the bound remain |
| concurrency | each entry written to a temporary file and renamed; a file that does not parse is a miss and is deleted |

What it saves: `origo cat` still lists the parent directory by revision on
every call, a read by name whose tag is the sequence, and only the blob
comes from the cache, so a repeated read of an unchanged file costs one
request instead of two and no blob bytes. `origo show` repeats its commit
read and takes its diff, a compare of two full ids, from the cache;
`origo diff` of two full ids costs no request. A first read, a read by
branch name, and every write cost what they cost today, and the command
prints the same lines, so a model reads the same bytes.

### Names the build adds

| Name | Kind | Meaning |
|---|---|---|
| ORIGO_CACHE_DIR | client variable | unset by default, no cache; a path names the directory `origo` creates and keeps its cache in |
| `api.ImmutableMaxAge` | Go constant | 365 days, the `max-age` of an immutable read |
| `origoclient.CacheBytes` | Go constant | 64 MiB, the bound of the client's cache |
| `origoclient.CacheEntryBytes` | Go constant | 1 MiB, the largest body the client stores |
| `origoclient.Config.Cache` | Go field | the cache `origocli` passes in; nil keeps none |

The build moves ORIGO_CACHE_DIR into spec 025's variable table, which
`docs/cli.md` mirrors.

## Not in this spec

Revalidating reads by name: their tag stays the sequence, and spec 025's
reason to cut that cache still holds for them. A shared cache in front of
the node. SHA-256 repositories.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A blob and a tree read by full id, in the segment and in `?ref=`, answer `ETag: "<id>"` and the `Cache-Control` line, keep both after a push, and answer `If-None-Match` with 304 carrying both and no subprocess started | `internal/api`, `TestImmutableReadsKeepTheirETag` | not built |
| A branch whose name is 40 hex digits does not change what a full-id read serves, and an upper-case id answers the sequence tag | `internal/api`, `TestImmutableNeedsALowercaseFullID` | not built |
| A caller without `read` sending the right `If-None-Match` is refused as today, a deleted repository is 404, and a purged one 410, never 304 | `internal/api`, `TestImmutable304ComesAfterAuthorization` | not built |
| `commits/{sha}`, `compare`, and `commits` with a full-id `ref` answer weak tags; an archive's tag changes on a rename and an old tag is answered 200 | `internal/api`, `TestWeakTagsAndTheArchive` | not built |
| With ORIGO_CACHE_DIR unset, `origo cat` writes no file; set, a second `origo cat` of an unchanged file sends one request, a different bearer sends `If-None-Match` and is served on 304, and a body over `origoclient.CacheEntryBytes` is not stored | `internal/origocli`, `TestCatSecondReadUsesTheCache` | not built |
| Writes past `origoclient.CacheBytes` evict the least recently used entries to three quarters of the bound, and two processes writing one key leave a parsable entry | `internal/origoclient`, `TestCacheEviction` | not built |
| The suite: `042/immutable` reads a blob by full id, pushes, and revalidates to 304 | `test/conformance`, `TestContract/042/immutable` | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 009 | the sentence on `ETag` gains the full-id rule, the weak tags, the archive's tag, and `Cache-Control` |
| 025 | the variable table gains ORIGO_CACHE_DIR; Byte defaults and Not in this spec say the command keeps no cache on disk unless it is set |
