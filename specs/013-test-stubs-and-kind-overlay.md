---
title: "Test stubs and the kind overlay"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/007-authentication-and-delegation.md
affects: [test/stubs/, test/e2e/, deploy/examples/kind/, Dockerfile.stubs, Makefile, .github/workflows/, internal/config/]
effort: medium
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Test stubs and the kind overlay

## Overview

Every spec after 007 needs the same three things beside the node to be
tested: an OIDC issuer that mints any token, an authorizer whose answer
a test chooses, and an event sink that records what it received. This
spec owns those stubs, a fourth that is the contract itself served
in-process, the `kind` overlay that runs the stubs and three nodes
beside MinIO, the test tiers and the targets that run them, and the CI
jobs that give the tiers a budget. It is built with spec 007 because
the stubs are what replaces `ORIGO_DEV_TOKEN`. The conformance suite
that runs on this stack is spec 021.

## Current state

`test/e2e` (build tag `e2e`) runs a built `origod` against MinIO with
the real git: `TestPushThenCloneFromAnEmptyDisk`,
`TestConcurrentPushesToDifferentBranchesOnTwoNodes`, `TestKillMidPush`,
and `TestMeasure`. `internal/wal`'s `TestS3Suite` (build tag
`integration`) runs the store suite against the same MinIO. Both run
through `make test-integration` and not in CI: the shared
`lateregate.yml` has no services step and Origo's `verify.yml` runs
only the gate and the spec cross-reference test. `test/stubs`,
`deploy/examples/kind`, and `Dockerfile.stubs` do not exist. The unit
suites cover every package at 90% or more on the in-process store and
the real git.

Three targets spec 002 assigned after it was complete are this spec's,
for the builder: `make fuzz`, which runs every fuzz function in the
module for 40 seconds (`go test -run=^$ -fuzz=<name> -fuzztime=40s`,
one package at a time, the list from `go test -list '^Fuzz'`) and the
`fuzz` job in `verify.yml` that calls it weekly from a `schedule`
trigger; `make test-tiers`, which runs the `integration` and `e2e`
tiers against the test bucket variables the environment carries
without starting compose; and the form of `make dev` that runs the
stub issuer and authorizer beside MinIO. Every fuzz function the deck
names (specs 004, 007, 009, 016, 020) runs under `make fuzz`.

## Design

### The stubs

`test/stubs/` holds four importable packages and one binary. Each
package has a `New(t testing.TB, ...) *Server` that starts an
`httptest.Server` and stops it with the test, a control API a test
drives, and no dependency beyond the standard library and
`latere.ai/x/pkg`. The binary `test/stubs/cmd/origo-stubs` runs the
first three from flags for `make dev` and as pods in the `kind`
overlay, and `Dockerfile.stubs` packages it as
`ghcr.io/latere-ai/origo-stubs`, built by `verify.yml` and loaded into
kind, never released.

| Package | Serves | Control |
|---|---|---|
| `test/stubs/issuer` | an OIDC issuer: the discovery document at `/.well-known/openid-configuration` with `jwks_uri`, the public keys at `/jwks`, one ES256 key generated at start or read from `-key <pem>`; a POST to `/mint` with `{"sub", "act", "aud", "exp", "nbf", "iat", "kid", "alg"}` answers `{"token"}`, every claim optional with defaults that verify, so a test mints the token for each row of spec 007's table by setting one field wrong; a POST to `/rotate` adds a key and drops the oldest; a POST to `/hang` makes discovery and JWKS never answer, for spec 007's `issuer_unavailable` case; it serves plain HTTP, which is why the node lists it in `ORIGO_OIDC_INSECURE_ISSUERS` (spec 007) wherever its host is not a loopback address | `Mint(claims) string`, `Rotate()`, `Hang()`, `URL()` |
| `test/stubs/authorizer` | spec 007's endpoint: a POST to its root with the bearer `-token` answers from a rule table keyed by `(subject, actor, repo id or owner/slug, action)` with a default of allow for every subject in `-allow <subjects>` (`*` for all); the probe id `00000000-0000-0000-0000-000000000001` (spec 018) is always denied; `ttl`, `replicas`, and `quota_bytes` are per rule; a PUT to `/rules` replaces the table, a GET of `/requests` lists every request seen in order, a DELETE of it clears the list; `-fail <status>` makes every answer that status and `-hang` makes it never answer, for the outage cases | `Allow(rule)`, `Deny(rule, reason)`, `Requests()`, `Fail(status)`, `Hang()` |
| `test/stubs/sink` | spec 008's sink: a POST to its root verifies `Origo-Signature` with `-secret`, records the headers and body, and answers the configured status (200 by default; a PUT to `/status` changes it, with a count it fails that many then recovers); a GET of `/deliveries` with `repo` and `kind` parameters lists deliveries in order, a DELETE of it clears them | `Deliveries(repo, kind)`, `Fail(n, status)`, `Wait(repo, kind, n, timeout)` |
| `test/stubs/origo` | the contract stub a consumer's tests target: the real handlers of `internal/httpgit`, `internal/api`, and `internal/lfs` wired to `wal.MemStore`, a temporary cache directory, and an in-process issuer, authorizer, and sink from the packages above; `New(t)` returns the base URL, a minting function, and the authorizer and sink handles; it speaks the whole contract because it is the node's own code, which spec 021 proves by running `TestContract` against it | `URL()`, `Token(sub, act)`, `Authorizer()`, `Sink()` |

A consumer imports `test/stubs/origo` to run its integration tests
against Origo in-process. The slow proxy of spec 015
(`test/stubs/slowproxy`) lives beside these and is that spec's.

### Local stack

`make dev` (spec 002) builds the binary, starts MinIO from
`docker-compose.yml`, and from this spec on also runs
`test/stubs/cmd/origo-stubs` beside it, points `ORIGO_OIDC_ISSUERS`,
`ORIGO_OIDC_INSECURE_ISSUERS`, and `ORIGO_AUTHORIZER_URL` at the stubs,
and prints a clone line with a token the stub issuer minted.
`make test-tiers` runs the `integration` and `e2e` tiers against
whatever values of the test bucket variables the environment carries,
which is what CI calls with its service container; `make
test-integration` is the developer's form that starts compose and
calls the same target.

### The stack

`deploy/examples/kind`, which spec 018 lists as one of its three
example overlays, applies MinIO, three `origod` replicas from the
candidate image, and `origo-stubs` running the issuer, the authorizer,
and the sink, to a kind cluster. It sets `ORIGO_OIDC_INSECURE_ISSUERS`
to the stub issuer's in-cluster URL, `ORIGO_GOSSIP_SECRET` (spec 005)
to a fixed value, and the HorizontalPodAutoscaler's scale-down window
to 60 seconds (spec 005) so the autoscaler test finishes inside its
job. `test/e2e` gains the scenarios that need a cluster: node kill
mid-push, cache pressure, compaction under load, the degraded-storage
cases of spec 015, the autoscaler and node-removal cases of spec 005,
and the event repair case of spec 008. From spec 021 on the
conformance suite runs on the same stack.

### CI

`verify.yml` gains four jobs beside the gate:

| Job | Runs | Budget |
|---|---|---|
| `integration` | MinIO as a service container, then `make test-tiers`: the `integration` tier and the `e2e` tier against one node, which is every end-to-end test that needs no cluster | 15 minutes |
| `e2e` | creates the kind cluster, builds and loads the two images, applies `deploy/examples/kind`, and runs the `e2e` tier's cluster scenarios except the two below; from spec 021 on, `TestContract` too | 30 minutes |
| `e2e-slow` | the same cluster, then spec 005's `TestAutoscalerScalesUp` and `TestReplicasScaleReads` and spec 008's `TestEventRepairAfterKill`, which each wait on a timer the others do not | 30 minutes |
| `fuzz` | `make fuzz` on a weekly `schedule` trigger | 60 minutes |

The two cluster jobs run in parallel on every push to `main` and every
pull request. A job over its budget fails the push; a scenario that
needs more time moves to `e2e-slow`, and one that makes `e2e-slow`
exceed its budget is a spec change, not a budget change. The shared
pipeline in `latere-ai/ci` gains an optional `services` input so any
service with a storage tier can run the `integration` job the same
way; until that input exists the jobs are plain jobs in Origo's own
`verify.yml`.

### Variables of the test tiers

Defined in spec 002's table with every other variable; what the tiers
do with them:

| Test variable | Purpose |
|---|---|
| `ORIGO_TEST_S3_ENDPOINT`, `ORIGO_TEST_S3_REGION`, `ORIGO_TEST_S3_BUCKET`, `ORIGO_TEST_S3_KEY`, `ORIGO_TEST_S3_SECRET`, `ORIGO_TEST_S3_PATH_STYLE` | the bucket the `integration` and `e2e` tiers use; the tiers skip when the endpoint is unset |
| `ORIGO_E2E_MEASURE` | `1` runs `TestMeasure`, which prints the measurements spec 004's Outcome records and asserts nothing; every threshold a spec names is a plain test of the `e2e` tier that runs without it |

## Not in this spec

The conformance suite, the code table, the mutation job, and the run
against the live service (spec 021). A stub of a provider other than
Origo.

## Acceptance criteria

- The stub issuer mints a token spec 007's verifier accepts and one
  refused token per row of its verification table, and stops answering
  on `Hang`; the stub authorizer answers its rule table, denies the
  probe id, and records requests in order; the stub sink refuses a bad
  signature and answers the configured failures then recovers
  (proposed: `test/stubs/issuer`, `TestMintsEachFailure`;
  `test/stubs/authorizer`, `TestRulesAndProbe`; `test/stubs/sink`,
  `TestSignatureAndFailures`).
- The contract stub serves a clone, a push, and the lifecycle table of
  spec 003 to the real git and `net/http` in-process (proposed:
  `test/stubs/origo`, `TestStubServesTheContract`; spec 021's
  `TestStubConforms` is the full proof).
- `make dev` prints a clone line whose token the node accepts, and
  `make test-tiers` with the test bucket variables set runs both tiers
  without compose (proposed: `Makefile`, exercised by the `integration`
  job; `test/e2e`, `TestDevTokenLineClones`).
- `kustomize build deploy/examples/kind` succeeds and the applied
  overlay reaches three ready nodes and the stubs within 5 minutes
  (proposed: `verify.yml`, the `e2e` job's set-up step).
- The `integration`, `e2e`, and `e2e-slow` jobs run on every push
  within their budgets, with spec 005's autoscaler test and spec 008's
  repair test in `e2e-slow` and spec 015's scenarios in `e2e`
  (`verify.yml`, the three jobs).
- `make fuzz` runs every fuzz function in the module for 40 seconds and
  the weekly schedule in `verify.yml` calls it (proposed: `Makefile`,
  the `fuzz` target; `verify.yml`, the `fuzz` job on `schedule`).
