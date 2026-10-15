---
title: "Host-neutral API profile: the read and write routes any git host can serve alike, marked and checkable"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
  - specs/009-read-api-and-archive.md
  - specs/017-release-and-versioning.md
  - specs/020-server-side-git-operations.md
  - specs/021-conformance-suite.md
  - specs/030-the-openapi-document.md
affects: [test/conformance/, tools/apidoc/openapi.go, tools/apidoc/openapi_test.go, api/openapi.yaml, docs/api.md, CHANGELOG.md, specs/002-repository-scaffold.md, specs/003-protocol-contract.md, specs/021-conformance-suite.md, specs/030-the-openapi-document.md, specs/README.md]
effort: medium
created: 2026-10-15
updated: 2026-10-15
author: changkun
---

# Host-neutral API profile

## Overview

A tool that reads history, diffs two commits, or commits a file on a
server works against one host's API at a time. Every host that serves
git also serves some JSON surface for those operations, each with its own
paths, fields and refusals, so a tool that works on several hosts carries
one client per host for the same handful of git operations, or clones.

Origo publishes its surface as an OpenAPI document of 41 operations
(spec 030) and proves it with a conformance suite (spec 021). Fourteen
of those operations describe git and nothing else: the three smart HTTP
routes, the seven reads of spec 009, and the four writes of spec 020.
The other 27 are Origo's own: the repository lifecycle and directory,
administration, tokens, LFS, and the service routes. Neither artifact
says which is which, and the suite cannot run against any host but
Origo: `expectStatus` and `expectError` in `test/conformance/conformance.go`
fail an answer without `Origo-Contract: 1` or with a sentence other than
Origo's own, `session.create` makes every repository with Origo's
`POST /v1/repos` body, `session.repoURL` clones from `/r/<id>.git`, and
`session.cleanup` deletes by Origo id. Spec 021 states the limit itself:
"A conformance run against a provider other than Origo" is out of its
scope.

This spec names the fourteen as a versioned profile, marks them in the
OpenAPI document, and gives the suite a mode that any host can run
against itself by supplying one provisioning command. It adds nothing to
the node. Documentation alone would let no host show it conforms, and a
flag in the suite alone fails every host at `session.create`; the
provisioning seam and the relaxed checks are the smallest additions that
make the profile checkable.

## Current state

Not built. `tools/apidoc/openapi.go` renders one operation per endpoint
row, tags each by path (`transport`, `repositories`, `read`,
`operations`, `administration`, `tokens`, `service`) and marks the
operator rows with `x-scope` from `scopesBySpec`; `info.version` is the
contract version of spec 003. `test/conformance` runs every case of
every spec in `specs()` against `Target.URL`, with six groups that skip
when a field of `Target` is empty (`Groups`), and reports
`Report{Passed, Failed, Skipped, SkippedGroups, Unverified, Created}`.
The fixture (`session.newFixture`) is created and pushed through the
Origo-only helpers above, and the read and operation cases build their
paths as `"/v1/repos/" + id + ...`.

## Decision record

| Option | Shape | Why not |
|---|---|---|
| Documentation only | a list of the fourteen in `docs/api.md` | nothing checks it: no host can show it serves the profile and no tool can test a host before relying on it |
| A flag in the suite alone | `Target.Profile` skipping the other cases | a host without Origo's lifecycle fails at the fixture, before its first case, and every answer is held to `Origo-Contract` and Origo's English sentences |
| A second OpenAPI document of the profile | `api/profile.yaml` beside `api/openapi.yaml` | the same rows committed twice; a mark on each operation lets a consumer filter the one document, which is the handle spec 030 already gives with tags and `x-scope` |
| Neutral names for `Origo-Commit` and `Origo-Truncated` | two new headers and a period that sends both | every existing client pays for an alias that no second implementation yet reads; a later major version of the profile can rename them with both served |
| A discovery route | a well-known path answering the profile version | one more route each host adds before the profile is any use, where the document a host publishes already carries the version |
| **A profile table here, marks in the document, a suite mode with a provisioning command, chosen** | | checkable by any host, nothing new on the node, and Origo's own runs unchanged |

## Design

### What the profile holds

Profile version 1.0 is these fourteen operations, as their owning spec
defines them, addressed relative to two URLs a host publishes for each
repository: its API base (Origo: `<origin>/v1/repos/<id>`) and its clone
URL (Origo: `<ORIGO_PUBLIC_URL>/r/<id>.git`). The path grammar of spec
009 (a full object id or a short name per segment, the placeholder `-`
with `?ref=`, `?base=`, `?head=`), its path rules, and its bounds are
part of the profile. The bounds (`api.MaxRefs`, `api.MaxCompareBytes`,
`api.MaxBlobBytes`, `api.TreePageSize`, the commit page limits) are
fixed in 1.0, so a host with other caps fails `009/blob_too_large` and
its kin; a later minor version may let a host advertise its own.

| Operation | Owner | What a caller runs without it |
|---|---|---|
| `GET /{repo}/info/refs` | 003 | nothing: this and the next two are git itself, with the capability table of spec 003 (`filter`, `shallow`, `deepen-since`, `deepen-not`, `allow-tip-sha1-in-want`, `allow-reachable-sha1-in-want`, `atomic`, `push-options`, `report-status-v2`) |
| `POST /{repo}/git-upload-pack` | 003 | as above |
| `POST /{repo}/git-receive-pack` | 003 | as above |
| `GET /v1/repos/{id}/refs` | 009 | `git ls-remote <clone URL>`, reading peeled tags from its `^{}` lines |
| `GET /v1/repos/{id}/commits` | 009 | `git clone --filter=tree:0 --bare`, then `git log --format=... [--since] [--until] [-- <path>]`, which fetches a tree per commit when a path is given |
| `GET /v1/repos/{id}/commits/{sha}` | 009 | a blobless clone, then `git show --numstat <sha>`, fetching the blobs of that commit's change |
| `GET /v1/repos/{id}/compare/{base}...{head}` | 009 | a blobless clone, then `git diff -M <base> <head>` |
| `GET /v1/repos/{id}/tree/{sha}` | 009 | `git clone --filter=blob:none --no-checkout`, then `git ls-tree -l <sha> -- <path>/` |
| `GET /v1/repos/{id}/blob/{sha}` | 009 | a blobless clone, then `git cat-file blob <sha>` |
| `GET /v1/repos/{id}/archive/{sha}.tar.gz` | 009 | `git clone --depth 1`, then `git archive --format=tar.gz <sha>` |
| `POST /v1/repos/{id}/commits` | 020 | a clone, the files written, `git add`, `git commit`, `git push`, and a fetch and retry when the push is refused |
| `POST /v1/repos/{id}/merge` | 020 | a clone, `git merge` with the strategy's flag, `git push` |
| `POST /v1/repos/{id}/cherry-pick` | 020 | a clone, `git cherry-pick [-m <n>] <sha>...`, `git push` |
| `POST /v1/repos/{id}/revert` | 020 | a clone, `git revert [-m <n>] <sha>...`, `git push` |

What every profile answer carries, and what it does not:

| Part | In the profile |
|---|---|
| credentials | `Authorization: Bearer <token>` on the API base, and the token as the basic auth password on the clone URL, the two forms git and an HTTP client send without host-specific code |
| refusals | the envelope `{"error": {"code", "message", "details"}}` of spec 003, with the code, the status and the `details` keys the owning row names. `message` is the host's own sentence: the suite compares codes, not prose |
| codes | `invalid_request`, `unauthenticated`, `forbidden`, `repo_not_found`, `ref_not_found`, `non_fast_forward`, `over_quota`, `rate_limited`, `storage_unavailable`, `blob_too_large`, `operation_timeout`, `merge_conflict`, `invalid_change`, `repo_frozen` |
| headers | `ETag` with `If-None-Match` answered 304, `Retry-After` on 429 and 503, and `Origo-Commit` and `Origo-Truncated` under these names, as the read rows define them |
| operation answers | `commit`, `branch`, `tree`, and `committed: false` on a dry run. `entry_seq` is a position in Origo's log and may be `null` on another host |
| not in it | `Origo-Contract`, `Origo-Stale`, `Origo-Prefer`, the committer name a host writes and any trailer it adds, push events, LFS (a protocol with its own published API), SSH, and every route of the other 27 operations |

What spec 037 adds to compare and spec 038 adds to merge joins the
profile in a minor version when each is built.

### Versioning

The profile has its own version, `<major>.<minor>`, independent of
`Origo-Contract` and of Origo's release tags:

| Change | Profile version |
|---|---|
| an operation, parameter, field or code added to the profile | minor |
| one removed, or its meaning changed | major, made only together with a major of Origo's contract, whose twelve months of the previous behavior (spec 017) cover the profile too |
| any change to an operation outside the profile | none |

This spec keeps the history, one row per version:

| Version | Adds |
|---|---|
| 1.0 | the fourteen operations and the parts above |

`CHANGELOG.md` names the profile version beside a release whenever it
moves. Origo's contract number stays the thing an Origo client pins;
the profile version is what a tool that targets several hosts pins.

### The OpenAPI document

`tools/apidoc/openapi.go` gains `profileOperations`, a map from
`METHOD path` to the profile version that added the operation, the way
`summaries` names each action and `scopesBySpec` marks operator rows.
Each operation in the map renders `x-profile` with that version, and
`info` renders `x-profile-version`, the current one. Every other
operation renders neither. A test in `tools/apidoc` reads this spec's
profile table from the index the tool already builds and fails when the
map and the table differ, so the mark and the list cannot drift.

### The suite's profile mode

`test/conformance` changes in four places; Origo's own runs keep today's
behavior because every new field defaults to it.

1. `Target.Profile` selects the profile's cases. Each `testCase` gains a
   `profile` flag, set on the cases that assert only profile behavior:
   `003/capability/*`, `003/fetch-by-hash`, `003/partial-clone`,
   `003/shallow`, `003/atomic-push`, `003/non_fast_forward`, every case
   of `009`, and every case of `020`. Assertions inside them that read
   an Origo-only answer (a push event, the repository representation,
   the label clone URL) run only outside profile mode.
2. `Target.Repositories`, a `Provisioner` with `Create(t, label)
   Repository` and `Remove(t, Repository)`, where `Repository` is
   `{API, Clone string}`. Nil means Origo's lifecycle, today's
   `session.create` and `session.cleanup`. Every case builds its paths
   from `Repository.API` and clones from `Repository.Clone` instead of
   `"/v1/repos/" + id` and `session.repoURL`.
3. In profile mode `expectStatus` does not read `Origo-Contract`, and
   `expectError` compares the status, the code and the `details` keys,
   not `message`.
4. `Report` gains `Profile`, the version the run checked, set only when
   nothing failed and nothing was skipped.

A host outside Go runs it through `TestProfile`, a test in the package
that skips unless its three variables are set, from a checkout of
Origo's repository at a release tag, with Go and git on the machine:

```sh
ORIGO_PROFILE_URL=https://git.example.com \
ORIGO_PROFILE_TOKEN=<bearer> \
ORIGO_PROFILE_PROVISION=./provision.sh \
go test ./test/conformance -run TestProfile -json
```

The provisioning command is called as `<command> create <label>` and
prints one JSON object, `{"api": "<API base>", "clone": "<clone URL>"}`,
for an empty repository whose default branch is `main` and on which the
token may read and write; it is called as `<command> remove` with that
object on standard input when the run ends. A non-zero exit fails the
case that asked. `-json` is the machine-readable report, one record per
subtest under `TestProfile/<spec>/<case>`, and the last log line names
the profile version passed or `profile not passed`.

### Proving the mode needs nothing Origo owns

`TestProfileNeedsNothingOrigoOwns` runs the profile mode against the
contract stub of spec 013 (`test/stubs/origo`) behind a reverse proxy in
the test that removes everything outside the profile: it serves the API
base at `/api/<owner>/<name>/` and rewrites to `/v1/repos/<id>/`, serves
git at `/git/<owner>/<name>.git`, deletes `Origo-Contract` and
`Origo-Stale` from every answer, replaces every `message` with a
sentence of its own, and answers 404 to every path outside the fourteen.
Its provisioner creates repositories through the stub's own lifecycle,
which the proxy does not expose. The run passes every profile case, so a
host that serves the fourteen operations and nothing else of Origo's
passes too.

### Names the build adds

Each moves, with the build, into the table of the spec that owns its
kind: the three variables into spec 002's reference beside
the row of `ORIGO_TEST_URL`, the `Target` fields and the test into spec 021,
the two document fields into spec 030.

| Name | Kind | Meaning |
|---|---|---|
| ORIGO_PROFILE_URL | variable of `TestProfile` | the target's base URL; unset, the test skips |
| ORIGO_PROFILE_TOKEN | variable of `TestProfile` | the bearer every case sends |
| ORIGO_PROFILE_PROVISION | variable of `TestProfile` | the provisioning command |
| `x-profile` | OpenAPI field on an operation | the profile version that added it |
| `x-profile-version` | OpenAPI field on `info` | the profile version the document describes |
| `Target.Profile`, `Target.Repositories`, `conformance.Provisioner`, `conformance.Repository`, `Report.Profile` | Go identifiers | the suite's profile mode |
| `TestProfile`, `TestProfileNeedsNothingOrigoOwns` | Go tests | the run and its proof |
| `profileOperations` | Go identifier in `tools/apidoc` | the map behind `x-profile` |

No route, header, error code or metric is added, and the node does not
change.

## Not in this spec

Publishing the profile anywhere but this repository, or under a body
other than its maintainers. A profile for repository creation, deletion,
or naming, which hosts model too differently to share yet. Webhook
payloads. SSH. LFS. Translating Origo's codes to another host's: a host
that serves the profile answers the profile's codes.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The generated document marks exactly the fourteen operations of the profile table with `x-profile: "1.0"`, carries `x-profile-version: "1.0"` on `info`, and marks no other operation | `tools/apidoc`, `TestProfileMarksMatchTheSpec` | not built |
| Every case flagged `profile` reads paths from `Repository.API` and clones from `Repository.Clone`; a run with `Target.Profile` and Origo's default provisioner passes against the stub with the same cases passing as in a full run | `test/conformance`, `TestStubConformsInProfileMode` | not built |
| Behind the proxy that renames the paths, drops `Origo-Contract` and `Origo-Stale`, rewrites every `message`, and refuses every other path, the profile mode passes all its cases and `Report.Profile` is `1.0`; the full mode against the same proxy fails at the fixture | `test/conformance`, `TestProfileNeedsNothingOrigoOwns` | not built |
| `TestProfile` with a provisioning script against a node of the stack creates and removes each repository it uses, leaves none behind, and its `-json` output names every profile case once | `test/conformance`, `TestProfile` in spec 013's `e2e` job | not built |
| A full run, without `Target.Profile`, asserts `Origo-Contract` and each sentence exactly as today | `test/conformance`, `TestStubConforms` unchanged | not built |

## Changes to other specs

Each lands with the build, in the same change:

| Spec | Change |
|---|---|
| 002 | the three profile variables join the test rows of the reference |
| 003 | one paragraph naming the profile and pointing here, beside the capability table |
| 021 | `Target.Profile`, `Target.Repositories`, `Report.Profile`, the `profile` flag on cases, and `TestProfile`; Not in this spec no longer excludes a run against another provider |
| 030 | `x-profile` and `x-profile-version` join the operation and `info` rules |
| README | the Open source readiness section names the profile and how another host runs it |
