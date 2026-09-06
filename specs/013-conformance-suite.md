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
affects: [test/conformance/, test/stubs/, test/e2e/, internal/contract/, internal/config/, internal/repo/, .github/workflows/, Makefile, deploy/examples/kind/, Dockerfile.stubs]
effort: large
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Conformance suite

## Overview

The contract in spec 003 is only worth something if it is checked. This
spec turns it into a Go test package that runs against any base URL: a
live Origo, a consumer's stub, or a future provider. Origo's own release
is gated on it, and consumers run it against their stubs so their
integration tests and the real service agree. The spec also owns the
integration tiers that exist today, the stubs every later spec's tests
need (an OIDC issuer, an authorizer, an event sink, and a stub of the
contract itself), and the `kind` overlay those tests run on. The stubs
and the overlay are built with spec 007, before the suite, because they
are what replaces `ORIGO_DEV_TOKEN`; the suite itself waits for specs
008 to 010. This spec depends on nothing above spec 012, so spec 017
(the conformance run) and spec 018 (the overlay) can depend on it.

## Current state

`test/e2e` (build tag `e2e`) runs a built `origod` against MinIO with
the real git: `TestPushThenCloneFromAnEmptyDisk`,
`TestConcurrentPushesToDifferentBranchesOnTwoNodes`, `TestKillMidPush`,
and `TestMeasure`. `internal/wal`'s `TestS3Suite` (build tag
`integration`) runs the store suite against the same MinIO. Both run
through `make test-integration` and not in CI: the shared
`lateregate.yml` has no services step. `test/conformance` and
`test/stubs` do not exist. The unit suites cover every package at 90%
or more on the in-process store and the real git.

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

### The stubs

`test/stubs/` holds four importable packages and one binary. Each
package has a `New(t testing.TB, ...) *Server` that starts an
`httptest.Server` and stops it with the test, a control API a test
drives, and no dependency beyond the standard library and
`latere.ai/x/pkg`. The binary `test/stubs/cmd/origo-stubs` runs the
first three from flags for `make dev` (spec 002) and as pods in the
`kind` overlay, and `Dockerfile.stubs` packages it as
`ghcr.io/latere-ai/origo-stubs`, built by `verify.yml` and loaded into
kind, never released.

| Package | Serves | Control |
|---|---|---|
| `test/stubs/issuer` | an OIDC issuer: the discovery document at `/.well-known/openid-configuration` with `jwks_uri`, the public keys at `/jwks`, one ES256 key generated at start or read from `-key <pem>`; a POST to `/mint` with `{"sub", "act", "aud", "exp", "nbf", "iat", "kid", "alg"}` answers `{"token"}`, every claim optional with defaults that verify, so a test mints the token for each row of spec 007's table by setting one field wrong; a POST to `/rotate` adds a key and drops the oldest | `Mint(claims) string`, `Rotate()`, `URL()` |
| `test/stubs/authorizer` | spec 007's endpoint: a POST to its root with the bearer `-token` answers from a rule table keyed by `(subject, actor, repo id or owner/slug, action)` with a default of allow for every subject in `-allow <subjects>` (`*` for all); the probe id `00000000-0000-0000-0000-000000000001` (spec 018) is always denied; `ttl`, `replicas`, and `quota_bytes` are per rule; a PUT to `/rules` replaces the table, a GET of `/requests` lists every request seen in order, a DELETE of it clears the list; `-fail <status>` makes every answer that status and `-hang` makes it never answer, for the outage cases | `Allow(rule)`, `Deny(rule, reason)`, `Requests()`, `Fail(status)`, `Hang()` |
| `test/stubs/sink` | spec 008's sink: a POST to its root verifies `Origo-Signature` with `-secret`, records the headers and body, and answers the configured status (200 by default; a PUT to `/status` changes it, with a count it fails that many then recovers); a GET of `/deliveries` with `repo` and `kind` parameters lists deliveries in order, a DELETE of it clears them | `Deliveries(repo, kind)`, `Fail(n, status)`, `Wait(repo, kind, n, timeout)` |
| `test/stubs/origo` | the contract stub a consumer's tests target: the real handlers of `internal/httpgit`, `internal/api`, and `internal/lfs` wired to `wal.MemStore`, a temporary cache directory, and an in-process issuer, authorizer, and sink from the packages above; `New(t)` returns the base URL, a minting function, and the authorizer and sink handles; it speaks the whole contract because it is the node's own code, and it is what `TestStubConforms` runs `TestContract` against | `URL()`, `Token(sub, act)`, `Authorizer()`, `Sink()` |

A consumer imports `test/stubs/origo` to run its integration tests
against Origo in-process, and `test/conformance` to prove its own stub
of the contract, if it writes one, matches.

### The stack

`test/e2e` gains a kind stack: MinIO, three `origod` replicas from the
candidate image, and `origo-stubs` running the issuer, the authorizer,
and the sink, applied from `deploy/examples/kind`, which this spec owns
and spec 018 lists as one of its three example overlays. It runs the
suite plus the failure scenarios that need a cluster: node kill
mid-push, cache pressure, compaction under load, the degraded-storage
cases of spec 015. Budget: 20 minutes on every push to `main`; after
every release, the suite runs against the live service with a dedicated
repository and its timings are attached to the release (spec 017).

### CI

The verify workflow gains a job `integration` that starts MinIO as a
service container and runs `make test-tiers`, the target spec 002
defines that runs the `integration` and `e2e` tiers against the test
bucket variables in the environment without starting compose;
`make test-integration` is the developer's form that starts compose and
calls the same target. The shared pipeline in `latere-ai/ci` gains an
optional `services` input so any service with a storage tier can do the
same; until that input exists the job is a plain job in Origo's own
`verify.yml`. A second job `kind` creates the cluster, builds and loads
the two images, applies `deploy/examples/kind`, and runs `TestContract`
and the scenarios above.

A mutation job runs `TestContract` once per capability the node
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
on every run. Any other value is a start-up error.

### Variables of the test tiers

Defined in spec 002's table with every other variable; what the tiers
do with them:

| Test variable | Purpose |
|---|---|
| `ORIGO_TEST_S3_ENDPOINT`, `ORIGO_TEST_S3_REGION`, `ORIGO_TEST_S3_BUCKET`, `ORIGO_TEST_S3_KEY`, `ORIGO_TEST_S3_SECRET`, `ORIGO_TEST_S3_PATH_STYLE` | the bucket the `integration` and `e2e` tiers use; the tiers skip when the endpoint is unset |
| `ORIGO_E2E_MEASURE` | `1` runs `TestMeasure`, which prints the measurements spec 004's Outcome records |
| `ORIGO_TEST_DROP_CAPABILITY` | set by the mutation job on the node under test, one name from the set above |

## Not in this spec

A conformance run against a provider other than Origo. Performance
assertions beyond the thresholds the owning specs name.

## Acceptance criteria

- `TestContract` passes against the kind stack on every push to `main`
  within the 20 minute budget, and against the live service after a
  release (`verify.yml`, the release evidence of spec 017).
- Removing any one git-controlled capability (`filter`,
  `allow-tip-sha1-in-want`, `allow-reachable-sha1-in-want`, `atomic`,
  `push-options`) from the server's advertised set fails at least one
  subtest, and an unknown value refuses start-up (proposed:
  `.github/workflows/verify.yml`, the `mutation` job driving
  `TestContract` with `ORIGO_TEST_DROP_CAPABILITY`; `internal/config`,
  `TestDropCapabilityIsOneOfTheSet`).
- The contract stub passes `TestContract` with an empty `Skip` list
  (proposed: `test/stubs/origo`, `TestStubConforms`).
- The stub issuer mints a token spec 007's verifier accepts and one
  refused token per row of its verification table; the stub authorizer
  answers its rule table, denies the probe id, and records requests in
  order; the stub sink refuses a bad signature and answers the
  configured failures then recovers (proposed: `test/stubs/issuer`,
  `TestMintsEachFailure`; `test/stubs/authorizer`, `TestRulesAndProbe`;
  `test/stubs/sink`, `TestSignatureAndFailures`).
- `make test-tiers` with the test bucket variables set runs both tiers
  without compose, and `kustomize build deploy/examples/kind` succeeds
  (`verify.yml`, the `integration` and `kind` jobs).
- Every code any package sends appears in the code table with one
  sentence, and no package sends a sentence the table does not have
  (proposed: `internal/contract`, `TestEveryCodeHasOneSentence`).
- The `integration` and `e2e` tiers run in CI on every push
  (`verify.yml`, the `integration` job).
