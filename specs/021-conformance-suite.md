---
title: "Conformance suite: the contract as executable tests"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/007-authentication-and-delegation.md
  - specs/008-push-events.md
  - specs/009-read-api-and-archive.md
  - specs/010-lfs.md
  - specs/013-test-stubs-and-kind-overlay.md
  - specs/019-repository-administration.md
affects: [test/conformance/, test/stubs/origo/, internal/contract/, internal/config/, internal/repo/, .github/workflows/]
effort: large
created: 2026-09-07
updated: 2026-09-07
author: changkun
---

# Conformance suite

## Overview

The contract in spec 003 is only worth something if it is checked. This
spec turns it into a Go test package that runs against any base URL: a
live Origo, the contract stub of spec 013, a consumer's own stub, or a
future provider. Origo's own release is gated on it (spec 017), and
consumers run it against their stubs so their integration tests and the
real service agree. The stubs and the stack it runs on are spec 013's;
this spec is the suite, the code table it compares against, the
mutation job that proves the suite can fail, and the run against the
live service after a release.

## Current state

Nothing of this spec exists. `test/e2e` covers the flows of specs 003
and 004 against one node with the phase 1 bearer; `internal/contract`
holds the codes and the header and no table of sentences; spec 003's
Outcome lists the sentences the code sends that differ from the
contract, which the code table test below is what fixes. Spec 013's
stubs and overlay, which the suite needs for its delegation and
deny-flipping cases and for its CI run, are not built either.

## Design

### The package

`test/conformance` is importable:

```go
conformance.Run(t, conformance.Target{
    URL:        "https://git.example.com",
    Token:      "<a token with admin on every repository the suite creates>",
    Issuer:     "<the stub issuer's URL, for the delegation and token cases>",
    Authorizer: "<the stub authorizer's control URL, to flip allow and deny>",
    EventsSink: "<the stub sink's control URL, to read deliveries>",
})
```

It drives the real `git` binary and `net/http` against the target and
asserts every table of spec 003 and of the specs it points at, one
subtest per row, named `TestContract/<spec>/<row>`: the repository
lifecycle, each advertised capability, fetch by reachable hash, atomic
pushes, non-fast-forward rejection, the read endpoints against a fixture
it pushes itself, archive reproducibility, delegation with `act`,
repository-bound tokens, push event delivery and signature, LFS, the
administration operations of spec 019, the server-side operations of
spec 020 once they exist, and every error code with its sentence. Every
repository the suite creates carries a slug prefixed `conformance-` and
is deleted at the end of the run, so a run against a shared installation
leaves nothing.

Cases a target does not support are skipped by a `Skip` list on the
target, never silently: each skipped case is reported by name. Two
groups skip on their own when the field they need is empty, because
they drive the stubs and a live service has none: the delegation cases
(`act` on a service token, which need `Issuer` to mint one) and the
deny-flipping cases (403 before lookup, the authorizer outage, and
`authorizer_unavailable`, which need `Authorizer` to flip an answer).
Against a live service with a real issuer and authorizer those two
groups are skipped and every other case runs, so the live run proves
the surface and the stack run proves the surface and the trust logic.

### The code table

`internal/contract` gains the code table as data: code, status, and
the one sentence, for every code spec 003 and the specs it points at
define. A test over the packages asserts that every code they send is
in the table with that sentence and that no package sends a sentence
the table does not have; the suite compares live responses to the same
table. The divergences spec 003's Outcome lists are fixed by making
this test pass.

### The mutation job

A job in `verify.yml` runs `TestContract` once per capability the node
controls through git configuration, with `ORIGO_TEST_DROP_CAPABILITY`
(spec 002) set to that capability's name, and asserts at least one
subtest fails each time. The variable is read by `internal/config` and
acted on by `internal/repo` when it writes the repository
configuration of spec 004: `filter` turns `uploadpack.allowFilter` off,
`allow-tip-sha1-in-want` and `allow-reachable-sha1-in-want` turn
`uploadpack.allowAnySHA1InWant` off, `atomic` turns
`receive.advertiseAtomic` off, `push-options` turns
`receive.advertisePushOptions` off. `shallow`, `deepen-since`,
`deepen-not`, and `report-status-v2` are advertised by git whatever the
configuration and are not in the mutation set; the suite asserts them
on every run. Any other value is a start-up error. The job runs against
one node with MinIO, like the `integration` job of spec 013, because
the capabilities are per node.

| Test variable | Purpose |
|---|---|
| `ORIGO_TEST_DROP_CAPABILITY` | defined in spec 002's table; set by the mutation job on the node under test, one name from the set above |

### Runs

| Run | Target | When |
|---|---|---|
| stack | the kind stack of spec 013, in its `e2e` job | every push to `main` and every pull request, inside that job's 30 minute budget |
| stub | `test/stubs/origo` in-process, `TestStubConforms` with an empty `Skip` list | every push, in the unit suite |
| mutation | one node with MinIO, once per capability | every push |
| live | the installation `ORIGO_RELEASE_DEPLOY` names, with a dedicated repository prefix and the two stub-driven groups skipped | after every release; the timings are attached to the release (spec 017) |
| previous release | the fixture of release N-1 on release N (spec 017, `TestPreviousReleaseFixture`) | in the release pipeline |

## Not in this spec

The stubs, the overlay, the tiers, and the CI budgets (spec 013). A
conformance run against a provider other than Origo. Performance
assertions beyond the thresholds the owning specs name.

## Acceptance criteria

- `TestContract` passes against the kind stack on every push to `main`
  inside spec 013's `e2e` job, and against the live service after a
  release with exactly the delegation and deny-flipping groups skipped
  and reported (`verify.yml`; the release evidence of spec 017).
- Removing any one git-controlled capability (`filter`,
  `allow-tip-sha1-in-want`, `allow-reachable-sha1-in-want`, `atomic`,
  `push-options`) from the server's advertised set fails at least one
  subtest, and an unknown value refuses start-up (proposed:
  `.github/workflows/verify.yml`, the `mutation` job driving
  `TestContract` with `ORIGO_TEST_DROP_CAPABILITY`; `internal/config`,
  `TestDropCapabilityIsOneOfTheSet`).
- The contract stub passes `TestContract` with an empty `Skip` list
  (proposed: `test/stubs/origo`, `TestStubConforms`).
- A consumer's integration tests written against the stub pass
  unchanged against a live node (spec 003's criterion; proposed:
  `test/conformance`, `TestSameAnswersOnStubAndStack`, which runs one
  consumer-shaped flow against both targets and compares the
  responses field by field).
- Every code any package sends appears in the code table with one
  sentence, and no package sends a sentence the table does not have
  (proposed: `internal/contract`, `TestEveryCodeHasOneSentence`).
- A run against a shared installation leaves no repository behind:
  after `TestContract`, `GET /v1/repos/{id}` answers 404 for every id
  the run created (proposed: `test/conformance`, `TestRunCleansUp`).
