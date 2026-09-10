---
title: "Repository administration: rename, transfer, freeze, delete, undelete, import, export, and garbage collection"
status: testing
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/004-write-ahead-log.md
  - specs/006-compaction.md
  - specs/007-authentication-and-delegation.md
  - specs/008-push-events.md
  - specs/010-lfs.md
  - specs/016-security-and-threat-model.md
affects: [internal/api/, internal/httpgit/, internal/wal/, internal/repo/, internal/events/, test/e2e/, docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-09
author: changkun
---

# Repository administration

## Overview

Beyond clone, fetch, and push, a consumer needs the operations that make
a repository something it can manage over years: change its labels, move
it to another owner, freeze it, delete it and change its mind, bring an
existing repository in with its history, take a full copy out, and trust
that storage does not grow without bound. This spec fixes those
operations on the JSON API.

## Current state

`internal/api` serves create, get, rename (`PATCH`), delete, and
undelete (spec 003). The sweeper's purge (`internal/wal`) removes every
object under the repository prefix including `meta`, so an undeleted-too-late
repository answers 404. Nothing else below exists.

## Design

All under `/v1/repos/{id}`, action `admin` unless noted, and every one
of them asks the authorizer before it reads `meta` (spec 007,
"Authorization before lookup"). `meta` (spec 004) gains `frozen_at`,
`purged_at`, `importing_since`, `import_node`, `import_error`,
`imported_at`, `import_refs`, and `import_bytes`; spec 014 adds
`verified_at` and `verified_equal`.

The three operations spec 003 owns gain events and one answer:
`PATCH /v1/repos/{id}` changes the clone URL at once and the old URL
answers 404, never a redirect, because a redirect would let a stale URL
keep working past a transfer between owners; it emits `renamed`
whichever labels it changed, `owner` included. `DELETE /v1/repos/{id}`
emits `deleted`. `POST /v1/repos/{id}/undelete` restores within the
hold and emits `undeleted`, the one event of an undelete: the `push`
entry it commits (spec 004) produces no `push` event, by the payload
rules of spec 008; after the purge it answers 410 `gone`.

| Method | Path | Behaviour |
|---|---|---|
| POST | `/v1/repos/{id}/transfer` | `{"owner": "<new>"}`: the same operation as `PATCH` with `owner` alone, recorded as `transferred` instead of `renamed` so a consumer can act on a change of owner without inspecting a rename; the id never changes, which is what makes transfer cheap |
| POST | `/v1/repos/{id}/freeze` | sets `frozen_at`; writes refuse with `repo_frozen` while reads continue: a push is refused at `info/refs?service=git-receive-pack`, before the client uploads a pack, with the same shape spec 015 uses for an open write breaker (HTTP 200, the advertisement content type, and one `ERR repo_frozen: <the sentence below>` pkt-line, so git prints it as `remote error`), and again by the hook's verdict `reject repo_frozen: <sentence>` as defence for a client that sends `git-receive-pack` without the advertisement; the JSON API's write operations of spec 020 answer 403 `repo_frozen`; `GET /v1/repos/{id}` reports `frozen_at`; a second freeze is 409 `repo_frozen`; emits `frozen` |
| POST | `/v1/repos/{id}/unfreeze` | clears `frozen_at`; 200 whether or not it was frozen; emits `unfrozen` when it was |
| POST | `/v1/repos/{id}/import` | `{"source": "<https URL>", "token": "<optional bearer for the source>"}`; 202 at once, the import running in the background on the receiving node under a 30 minute budget and the repository size rule of spec 012 (`quota_bytes` over packs and LFS bytes) as the cap; the procedure is below; only `https` sources on the egress allow-list of spec 016 (`ORIGO_EGRESS_ALLOW`, else 400 `invalid_request` with `details.reason: "egress"`), fetched with `transfer.fsckObjects` on and no credential helper; pushes answer 409 `repo_importing` while `importing_since` is set; 409 `repo_not_empty` when the newest index names any entry; a second `POST` while one runs is 409 `repo_importing`; emits `imported` when done |
| GET | `/v1/repos/{id}/import` | `{"state": "running"\|"done"\|"failed", "refs", "bytes", "started_at", "finished_at", "error"}` from `meta`: `running` while `importing_since` is set, `failed` when `import_error` is set, `done` when `imported_at` is set, and 404 `import_not_found` when none of them is; `refs` and `bytes` are `meta`'s `import_refs` and `import_bytes`, written with `imported_at`: the length of the import entry's reference transaction and the bytes of the uploaded `.pack` files, the entry's `PacksBytes`, because the entry itself carries no pack and its `pack_bytes` is 0; the endpoint serves what `meta` holds, so `started_at` is null once an import has finished and `finished_at` is null for one that failed; action `read` |
| GET | `/v1/repos/{id}/export.bundle` | `git bundle create - --all` streamed as `application/x-git-bundle`: the complete repository in one portable file; action `read`; the subprocess runs under a 10 minute deadline (spec 012), and a bundle cut by the deadline is a truncated body the client refuses, never a status, because the headers are sent with the first byte: `git bundle verify` reads the header and the prerequisites, so it refuses a cut inside those, and a `git clone` from the bundle refuses a cut inside the pack; a repository with no reference is 404 `ref_not_found` (spec 003), because `git bundle create` writes no empty bundle and an empty repository is a state and not a failure of the node |
| GET | `/v1/repos/{id}/stats` | `{"size_bytes", "lfs_bytes", "packs", "entries_since_compaction", "refs", "pushed_at", "compacted_at"}` from the newest index and one listing of `lfs/`: `size_bytes` is the index object's `size_bytes` as spec 004 defines it, the bytes of the listed packs plus the pack bytes of the entries since the last compaction, which a `compact` commit sets and a push adds to, so the figure falls after a `gc`; `pushed_at` is the index object's `pushed_at` (spec 004), `compacted_at` the `at` of the newest `compact` entry the index names, null when it names none, read from that entry's head because `wal.IndexEntry` carries no time of its own; `refs` is the size of the index's reference map and counts `HEAD` with them; action `read` |
| POST | `/v1/repos/{id}/gc` | on the repository's compaction primary (spec 005), starts compaction now (spec 006) and waits for it at most 10 seconds: 200 `{"before": {"packs", "entries", "size_bytes"}, "after": {…}}` when it finished, else 202 `{"status": "running", "details": {"running": true, "started_at": "<RFC 3339>"}}`, the same answer when a compaction was already running, and the caller polls `stats`; never longer than 10 seconds, because an ingress cuts a longer response (spec 006); on any other node, forwards nothing and compacts nothing: it creates the request object of spec 006 and answers 202 `{"status": "scheduled", "details": {"primary": "<node name>", "within_seconds": 600}}`, and the primary's sweep compacts within 10 minutes; 429 `rate_limited` with `Retry-After` and `details.limit: "repository"`, `details.retry_after` when a compaction ran on the repository within the last hour, a threshold compaction counting the same as a `gc`, counted on `origo_rate_limited_total{limit="repository"}`; emits `compacted` from the node that compacted |

| Code | Status | Message | Details |
|---|---|---|---|
| `gone` | 410 | This repository was deleted and its hold has passed. It cannot be restored. | `id`, `purged_at` |
| `repo_frozen` | 403 on a write, 409 on a second freeze | This repository is frozen and does not accept pushes. | `frozen_at` |
| `repo_importing` | 409 | This repository is importing and does not accept pushes until the import finishes. | `started_at` |
| `repo_not_empty` | 409 | This repository already has history; import into an empty repository. | `seq` |
| `import_not_found` | 404 | No import has been started for this repository. | `id` |

The push path reads `meta` once, at `info/refs`, after the authorizer
allowed the request, and caches it for that request and the
`git-receive-pack` that follows it (keyed by the request's token hash
and repository id for 60 seconds, the advertisement's lifetime in git),
so `frozen_at`, `importing_since`, and `deleted_at` are checked at the
advertisement with no second `meta` read at the upload. The hook's
verdict reads that cache and the newest index under the write lock
before it commits, which is `internal/httpgit/frozen.go` with
`MetaTTL` as the window: `deleted_at` is on the index the verdict
acquires anyway and so is always fresh, while a freeze that lands
inside the window is caught at the pusher's next advertisement rather
than by the verdict. One read across the two requests and a verdict
reading `meta` again under the lock cannot both hold, and the read
count is what the criterion below measures.

### Import

One import is one log entry. The node clones the source with `git
clone --mirror --end-of-options <source>` into a scratch directory
under `ORIGO_DATA_DIR/spool/`, with the source token and the
pinned-address forward proxy passed through the environment the way
spec 016's egress row states them (`GIT_CONFIG_COUNT=2`, the `extraheader` and the `http.proxy` keys), so
neither is in a process listing or a log line, while
`-c transfer.fsckObjects=true` goes on the command line, because it is
no secret and spec 016 keeps the environment to what is (the clone
lands in a scratch directory that has no repository configuration of
spec 004 yet, which is why the option travels with the command); the proxy is what dials
the source and terminates its TLS, trusting the system roots and the
bundle `ORIGO_EGRESS_CA_BUNDLE` names (spec 016), which is how the
import fixture test below trusts its stub source's certificate. It then runs `git repack -a -d` and `git
fsck --connectivity-only`, uploads every pack under `objects/pack/`
under the log key the mapping of spec 004 gives its file name
(`pack-<hash>.pack` is `packs/<hash>.pack`, `.idx` first) the way
compaction does (spec 006, step 4), and commits one entry through `Log.Commit` in
the shape of a compaction: kind `compact`, no pack in the entry,
`Packs` = the uploaded packs, `PacksBytes` = the bytes of their `.pack`
files, which the commit sets as the index's `size_bytes` (spec 004),
`CompactedThrough` = the sequence before
its own (the index format of spec 004 lists entries in
`(compacted_through, seq]`, so an entry cannot fold itself), and a full
reference transaction creating every reference from zeros; `HEAD` is in
the transaction with `old` the symbolic value `index/0` holds and `new`
the source's, and is omitted when the two are equal, because a
transaction never names a reference that does not change. The index
object after it lists the packs and one entry, and any node materializes
it by step 2 of spec 004. There is no
batching and no ordering to get right: the mirror is the state, and
the history arrives as packs. The entry carries no subject and no
actor: the run outlives the request that started it, so the caller's
identity travels on the `imported` event instead. The whole run is bounded by 30 minutes
and by `quota_bytes` over the pack bytes; over either, nothing is
committed, the scratch directory is removed, and `import_error` says
which.

Import state lives in `meta` so any node can answer for it: the node
reads `meta`, answers 409 `repo_importing` when `importing_since` is
set, otherwise writes `importing_since` and `import_node` and starts;
at the end it rewrites `meta` with them cleared and `imported_at`,
`import_refs`, and `import_bytes` or `import_error` set. `meta` is rewritten unconditionally (spec 004), so
two imports that start within one read-write window both run; the
second one's `Log.Commit` loses its round to the first, its callback
refuses like a compaction's (spec 006), and it ends with
`import_error: "not empty"`, so the log never holds two imports. A node that reads an `importing_since` older than 45 minutes
whose `import_node` is not in the live set of spec 005 (a set only a
node holding the gossip secret can enter) clears both
and sets `import_error: "import node lost"`, so a node killed
mid-import leaves a repository that reports `failed` and accepts a new
import within 45 minutes. A node that starts also clears every lease
naming itself: it lists the repositories under its scratch directory,
which is where a running import leaves a directory, and for each whose
`meta` carries its own name in `import_node` writes `import_error:
"import node restarted"` with the lease cleared, then removes the
scratch directories, so a restart under the same name frees the
repository at once rather than after 45 minutes.

### Purge and the tombstone

The purge of spec 004 changes: it deletes every object under the prefix
except `meta`, which is rewritten with `purged_at`, and deletes the name
so it can be reused. `meta` is the tombstone that makes `gone` possible
and keeps the id unique forever (spec 003): `POST /v1/repos` with a
purged id is 409 `repo_exists`.

### Garbage collection guarantees

Storage per repository is bounded by compaction: after any compaction
the log holds the packs plus at most 64 entries, and unreachable objects
are dropped by `git repack` on the primary. Deleted repositories are
purged after the hold. LFS objects with no `lfs/verified/<oid>` marker (spec 010)
are deleted 7 days after upload. A weekly sweep lists the bucket prefix
and understands exactly the prefixes the deck defines: `repos/` with
`meta`, `wal/`, `index/`, `packs/`, and `lfs/` under each id (specs
004, 010), `names/` (004), `events/` (008), `gc/` (006), `sweep/` (this
spec), and `check/` (018); an object under those no index, marker, or
metadata names and older than a day is an orphan, and so is every
object under `origo/` outside them, which the sweep reports with its
key so an operator sees what wrote it. It reports the orphan count as
`origo_orphan_objects`, deletes the orphans after 7 days, and reports
the bytes under the prefix as `origo_storage_bytes`. The sweep is
`api.Sweeper` over a `*wal.Log` alone rather than a method on the
handler, so a test and the cluster job can run one over a bucket
without a cache, a guard, or a signer. The sweep runs on Sundays at 03:00 UTC on one
node: the one whose `ORIGO_NODE_NAME` sorts first in the live set of
spec 005 at that hour, so an installation of any size lists the
prefix once a week and a node that leaves hands the sweep to the next
name without coordination; the other nodes report the two gauges from
the last run they read from `origo/sweep/latest`, which the sweeping
node writes. An operator can bound total storage as the sum of
`size_bytes` and `lfs_bytes` over `stats`.

### Events

Each operation emits an event of its kind through `events.Emit` of
spec 008, after its write and before its response; that spec fixes the
key, the id (a UUID v5 of the repository, the kind, and `at`, so a
repeated emit is one event), delivery, retry, dead-letter, the cursor,
and repair for every kind, and this spec adds nothing to them. The
shared fields are `id`, `kind`, `repo`, `owner`, `slug`, `at`, and
`pusher` (`{"sub", "actor"}` of the caller, the same field and shape
as the `push` event, so a consumer decodes one identity for every
kind):

| Event | Extra fields |
|---|---|
| `renamed` | `from: {"owner", "slug"}`, `to: {"owner", "slug"}` |
| `transferred` | `from: {"owner"}`, `to: {"owner"}` |
| `frozen`, `unfrozen` | none |
| `deleted` | `purge_after` |
| `undeleted` | none |
| `imported` | `source` (the host and path, never the token), `refs`, `bytes`, the two figures `meta` holds as `import_refs` and `import_bytes` |
| `compacted` | `before`, `after` as in the `gc` response |

## Not in this spec

Import from a non-`https` source. Export of LFS objects. A sweep faster
than weekly.

## Acceptance criteria

- Each operation has a conformance case in `TestContract` (spec 021)
  covering the success path, the 403 on a caller without `admin`, and
  the state conflict: freeze twice (409 `repo_frozen`), undelete after
  the purge (410 `gone`), import into a repository with an entry (409
  `repo_not_empty`), a push during an import (409 `repo_importing`),
  `GET` of an import that was never started (404 `import_not_found`)
  (proposed: `internal/api`, `TestAdministrationOperations`).
- A push to a frozen repository is refused at `info/refs` with the
  `ERR repo_frozen` pkt-line and the sentence in git's `remote error`
  before any pack is sent, a `git-receive-pack` sent without the
  advertisement is refused by the hook's verdict with the same
  sentence, the push path reads `meta` once for the two requests, and
  a clone succeeds throughout (proposed: `internal/httpgit`,
  `TestFrozenRepositoryRefusesAtInfoRefs`, with a store that counts
  `meta` reads).
- An export of a fixture whose `git bundle` is held past the deadline
  by a stub is cut with the deadline lowered by an option in the test
  and the client's `git bundle verify` refuses the result (proposed:
  `internal/api`, `TestExportDeadline`).
- With three nodes, the weekly sweep runs on the node whose name sorts
  first and on the next name once that node is out of the live set,
  with a fake clock, the other nodes report the gauges from
  `origo/sweep/latest`, and an object under a prefix the sweep does not
  understand is reported by key and counted (proposed: `internal/api`,
  `TestOrphanSweepRunsOnOneNode`).
- An import of the 5 000-commit fixture the source stub of spec 013
  embeds (`test/stubs/source`, `git http-backend` behind TLS requiring
  the bearer), fetched by the stack from the in-cluster source
  `https://origo-stubs.origo.svc:8443/fixture.git`, which the nodes
  trust through the overlay's `ORIGO_EGRESS_CA_BUNDLE` and reach
  because the overlay names the host in `ORIGO_EGRESS_ALLOW` with its
  pinned `clusterIP` and the dialer's cluster exception of spec 016
  admits that address, completes within
  the budget as one `compact` entry, `import` reports `done` with
  `refs` equal to the source's reference count and `bytes` equal to
  the uploaded packs' bytes, the stub's request list read through its host port
  of spec 013's ports table shows every request carried the bearer,
  and a clone from Origo through the balanced port has the same
  `rev-list --all` as a clone of the source through that host port
  (proposed: `test/e2e`, `TestClusterImportFixture`, in the `e2e` job
  of spec 013 for its size, against the stack through
  `ORIGO_TEST_URL`; that the bearer appears in no process argument and
  no log line is asserted in-process by spec 014's
  `TestSourceTokenIsNeverLogged`, which runs an import and a verify
  with `AllowLoopback` of spec 016 against the same stub).
- A node killed during an import leaves `importing_since` set; another
  node reports `running` for 45 minutes with a fake clock, then
  `failed` with `import node lost`, and accepts a new import; a node
  restarted under the same name clears the lease at start-up with
  `import node restarted` (proposed: `internal/api`,
  `TestImportLeaseExpires`, `TestRestartClearsOwnImportLeases`).
- A `gc` on a node that is not the primary answers 202 naming the
  primary and compacts nothing there, a `gc` on the primary while a
  compaction runs answers 202 with `running: true` and its start time
  within 10 seconds, and a second `gc` on the primary within an hour of
  any compaction is 429 with `details.limit: "repository"` (proposed:
  `internal/api`, `TestGcRoutesToThePrimary`).
- `PATCH` with `owner` emits `renamed` and `transfer` emits
  `transferred`, both with `pusher` (`internal/api`,
  `TestRenameAndTransferEvents`: the operations are handlers, and a
  test of them inside `internal/events` cannot compile against a
  package that imports it).
- An export re-imported into a fresh repository has identical
  `rev-list --all` (proposed: `internal/api`, `TestExportRoundTrip`).
- After 500 pushes, a `gc` answers 429 `rate_limited` with
  `details.limit: "repository"`, because the threshold compactions the
  pushes triggered (spec 006) count as compactions within the hour;
  the test then waits for the background run the last threshold
  crossing scheduled to finish (`stats.entries_since_compaction` at
  most 64 and `compacted_at` set) and asserts on `stats` after it:
  `size_bytes` is within 10% of the pack size of a fresh `git clone
  --mirror`, which holds because the `compact` entry sets `size_bytes`
  to the bytes of the packs it lists (spec 004) rather than leaving
  the 500 pushes' bytes in the sum, and the weekly sweep run
  once reports `origo_orphan_objects` 0 (proposed: `test/e2e`,
  `TestClusterGcBoundsStorage`, in the `e2e` job of spec 013 against
  its stack, because 500 pushes take minutes).
- A purged repository answers 410 `gone` on every endpoint, its id is
  refused by `POST /v1/repos` with 409, and its name is accepted
  (proposed: `internal/wal`, `TestPurgeLeavesATombstone` for the
  objects and the name; `internal/api`, `TestPurgedRepositoryIsGone`
  for the responses).
- Every operation delivers one event of its kind with the listed fields
  (`internal/api`, `TestAdministrationEvents`).

## Outcome

Built on 2026-09-09 in eleven commits: the five codes, the `meta`
fields with the purge tombstone, the three operations with their
events, the frozen push path, `stats` and `gc`, the bundle export, the
import, the orphan sweep, the node's wiring, the cluster tests, and the
documentation.

| Criterion | Test |
|---|---|
| each operation's success path, its 403, and its state conflict | `internal/api`, `TestAdministrationOperations` (transfer, freeze twice, unfreeze, `stats`, `gc`); `TestPurgedRepositoryIsGone` (410 `gone` after the purge); `TestImportRefusals` (404 `import_not_found`, the egress refusal, the 403); `TestExportRoundTrip` (409 `repo_not_empty`); `TestImportLeaseExpires` (409 `repo_importing`); `internal/httpgit`, `TestFrozenRepositoryRefusesAtInfoRefs` (a push during an import). Spec 021's `cases019` carries the success paths and three of the conflicts (freeze twice, `repo_not_empty`, `import_not_found`); the 403 on a caller without `admin` is in none of them, the `repo_importing` push is asserted only when the import is still running, and 410 `gone` is proved by the code table, which the decision row of `specs/README.md` fixed |
| a push to a frozen repository is refused at `info/refs` with the `ERR repo_frozen` pkt-line before any pack is sent, the hook's verdict refuses one sent without the advertisement, the push path reads `meta` once, and a clone succeeds throughout | `internal/httpgit`, `TestFrozenRepositoryRefusesAtInfoRefs`, with a store that counts `meta` reads |
| an export held past the deadline is cut and the client refuses the result | `internal/api`, `TestExportDeadline`; `TestExportServesTheWholeRepository` for the whole file |
| with three nodes the weekly sweep runs on the name that sorts first and on the next once it leaves, the others report the gauges from `origo/sweep/latest`, and an object under a prefix the sweep does not understand is reported by key and counted | `internal/api`, `TestOrphanSweepRunsOnOneNode`, `TestSweepLoopRunsInItsHour`, `TestSweepReportFailuresAreLogged`, `TestSweepLeavesARepositoryItCannotRead` |
| an import of the 5 000-commit fixture from the in-cluster source completes as one `compact` entry, reports `done` with the source's reference count and the uploaded packs' bytes, every request carried the bearer, and a clone matches the source's `rev-list --all` | `test/e2e`, `TestClusterImportFixture`, in the `e2e` job |
| a node killed during an import leaves the lease, another node reports `running` for 45 minutes and then `failed` with `import node lost` and accepts a new import; a node restarted under the same name clears its own at start-up | `internal/api`, `TestImportLeaseExpires`, `TestRestartClearsOwnImportLeases`, `TestClearLeaseFailureIsLogged` |
| a `gc` away from the primary schedules and compacts nothing, one on the primary while a run goes answers 202 with the start time, and a second within an hour of any compaction is 429 with `details.limit: "repository"` | `internal/api`, `TestGcRoutesToThePrimary`, `TestStatsFailures` |
| `PATCH` with `owner` emits `renamed` and `transfer` emits `transferred`, both with `pusher` | `internal/api`, `TestRenameAndTransferEvents` |
| an export re-imported into a fresh repository has identical `rev-list --all` | `internal/api`, `TestExportRoundTrip` |
| after 500 pushes a `gc` is 429 `rate_limited` with the repository limit, `size_bytes` after the background run is within 10% of a fresh mirror's packs, and one sweep reports no orphan | `test/e2e`, `TestClusterGcBoundsStorage`, in the `e2e` job |
| a purged repository answers 410 `gone` on every endpoint, its id is refused by `POST /v1/repos`, and its name is accepted | `internal/wal`, `TestPurgeLeavesATombstone`; `internal/api`, `TestPurgedRepositoryIsGone` |
| every operation delivers one event of its kind with the listed fields | `internal/api`, `TestAdministrationEvents`, `TestRenameAndTransferEvents`, `TestExportRoundTrip` (the `imported` event) |

Divergences and interpretations, each kept, with the reason:

- **The push path's `meta` is cached for 60 seconds and the hook's
  verdict reads that cache.** The Design asks for one `meta` read
  across the advertisement and the `git-receive-pack` that follows it
  and, in the same sentence, for the verdict to read `meta` under the
  write lock; a normal push cannot do both in one read. The criterion
  is the testable sentence and it counts the reads, so the cache is
  what the verdict reads too: `deleted_at` stays fresh, because it is
  on the index the verdict acquires anyway, while a freeze that lands
  inside the window is caught at the pusher's next advertisement rather
  than by the verdict. `internal/httpgit/frozen.go` holds the rule and
  `MetaTTL` names the window.
- **`TestRenameAndTransferEvents` and `TestAdministrationEvents` live
  in `internal/api`, not the proposed `internal/events`.** The
  operations are handlers, and `internal/api` imports `internal/events`,
  so a test of the handlers inside that package cannot compile; an
  external test package there would rebuild the whole handler harness
  for nothing. The events themselves are asserted through the stub sink
  either way.
- **`git bundle verify` reads the header and the prerequisites, never
  the pack.** The criterion's sentence therefore holds for a cut inside
  the bundle's signature, which `TestExportDeadline` asserts; a cut
  inside the pack, the likelier one, is refused by `git clone` from the
  bundle, which the same test asserts beside it.
- **An export of a repository with no reference is 404
  `ref_not_found`.** `git bundle create` refuses to write an empty
  bundle, and that is a state of the repository rather than a failure
  of the node, so it is not a 503.
- **A frozen push is the `ERR` pkt-line and an importing push is 409.**
  The freeze row states the pkt-line shape; the import row states 409
  `repo_importing`, which is what a consumer driving a migration reads.
  Both travel as the hook's verdict for a `git-receive-pack` sent
  without an advertisement.
- **The import entry carries no subject.** The run outlives the request
  that started it, so the entry's `subject` and `actor` are empty and
  the identity of the caller travels on the `imported` event instead,
  which is where the Design puts it.
- **The orphan sweep is `api.Sweeper`, built over a `*wal.Log`
  alone.** `TestClusterGcBoundsStorage` runs one sweep over the stack's
  bucket from the runner, which a method on the handler would have
  needed a cache, a guard, and a signer for.
- **`stats.refs` counts `HEAD`**, because it is the size of the index's
  reference map, which is what the index holds.

Spec defects found while building, each implemented as written and left
for the spec that closes them:

- `wal.IndexEntry` carries no timestamp, so `compacted_at` and the
  `gc` hour both cost one `GET` of the newest `compact` entry's head.
  The object is two lines, a `compact` entry having no pack of its own,
  so the read is cheap; a field on the index row would be a spec 004
  change and is not made here.
- `started_at` is null once an import finishes and `finished_at` is
  null for one that failed, because `meta` clears `importing_since` at
  the end and sets `imported_at` on success alone. The endpoint serves
  what `meta` holds; a spec that wants both figures after the fact adds
  a field.

Items this spec closes for others:

- Spec 004: the purge no longer removes `meta`. It rewrites it with
  `purged_at` and deletes the name, which is what makes `gone` possible
  and keeps the id unique forever;
  `TestSweepRemovesOrphansAndKeepsWhatAnIndexNames` expects the
  tombstone as the one surviving key. Spec 004's Outcome records it.
- Spec 006: `compact.Manager.GC`, `compact.Figures`, and
  `compact.Manager.Primary` are consumed by the `gc` endpoint, and
  `compacted_at` is the `at` of the newest `compact` entry the index
  names, as that Outcome's item said.
- Spec 010: the orphan sweep and `lfs_bytes` are built here. An LFS
  object with no `lfs/verified/<oid>` marker is deleted 7 days after
  its upload, and `stats.lfs_bytes` is the sum `limits.LFSBytes`
  measures.
- Spec 016: that an `import` runs through the pinned dialer with
  `-c transfer.fsckObjects=true` on the command line, and that neither
  the source URL nor its bearer reaches git's arguments, is asserted by
  `internal/api`, `TestExportRoundTrip`.

Deferred: the conformance half of the first criterion is spec 021's
`TestContract`, which owns the code table and the stub. `cases019` is
in the tree and registered, so what is deferred is now the part of it
that is missing rather than the whole: a 403 case for each operation
of the row table, and the `repo_importing` push asserted whatever the
import's state, which `cases019.go` reads inside an `if` today. Spec
014's `TestSourceTokenIsNeverLogged` asserts in-process that the
bearer appears in no log line, and landed on 2026-09-09.

Items left to another builder, found by the eighteenth round's second
review:

- Spec 020's builder, who owns `internal/api`: the `compacted` event of
  the Events table is asserted in no unit test. `gc.go` emits it, and
  neither `TestAdministrationEvents` nor `TestGcRoutesToThePrimary`
  installs a sink over a `gc`, so the last criterion covers seven of
  the eight kinds; `cases019` asserts it on the stack only when the
  `gc` answered 200 and a sink is configured. The same round: `pusher`,
  a shared field of every kind, is checked for `renamed`,
  `transferred`, `imported`, and `undeleted` and not for `frozen`,
  `unfrozen`, or `deleted`, because `waitEvent` reads `kind`, `repo`,
  `id`, and `at` alone.
- Spec 021's builder, who owns `test/conformance`: the two `cases019`
  gaps above.

Items for `latere.ai/x/pkg`: none. `pkg/cache` is the push path's
`meta` cache, `pkg/wait` the sweep's ticker, and `pkg/hostmatch` the
egress list through spec 016's dialer.

Fixed after the first push, in the packages this spec owns. Three of
019's tests failed the `race` gate of the push run 34300095179 on
timing rather than on an assertion, and the detector found a fourth
defect beside them. `TestExportDeadline` spent its 2 second export
budget on a fake git that made the whole bundle before its first
write, so the deadline raced the making of the bundle instead of
cutting a body already begun; the bundle is made before the request
now and the budget is 5 seconds, which bounds the test's wait alone.
`TestGcRoutesToThePrimary` and `TestStatsFailures` asserted a finished
compaction inside `compact.GCWait`, the 10 seconds that bound a
response an ingress would cut and not the run, against a repack
costing 1.4 seconds under `-race` on an idle machine in a package the
runner ran 3.4 times slower; the harness waits a minute, so the
assertion depends on the run and not on the runner. The detector then
reported `TestAdministrationEvents` assigning to the clock the event
dispatcher's goroutine reads through the store, which five other tests
did the same way; `testClock` in the harness is the one clock a test
moves, under a mutex.

One defect the dispatched run 34331528967 found, in
`internal/httpgit`: with the read breaker open,
`TestClusterDegradedStorage/unreachable` (spec 015's criterion) read
the JSON envelope where the criterion reads the `ERR` pkt-line. The
`meta` read this spec put between the write breaker's check and the
lease answered a storage failure through `storageError` while the
lease's own refusal went to `refuseAdvertisement`, so a push whose
`meta` was not in the 60 second cache saw a shape git shows no user.
`Handler.advertisementError` is the one answer for a storage failure
before the pack now, an open breaker the pkt-line and anything else
the envelope, and both reads go through it;
`TestPushAdvertisementRefusesWithoutACachedMeta` drives the cache miss
and fails on the old path.

Closed for spec 016, whose seventeenth round left them to this
spec's builder: `TestClusterPodSecurityContext` reads the CPU request
at the base's 250m or the overlay's 50m in place of "a request is
set"; `TestValidLabel` admits `a..b`; and the dialer applies a
`host=address` pin wherever its address is, which decides 016's open
pinned-address question with a third rule and
`TestEgressPinAppliesOutsideClusterRanges`. Spec 016's Design and
Outcome, spec 002's variable row, and the README's decision row carry
the rule.

Stack proof: `TestClusterImportFixture` and `TestClusterGcBoundsStorage`
passed in the `e2e` job of the dispatched run 34335095125 of
`verify.yml` on main, at commit `8007a06`, with every other job of the
run green.

The spec stays at `testing` by the lifecycle rule of
`specs/README.md`: what remains is a criterion another spec owns the
test for. The import criterion's second half, that the source bearer
appears in no process argument and no log line, landed with spec 014 on
2026-09-09: `TestSourceTokenIsNeverLogged` in `internal/api` runs an
import and a verify against the stub source and asserts the bearer is
in no line the node wrote, no git argument, no event, and no request
the stub recorded. The first criterion's conformance cases are spec
021's `TestContract`, the way specs 003, 004, and 016 wait.

Those cases are now written and green against the stack. The `e2e` job
of the dispatched run 34358421294 of `verify.yml` on main, at commit
`e9e516e`, ran spec 021's suite against the kind stack and passed, and
`test/conformance/cases019.go` holds one case per row of this spec's
operations, transfer, freeze and unfreeze, `stats`, `gc`, the purge
tombstone and the 410 `gone` among them. The step ran without `-v`, so
no case is named in the log; what makes the `ok` a statement about
every case is `TestContract` itself, which fails on any failed case, on
anything skipped while a `Fault` is wired, and on a non-empty
`Report.Unverified`. The earlier dispatched run 34353736553 failed on
`019/gc`. No case of `test/conformance/cases019.go` changed between the
two commits; the one change to the stack run in between is `e2c62d9`,
which points the `e2e` job's conformance step at node 1 of spec 013's
ports table, where a bucket is per node, which is what the case's 429
on a second `gc` reads.

What is left of the criterion is therefore the run of the same cases
against the installation `ORIGO_LIVE_URL` names, which spec 021 owns.

The first release ran on 2026-09-10 and did not produce the live run.
The `live` job of the tag run 34461460766 of `v0.1.0` executed and its
`TestContract` skipped, because the repository carries no
`ORIGO_LIVE_URL` and no `ORIGO_LIVE_TOKEN` secret and nothing answers
at `https://git.latere.ai`; the job's log reads `nothing answers at
ORIGO_TEST_URL` then `--- SKIP: TestContract (0.00s)`. A skipped test
passes, so the job is green, and this spec does not read that green as
the run. Spec 017's Outcome records the limit. What closes it: the two
secrets set on the repository, with an installation behind the URL, and
a tag or a re-run of that job.
