---
title: "Hidden references: namespaces a fetching client is not shown, while pushes and fetches by id keep working"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
  - specs/004-write-ahead-log.md
  - specs/009-read-api-and-archive.md
  - specs/012-limits-and-abuse.md
  - specs/019-repository-administration.md
  - specs/024-ssh-access.md
affects: [internal/config/, internal/limits/, internal/httpgit/, internal/sshd/, internal/api/, test/e2e/, docs/configuration.md, docs/api.md, docs/operations.md, specs/002-repository-scaffold.md, specs/003-protocol-contract.md, specs/009-read-api-and-archive.md, specs/012-limits-and-abuse.md, specs/019-repository-administration.md, specs/README.md]
effort: small
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Hidden references

## Overview

An agent runtime that keeps one checkpoint reference per session in the
repository the session works in, such as
`refs/topos/checkpoints/<session>/latest`, adds a 112 byte line per
session to git's protocol v0 advertisement (measured, git 2.54). At
1 000 sessions a v0 clone reads about 110 KiB of references before the
first object, at 10 000 about 1.1 MB, at `limits.MaxRefs` about 11 MB.
`git ls-remote` lists them all, a mirror clone copies them and every
session's working files they reach, and `GET /v1/repos/{id}/refs` fills
its default listing with them. A protocol v2 clone or fetch with the
default refspecs never sees them, because it asks only for `refs/heads/`,
`refs/tags/` and `HEAD` (measured), but every SSH client speaks v0 (spec
024, decision 9), and so does an HTTP client configured for it.

This spec adds one installation setting naming reference namespaces that
upload-pack does not advertise and the read API does not enumerate. A
push to a hidden reference, a fetch by object id, and a lookup by exact
name keep working, so the runtime still pushes its checkpoint every turn
and still restores a fork from it.

## Current state

Not built. `Cache.initBare` (`internal/repo/cache.go`) writes a copy's
git configuration once, `uploadpack.allowAnySHA1InWant` on and no
`hideRefs` key. Upload-pack runs in `Handler.infoRefs`
(`--advertise-refs`), `Handler.uploadPack` (`internal/httpgit/handler.go`)
and `Handler.UploadPack` (`stream.go`), which `Server.run`
(`internal/sshd/sshd.go`) calls. `Handler.gitCommand` passes the client's
`Git-Protocol` header as `GIT_PROTOCOL`; `Handler.streamCommand` passes
none, so SSH is v0. `Handler.refs` (`internal/api/read.go`) lists `git
for-each-ref` under the prefix's directory, at most `api.MaxRefs`
entries. Export is `git bundle create - --all`
(`internal/api/export.go`), action `read`.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| `transfer.hideRefs` or `receive.hideRefs` | hide the namespace from both services, so a push is also spared the lines | git refuses to update, create, or delete a reference hidden from receive-pack: each is rejected with `deny updating a hidden ref` or `deny deleting a hidden ref` (checked), so the runtime's per-turn push would fail |
| A list per repository | a field set through `PATCH /v1/repos/{id}`, kept in `meta` or the index object | in `meta`, every fetch pays a read or a cache that lags behind a change; in the index object, it is a format change for every repository; and a namespace is a consumer's convention, the same in every repository it writes to |
| A namespace hidden by default | `refs/topos/` hidden on every installation | Origo is a general git host and knows no consumer's names; an installation that has none pays nothing either way |
| One list per installation | `uploadpack.hideRefs` from one variable, on every upload-pack | chosen: one trust domain per installation (spec 001), so one consumer's conventions are the installation's, and a namespace no repository uses costs nothing |

## Design

### The setting

ORIGO_HIDDEN_REFS is a comma separated list of reference namespaces,
such as `refs/topos/`; unset hides nothing. Each entry:

| Rule | Why |
|---|---|
| starts with `refs/` and names at least one component after it | a namespace, never the whole reference space |
| with any trailing `/` removed, passes `wal.ValidRefName` | the same names a push may create |
| is not `refs/heads` or `refs/tags` and is not below either | a clone needs every branch and tag, and `HEAD` points at a branch |
| carries no leading `!` or `^` | git's negation and full-name forms are not offered; a later entry never reveals an earlier one |
| at most `limits.MaxHiddenRefPrefixes` entries | every advertised reference is matched against every entry |

A value that breaks a rule fails start-up with one message naming the
variable, the entry, and the rule. An entry hides the reference of that
name and every reference below it, nothing else: with `refs/topos` or
`refs/topos/`, `refs/topos/a` is hidden and `refs/toposx/a` is not, and
`refs/top` or the glob `refs/topos/*` hides nothing (git's rule, checked).

### What git is told

Every upload-pack subprocess gets one `-c uploadpack.hideRefs=<entry>`
per entry before the subcommand, in `infoRefs` for `git-upload-pack`,
`uploadPack` and `UploadPack`, so smart HTTP and SSH hide alike;
receive-pack gets none. On the command line rather than in the copy's
configuration, which `initBare` writes once, a changed value applies to
every copy at the next request. `repo.Subcommand` already skips `-c`
values, so spans keep their names.

Measured with git 2.54, as is every git behavior in this spec (the
build's tests run them on the image's git 2.47 and check
`--exclude-hidden` against spec 018's 2.40 floor), on `refs/heads/main`
and 1 000 references of the shape above:

| Exchange | Not hidden | Hidden |
|---|---|---|
| upload-pack advertisement, v0 | 112 261 bytes | 261 bytes |
| v2 `ls-remote`, no pattern | 1 001 references | 1 |
| v2 `ls-remote` naming one hidden reference | 1 | 0 |
| `clone --mirror` | 1 001 references and every checkpoint's objects | 1 reference |
| receive-pack advertisement, the start of every push | 112 188 bytes | 112 188 bytes |

### What keeps working

| Operation on a hidden reference | Result, checked unless noted |
|---|---|
| a forced update, a create, a delete by push | accepted: receive-pack still advertises the namespace |
| a fetch of its object by id, protocol v2 (HTTP) | served |
| a fetch by id, protocol v0 (SSH, or HTTP set to v0) | served, because `uploadpack.allowAnySHA1InWant` is on in every copy (spec 004) and advertises `allow-tip-sha1-in-want` and `allow-reachable-sha1-in-want` (spec 003); with neither key git refuses with `Server does not allow request for unadvertised object` |
| a fetch by its name, v0 or v2 | refused by git: `couldn't find remote ref <name>`; a client reads the id with the read API's exact-name lookup below and fetches by id. The runtime above restores a fork by `git fetch <url> <commit>` with the id from its own log, which works over both transports |
| a read API route that takes a reference name (`?ref=` of `commits`, `tree`, `compare`, `archive`) | resolved as today, by reading: `readRequest.resolve` runs `git rev-parse` on the copy, which does not consult `uploadpack.hideRefs` |
| compaction, materialization, the sweeper | unchanged: upload-pack alone reads the setting, and a repack keeps every object a hidden reference reaches |

### Other surfaces

| Surface | With a hidden namespace |
|---|---|
| `GET /v1/repos/{id}/refs` (spec 009) | omits every hidden reference, except that a `prefix` equal to a hidden reference's full name returns that one reference: enumeration is what hiding removes, and a caller who knows a name already gets its id from every route that takes one. `api.MaxRefs` counts listed references only |
| push events (spec 008) | unchanged: an update of a hidden reference is in its push's event like any other. The sink is the consumer, not a fetching client, and a consumer that wants no event for a push sends `origo.event=off` |
| export (spec 019) | `git -c uploadpack.hideRefs=<entry> bundle create - --exclude-hidden=uploadpack --all`: the bundle leaves out hidden references and the objects only they reach (checked: 1 001 heads became 1, 134 105 bytes became 807). Export's action is `read`, so it is a reader's whole copy, the set a mirror clone gets |
| import and verify (specs 014, 019) | unchanged. An import mirrors what its source advertises; verify compares that with every reference the copy holds. A source that hides references gives neither side those |
| a consumer's own mirror | a mirror clone holds no hidden reference, and `git push --mirror` from it deletes every hidden reference, because receive-pack still advertises them (checked: 1 001 references became 1). `docs/operations.md` says so beside the variable |
| the directory (spec 026), the representation | unchanged: neither lists references, and `HEAD` points under `refs/heads/`, which cannot be hidden |
| quota, `limits.MaxRefs`, `size_bytes`, `stats.refs` | unchanged: hidden references count, since each measures the index object |

### What it does not do

Hiding is not access control: every reader may fetch a hidden
reference's objects by id and resolve a name it knows; hiding takes away
the list. Readers kept out of a namespace need it in a repository they
cannot read, which spec 032's forks make cheap. Hiding is not a storage
fix either: every index object still carries every reference (spec 004),
and spec 032 is that fix.

Pushes keep paying. Receive-pack has no protocol v2 (asked for one, git
answers in v0, checked) and lists every reference, so a push every turn
downloads 112 bytes per checkpoint reference, about 1.1 MB at 10 000
sessions; hiding from receive-pack would shrink it and refuses the push.

### Names the build adds

| Name | Kind | Meaning |
|---|---|---|
| ORIGO_HIDDEN_REFS | variable | the comma separated namespaces upload-pack does not advertise and the read API does not enumerate; unset by default |
| `limits.MaxHiddenRefPrefixes` | constant | 16, the most entries the variable takes |

The build moves the variable into spec 002's table and the constant into
spec 012's.

## Not in this spec

Deleting the checkpoint references of finished sessions, a retention
rule of the consumer's. Hiding a namespace from receive-pack. A list per
repository. Authorization per reference. Shrinking the index object's
reference map (spec 032).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| ORIGO_HIDDEN_REFS accepts `refs/topos/,refs/review` and fails start-up naming the entry for `refs/`, `refs/heads/x`, `refs/tags`, `!refs/a`, `heads/x`, and one entry more than `limits.MaxHiddenRefPrefixes` | `internal/config`, `TestHiddenRefsVariable` | not built |
| With `refs/topos/` hidden and 1 000 references under it, the v0 advertisement over HTTP is the one of a repository without them, a v2 `ls-remote` lists none even when it names one, and a fetch of a hidden name fails with `couldn't find remote ref` | `internal/httpgit`, `TestHiddenRefsAreNotAdvertised` | not built |
| A forced update, a create, and a delete of a hidden reference by `git push` are accepted, and a fetch of a hidden reference's commit by id succeeds over v2 and over v0 | `internal/httpgit`, `TestHiddenRefsStillTakePushesAndFetchesByID` | not built |
| Over SSH, the advertisement omits the namespace, a push to it is accepted, and a fetch by id is served | `internal/sshd`, `TestSSHHidesReferences` | not built |
| A copy materialized while the variable was unset hides the namespace on the first request after the node starts with it set | `internal/httpgit`, `TestHiddenRefsApplyToAnExistingCopy` | not built |
| With the variable unset, no git subprocess gets a `-c uploadpack.hideRefs` argument and each advertisement equals `git upload-pack --advertise-refs` run on the copy directly | `internal/httpgit`, `TestNoHiddenRefsByDefault` | not built |
| `refs` omits hidden references for `prefix` `refs/` and `refs/topos/`, returns exactly one for its full name, and `commits?ref=<hidden name>` answers 200 | `internal/api`, `TestRefsListingHidesNamespaces` | not built |
| An export omits hidden references and the objects only they reach, and re-imported it has the `rev-list --all` of the source's references that are not hidden | `internal/api`, `TestExportOmitsHiddenRefs` | not built |
| Against a node with the variable set and a repository holding 1 000 checkpoint references: a `git push` of one more is accepted, an SSH clone's advertisement is under 1 KiB, and a fresh clone fetches a checkpoint by id and checks out its files | `test/e2e`, `TestE2EHiddenCheckpointRefs` | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 002 | the variable's row |
| 003 | a sentence: references under an installation's hidden namespaces are pushed and fetched by id like any other and are not advertised |
| 009 | `refs` omits hidden references except by exact name |
| 012 | `limits.MaxHiddenRefPrefixes` |
| 019 | export leaves hidden references out; the export round trip compares the references that are not hidden |
