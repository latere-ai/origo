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
updated: 2026-09-08
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
holds the codes and the header and no table of sentences; the
sentences the code sends are the `Message` fields of the
`httpjson.Error` literals in `cmd/origod`, `internal/httpgit`, and
`internal/api`, and spec 003's Outcome lists the ones that differ from
the contract, which the code table test below is what fixes. Spec 013's
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
    Fault:      <a Fault, to cut the bucket and delete an object; nil on a live target>,
    Skip:       []string{"<a subtest name>"},
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
Two rows need a fault in the bucket that a caller outside the
installation cannot cause: `storage_unavailable` (spec 003) needs the
bucket unreachable, and `repository_unavailable` (spec 015) needs a
pack object gone. `Fault` is an interface with `CutStorage(t)`, which
makes the bucket unreachable until the test ends, and
`DeleteObject(t, key)`; the stack run implements it with the
NetworkPolicy spec 015's cluster scenario uses (enforced by the Cilium
row of spec 013's overlay table) and a delete through the MinIO host
port, the stub run implements it on `wal.MemStore` in-process, and a
live target has none. The live run's `Skip` list is exactly the table
below and nothing else; the run prints each entry as skipped, so a
report with fewer or more skipped names is a failure of the run.

| Skipped on the live run | Why |
|---|---|
| the delegation group: `act` on a service token, the repository-bound token minted through delegation | needs `Issuer` to mint the token |
| the deny-flipping group: 403 before lookup, the authorizer outage, `authorizer_unavailable` | needs `Authorizer` to flip an answer |
| the `storage_unavailable` row of spec 003 | needs `Fault` to cut the bucket |
| the `repository_unavailable` row of spec 015 | needs `Fault` to delete a pack object |

Every other case runs against the live service, so the live run proves
the surface and the stack run proves the surface, the trust logic, and
the degraded rows.

The stack run reads its target from `ORIGO_TEST_URL` and
`ORIGO_TEST_ADMIN_TOKEN` (spec 002), whose defaults are the ports
table of spec 013 (`http://localhost:30080`, and a token minted at the
issuer's host port for the dev subject), and reaches the three stub
control endpoints at the host ports of the same table
(`http://localhost:30081`, `30082`, `30083`) and MinIO for its `Fault`
at `http://localhost:30900`, all held by the package as defaults so
spec 013's `e2e` job sets nothing. The live run reads `ORIGO_LIVE_URL` and
`ORIGO_LIVE_TOKEN` (spec 002), two repository secrets: the installation
a release reaches and a token with `admin` on the `conformance-`
prefix.

### The code table

`internal/contract` gains the code table as data: code, status, and
the one sentence, for every row of the Code tables of spec 003 and of
specs 007, 009, 015, 019, and 020, which is every code the
cross-reference in `specs/README.md` lists. `TestEveryCodeHasOneSentence`
walks every Go file of the module outside `tools/`, parses it with
`go/parser`, and collects every composite literal of type
`httpjson.Error` (the envelope type of `latere.ai/x/pkg/httpjson`,
which every handler renders through `httpjson.WriteError`): its `Code` field must be a `contract.Code*` constant
the table holds, and its `Message` field must be a string literal equal
to the table's sentence for that code. A `Message` built from an
expression (`err.Error()`, a concatenation, a `fmt.Sprintf`) is a
failure, because the reason belongs in `details`, and so is a table row
no literal sends, so a dead row is noticed. Each failure names the file
and line. The sideband and hook lines (`ERR <code>: <sentence>`,
`reject <code>: <sentence>`) are rendered from the same table by
`contract.Sentence(code)` and hold no sentence of their own, so the
grep over `httpjson.Error` literals is the whole of what can drift. The
suite compares live responses to the same table. The divergences spec
003's Outcome lists are fixed by making this test pass. The test walks
the module from a root held in a test-only constant resolved from its
own source file with `runtime.Caller`, never from the working
directory, so the `tempdir` gate of spec 002, which runs the suite
from an empty directory, and the `hermetic` gate see the module's
files; spec 011's register test reads its spec the same way, and the
builder is told here.

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
| `ORIGO_TEST_URL`, `ORIGO_TEST_ADMIN_TOKEN` | defined in spec 002's table; the stack run's target, set by spec 013's `e2e` job |
| `ORIGO_LIVE_URL`, `ORIGO_LIVE_TOKEN` | defined in spec 002's table; the live run's target, two repository secrets; the `live` job is skipped when the URL is unset, so a fork runs no live run |

### Runs

```mermaid
flowchart LR
  C[TestContract] --> K[kind stack of spec 013<br/>ORIGO_TEST_URL, stubs, Fault<br/>Skip: none]
  C --> S[test/stubs/origo in-process<br/>MemStore Fault<br/>Skip: none]
  C --> M[one node with MinIO<br/>ORIGO_TEST_DROP_CAPABILITY<br/>must fail]
  C --> L[ORIGO_LIVE_URL after a release<br/>no stubs, no Fault<br/>Skip: the four-entry list]
```

| Run | Target | When |
|---|---|---|
| stack | the kind stack of spec 013, in its `e2e` job, through `ORIGO_TEST_URL` and `ORIGO_TEST_ADMIN_TOKEN` with the stubs and `Fault` wired; `TestSameAnswersOnStubAndStack` runs in the same job, because it needs the stack as its second target | every push to `main` and every pull request, inside that job's 30 minute budget |
| stub | `test/stubs/origo` in-process, `TestStubConforms` with an empty `Skip` list | every push, in the unit suite |
| mutation | one node with MinIO, once per capability | every push |
| live | the installation `ORIGO_LIVE_URL` names, with `ORIGO_LIVE_TOKEN`, the `conformance-` prefix, and the skip list above | the `live` job of `release.yml`, after spec 017's deploy step and before its publish step, when the secret is set; its report and timings are attached to the release (spec 017) |
| previous release | the fixture of release N-1 on release N (spec 017, `TestPreviousReleaseFixture`) | in the release pipeline |

## Not in this spec

The stubs, the overlay, the tiers, and the CI budgets (spec 013). A
conformance run against a provider other than Origo. Performance
assertions beyond the thresholds the owning specs name.

## Acceptance criteria

- `TestContract` passes against the kind stack on every push to `main`
  inside spec 013's `e2e` job with nothing skipped, and against the
  installation `ORIGO_LIVE_URL` names after a release with exactly the
  four entries of the skip list skipped and each reported by name
  (`verify.yml`; `release.yml`, the `live` job; the release evidence of
  spec 017).
- With `Fault` wired, the `storage_unavailable` row answers 503 with
  the table's sentence while the bucket is cut and the
  `repository_unavailable` row answers 503 with `details.key` naming
  the deleted pack, on the stack and on the stub; on a target with no
  `Fault` both rows are skipped and reported (proposed:
  `test/conformance`, `TestContract/003/storage_unavailable`,
  `TestContract/015/repository_unavailable`).
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
  consumer-shaped flow against the in-process stub and against the
  stack at `ORIGO_TEST_URL` and compares the responses field by field;
  it runs in spec 013's `e2e` job beside `TestContract`).
- Every `httpjson.Error` literal in the module carries a `Code` the
  table holds and a `Message` that is the string literal of the
  table's sentence, every table row is sent by at least one literal,
  and a `Message` built from an expression fails with its file and
  line; the test fails on the tree as it stands today, on the
  lower-case sentences spec 003's Outcome lists, and passes once they
  are the table's (proposed: `internal/contract`,
  `TestEveryCodeHasOneSentence`).
- A run against a shared installation leaves no repository behind:
  after `TestContract`, `GET /v1/repos/{id}` answers 404 for every id
  the run created (proposed: `test/conformance`, `TestRunCleansUp`).
