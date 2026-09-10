# The Origo API

For whoever writes against an Origo installation. Everything here is
generated from the design specs by `make docs`, so a name on this page is a name
the implementation is held to; run it again after a spec changes.

This is contract version 1. Every response carries `Origo-Contract: 1`.
A later version keeps every field and every code of this one and may add
more, so a client reads the fields it knows and ignores the rest. A
breaking change raises the number, and the upgrade notes say what moved.

Every route is authenticated: send `Authorization: Bearer <token>` on
every request, including the git routes. How to get a token, and what a
token may do, is your installation's own configuration; see
[`install.md`](install.md) for the operator's side and
[`configuration.md`](configuration.md) for the variables behind it.

## Endpoints

Every path below is under the base URL of the installation. `{repo}` is either `r/{id}.git` or `{owner}/{slug}.git`; both address the same repository, and the id form never changes.

Defined by [002 repository scaffold](../specs/002-repository-scaffold.md).

| Method | Path | Body |
|---|---|---|
| GET | `/livez` | 200 `ok`, touches no dependency |
| GET | `/readyz` | 200 `ok` when every check passes; 503 `not ready: <check>: <error>` otherwise, and `not ready: draining` during shutdown; text, the developer register. The `storage` check passes while a storage breaker is open, once the bucket has answered this replica at least once since it started, because such a node still serves warm repositories stale and refuses writes with a code, and taking it out of rotation would lose those reads (spec 015) |
| GET | `/version` | `{"version","commit","build_time"}` from `internal/version`, set by `-ldflags` |
| GET | `/metrics` | the `latere.ai/x/pkg/metrics` registry in the Prometheus text format |

Defined by [003 protocol contract](../specs/003-protocol-contract.md).

| Method | Path | Request | Response |
|---|---|---|---|
| POST | `/v1/repos` | `{"id", "owner", "slug", "default_branch"}`; `default_branch` defaults to `main` and must be a valid branch name | 201 with the representation; 409 `repo_exists` on a duplicate id or a taken `owner/slug`; 400 `invalid_request` |
| GET | `/v1/repos/{id}` |  | 200 `{"id", "owner", "slug", "default_branch", "size_bytes", "head", "updated_at"}`; 404 `repo_not_found` for an unknown, malformed, or deleted id; 410 `gone` for a purged id (spec 019). Other specs add fields to the representation and own them: `pushed_at` (spec 009), `frozen_at` (spec 019), `verified_at` and `verified_equal` (spec 014); each is null until its operation ran |
| PATCH | `/v1/repos/{id}` | any subset of `{"owner", "slug", "default_branch"}` | 200 with the representation; a rename takes effect at once and the old URL answers 404; `default_branch` moves `HEAD` through the log; 409 `repo_exists` when the name is taken |
| DELETE | `/v1/repos/{id}` |  | 202 `{"id", "deleted_at", "purge_after"}`; every other endpoint answers 404 from then on and 410 `gone` once the objects are purged after the 7 day hold (spec 019); repeated on a deleted repository, 202 with the original times |
| POST | `/v1/repos/{id}/undelete` |  | 200 with the representation within the hold; 410 `gone` after the purge (spec 019) |

Defined by [003 protocol contract](../specs/003-protocol-contract.md).

| Method | Path | Behaviour |
|---|---|---|
| GET | `/{repo}/info/refs` | `?service=git-upload-pack` or `git-receive-pack`; any other value 400 `invalid_request`; protocol v2 advertised when the client sends `Git-Protocol: version=2`, v0 otherwise |
| POST | `/{repo}/git-upload-pack` | a fetch or clone; `Content-Encoding: gzip` accepted |
| POST | `/{repo}/git-receive-pack` | a push; `Content-Encoding: gzip` accepted; the body is spooled to disk before git runs |

Defined by [007 authentication and delegation](../specs/007-authentication-and-delegation.md).

| Method | Path | Behaviour |
|---|---|---|
| POST | `/v1/repos/{id}/tokens` | action `admin`; body `{"scope": "read"\|"write", "ttl": <seconds, 1 to 3600>}`; 201 `{"token": "<jwt>", "expires_at": "<RFC 3339>"}`; 400 `invalid_request` for another scope or ttl |
| GET | `/.well-known/jwks.json` | the public key set Origo signs with, no token required; unauthenticated like `GET /readyz` and `GET /version` (spec 002), and the only unauthenticated path that is part of the contract |

Defined by [009 read api and archive](../specs/009-read-api-and-archive.md).

| Method | Path | Response and bounds |
|---|---|---|
| GET | `/v1/repos/{id}/refs` | `?prefix=refs/heads/` (default `refs/`), a string prefix of the full name, so `refs/tags/v1` lists `refs/tags/v1` and `refs/tags/v1.2`; `[{"name", "sha", "peeled"}]` from `git for-each-ref` over the directory of the prefix, the node filtering the names, because a `for-each-ref` pattern matches a whole name or a directory and `refs/tags/v1` as a pattern would list nothing; `peeled` the tag's target or null; at most 10 000 entries, `Origo-Truncated: true` past that |
| GET | `/v1/repos/{id}/commits` | `?ref=<sha or name>` (default `HEAD`) `&path=&since=&until=&limit=&cursor=`; newest first in `git rev-list` order; `limit` default 50, at most 200; `since` and `until` RFC 3339; `cursor` is the sha of the last commit of the previous page: the node walks `git rev-list --end-of-options <ref>` from the start, discards output up to and including the cursor, and returns the next `limit` commits, so a page is exact whatever the graph and `--skip` is never used; a cursor not in the walk is 400 `invalid_request` with `details.reason: "cursor"`; `{"commits": [{"sha", "parents", "author": {"name", "email", "at"}, "committer": {…}, "message", "trailers": [{"key", "value"}]}], "next_cursor"}`, `trailers` from `%(trailers:only,unfold)` in the format of the same `rev-list`, git's own trailer parser as `interpret-trailers --parse` runs it, so a page is one subprocess and not one per commit, in the order they appear with duplicate keys kept, empty when there are none; a repository whose `ref` does not exist because it has no commit yet (the default `HEAD` of an empty repository) answers 200 with `commits: []` and `next_cursor: null`, while a named `ref` that does not exist in a repository with history is 404 `ref_not_found` |
| GET | `/v1/repos/{id}/commits/{sha}` | the commit as above plus `"stats": {"files", "additions", "deletions"}` from `git show --numstat`; binary files count as a file with 0 lines |
| GET | `/v1/repos/{id}/compare/{base}...{head}` | `?path=&base=&head=`; `text/x-diff` from `git diff -M --no-color --end-of-options <base> <head> -- <path>`; at most 1 MiB, cut at a file boundary with `Origo-Truncated: true`; binary files listed as `Binary files differ` |
| GET | `/v1/repos/{id}/tree/{sha}` | `?path=&recursive=0&cursor=`; `{"entries": [{"path", "mode", "type", "sha", "size"}], "next_cursor"}` from `git ls-tree -l`; 5 000 entries per page, `cursor` the last path |
| GET | `/v1/repos/{id}/blob/{sha}` | raw bytes of a blob with `Content-Type` from `http.DetectContentType` over the first 512 bytes and `Content-Length`; `Range` honoured; a blob over 50 MiB without a `Range` of at most 50 MiB is 413 `blob_too_large` |
| GET | `/v1/repos/{id}/archive/{sha}.tar.gz` | `git archive --format=tar.gz --prefix=<slug>-<7 hex>/ <sha>` streamed; entries in git's tree order, mtime the commit time, no `.git`; reproducible for one git version |

Defined by [010 lfs](../specs/010-lfs.md).

| Method | Path | Behaviour |
|---|---|---|
| POST | `/{repo}/info/lfs/objects/batch` | `{"operation": "download"\|"upload", "objects": [{"oid", "size"}], "transfers": ["basic"]}`, body at most 1 MiB (spec 012); action `read` for download, `write` for upload; answers each object with its action, or with no `actions` for an upload of an object the store already holds (Objects, below) |
| POST | `/{repo}/info/lfs/verify` | the `verify` action of an upload: `{"oid", "size"}`; action `write`, because it completes the upload; 200 when the object exists with that size, 422 otherwise. The check is the size alone, from one `HEAD` of `lfs/<oid>`; the hash is not checked, because the node would have to read the whole object to compute it, and the bytes never passing through a node is the point of the presigned transfer. An object whose bytes do not match its `oid` is what the client's own `git lfs` refuses on download |
| POST | `/{repo}/info/lfs/locks` | 501 `lfs_locks_unsupported` with the LFS body below carrying its sentence; every path under `locks`, whatever the method, answers the same |

Defined by [014 repository migration](../specs/014-repository-migration.md).

| Method | Path | Behaviour |
|---|---|---|
| POST | `/v1/repos/{id}/verify` | `{"source": "<https URL>", "token": "<optional bearer for the source>"}`, the same body shape as spec 019's `import`, action `admin`; compares the source and Origo's copy and answers the document below; read-only on both sides and idempotent, a `POST` only because the source bearer travels in the body, where it is never logged, and not in a header or a query string; 400 `invalid_request` for a non-HTTPS source, one the egress rules of spec 016 refuse, or one that does not answer `ls-remote`, which is the caller's input and carries `field: "source"` in `details` |

Defined by [019 repository administration](../specs/019-repository-administration.md).

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

Defined by [020 server side git operations](../specs/020-server-side-git-operations.md).

| Method | Path | Body beyond the common fields | Result |
|---|---|---|---|
| POST | `/v1/repos/{id}/commits` | `changes: [{"path", "content" (base64, at most 10 MiB decoded per file) \| "content_ref" (a blob sha already in the repository) \| "delete": true, "mode": "100644"\|"100755"\|"120000"}]`, 1 to 1 000 changes in a body of at most 64 MiB, each `path` checked by the rules below | one commit with `expected_head` as parent |
| POST | `/v1/repos/{id}/merge` | `source: <branch or sha>`, `strategy: "fast_forward_only"\|"merge_commit"\|"fast_forward_if_possible"` (default), `message` optional for a merge commit, defaulting to `Merge <source> into <branch>` with both names as the request gave them | fast-forward moves the branch with no new commit and answers the source's sha; a merge commit has two parents; a conflict is 409 `merge_conflict` with `details.paths` |
| POST | `/v1/repos/{id}/cherry-pick` | `commits: [<sha>]`, 1 to 100, applied in order, `mainline` for a merge commit | one commit per picked commit, all in one entry and one transaction, so partial application never lands; a conflict is 409 `merge_conflict` naming the commit and paths |
| POST | `/v1/repos/{id}/revert` | `commits: [<sha>]`, 1 to 100, `mainline` | one revert commit per input, same atomicity and conflict rule |

Defined by [022 landing page](../specs/022-landing-page.md).

| Method | Path | Body |
|---|---|---|
| GET | `/` | 200, the landing page; `text/html; charset=utf-8` when `Accept` contains `text/html`, `text/plain; charset=utf-8` otherwise, `Vary: Accept` on both; no token, no `WWW-Authenticate` |
| GET | `/favicon.ico` | 204, empty, no token; so a browser rendering the page is never asked for credentials it cannot supply |

Defined by [026 repository directory](../specs/026-repository-directory.md).

| Method | Path | Behaviour |
|---|---|---|
| GET | `/v1/repos` | two modes, chosen by the query. **Directory:** `?cursor=&limit=` asks the authorizer the `list` question and answers `{"repos": [<the representation of GET /v1/repos/{id}>], "next_cursor": <the authorizer's, or null>}`, dropping every id the log no longer holds; `limit` default 50, at most 200, and a value outside it is 400 `invalid_request` with `details.reason: "limit"`; 403 `forbidden` when the authorizer denied; 501 `directory_unsupported` when it answered `{"directory": false}`. **Name:** `?owner=&slug=` resolves the name through `origo/names/<owner>/<slug>`, the index the git label form already reads, then answers exactly as `GET /v1/repos/{id}` does for the id it resolved to: the authorizer is asked `read` on that id first and a deny is 403 whether or not the name resolved, so a refused caller learns nothing (spec 007, authorization before lookup); an allowed caller gets 404 `repo_not_found` when it did not resolve. One of `owner` and `slug` without the other is 400 `invalid_request` naming the missing field, and either together with `cursor` or `limit` is 400 `invalid_request` with `details.reason: "modes"` |

## Headers

Headers Origo sets on its responses. A consumer reads them; none is sent by a client.

Defined by [003 protocol contract](../specs/003-protocol-contract.md).

| Header | Meaning |
|---|---|
| `Origo-Contract` | the contract version, `1`; on every response of the public listener |
| `Retry-After` | on a refusal that names a time to wait, in whole seconds and at least 1: a 429 `rate_limited` from the limits of spec 012, and a 503 `storage_unavailable` a storage breaker of spec 015 refused, whose value is the remaining open interval of the breaker that refused. It is on every such refusal, on the git routes and on the JSON API alike |

Defined by [005 placement and replication](../specs/005-placement-and-replication.md).

| Header | Meaning |
|---|---|
| `Origo-Prefer` | on every response to a request that names a repository by its id, whatever the status: the comma separated names of the preferred nodes for that repository, highest score first, with `k` = 1 on a refused request because it made no authorizer call. Absent on a response that names no repository (`POST /v1/repos`, the probes, `GET /.well-known/jwks.json`), and absent on a 401 or 403 to a request that names a repository by owner and slug, whether or not the name resolved: a header only on a resolved name would tell a refused caller the repository exists, which spec 007's authorization before lookup forbids, and the score needs the id. A request is served wherever it lands, so the header is a hint for an ingress or a client that can route by pod, never a redirect |

Defined by [008 push events](../specs/008-push-events.md).

| Header | Value |
|---|---|
| `Origo-Signature` | `sha256=<hex HMAC-SHA256 over the body with ORIGO_EVENTS_SECRET>` |
| `Origo-Event` | the `kind` |
| `Origo-Delivery` | the event `id` |

Defined by [009 read api and archive](../specs/009-read-api-and-archive.md).

| Header | Meaning |
|---|---|
| `Origo-Commit` | the object id `{sha}` resolved to, on every read response |
| `Origo-Truncated` | `true` when `refs` hit its cap or `compare` was cut at 1 MiB |

Defined by [012 limits and abuse](../specs/012-limits-and-abuse.md).

| Header | Meaning |
|---|---|
| `RateLimit-Limit` | the requests the effective subject of this response may send this node in a minute: the rate the authorizer named for that subject (spec 007's `requests_per_minute`) where it named one, `ORIGO_REQUESTS_PER_MINUTE` otherwise. On every response of the rate-limited surface; the `RateLimit-Limit` field of the IETF draft [RateLimit header fields for HTTP](https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/). A client reads the figure in force rather than assuming the default, which is what lets spec 021's `rate_limited` case send past the limit against any installation. The bucket runs in front of the authorizer, so the first response of a subject the authorizer names a rate for still carries the node's figure and every later one carries the subject's. Absent when the limit is off, whatever rate the subject carries. |

Defined by [015 degraded storage](../specs/015-degraded-storage.md).

| Header | Meaning |
|---|---|
| `Origo-Stale` | on a response served without a currency check: the whole seconds since the last check that answered; absent on every consistent response |

## Error codes

Every refusal is one JSON body, `{"error": {"code", "message", "details"}}`. `code` is the stable name to branch on, `message` is one sentence to show a person, and `details` carries the developer fields listed here.

Defined by [003 protocol contract](../specs/003-protocol-contract.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `invalid_request` | 400 | The request is malformed. | `reason`: the validation failure in the developer register; `field` when one field is at fault |
| `unauthenticated` | 401 | A bearer token is required. | `reason`: `missing`, `malformed`, `size`, `signature`, `issuer`, `issuer_unavailable`, `audience`, `expired`, `nbf`, `iat`, `unknown_key`; spec 007 says which check produces each |
| `forbidden` | 403 | You do not have permission to do this. | `action`, `subject`, `reason` from the authorizer |
| `repo_not_found` | 404 | Repository not found. | `id`, or `owner` and `slug` |
| `ref_not_found` | 404 | The reference or object does not exist in this repository. | `ref` |
| `repo_exists` | 409 | A repository with this id or name already exists. | `field`: `id` or `name`; `id`, `owner`, `slug` |
| `non_fast_forward` | sideband; 409 on the JSON API | The reference moved since you fetched. Fetch, then push again. | `ref`, `expected`, `actual` |
| `over_quota` | 413 | The request exceeds this repository's storage limit. | `limit`: `repository`, `push`, or `refs`; `bytes`; `max` |
| `rate_limited` | 429 with `Retry-After` | Too many requests. Wait and try again. | `limit`, `retry_after` |
| `storage_unavailable` | 503 | The repository is temporarily unavailable. Nothing was lost. Try again in a few minutes. | `op`, `key`, `error` |

Defined by [007 authentication and delegation](../specs/007-authentication-and-delegation.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `authorizer_unavailable` | 503 | Permissions cannot be checked right now. Nothing was lost. Try again in a few minutes. | `url`, `status`, `error` |

Defined by [009 read api and archive](../specs/009-read-api-and-archive.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `blob_too_large` | 413 | This file is larger than 50 MiB. Request it in ranges of at most 50 MiB. | `size`, `max` |
| `operation_timeout` | 504 | The operation took too long and nothing was changed. | `operation`, `budget_seconds` |

Defined by [010 lfs](../specs/010-lfs.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `lfs_object_mismatch` | 422 | The uploaded object does not match its declared size. | `verify` of an object whose stored size differs from the declared one, or that was never uploaded; the hash is not checked (the endpoint table says why); `oid` is the one the request named; the LFS shape carries no `details`, so the body is the sentence, `request_id`, and `documentation_url` |
| `lfs_object_not_stored` | 404, per object | This object is not stored. | a `download` action for an object without a `lfs/verified/<oid>` marker; `oid` is the batch object the `error` sits on |
| `lfs_locks_unsupported` | 501 | Locking is not supported. | every path under `locks`, whatever the method |

Defined by [015 degraded storage](../specs/015-degraded-storage.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `repository_unavailable` | 503 | This repository cannot be served until an operator restores it. Other repositories are not affected. | `key`, `error` |

Defined by [019 repository administration](../specs/019-repository-administration.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `gone` | 410 | This repository was deleted and its hold has passed. It cannot be restored. | `id`, `purged_at` |
| `repo_frozen` | 403 on a write, 409 on a second freeze | This repository is frozen and does not accept pushes. | `frozen_at` |
| `repo_importing` | 409 | This repository is importing and does not accept pushes until the import finishes. | `started_at` |
| `repo_not_empty` | 409 | This repository already has history; import into an empty repository. | `seq` |
| `import_not_found` | 404 | No import has been started for this repository. | `id` |

Defined by [020 server side git operations](../specs/020-server-side-git-operations.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `merge_conflict` | 409 | The change conflicts with the branch. Resolve it in a clone and push. | `commit`, `paths` |
| `invalid_change` | 400 | A change in the request is not valid. | `index`, `reason` (`path`, `mode`, `content`, `too_many`, `too_large`) |

Defined by [026 repository directory](../specs/026-repository-directory.md).

| Code | Status | Message | Details |
|---|---|---|---|
| `directory_unsupported` | 501 | This installation does not list repositories. | `reason` |

## The authorization endpoint

Every path above is one Origo serves. This is the one call it makes: to the authorization endpoint the operator runs, which decides every repository operation. Whoever writes that endpoint is the reader here; an operator installing one starts at [`install.md`](install.md).

Defined by [007 authentication and delegation](../specs/007-authentication-and-delegation.md).

Origo holds no permission. Before every operation that names a
repository it calls one endpoint the operator runs and asks whether a
subject may do one thing to one repository. That endpoint is the whole
permission model, and Origo adds nothing to it and caches its answer.
Nothing here is any one operator's: it is what an endpoint must do to
serve any installation, whatever holds the permissions behind it.

The call, after verification, per request that names a repository:

```
POST <ORIGO_AUTHORIZER_URL>
Authorization: Bearer <ORIGO_AUTHORIZER_TOKEN>
Content-Type: application/json

{"subject": "…", "actor": "…", "repo": {"id": "…", "owner": "…", "slug": "…"}, "action": "read"}
```

| Field | Value |
|---|---|
| `subject` | the effective subject: the token's `sub`, or its `act` when the token carried one. It is empty only for the probe below |
| `actor` | the delegating service when the token carried `act`, else empty |
| `repo.id` | the repository id, a lower-case UUID. Empty for a name Origo could not resolve |
| `repo.owner`, `repo.slug` | set on the name form and on a creation, empty on the id form |
| `action` | `read`, `write`, or `admin` |

The answer is 200 either way, `{"allow": true}` with the optional
figures below, or `{"allow": false, "reason": "…"}` whose `reason`
reaches the client as `details.reason` on Origo's 403 `forbidden`:

```
200 {"allow": true, "ttl": 60, "replicas": 1, "quota_bytes": 53687091200, "requests_per_minute": 600}
200 {"allow": false, "reason": "…"}
```

The action Origo sends per operation:

| Action | Operations |
|---|---|
| `read` | `info/refs?service=git-upload-pack`, `git-upload-pack`, LFS download, `GET /v1/repos/{id}`, the read API and archive of spec 009, and the three reads of spec 019: import state, `export.bundle`, and `stats` |
| `write` | `info/refs?service=git-receive-pack`, `git-receive-pack`, LFS upload, and the server-side git operations of spec 020 |
| `admin` | `POST /v1/repos`, `PATCH`, `DELETE`, `undelete`, minting a repository-bound token, and the rest of spec 019: transfer, freeze, unfreeze, starting an import, and `gc` |

Spec 026 adds a fourth action, `list`, which names no repository and
asks which repositories a subject may see. It is that spec's to state
and it changes nothing here: the three actions above, the five rules,
the answer shape, and the caches are unchanged, and an endpoint built to
this spec alone stays correct.

Spec 019 marks three of its own operations `read`, and the per-operation
row wins over the sentence that calls its operations `admin`: a reader
who may clone may also read the size of what they cloned. On
`POST /v1/repos` the `repo` object carries the id, owner, and slug the
body names, so the endpoint decides a creation from the name the caller
chose.

**The five rules.** An endpoint that keeps them serves any installation.

1. **Answer 200 for both verdicts.** Anything else, a 500, a timeout, a
   body that does not parse, is a refusal, and Origo renders it as
   `authorizer_unavailable`. There is no fail-open.
2. **Deny `00000000-0000-0000-0000-000000000001` for every subject and
   every action**, the empty subject included. That repository id is
   reserved as a probe: `origod check` (spec 018) sends it with an empty
   subject and reads an allow as an endpoint that does not read the
   request. The stub of spec 013 denies it.
3. **Key on the repository id when the answer varies by repository.**
   The id form sends the id alone, with no owner and no slug, and it is
   what every clone by id and every API call uses.
4. **Decide without the repository.** An empty or unknown id is the
   ordinary case, for a creation and for a name that did not resolve.
   Answer it without revealing which, because Origo asks before it reads
   any metadata, so that a deny and a repository that does not exist
   look alike.
5. **Treat the endpoint's availability as Origo's.** It is called on the
   request path of every repository operation, so it sits near the nodes,
   answers from memory, and keeps nothing slow in front of the answer.

**A single-tenant installation needs no service.** Nothing above asks
for a database, a permission model, or an answer that varies by
repository. An endpoint that answers `{"allow": true}` for a list of
subjects, `{"allow": false}` for everyone else, and always denies the
probe id keeps the contract in full: rule 3 does not apply when the
answer is the same everywhere, and every figure may be omitted for
Origo's defaults. That is a few dozen lines behind the same bearer, and
it is where a team hosting its own repositories starts.

**The figures, and when to send them.** Each is optional and each has a
default, so an endpoint that sends `allow` alone is complete.

| Field | Omit it when | Send it when |
|---|---|---|
| `ttl` | 60 seconds of revocation lag suits you | you want fewer calls; the cap is 600 |
| `replicas` | always, unless you run Origo's placement policy (spec 005); absent is 1 | a repository needs more than one warm node |
| `quota_bytes` | 50 GiB per repository suits you (spec 012); absent is 53687091200 | you sell plans or cap by tenant |
| `requests_per_minute` | your subjects are people; absent buckets the subject at `ORIGO_REQUESTS_PER_MINUTE` (spec 012), and absent is not zero | one subject drives many repositories, a build fleet under one token, which spec 020 names as its case |

**What Origo does with the answer.** An allow is cached per
`(subject, actor, repo id, action)` for `ttl`; a deny for 5 seconds; an
answer for an unresolved name (an empty id) is not cached. So the call
rate an endpoint sees is set by the cache and not by the traffic, and a
higher `ttl` divides it. The call is made once and retried once when the
connection failed before a response line arrived (a refused or reset
connection, a dial timeout); a 5xx, a timeout after the request was
sent, and a body that does not parse are never retried.
