---
title: "Partial clones in the agent skill: when to clone, with which filter, and how to fetch history in one request"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/007-authentication-and-delegation.md
  - specs/012-limits-and-abuse.md
  - specs/025-agent-client.md
affects: [skills/origo/SKILL.md, docs/cli.md, internal/httpgit/, internal/origocli/, specs/025-agent-client.md, specs/README.md]
effort: small
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Partial clones in the agent skill

## Overview

A node serves partial clones: every copy has `uploadpack.allowFilter`
and `uploadpack.allowAnySHA1InWant` on (`internal/repo/cache.go`), and
spec 003 lists `filter` (`blob:none`, `tree:0`) among the capabilities a
client may rely on. The agent skill says "No clone", so an agent that
needs files on disk to build or test does a full clone, or a blobless
clone in which every missing file of history is fetched on demand.

Measured against a bare repository configured as a node configures its
copies and served over smart HTTP, 400 files in four directories of 100
and 25 commits, 785 blobs in all, counting HTTP requests:

| Pattern | Requests |
|---|---|
| full clone | 3, every blob |
| `git clone --filter=blob:none --sparse`, then `git sparse-checkout set src/a` | 3, then 2 for the 100 files |
| `git log -p -- src/a` in that clone | 48: one fetch per commit, each a `GET .../info/refs` and a `POST .../git-upload-pack` |
| `git backfill --sparse` first, then the same `log -p` | 2, then 0 |
| `git log -p -- src/b`, outside the sparse set | 50 |
| `git grep` at an older revision, outside the sparse set | 200: one fetch per file |
| `git clone --filter=tree:0`, then `git log --oneline -- docs` | 3, then 50: one fetch per commit |

Every request counts against the caller's bucket of spec 012
(`limits.RequestsPerMinute`, 600 a minute unless the authorizer names a
figure), so a `git log -p` that fetches for more than 300 commits
inside a minute is past it. A 429 on a fetch git makes on demand ends the git command with
exit status 128 (`could not fetch <id> from promisor remote`), and
`-c http.maxRetries=3` did not retry it on git 2.54.0.

This spec gives the skill a decision table (`origo` or a clone, and
which filter), a one-request fetch of a sparse set's history, a way to
hand git the token, and a test of the clone and backfill on a node.

## Current state

Not built. `skills/origo/SKILL.md` closes with "No clone, no git
subprocess", and spec 025 keeps git out of the `origo` command while
saying an agent that wants a whole tree clones it with git. Two tests
cover a filter: `case003PartialClone` in `test/conformance/cases003.go`
(`--filter=blob:none --no-checkout`, then `cat-file` of one blob through
the promisor) and the protocol v2 blobless clone in
`TestCloneFetchPushOverSmartHTTP` (`internal/httpgit/handler_test.go`).
Neither covers a sparse checkout or a batched fetch of history.
`TestTheSkillsResidentCostIsItsFrontmatter` and
`TestDocumentedCommandsMatchTheBinary` (`internal/origocli/docs_test.go`)
hold the skill to a frontmatter of at most 256 bytes and to commands the
binary serves.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| An `origo clone` command | the command runs the three git steps | spec 025 keeps git out of the command; it would need git on the machine anyway, and three lines in the skill cost bytes only when the skill is loaded |
| A clone URL in the repository representation | `GET /v1/repos/{id}` answers the URL git uses | spec 003 already fixes the id form, `<ORIGO_PUBLIC_URL>/r/<id>.git`, and `origo info --json` answers the id; a field is a contract change for a value a caller can build |
| The token in the clone URL | `https://x:<token>@host/r/<id>.git` | git stores the URL in `.git/config`, and the hour-long token of the skill stops working mid-task with no way to refresh it but rewriting the remote |
| **A decision table, a credential helper, and the backfill with its fallback, chosen** | skill text and one test | no change to the node or the command |

## Design

### The decision table

The skill gains a section, "When you need files on disk", around this:

| Task | Use | Requests |
|---|---|---|
| read one file, or a few | `origo cat <path>` | two per file |
| browse: list, read history, compare | `origo ls`, `origo log`, `origo diff` | one per page |
| build or test in a subtree, at one commit | `git clone --filter=blob:none --sparse <url>`, then `git sparse-checkout set <dir>...` | 3, then 2, whatever the subtree's size |
| the same, and read its history (`log -p`, `blame`, a diff across commits) | the above, then `git backfill --sparse` before the first history command | 2 more, then none |
| the whole tree at one commit | `git clone --filter=blob:none <url>` | 5 |
| history alone: commits, authors, merge bases | `git clone --filter=tree:0 --no-checkout <url>` | 3; but any command that names a path fetches trees one commit at a time |

And the commands never to run in a blobless clone before a backfill,
because each fetches one commit or one file at a time: `git log -p`,
`git blame` across history, and `git grep` at another revision; and,
after a backfill of the sparse set, any of them outside it.

### Without `git backfill`

`git backfill` is present in git 2.54.0 and absent in 2.47.3, the git of
Debian trixie, the base of this repository's own runtime image. The skill gives the
fallback, which needs `uploadpack.allowAnySHA1InWant` and therefore
works against every node:

```sh
git rev-list --objects --missing=print HEAD -- <dir> \
  | sed -n 's/^?//p' | xargs git fetch -q origin
```

On both versions the 96 blobs of `src/a`'s history arrived in one fetch
of 3 requests, after which `GIT_NO_LAZY_FETCH=1 git log -p -- src/a`
exited 0 with no request. Unlike `backfill --sparse`, it covers the
history of `HEAD` alone.

### The credential and the URL

```sh
export GIT_TERMINAL_PROMPT=0
ID=$(origo info --json | jq -r .id)
git clone --filter=blob:none --sparse \
  --config 'credential.helper=!f() { test "$1" = get && echo username=x && echo "password=$ORIGO_TOKEN"; }; f' \
  "$ORIGO_URL/r/$ID.git" work
```

Spec 007 accepts the token as the basic auth password. The helper is
stored in the clone's `.git/config` and reads `ORIGO_TOKEN` at each
request: the token is never on disk, and one minted after the hour
takes effect at the next fetch.
`GIT_TERMINAL_PROMPT=0` turns a refused credential into an error rather
than a prompt that hangs an agent. The skill says the clone URL names the
git host, which is `ORIGO_URL` unless the installation serves its API at
a separate origin (spec 029), and then the host its clone URLs name.

### What the skill's other text changes

The frontmatter description becomes "Read and change a repository on an
Origo git host with the origo command, and clone only what a task
needs." (112 bytes with the name, under the 256 the test allows). "What
it will not do" keeps "no git subprocess" for the command and points at
the new section for a clone. `docs/cli.md` gains one sentence linking
the section. The build adds no name: no command, flag, variable,
header, route, code or metric.

### Verified against git

Against a server running `git http-backend` with the two settings above,
with git 2.54.0 locally and 2.47.3 in the `golang:1.27` image (Debian
trixie):

| Behavior | 2.54.0 | 2.47.3 |
|---|---|---|
| the request counts of the Overview, `backfill` aside | as stated | identical |
| `git backfill --sparse` | one fetch, 2 requests | `'backfill' is not a git command` |
| the fallback above | one fetch, 3 requests | the same |
| `GIT_NO_LAZY_FETCH=1`: 0 inside the fetched set, 128 outside it | yes | yes |
| the credential helper: no token in `.git/config`, the old token refused with exit 128 after a rotation, the new one read from the environment | yes | yes |
| a 429 on a lazy fetch ends the command with 128, `http.maxRetries=3` notwithstanding | yes | not run; the string `maxRetries` is absent from its `git` and `git-remote-http` |

## Not in this spec

A clone URL field in the repository representation. A command that
wraps git. Raising `limits.RequestsPerMinute` for git traffic, or
counting a lazy fetch's two requests as one. SSH (spec 024). Non-cone
sparse patterns.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Against an in-process node with a fixture of 300 files in three directories and 20 commits, a `--filter=blob:none --sparse` clone, `sparse-checkout set` of one directory, and `backfill --sparse` make at most 3, 2 and 2 requests; afterwards `GIT_NO_LAZY_FETCH=1 git log -p` exits 0 in the directory and 128 outside it; on a client git without `backfill` the test runs the fallback instead and asserts the same | `internal/httpgit`, `TestBloblessSparseCloneAndBackfill` | not built |
| The same clone through the credential helper keeps no token in `.git/config` and fetches with a token set after the clone | `internal/httpgit`, `TestCloneCredentialHelperReadsTheEnvironment` | not built |
| The skill's frontmatter stays under 256 bytes and contains "origo command", and the body teaches `--filter=blob:none --sparse`, `git backfill --sparse`, the fallback's `--missing=print`, `credential.helper` and `GIT_TERMINAL_PROMPT=0` | `internal/origocli`, `TestTheSkillsResidentCostIsItsFrontmatter` with the new marks | not built |
| The skill names no `origo` command the binary does not serve | `internal/origocli`, `TestDocumentedCommandsMatchTheBinary`, unchanged | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 025 | The skill section's list of what the skill teaches gains the clone section, and Not in this spec's "Git" bullet points at it: the command still runs no git, and the skill tells an agent when git is the cheaper tool |
