---
title: "Conformance suite: the contract as executable tests"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/008-push-events.md
  - specs/009-read-api-and-archive.md
affects: [test/conformance/, test/e2e/, internal/contract/, .github/workflows/, Makefile, deploy/examples/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Conformance suite

## Overview

The contract in spec 003 is only worth something if it is checked. This
spec turns it into a Go test package that runs against any base URL: a
live Origo, a consumer's stub, or a future provider. Origo's own release
is gated on it, and consumers run it against their stubs so their
integration tests and the real service agree. The spec also owns the
integration tiers that exist today and their variables.

## Current state

`test/e2e` (build tag `e2e`) runs a built `origod` against MinIO with
the real git: `TestPushThenCloneFromAnEmptyDisk`,
`TestConcurrentPushesToDifferentBranchesOnTwoNodes`, `TestKillMidPush`,
and `TestMeasure`. `internal/wal`'s `TestS3Suite` (build tag
`integration`) runs the store suite against the same MinIO. Both run
through `make test-integration` and not in CI: the shared
`lateregate.yml` has no services step. `test/conformance` does not
exist. The unit suites cover every package at 90% or more on the
in-process store and the real git.

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
administration operations of spec 019, and every error code with its
sentence. Cases a target does not support are skipped by a `Skip` list
on the target, never silently.

`internal/contract` gains the code table as data (code, status,
sentence) and a test that every code the packages send is in it with
that sentence; the suite compares live responses to the same table.

### The stack

`test/e2e` gains a kind stack: MinIO, three `origod` replicas from the
release image, a stub issuer (`test/conformance/stub/issuer`), a stub
authorizer and event sink (`test/conformance/stub/consumer`), applied
from `deploy/examples/kind` (spec 018). It runs the suite plus the
failure scenarios that need a cluster: node kill mid-push, cache
pressure, compaction under load, the degraded-storage cases of spec 015.
Budget: 20 minutes on every push to `main`; after every release, the
suite runs against the live service with a dedicated repository and its
timings are attached to the release (spec 017).

### CI

The verify workflow gains a job that starts MinIO as a service container
and runs `make test-integration`, which today is the `integration` and
`e2e` tags. The shared pipeline in `latere-ai/ci` gains an optional
`services` input so any service with a storage tier can do the same;
until that input exists the job is a plain step in Origo's own
`verify.yml`. A mutation run in CI removes each capability of spec 003's
table from the server configuration in turn and asserts at least one
subtest fails for each.

### Variables of the test tiers

Read by the tests, never by `origod`:

| Variable | Purpose |
|---|---|
| `ORIGO_TEST_S3_ENDPOINT`, `ORIGO_TEST_S3_REGION`, `ORIGO_TEST_S3_BUCKET`, `ORIGO_TEST_S3_KEY`, `ORIGO_TEST_S3_SECRET`, `ORIGO_TEST_S3_PATH_STYLE` | the bucket the `integration` and `e2e` tiers use; the tiers skip when the endpoint is unset |
| `ORIGO_E2E_MEASURE` | `1` runs `TestMeasure`, which prints the measurements spec 004's Outcome records |
| `ORIGO_TEST_DROP_CAPABILITY` | the mutation job sets it to one capability name; the node under test stops advertising it |

## Not in this spec

A conformance run against a provider other than Origo. Performance
assertions beyond the thresholds the owning specs name.

## Acceptance criteria

- `TestContract` passes against the kind stack on every push to `main`
  within the 20 minute budget, and against the live service after a
  release (`verify.yml`, the release evidence of spec 017).
- Removing any one capability from the server's advertised set fails at
  least one subtest, for every capability in spec 003's table
  (proposed: `.github/workflows/verify.yml`, the `mutation` job driving
  `TestContract` with `ORIGO_TEST_DROP_CAPABILITY`).
- A consumer's stub of the contract passes `TestContract` with the same
  `Skip` list as the live service (proposed: the stub under
  `test/conformance/stub/origo`, `TestStubConforms`).
- Every code any package sends appears in the code table with one
  sentence, and no package sends a sentence the table does not have
  (proposed: `internal/contract`, `TestEveryCodeHasOneSentence`).
- The `integration` and `e2e` tiers run in CI on every push
  (`verify.yml`, the `integration` job).
