---
title: "Web interface: a separate service that browses an Origo installation"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/007-authentication-and-delegation.md
  - specs/009-read-api-and-archive.md
  - specs/018-installation.md
affects: [specs/README.md]
effort: large
created: 2026-09-10
updated: 2026-09-10
author: changkun
---

# Web interface

## Overview

Origo has no web surface. Spec 022 puts a sign on the door: a browser
that opens an installation reads a page saying this address is a git
remote. It stops there on purpose, and this spec is what stands behind
it for an operator who wants more: a small, read-only browsing interface
in the spirit of cgit and sourcehut. Repositories, branches and tags,
the commit log, a commit's diff, the file tree, a file. No comments, no
reviews, no stars, no forks, no issues.

It is **a separate service**, not part of Origo. The reasons, recorded
so they are not relitigated:

- Origo is stateless infrastructure with no session, no cookie, no
  template, and no HTML beyond spec 022's one constant page. A browsing
  interface needs all four. Putting them in `origod` would put an OIDC
  relying party, a cookie key, and a template tree inside the process
  that holds the write-ahead log.
- The interface is optional. A self-hoster who wants git hosting and
  nothing else runs Origo alone, and an optional thing that ships inside
  the required thing is not optional in practice: it would be in the
  release archive, in the image, in the configuration reference, and in
  the threat model.
- It is a pure client of a published API. Everything it needs is in
  spec 009 and documented in `docs/api.md`. A contributor to the
  interface reads that page, not Origo's internals, and the boundary
  stays where a boundary that is enforced by a repository split stays.
- The split is sourcehut's: the git host and the browsing interface are
  separate programs that speak over a documented surface.

This spec is written in Origo's deck because the repository it belongs
in does not exist yet. **It moves on the first commit of that
repository** and leaves Origo's deck in the same change; nothing in
Origo depends on it, and its removal from this deck breaks no
cross-reference, because it defines no Origo name.

## Current state

Nothing is built. The pieces it stands on:

- Spec 009's read API is built and released in `v0.1.0`, at `testing`
  on the two items its Outcome names:
  `/v1/repos/{id}/refs`, `/v1/repos/{id}/commits`,
  `/v1/repos/{id}/commits/{sha}`,
  `/v1/repos/{id}/compare/{base}...{head}`,
  `/v1/repos/{id}/tree/{sha}`, `/v1/repos/{id}/blob/{sha}`,
  and `/v1/repos/{id}/archive/{sha}.tar.gz`, with cursor paging,
  `ETag` revalidation, `Origo-Commit` on every response, and
  `Origo-Truncated` where a body was cut.
- Spec 003 serves the repository representation at
  `/v1/repos/{id}` with `owner`, `slug`, `default_branch`, `head`,
  `size_bytes`, `updated_at`, and `pushed_at`.
- Spec 007 verifies a bearer token from any configured issuer, requires
  `aud` to contain `origo`, and asks an operator-run authorizer one
  question per request: may this subject take this action on this one
  repository.
- `latere.ai/x/pkg/authkit/oidc` is the relying party every Latere web
  surface uses: the authorization-code flow with PKCE, ID-token
  verification, an encrypted `__Host-` session cookie holding the
  access and refresh tokens, and refresh. `latere.ai/x/pkg/md` renders
  GitHub-flavoured Markdown and `latere.ai/x/pkg/sanitize` cleans HTML.

Three things it needs and cannot have today, each with its own section
below: a way to list the repositories a person may see, a way to turn
`<owner>/<slug>` into a repository id, and, for a public installation,
a read that carries no token at all.

## Design

### Where it lives

A new repository, `latere-ai/origo-web`, holding one deployable, one
binary `origoweb`, its own `deploy/`, its own gate, and its own tags.

| Option | For | Against |
|---|---|---|
| a directory in `latere-ai/origo` | one CI proves the client against the server; a contract change and its consumer land together | the interface's release cadence is the storage core's; an optional component ships inside the required one; nothing stops a handler reaching into `internal/` and the boundary erodes inside one release; a self-hoster who wants Origo alone still reads about it in the configuration reference |
| **a new repository `latere-ai/origo-web`** | the interface tags on its own clock, and a markup change never asks Origo for a release; a self-hoster runs Origo without it, or runs it against an Origo they did not build; a contributor reads `docs/api.md`, which is the surface the boundary is made of; one deployable per repository is the fleet convention (`drive`, `insula`, `auth`, `pay` each own theirs) | contract drift is not caught by one `make`; the interface must pin an Origo version and prove itself against it |
| folded into another product | the repository list is already in a product's own table | the interface stops being an Origo component and a self-hoster cannot run it |

The one real cost of the split is drift, and it is paid the way Insula
already pays it (its spec 026): the interface pins an `origod` image by
digest and runs its end-to-end tests against that container with the
stub issuer and authorizer of Origo's spec 013, so a contract change
that breaks it fails its own gate on the next bump. Origo's spec 021
conformance suite is what proves the server side; this repository does
not repeat it.

### What it is, in one rule

**The interface holds no git knowledge and no access-control model.** It
never runs git, never reads the bucket, never resolves a reference,
never computes a diff, and never decides who may see what. It sends the
signed-in person's own token to Origo, renders what comes back, and
renders a refusal as a refusal. The one piece of parsing it does is
reading the unified diff text `/v1/repos/{id}/compare/{base}...{head}`
returns so it can be shown as a table; that is presentation, not
computation, and it produces no fact the server did not send.

A consequence that must be written down before someone optimises it
away: **no cache of a rendered page may be shared between subjects.**
Origo's `ETag` is the repository's index sequence, which is the same for
every reader, so a cache keyed on it alone would hand one person's
private repository to the next visitor. Revalidation with
`If-None-Match` is per session and per subject, or it is not done.

### Identity and session

```mermaid
sequenceDiagram
  participant B as browser
  participant W as origoweb
  participant I as OIDC issuer
  participant O as origod
  B->>W: GET /acme/api
  W-->>B: 302 to the issuer (PKCE, state, nonce)
  B->>I: sign in
  I-->>B: 302 back with the code
  B->>W: GET /auth/callback?code=...
  W->>I: exchange the code
  I-->>W: access token (aud: origo) + refresh token
  W-->>B: Set-Cookie: encrypted session, 302 to /acme/api
  B->>W: GET /acme/api
  W->>O: GET /v1/repos/{id} + Authorization: Bearer <access token>
  O-->>W: 200 or 403
  W-->>B: the page, or "not found or not permitted"
```

| Decision | Value |
|---|---|
| flow | authorization code with PKCE (S256) against the same issuer Origo trusts, through `latere.ai/x/pkg/authkit/oidc`; the client is registered so its access token carries `aud: origo` |
| where the token lives | only in the AES-encrypted `__Host-` session cookie, `HttpOnly`, `Secure`, `SameSite=Lax`; never in `localStorage`, never in a URL, never in the rendered HTML, never in a log line |
| what is sent to Origo | the person's own access token, unchanged, as `Authorization: Bearer` |
| delegation | none. The interface holds no service credential and mints no token with `act`, so it can do nothing a signed-in person could not do, and only while their token lives |
| access-token lifetime | the issuer's. Spec 007 refuses a token whose `iat` is over 24 hours old, so a session that outlives its access token must refresh, not reuse |
| refresh | when the stored expiry is within 60 seconds, before the call to Origo; the refreshed session is written back to the cookie. A refresh that fails clears the session and redirects to sign-in with the requested path preserved |
| session lifetime | 12 hours from sign-in, not extended by refresh, so a stolen cookie has a bounded life |
| sign-out | a `POST` of `/sign-out` with a CSRF token from `authkit.CSRFIssue`, clearing the cookie |
| CSRF | the two `POST` routes only, sign-out and the key screen. Everything else is a `GET` and changes nothing |

A 401 from Origo means the token was refused; the interface clears the
session and sends the person to sign in once. A repeat is an error page,
not a redirect loop.

### What a signed-out visitor sees

Today: **a sign-in page and nothing else.** Not because the interface
chose that, but because Origo has no anonymous read. Spec 007 requires a
credential on every path of the public listener but `/readyz`,
`/version`, `/.well-known/jwks.json`, and spec 022's two, and spec 016
puts "the authorizer answering allow for an anonymous subject" and
"anonymous reads" outside its scope. There is no public tier and no
`visibility` field anywhere in Origo's model: access is one boolean per
subject, repository, and action, answered by the operator's authorizer.

So a public installation with public repositories is not something this
interface can offer by trying harder. It needs Origo to grow a path
where a request with no credential reaches the authorizer with a subject
that means "nobody". The empty subject is already taken, because
`origod check` sends the reserved probe id with an empty subject and
reads an allow as a misconfigured authorizer (spec 007). A sentinel that
is not the empty string is therefore part of that future work, and it
belongs to an Origo spec, not this one.

What this spec does is make sure the day it lands costs the interface
nothing: **every read is issued with the token if there is one and
without an `Authorization` header if there is not, and the response is
rendered as it arrives.** A signed-out visitor today gets 401 on the
first call and is shown the sign-in page. On an installation that later
answers, the same code path shows the repository.

### The repository list

This is the sharpest question in the design, and the answer is that
Origo cannot answer it.

**Checked.** Origo's whole route table is keyed by a repository id the
caller already holds: the create and lifecycle verbs on `/v1/repos` and
`/v1/repos/{id}`, spec 009's seven read paths, spec 019's
administration paths, spec 020's operations, and the two git URL forms. There is no
collection path and no owner-scoped path. Origo stores no user and no
permission (spec 007), so it could not filter a list even if it had one.
The authorizer contract is a single yes/no oracle over one named
repository, `{"subject", "actor", "repo": {"id", "owner", "slug"},
"action"}` in and `{"allow"}` out, with no batch form, no wildcard, and
no enumerate verb. Nothing published by the authorizer Latere runs exposes
a read at all: its git-plane surface is one authorize call and five
idempotent writes.

**Where the list actually lives.** Not in Origo and not in the bucket.
Origo's name index (`origo/names/<owner>/<slug>` holding an id) knows
which repositories *exist*, and the authorizer knows which of them a
subject may *see*. The list is the intersection, and only the authorizer
can produce the subject side of it without walking every repository in
the installation. So the list belongs to the authorizer, which is the
component that already owns the ownership model. For Latere's
installation that is the `auth` service, whose git-plane tables already
hold repositories, grants, owners, org roles, and team membership in a
snapshot refreshed every ten seconds. It has the answer and no way to
say it.

**What must be added, and to which component.** Two additions, both in
Origo, one of which extends the authorizer contract of spec 007. They
are Origo's to specify and build; this spec states the shape it needs so
that spec is a transcription rather than a design.

```
1. A directory question on the authorizer contract (Origo spec 007).

   POST <ORIGO_AUTHORIZER_URL>
   {"subject": "…", "actor": "…", "action": "list",
    "cursor": "…", "limit": 50}

   200 {"repos": [{"id": "…", "owner": "…", "slug": "…"}],
        "next_cursor": "…"}          the visible page, newest or
                                     lexical order, the authorizer's
                                     choice, stable across pages
   200 {"allow": false, "reason": "…"}   this subject sees nothing
   200 {"directory": false}              this authorizer has no directory

   Every answer is 200, as every answer on this contract already is: a
   non-200 is what spec 007 reads as authorizer_unavailable and fails
   closed on, so an authorizer that has not been taught the question
   must be able to say so without being taken for a broken one. The
   request carries no "repo" object, which is what distinguishes it
   from the three existing actions.

2. A collection route on Origo, in two modes.

   GET /v1/repos?cursor=&limit=
       asks the authorizer the directory question, then serves the
       repository representation for each id it returned, dropping any
       the name index no longer holds. 501 with a code of its own when
       the authorizer answered {"directory": false}.

   GET /v1/repos?owner=&slug=
       resolves the name through the index Origo already reads for the
       git label form, then answers exactly as GET /v1/repos/{id} does
       for the resolved id: the authorizer is asked "read" on that id
       first, and a deny is 403 whether or not the name resolved, so a
       refused caller still learns nothing (spec 007, authorization
       before lookup).
```

Mode 2 is a small change with a large effect: it is what lets the
interface use `/{owner}/{slug}` URLs, which is what a person types, what
the clone URL shows, and what cgit and sourcehut both do. Origo already
performs exactly this resolution for every git request in the label form;
only the JSON surface lacks it.

**Neither addition blocks the build.** Until mode 2 lands the interface
addresses a repository by its id, at `/r/{id}` and below, which every
screen but the list already serves from the routes of spec 009; the
`/{owner}/{slug}` form and the name box arrive together with mode 2, and
the id form keeps working afterwards because an id is what survives a
rename. So the order is: build against ids, gain names with mode 2, gain
the list with mode 1.

**One alternative, rejected on the record.** The signed-in person's
token carries organisation and role claims, so the interface could read
them and ask for each claimed organisation's repositories, with no
change to any contract. That is the second access-control model this
design forbids. It decides visibility from a claim instead of from the
authorizer, and it diverges silently the first time the authorizer
grants a repository outside a claimed organisation, or revokes one
inside it: the person sees a repository they cannot open, or fails to
see one they can. A token says who someone is; only the authorizer says
what they may see.

**Until it lands.** The interface ships with the list screen degrading,
not missing. When the collection read of `/v1/repos` is absent (today
it is answered `invalid_request` as an unknown route) or answers 501,
the home page is a "go to a repository" form, taking `<owner>/<slug>` once
mode 2 is there and an id before that, plus the repositories this
session has already opened, held in the session cookie
and in no server-side store. That is cgit without a scan: navigable, and
honest that it cannot enumerate. Mode 2 is what makes even that form
work, so of the two additions it is the one to build first.

### Screens

`{rev}` is a branch, a tag, or an object id. `{path}` is a path inside
the tree. Every call below goes to the Origo installation with the
person's token; a 403 and a 404 both render as one sentence, "no such
repository, or you cannot see it", because that is the distinction
Origo deliberately refuses to make. Every call in the last column is a
`GET`, and every one of them is a path spec 003 or spec 009 already
serves except the two the section above proposes.

| Screen | URL | What it shows | Calls |
|---|---|---|---|
| sign in | `/sign-in`, `/auth/callback` | what this installation is and one button; the callback exchanges the code and returns to the requested path | the issuer only |
| repository list | `/` | every repository the subject may see: name, owner, default branch, last push, size; paged by cursor | `/v1/repos?cursor=&limit=` (the addition above); degrades to the name form when absent |
| overview | `/{owner}/{slug}` | clone URLs in both forms, the branch and tag selector, the root tree at the default branch, the rendered README below it, the last commit line, a link to the archive | `/v1/repos?owner=&slug=` for the id and the default branch; `/v1/repos/{id}/refs?prefix=refs/heads/` and `?prefix=refs/tags/`; `/v1/repos/{id}/tree/{rev}`; `/v1/repos/{id}/blob/{sha}` for the README entry's sha; `/v1/repos/{id}/commits?ref={rev}&limit=1` |
| branches and tags | `/{owner}/{slug}/refs` | every branch and tag with its target, tags peeled | `/v1/repos/{id}/refs?prefix=refs/heads/` and `?prefix=refs/tags/` |
| commit log | `/{owner}/{slug}/log/{rev}` | one row per commit: short id, summary, author, date; optionally filtered to a path; a "next" link carrying the cursor | `/v1/repos/{id}/commits?ref={rev}&path=&limit=50&cursor=` |
| commit | `/{owner}/{slug}/commit/{sha}` | the message with its trailers, author and committer, parents, the file summary, and the diff against the first parent | `/v1/repos/{id}/commits/{sha}` for the metadata and stats; `/v1/repos/{id}/compare/{base}...{head}` with the first parent as base for the diff |
| compare | `/{owner}/{slug}/compare/{base}...{head}` | the same diff view between any two revisions | `/v1/repos/{id}/compare/{base}...{head}` |
| tree | `/{owner}/{slug}/tree/{rev}/{path}` | one row per entry: name, type, mode, size, and the entry's last commit is **not** shown, because that is one call per row and Origo offers no batch for it | `/v1/repos/{id}/tree/{rev}?path={path}&cursor=` |
| file | `/{owner}/{slug}/blob/{rev}/{path}` | the file with line numbers and anchors, its size and mode, links to raw and to history | `/v1/repos/{id}/tree/{rev}?path={dir}` to find the entry's blob sha and size, then `/v1/repos/{id}/blob/{sha}`; spec 009's blob route takes no `path`, which is why the tree call comes first and why the size is known before any bytes are fetched |
| raw | `/{owner}/{slug}/raw/{rev}/{path}` | the bytes, streamed through with the `Content-Type` Origo detected and `Content-Disposition: attachment` | the same two calls; the blob body is copied, never buffered |
| SSH keys | `/keys` | the signed-in person's public keys, add and remove | a contract that does not exist yet; see below |

The archive link on the overview points at
`/v1/repos/{id}/archive/{sha}.tar.gz` on the Origo installation
directly, so a large tarball never passes through this service.

**The key screen is separable, and it is the one screen with no contract
behind it.** SSH access is being specced in parallel (spec 024) and it
puts the key store outside Origo on purpose: Origo stores no public key
and resolves an offered key to a subject through an operator-run
endpoint it only reads. So no component owns a place to add or remove a
person's key, and this spec does not invent one. What the screen needs,
whenever some component grows it, is a list of the signed-in subject's
keys with a comment and a fingerprint, an add taking one key, and a
remove taking one fingerprint, each authorised as that subject.

Until then the screen and its navigation entry exist only when a key
surface is configured, which the interface probes for once at start-up
and which is unconfigured by default. Everything else in this document
is built, reviewed, and shipped without it, and the interface is
complete and useful with the whole screen absent.

### Rendering

Server-rendered HTML from Go's `html/template`, one binary, no build
step, no bundler, no framework, no dependency the page fetches from
another host. Every page is complete and usable with JavaScript
disabled. The only script is progressive enhancement that a page works
without: the branch selector is a `<form>` that submits, and a script
may make it navigate on change.

| Constraint | Rule |
|---|---|
| structure | one `<h1>` per page, sections under `<h2>`; the tree and the log are `<table>` with `<th scope="col">`; the diff is a table with a row per line |
| theme | light and dark from `prefers-color-scheme`, both defined; no theme switch, no cookie, no flash of the wrong theme because there is no script to cause one |
| code and hashes | one monospace stack, used for paths, object ids, diffs, and file content, nowhere else |
| narrow width | usable from 320 CSS pixels: the body never scrolls sideways; the tree, log, and diff tables scroll inside their own container |
| focus and contrast | every interactive element has a visible focus ring that is not the colour alone; text and its background meet WCAG AA in both themes |
| links | every navigation is an `<a href>` to a real URL, so a page can be bookmarked, opened in a new tab, and read by a crawler that runs nothing |
| identifiers | line anchors on the file view (`#L42`) and per-file anchors on the diff |
| syntax highlighting | not in v1. It is a large dependency, a large attack surface over untrusted file content, and it is not needed to read a diff |
| staleness | when Origo answers with `Origo-Stale`, the page carries one line saying this view may be a few minutes behind and why. A machine consumer ignores that header; a person reading a commit log must not |

### Limits

The three cases that decide whether this is usable on a real repository.

| Case | What Origo does | What the page does |
|---|---|---|
| a 5 000-line diff | `compare` returns at most 1 MiB, cut at a file boundary, with `Origo-Truncated: true` | renders every file it received, each collapsed past 500 lines with a control that expands it; above the diff, one line saying the server cut it and how to see the rest. A single large file is re-requested on its own with `path=`, which is exactly the case the 1 MiB cap was hit by |
| a binary file | `compare` writes `Binary files differ`; `blob` serves the bytes with the type from the first 512 bytes | the diff shows the file's row with "binary" and no body. The file view shows name, size, mode, and detected type, with a download link and no bytes: a file is rendered as text only when the detected type is textual and the bytes decode as UTF-8 |
| a very large file | over 50 MiB `blob` answers 413 `blob_too_large` without a `Range`; under it the whole body is served | the tree gives the size before any bytes are fetched. Over 1 MiB the file view is not rendered: it shows the size and offers raw and clone. Under it, the body is requested with `Range: bytes=0-1048575` so the cap is enforced by the request and not by reading and discarding. The raw route streams whatever Origo gives, up to Origo's own limit, and a 413 renders as "too large to serve; clone the repository" |

Two more bounds worth stating because a slow page is a broken page: a
tree page is one `tree` call and never `recursive=1`, so a repository
with 100 000 files costs one page of 5 000 entries; and no screen makes
a number of calls that depends on the number of rows it shows.

### Deployment

One container image, one binary, one process, no database, no volume,
no queue, no migration. It is stateless: everything it knows is in the
request and the cookie.

| Setting | Default | Purpose |
|---|---|---|
| `ORIGOWEB_ADDR` | `:8080` | the listener |
| `ORIGOWEB_ORIGO_URL` | none, required | the base URL of the Origo installation, the one address it talks to |
| `ORIGOWEB_PUBLIC_URL` | none, required | its own base URL, used to build the redirect URI and absolute links |
| `ORIGOWEB_CLONE_HOST` | `ORIGOWEB_ORIGO_URL` | what the clone URLs on the overview name, for an installation whose git host differs from its API address |
| `ORIGOWEB_SSH_CLONE_HOST` | unset | the host shown for the SSH clone form; unset means only the HTTPS form is shown, which is what an installation without the SSH surface gets |
| `AUTH_URL`, `AUTH_CLIENT_ID`, `AUTH_CLIENT_SECRET`, `AUTH_REDIRECT_URL`, `AUTH_COOKIE_KEY`, `AUTH_AUDIENCE`, `AUTH_SCOPES` | the library's | read by `authkit/oidc` with the prefix `ORIGOWEB`; `AUTH_AUDIENCE` must be the audience Origo verifies, and `AUTH_URL` must be an issuer in the installation's `ORIGO_OIDC_ISSUERS` |

**It runs on its own hostname**, not on Origo's. Origo's git surface
claims `/{owner}/{slug}/...` for smart HTTP, which is precisely the
shape a browsing interface wants; one hostname cannot serve both without
a rule that will be wrong the first time a repository is named like a
route. `code.example.com` in front of the interface and
`git.example.com` in front of Origo, two Ingress rules, one certificate
each. The interface reaches Origo over the address an operator gives it,
which may be the in-cluster Service.

**What a self-hoster does**, after Origo's `docs/install.md`: register a
second OIDC client for the browser flow whose tokens carry Origo's
audience, generate a cookie key, apply this repository's manifests with
the settings above, point a hostname at it, open it, and sign in.
Nothing in Origo's installation changes, and an operator who skips all
of it has an installation that works exactly as before.

### What the visual design must supply

The design is produced separately. This spec fixes structure, content,
and behaviour and fixes no appearance. What the design owes back, as
slots rather than qualities:

- The diff table: the added, removed, and context row treatments, the
  two line-number gutters, the per-file header with its path and its
  add/remove counts, the collapsed-file state, and the row that says a
  file is binary.
- The tree row: name, type, mode, size, and how a directory differs from
  a file at a glance and to a screen reader.
- The commit log row and the commit header, including how a merge and
  its parents read.
- The reference selector: branches and tags in one control that works as
  a plain form.
- Every empty, truncated, refused, and error state, each of which has a
  sentence in this spec and needs a place to sit: an empty repository, a
  truncated diff, a file too large, a binary file, a stale view, "no such
  repository, or you cannot see it", and Origo unreachable.
- The token pairs for both themes with their measured contrast ratios,
  the focus ring, and the monospace and text stacks with real fallbacks.
- The narrow-width behaviour of the three tables, and what the header
  and the reference selector become at 320 pixels.

## Not in this spec

Written out so a later contributor reads it as a boundary and not as a
gap. The interface has **no write path to repository content of any
kind** and never will: no editing a file, no creating a branch or tag,
no merging, no reverting, no uploading, no repository creation, rename,
transfer, freeze, or deletion. Origo's specs 019 and 020 serve those to
a platform's own API, which is where the audit trail and the workflow
belong.

It also does not have, and must not grow: comments, code review, pull or
merge requests, discussions, stars, watches, forks, issues, wikis,
projects, milestones, activity feeds, notifications, user profiles,
follower graphs, badges, or a search over content. Nothing about it
counts, ranks, or recommends. It is a window onto a git repository for
people who already know which repository they want.

Also out: syntax highlighting, blame, a graph view of history, an
in-browser terminal, and any rendering of a file format beyond Markdown
and plain text. Origo's spec 009 already scopes out search, blame, and
rendering on the server side; this spec does not smuggle them onto the
client.

## What must land first

| Item | Owner | Blocks |
|---|---|---|
| name resolution on Origo's JSON surface, mode 2 above | a new Origo spec | nothing; without it every screen is addressed by id at `/r/{id}` and the `/{owner}/{slug}` URLs and the name box wait for it |
| the directory question on the authorizer contract and the collection route, mode 1 above | a new Origo spec, and the authorizer each installation runs | the repository list screen alone; everything else degrades to the name form |
| the repository `latere-ai/origo-web` | this spec moves into it on its first commit | the build |
| a key management surface: spec 024 keeps keys out of Origo, behind an operator-run resolver Origo only reads, so no component today owns an add or a remove | unassigned; not spec 024's, which needs only the read | the key screen alone |
| an anonymous read path, with a subject sentinel that is not the empty string | an Origo spec; spec 016 scopes it out today | a public installation showing anything to a signed-out visitor |

The first two touch the authorizer contract, which is a contract an
operator implements, so they are Origo's to decide and not this
service's to assume.

## Acceptance criteria

Every test named below is proposed and lands in `latere-ai/origo-web`.
The end-to-end tests run against a pinned `origod` container with
Origo's stub issuer and stub authorizer (spec 013) and a fixture
repository, so they assert against the real read API and not a mock.

- Signing in redirects to the issuer with PKCE and a nonce, the callback
  exchanges the code and lands on the originally requested path, and the
  session cookie is `__Host-` prefixed, `HttpOnly`, `Secure`,
  `SameSite=Lax`, and decrypts only with the configured key (proposed:
  `internal/session`, `TestSignInRoundTrip`).
- No response body, no URL, and no log line contains the access or
  refresh token, asserted over every screen in the end-to-end run
  (proposed: `test/e2e`, `TestTokenNeverLeavesTheCookie`).
- A session whose access token is within 60 seconds of expiry is
  refreshed before the call to Origo, a refresh failure clears the
  session and redirects to sign-in once with the path preserved, and a
  session older than 12 hours is refused whatever its tokens say
  (proposed: `internal/session`, `TestRefreshAndSessionLifetime`).
- A signed-out visitor gets the sign-in page on every repository URL,
  and the same handler, given an Origo that answers a tokenless read,
  renders the repository instead, with no branch on "is there a session"
  in the read path (proposed: `internal/web`,
  `TestAnonymousRendersWhateverOrigoAnswers`).
- Every screen renders with JavaScript disabled and every navigation is
  an `<a href>` or a `<form>`: the end-to-end run visits each screen with
  scripting off and asserts the same content as with it on (proposed:
  `test/e2e`, `TestEveryScreenWorksWithoutScript`).
- Each screen makes exactly the calls this spec's table lists and no
  more, against a recording Origo, and no screen's call count grows with
  the number of rows it renders (proposed: `internal/web`,
  `TestScreenCallsAreExact`).
- A 403 and a 404 from Origo render the same sentence and the same
  status, so the interface leaks no distinction Origo refuses to make
  (proposed: `internal/web`, `TestRefusalAndAbsenceAreIndistinguishable`).
- A rendered page is never served to a second subject: a cache
  populated by one session is not read by another, asserted by two
  sessions against one repository where the authorizer allows the first
  and denies the second (proposed: `test/e2e`,
  `TestNoCacheIsSharedBetweenSubjects`).
- A commit whose diff Origo truncates renders every file received, the
  truncation notice, and a per-file link that re-requests one path;
  a 5 000-line diff renders in under 500 ms after the response
  (proposed: `internal/diff`, `TestTruncatedDiffRenders`;
  `internal/web`, `TestLargeDiffRenderBudget`).
- A binary file shows type, size, and a download link and no bytes; a
  10 MiB text file is requested with a `Range` and shows a truncation
  notice; a 60 MiB file is not fetched at all and shows the size and the
  clone hint (proposed: `internal/web`, `TestFileViewLimits`).
- The tree, log, and file screens page with Origo's cursors and every
  entry appears exactly once over a repository with a 5 001-entry
  directory and 100 commits (proposed: `test/e2e`, `TestPagingIsExact`).
- The reference selector lists branches and tags from `refs` and
  switching one is a form submission that lands on the same screen at
  the new revision (proposed: `internal/web`, `TestReferenceSelector`).
- The overview shows both clone forms, the id form and the name form,
  built from the configured clone host and never from a request header
  (proposed: `internal/web`, `TestCloneURLsComeFromConfiguration`).
- Every page has one `<h1>`, every table a header row with `scope`,
  every interactive element a visible focus style, and no automated
  accessibility violation at AA, over each screen in both themes
  (proposed: `test/e2e`, `TestAccessibility`).
- Every screen renders at 320, 768, and 1280 CSS pixels with no
  horizontal overflow of the document (proposed: `test/e2e`,
  `TestNarrowWidths`).
- A response carrying `Origo-Stale` renders the staleness line and the
  same content (proposed: `internal/web`, `TestStaleNotice`).
- The interface has no route that changes repository content: the route
  table is asserted against a checked-in list, and the only non-`GET`
  routes are sign-out and the key screen, both requiring a CSRF token
  (proposed: `internal/web`, `TestRoutesAreReadOnly`).
- With the key surface unconfigured, the key screen and its navigation
  entry are absent and every other screen is unchanged (proposed:
  `internal/web`, `TestKeyScreenIsOptional`).
- With the collection read of `/v1/repos` absent or answering 501, the
  home page is the
  name form and the recently-opened list, and the rest of the interface
  is unaffected (proposed: `test/e2e`, `TestListDegradesWithoutDirectory`).
