# The Origo API

For whoever writes a client of an Origo installation: a platform that
creates repositories for its users, a build system that reacts to
pushes, a service that commits on a person's behalf, or a script. It
covers how to authenticate, how to name a repository, every route with
the permission it needs, the errors and headers a client branches on,
the limits, and the push events. Whoever writes the authorization
endpoint Origo calls reads [`authorizer.md`](authorizer.md).

The same surface is published as an OpenAPI 3.1 document. Every
installation serves it at `GET /openapi.yaml` without a token, and the
repository carries the same bytes at
[`api/openapi.yaml`](../api/openapi.yaml), so a client generator reads
the exact field list from either.

## The contract version

Every response carries `Origo-Contract: 1`. A release inside one
contract version only adds: new routes, new fields, new error codes. A
client reads the fields it knows, ignores the rest, and treats an error
code it does not know by its HTTP status. A change that removes or
redefines anything raises the number, and the upgrade notes say what
moved.

## Authenticating

Every route that reads or changes a repository needs a credential. The
exceptions are the service routes below (`/`, `/version`, `/readyz`,
`/.well-known/jwks.json`, `/openapi.yaml`), and the read routes on an
installation that allows anonymous reads.

**A token from your issuer.** The installation trusts one or more OIDC
issuers (`ORIGO_OIDC_ISSUERS`) and one or more audiences
(`ORIGO_OIDC_AUDIENCE`, `origo` unless the operator changed it). A token
is accepted when its `iss` is one of those issuers character for
character, its `aud` names one of those audiences, its signature
verifies against the issuer's key set, and it is inside its validity
window. How a person or a service obtains one is the issuer's business;
[`install.md`](install.md) shows the shape of a client credentials
request.

Send it as `Authorization: Bearer <token>`. On the git routes git
itself does not send a bearer, so Origo also reads the password of
Basic authentication as the token, with any username:

```
git clone https://x:<token>@git.example.com/acme/api.git
git -c http.extraHeader="Authorization: Bearer <token>" clone https://git.example.com/acme/api.git
```

**Acting for a person.** A service that pushes or commits for a user
presents the token its issuer minted for that user, so the user is the
subject of the push and of every event it produces. A token that
carries an `act` claim names two parties and is refused with 401 and
`details.reason: "delegation"`.

**The subject.** Origo identifies a caller as `<issuer>|<sub>`: the
issuer URL with any trailing slash removed, a pipe, and the token's
`sub`. Two issuers that agree on a `sub` are two subjects. That string
is what the authorization endpoint receives, what a push is recorded
under, and what an event names as its `pusher`.

**Repository-bound tokens.** A caller with `admin` on a repository can
mint a short-lived token for that one repository and hand it to a
build, a bot, or an agent instead of its own credential:

```
POST /v1/repos/{id}/tokens
{"scope": "read", "ttl": 3600}

201 {"token": "<jwt>", "expires_at": "2026-09-24T13:00:00Z"}
```

`scope` is `read` or `write`, and `write` includes read. `ttl` is 1 to
3600 seconds. The token is signed by the installation with
`ORIGO_TOKEN_KEY`, its issuer is `ORIGO_PUBLIC_URL`, and its key set is
served at `/.well-known/jwks.json`. It is decided by its scope alone:
the authorization endpoint is never asked, it never permits an `admin`
operation, it is refused on every other repository, and it cannot list
repositories. There is no revocation list. A token ends when it
expires, or for every token at once when the operator replaces the
signing key.

**Anonymous reads.** An installation that sets `ORIGO_ANONYMOUS_READ`
admits a request with no credential at all on the clone and fetch
routes and on the read routes of the JSON API, and asks its
authorization endpoint about it with an empty subject.
[`configuration.md`](configuration.md#anonymous-read) lists the routes.
A request that carries a credential Origo refuses is answered 401 and
is never downgraded to anonymous. A repository the endpoint does not
open to anonymous callers answers 401 with `WWW-Authenticate: Basic`,
the same answer a missing repository gets, so git asks for a
credential.

**A refused credential** is 401 `unauthenticated` with
`WWW-Authenticate: Basic realm="origo"` and one reason in
`details.reason`: `missing`, `malformed`, `size`, `signature`,
`issuer`, `issuer_unavailable`, `unknown_key`, `audience`, `expired`,
`nbf`, `iat`, `subject`, or `delegation`.

## Naming a repository

A repository has two names.

- **The id** is a lower-case UUID the caller chooses when it creates
  the repository. It never changes: a rename, a transfer, and a change
  of owner leave it alone. Store it, and use it in every API call.
- **The owner and slug** are the name a person sees, `acme/api`. Each
  is 1 to 128 characters of letters, digits, `.`, `_`, and `-`, starting
  with a letter or digit, and neither is `.` or `..`. The owners `r`
  and `v1` are reserved. A rename takes effect at once, and the old
  name answers 404 from then on, never a redirect.

Git reaches a repository by either name, with or without `.git`:

```
https://git.example.com/r/0f5c1d2e-7a4b-4c1e-9d3a-2b8f6e4c1a90.git
https://git.example.com/acme/api.git
ssh://git@git.example.com/r/0f5c1d2e-7a4b-4c1e-9d3a-2b8f6e4c1a90.git
git@git.example.com:acme/api.git
```

The JSON API takes the id. To turn a name into an id, call
`GET /v1/repos?owner=acme&slug=api`.

A read route that takes a revision, written `{sha}` below, accepts a
full object id or a short branch or tag name without a slash. For a
name with a slash, send `-` as the segment and the name in the query:
`/v1/repos/{id}/tree/-?ref=release/2026-09`. A query value that starts
with `-` is refused.

## Routes

Each route below names the action your authorization endpoint is asked
for. `{repo}` in a git route is `r/{id}.git` or `{owner}/{slug}.git`.
The request and response fields of every route are in the OpenAPI
document; this section says what each is for.

### Service

None of these needs a token.

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/` | none | a short page naming the installation and its release; HTML for a browser, text otherwise |
| GET | `/favicon.ico` | none | 204, so a browser opening the page is never asked for a credential |
| GET | `/version` | none | `{"version", "commit", "build_time"}` of the running release |
| GET | `/readyz` | none | `ok` when the node reaches the bucket and its disk; 503 with the failing check otherwise |
| GET | `/livez` | none | `ok` while the process runs. Served on the internal listener, not the public one |
| GET | `/metrics` | none | Prometheus metrics. Served on the internal listener, not the public one |
| GET | `/.well-known/jwks.json` | none | the public keys repository-bound tokens are signed with |
| GET | `/openapi.yaml` | none | the OpenAPI document of this release |

### Repositories

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/repos` | `repo.admin` | create a repository with the id, owner, slug, and optional `default_branch` (`main` if absent) you send. 201 with the repository; 409 `repo_exists` when the id or the name is taken |
| GET | `/v1/repos` | `repo.list` | with `?cursor=&limit=`, the repositories the caller may see, a page at a time; with `?owner=&slug=`, the one repository of that name (action `repo.read`) |
| GET | `/v1/repos/{id}` | `repo.read` | the repository: `id`, `owner`, `slug`, `default_branch`, `size_bytes`, `head`, `updated_at`, `pushed_at`, `frozen_at`, `verified_at`, `verified_equal` |
| PATCH | `/v1/repos/{id}` | `repo.admin` | change any of `owner`, `slug`, `default_branch` |
| DELETE | `/v1/repos/{id}` | `repo.admin` | delete it. 202 with `purge_after`, seven days on. Every route answers 404 from then on, and 410 `gone` once the objects are purged |
| POST | `/v1/repos/{id}/undelete` | `repo.admin` | bring a deleted repository back whole, inside the seven days |

Create a repository only after your authorization endpoint knows about
it. Origo asks the endpoint before it creates anything, and an endpoint
that has never heard of an id denies it; [`install.md`](install.md)
walks through the order.

The directory form answers 501 `directory_unsupported` when the
installation does not list repositories, which is the endpoint's
choice. `limit` is 1 to 200, 50 by default.

### Tokens

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/repos/{id}/tokens` | `repo.admin` | mint a repository-bound token, as above |

### Git

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/{repo}/info/refs` | `repo.read` or `repo.write` | the reference advertisement for `?service=git-upload-pack` (read) or `?service=git-receive-pack` (write) |
| POST | `/{repo}/git-upload-pack` | `repo.read` | a clone or a fetch |
| POST | `/{repo}/git-receive-pack` | `repo.write` | a push |

Protocol v2 is served when the client asks for it, v0 otherwise.
Shallow clones, partial clones (`--filter`), and fetching any reachable
commit by its id work. A push is acknowledged only after it is written
to object storage, and a push whose reference moved since the client
fetched is refused with `non_fast_forward`. `git push -o
origo.event=off` pushes without producing a push event.

Over SSH the same three operations run under the key's subject, and
nothing else is served: no shell, no JSON API, no LFS.

### Git LFS

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/{repo}/info/lfs/objects/batch` | `repo.read` to download, `repo.write` to upload | the batch API with the `basic` transfer. Each object is answered with a presigned URL on the bucket, so the bytes go between the client and the bucket directly |
| POST | `/{repo}/info/lfs/verify` | `repo.write` | completes an upload after checking the stored size. An object is downloadable only after it is verified |
| any | `/{repo}/info/lfs/locks` | a valid token; the endpoint is not asked | 501 `lfs_locks_unsupported`; file locking is not offered |

These routes answer in the Git LFS error shape, a `message`, a
`request_id`, and a `documentation_url`, rather than the envelope
below. LFS works over HTTPS only.

### Reading without a clone

Each read is served from the node's copy after it confirms the copy is
current, and carries the object id the revision resolved to in
`Origo-Commit`.

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/v1/repos/{id}/refs` | `repo.read` | references under `?prefix=` (default `refs/`) with their object ids and peeled tags. At most 10 000, with `Origo-Truncated: true` past that |
| GET | `/v1/repos/{id}/commits` | `repo.read` | history from `?ref=` (default `HEAD`), filtered by `path`, `since`, `until`; newest first, `limit` 1 to 200 (50 by default), paged with `cursor` set to the previous page's `next_cursor`. Each commit carries its parents, author, committer, message, and trailers |
| GET | `/v1/repos/{id}/commits/{sha}` | `repo.read` | one commit with its file, addition, and deletion counts |
| GET | `/v1/repos/{id}/compare/{base}...{head}` | `repo.read` | the diff between two revisions as `text/x-diff`, optionally limited to `?path=`. Cut at a file boundary past 1 MiB, with `Origo-Truncated: true` |
| GET | `/v1/repos/{id}/tree/{sha}` | `repo.read` | the entries of a directory (`?path=`), or the whole tree with `?recursive=1`, 5 000 entries a page |
| GET | `/v1/repos/{id}/blob/{sha}` | `repo.read` | the bytes of a blob by its object id, with `Range` honored. A blob over 50 MiB is served in ranges of at most 50 MiB |
| GET | `/v1/repos/{id}/archive/{sha}.tar.gz` | `repo.read` | a reproducible tarball of a revision, streamed |

A read that runs longer than 30 seconds is stopped and answers 504
`operation_timeout`. An empty repository answers `/commits` with an
empty list; a named revision that does not exist is 404
`ref_not_found`.

### Writing without a clone

These create commits on the node, with no working copy anywhere. Each
takes the same common fields:

| Field | Meaning |
|---|---|
| `branch` | the branch to move, `main` or `refs/heads/main` |
| `expected_head` | the commit the branch is at now, as you last read it. If the branch moved, the request is refused with 409 `non_fast_forward` naming the actual head, and nothing is written |
| `author` | `{"name", "email"}`, required. The committer is Origo at the installation's host |
| `message` | the commit message, at most 64 KiB |
| `dry_run` | `true` checks the whole request and answers 200 without writing |

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/repos/{id}/commits` | `repo.write` | one commit from 1 to 1 000 file changes: new content (base64, at most 10 MiB a file), a blob already in the repository (`content_ref`), or a deletion. `create_branch` with `from` starts a new branch |
| POST | `/v1/repos/{id}/merge` | `repo.write` | merge a branch or commit into `branch`: `fast_forward_only`, `merge_commit`, or `fast_forward_if_possible` (the default). A conflict is 409 `merge_conflict` with the paths |
| POST | `/v1/repos/{id}/cherry-pick` | `repo.write` | apply 1 to 100 commits onto `branch` in order, all or none |
| POST | `/v1/repos/{id}/revert` | `repo.write` | revert 1 to 100 commits on `branch`, all or none |

A frozen repository refuses these with 403 `repo_frozen`. Each
repository accepts 60 of these operations a minute on each node, past
which the answer is 429 `rate_limited`. A successful write produces a
push event whose `operation` names the route.

### Administration

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/repos/{id}/transfer` | `repo.admin` | change the owner, recorded as a transfer rather than a rename |
| POST | `/v1/repos/{id}/freeze` | `repo.admin` | refuse every push and write while reads continue. A second freeze is 409 `repo_frozen` |
| POST | `/v1/repos/{id}/unfreeze` | `repo.admin` | accept writes again |
| GET | `/v1/repos/{id}/stats` | `repo.read` | `size_bytes`, `lfs_bytes`, `packs`, `entries_since_compaction`, `refs`, `pushed_at`, `compacted_at` |
| POST | `/v1/repos/{id}/gc` | `repo.admin` | compact the repository now. At most once an hour per repository, else 429 `rate_limited` |
| GET | `/v1/repos/{id}/export.bundle` | `repo.read` | the whole repository as one `git bundle`, streamed. Check it with `git bundle verify`: a bundle cut by the 10 minute budget arrives truncated rather than as an error status |

### Moving repositories in

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/repos/{id}/import` | `repo.admin` | copy the history of an `https` source into an empty repository in the background. 202 at once; pushes answer 409 `repo_importing` until it ends |
| GET | `/v1/repos/{id}/import` | `repo.read` | the import's `state` (`running`, `done`, `failed`), its counts and times, and git's message when it failed |
| POST | `/v1/repos/{id}/verify` | `repo.admin` | compare every reference of the source with Origo's copy and answer whether they are equal. The result is recorded on the repository as `verified_at` and `verified_equal`; neither history changes |

The source host must be on the installation's `ORIGO_EGRESS_ALLOW`
list. The source's bearer travels in the request body, never in a URL
or a header. [`migration.md`](migration.md) is the whole procedure.

## Errors

Every refusal on the JSON API is one body:

```json
{"error": {"code": "forbidden", "message": "You do not have permission to do this.", "details": {"action": "repo.write", "subject": "https://auth.example.com|user_42", "reason": "not a member"}}}
```

Branch on `code`. `message` is a fixed sentence written for a person
and safe to show. `details` carries the fields a program or an
operator needs, and varies by code.

On the git routes the same refusal reaches the person through git, as
`remote: <code>: <message>` or `remote error: <code>: <message>`. On
the LFS routes it is the LFS error shape.

| Code | Status | When | What to do |
|---|---|---|---|
| `invalid_request` | 400, or 416 for a `Range` past the end of a blob | the request is malformed; `details.reason` says how and `details.field` names the field at fault | fix the request |
| `unauthenticated` | 401 | no credential, or one Origo refuses; `details.reason` above | obtain a valid token |
| `forbidden` | 403 | the authorization endpoint denied, or a repository-bound token does not cover the request; `details.action`, `details.subject`, `details.reason` | ask whoever grants access; the reason is the endpoint's own |
| `repo_not_found` | 404 | no repository has this id or name, or it is deleted | check the id |
| `ref_not_found` | 404 | the revision or object does not exist, or the repository has no references to export | read the references |
| `import_not_found` | 404 | no import was started for this repository | start one |
| `lfs_object_not_stored` | 404, per object | an LFS download of an object that was never uploaded and verified | upload it |
| `repo_exists` | 409 | the id or the owner and slug are taken; `details.field` says which | choose another |
| `non_fast_forward` | 409, or in git's output | the branch moved since you read it; `details.actual` is where it is now | fetch, then retry against the new head |
| `merge_conflict` | 409 | a merge, cherry-pick, or revert conflicts; `details.paths` | resolve it in a clone and push |
| `repo_frozen` | 403 on a write, 409 on a second freeze | the repository is frozen | unfreeze it, or wait for whoever froze it |
| `repo_importing` | 409 | an import is running | wait for it to end |
| `repo_not_empty` | 409 | an import into a repository that already has history | import into a new, empty repository |
| `invalid_change` | 400 | one change of a commits request is invalid; `details.index` and `details.reason` (`path`, `mode`, `content`, `too_many`, `too_large`) | fix that change |
| `gone` | 410 | the repository was deleted and its seven day hold has passed | nothing; the id is never reused |
| `over_quota` | 413 | a push or upload would pass a limit; `details.limit` is `repository`, `push`, or `refs` | delete data, compact, or ask for a higher quota |
| `blob_too_large` | 413 | a blob over 50 MiB requested without a `Range` | request it in ranges |
| `lfs_object_mismatch` | 422 | an LFS upload's stored size differs from its declared size | upload it again |
| `rate_limited` | 429 | a limit below was reached; `details.limit` names it and `Retry-After` says how long to wait | wait that long |
| `lfs_locks_unsupported` | 501 | any LFS lock request | turn locking off in the client |
| `directory_unsupported` | 501 | the installation does not list repositories | address repositories by id or name |
| `authorizer_unavailable` | 503 | the authorization endpoint did not answer | retry later; nothing was changed |
| `storage_unavailable` | 503 | the bucket did not answer in time; `Retry-After` is set when the node knows how long | retry later; nothing was lost |
| `repository_unavailable` | 503 | an object of this repository's log is missing or damaged, and an operator must restore it | tell the operator; other repositories are unaffected |
| `operation_timeout` | 504 | a read or an operation ran past its budget | narrow the request |

## Headers a client reads

| Header | On | Meaning |
|---|---|---|
| `Origo-Contract` | every response | the contract version, `1` |
| `Origo-Commit` | every read response | the object id the revision resolved to |
| `Origo-Truncated` | `refs`, `compare` | `true` when the list or the diff was cut |
| `Origo-Stale` | a read served while the bucket is unreachable | the seconds since the node last confirmed its copy was current. A client that must not read a possibly stale answer refuses a response that carries it |
| `Origo-Prefer` | every response naming a repository by id | the nodes that hold the repository warm, most preferred first. A hint for a router that can pick a pod; any node serves any request |
| `RateLimit-Limit` | every response of the git, LFS, and JSON routes while the limit is on | the requests a minute this subject may send the node that answered; for a caller with no credential, the figure every anonymous caller shares |
| `RateLimit-Remaining` | beside `RateLimit-Limit` | what the subject has left on that node |
| `Retry-After` | a 429, and a 503 from an unreachable bucket | whole seconds to wait |
| `X-Trace-Id` | every response | the id of the request's trace and log line; quote it when you report a problem |

## Limits

The operator sets most of these, so read the headers rather than
assume the defaults.

| Limit | Default | Scope |
|---|---|---|
| requests a minute | 600, or the figure the authorization endpoint names for the subject | per subject, per node |
| anonymous requests a minute | 60, shared by every anonymous caller | per node |
| server-side operations a minute | 60 | per repository, per node |
| compaction on request | once an hour | per repository |
| repository size | 50 GiB, or the endpoint's `quota_bytes` | per repository, the log plus LFS objects |
| one push | 2 GiB | per push |
| references | 100 000 | per repository |
| one read | 30 seconds | per request |
| blob without a `Range` | 50 MiB | per request |
| diff | 1 MiB | per request |

One HTTP clone or push is two requests (`info/refs`, then the pack), so
a loop of pushes under one token reaches the per-subject figure sooner
than its count suggests. A service that runs bulk work under one token
asks the operator for a higher figure through the authorization
endpoint's `requests_per_minute`.

## Push events

When the operator sets `ORIGO_EVENTS_URL` and `ORIGO_EVENTS_SECRET`,
Origo posts one JSON event to that URL for every accepted push and for
each administration operation. Delivery is at least once: a sink answers
2xx to accept, and anything else is retried after 1 second, 10
seconds, 1 minute, 10 minutes, and then hourly for 24 hours. A sink
keys on the event `id`, because it may see an event more than once.

Each delivery carries three headers:

| Header | Value |
|---|---|
| `Origo-Event` | the event's `kind` |
| `Origo-Delivery` | the event's `id`, the same on every retry |
| `Origo-Signature` | `sha256=` and the hex HMAC-SHA256 of the raw body under `ORIGO_EVENTS_SECRET` |

A push event:

```json
{
  "id": "6b1e0c2a-3f4d-5a6b-8c7d-9e0f1a2b3c4d",
  "kind": "push",
  "repo": "0f5c1d2e-7a4b-4c1e-9d3a-2b8f6e4c1a90",
  "seq": 42,
  "owner": "acme",
  "slug": "api",
  "pusher": {"sub": "https://auth.example.com|user_42"},
  "updates": [
    {"ref": "refs/heads/main", "before": "9fceb02…", "after": "1a410ef…", "forced": false}
  ],
  "at": "2026-09-24T12:00:00Z"
}
```

`forced` is true when the update was not a fast-forward. A push that
only moves `HEAD`, which is a change of default branch, carries
`"kind_detail": "default_branch"`. A commit made through the API
carries `"operation"` naming it: `commits`, `merge`, `cherry-pick`, or
`revert`.

The other kinds share `id`, `kind`, `repo`, `owner`, `slug`, `at`, and
`pusher`, and add:

| Kind | Extra fields |
|---|---|
| `renamed` | `from` and `to`, each `owner/slug` |
| `transferred` | `from` and `to`, each an owner |
| `frozen`, `unfrozen`, `undeleted` | none |
| `deleted` | `purge_after` |
| `compacted` | `before` and `after`, each `{"packs", "entries", "size_bytes"}` |
| `imported` | `source` with any credential removed, `refs`, `bytes` |
| `verified` | `equal`, `refs`, `objects`, `checked_at` |

`origod check` also posts one signed event of kind `ping`, carrying only
`id`, `kind`, and `at`, to prove the sink is reachable; any status below
500 satisfies it, so a sink may acknowledge and ignore it.

Verify the signature over the exact bytes you received, before parsing
them, with a constant-time comparison:

```go
func verified(secret, body []byte, header string) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}
```

## Testing a client

Two Go packages in this module let a client test against the real
behavior without an installation:

- `latere.ai/x/origo/test/stubs/origo` starts the node's own git and
  API handlers in-process, over an in-memory bucket, a stub issuer, a
  stub authorizer, and a stub event sink.
- `latere.ai/x/origo/test/conformance` runs the contract, one subtest
  per rule, against any base URL: a stub, a staging installation, or
  your own implementation of the same surface.

For a whole installation on your machine, `make dev` in a checkout
starts one node with a bucket and stub services; see the
[README](../README.md#try-it-in-a-few-minutes).
