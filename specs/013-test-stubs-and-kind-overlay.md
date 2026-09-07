---
title: "Test stubs and the kind overlay"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/007-authentication-and-delegation.md
affects: [test/stubs/, test/e2e/, deploy/examples/kind/, Dockerfile.stubs, Makefile, .github/workflows/, internal/config/, tools/docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-08
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

One more item for the builder: the CI jobs below select tests by a
name prefix, so the three phase 1 scenarios are renamed
`TestE2EPushThenCloneFromAnEmptyDisk`,
`TestE2EConcurrentPushesToDifferentBranchesOnTwoNodes`, and
`TestE2EKillMidPush`, the names specs 001 and 004 now carry;
`TestMeasure` keeps its name because `ORIGO_E2E_MEASURE` selects it
and no job regex does.

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
| `test/stubs/authorizer` | spec 007's endpoint: a POST to its root with the bearer `-token` answers from a rule table keyed by `(subject, actor, repo id or owner/slug, action)` with a default of allow for every subject in `-allow <subjects>` (`*` for all); the probe id `00000000-0000-0000-0000-000000000001` is always denied, the rule spec 007's authorizer contract states and spec 018's check relies on; `ttl`, `replicas`, and `quota_bytes` are per rule; a PUT to `/rules` with `{"rules": [{"subject", "actor", "repo", "action", "allow", "reason", "ttl", "replicas", "quota_bytes"}]}` replaces the table, `subject`, `actor`, `repo`, and `action` each `*` or a value, `repo` an id or `owner/slug`, `allow` a boolean and the rest optional; a GET of `/requests` lists every request seen in order, a DELETE of it clears the list; `-fail <status>` makes every answer that status and `-hang` makes it never answer, for the outage cases | `Allow(rule)`, `Deny(rule, reason)`, `Requests()`, `Fail(status)`, `Hang()` |
| `test/stubs/sink` | spec 008's sink: a POST to its root verifies `Origo-Signature` with `-secret`, records the headers and body, and answers the configured status (200 by default; a PUT to `/status` with `{"status": <int>, "body": <json>, "count": <int>}` changes it, answering `status` with `body` for the next `count` deliveries, every one when `count` is 0, then 200 again); a GET of `/deliveries` with `repo` and `kind` parameters lists deliveries in order, a DELETE of it clears them | `Deliveries(repo, kind)`, `Fail(n, status)`, `Wait(repo, kind, n, timeout)` |
| `test/stubs/origo` | the contract stub a consumer's tests target: the real handlers of `internal/httpgit` and `internal/api`, and `internal/lfs` once spec 010 lands, wired to `wal.MemStore`, a temporary cache directory, and an in-process issuer, authorizer, and sink from the packages above; `New(t)` returns the base URL, a minting function, and the authorizer and sink handles; it speaks the whole contract because it is the node's own code, which spec 021 proves by running `TestContract` against it | `URL()`, `Token(sub, act)`, `Authorizer()`, `Sink()` |

A consumer imports `test/stubs/origo` to run its integration tests
against Origo in-process. The slow proxy of spec 015
(`test/stubs/slowproxy`) lives beside these and is that spec's.

### Local stack

`make dev` (spec 002) builds the binary, starts MinIO from
`docker-compose.yml`, and from this spec on also runs
`test/stubs/cmd/origo-stubs` beside it, points `ORIGO_OIDC_ISSUERS`,
`ORIGO_OIDC_INSECURE_ISSUERS`, and `ORIGO_AUTHORIZER_URL` at the stubs,
and prints a clone line with a token the stub issuer minted.
`make dev-down`, added by this spec, stops what `make dev` started for
the checkout's `DEV_PROJECT` (the compose stack and the stubs) and
leaves `out/` in place; `make clean` (spec 002) removes `out/` as well.
`make test-tiers` runs the `integration` and `e2e` tiers against
whatever values of the test bucket variables the environment carries,
which is what CI calls with its service container; `make
test-integration` is the developer's form that starts compose and
calls the same target.

### Documents as tests

`tools/docs/run-blocks.sh <document>`, owned by this spec as test
tooling, extracts the fenced `sh` blocks of one Markdown document and
runs them in order in one shell with `set -e`, with `ORIGO_TEST_URL`
and `ORIGO_TEST_ADMIN_TOKEN` in the environment as the stack's values,
so a document is the test of its own commands and a step that drifts
from the tree fails. Spec 014 runs `docs/migration.md` through it and
spec 018 runs `docs/install.md`.

### The stack

`deploy/examples/kind`, which spec 018 lists as one of its three
example overlays, applies MinIO, three `origod` replicas from the
candidate image, and `origo-stubs` running the issuer, the authorizer,
and the sink, to a kind cluster the job creates from a `kind` config
this directory carries. The stack has no ingress controller: every
address the runner reaches is a host port the `kind` config maps to a
NodePort, listed in the ports table below. Every capability the stack
has beyond a plain cluster is one row here with the spec that needs
it, so a later spec that needs another adds a row rather than a step
in a job:

| Row | Provides | Needed by |
|---|---|---|
| MinIO | one in-cluster bucket, path style, on the host port of the ports table so the runner reaches it too; `ORIGO_S3_PUBLIC_ENDPOINT=http://localhost:30900` on every node, which is what a presigned URL and the harness use from outside the cluster | every spec; 010 for LFS transfers from the runner, 017 for the fixture the harness extracts |
| three `origod` replicas | the candidate image with `ORIGO_GOSSIP_PEERS` on the headless Service, `ORIGO_GOSSIP_SECRET` a fixed value (spec 005), `ORIGO_OIDC_INSECURE_ISSUERS` naming the stub issuer's in-cluster URL (spec 007), and `ORIGO_TOKEN_KEY` from a Secret the overlay's apply script generates with `openssl ecparam -genkey -name prime256v1` (spec 007); the public listener on the host port of the ports table | every spec |
| `origo-stubs` | the issuer, the authorizer with `-allow *`, and the sink as pods, each on a Service, each control endpoint on its host port of the ports table so `TestContract` drives them from the runner (spec 021) | 007, 008, 021 |
| `metrics-server` | the resource metrics API a CPU-target HorizontalPodAutoscaler reads, installed with the flag that accepts kind's kubelet certificates | 005 |
| Cilium | the CNI, installed in place of kindnet (`disableDefaultCNI` in the `kind` config), so NetworkPolicy is enforced | 015 for the unreachable-bucket policy, 016 for the gossip policy |
| Pod Security admission | the label `pod-security.kubernetes.io/enforce=restricted` on the `origo` namespace | 016 |
| HPA scale-down window | a patch setting the stabilization window to 60 seconds, which spec 005 adds to this overlay | 005 |

#### Ports

The `kind` config maps each host port to the NodePort of the Service
named, so every address a test or a document uses is fixed and the
jobs set nothing:

| Host port | Service | Reached as | Used by |
|---|---|---|---|
| 30080 | `origod` public listener, all three replicas behind one Service | `http://localhost:30080`, the default of `ORIGO_TEST_URL` | every `TestCluster` and `TestSlow` test, `TestContract` (021), the documents of 014 and 018 |
| 30081 | the stub issuer, its discovery, JWKS, and control endpoints | `http://localhost:30081` | the harness minting tokens, `TestContract` (021) |
| 30082 | the stub authorizer, its endpoint and control endpoints | `http://localhost:30082` | `TestContract` (021) flipping allow and deny |
| 30083 | the stub sink, its endpoint and control endpoints | `http://localhost:30083` | `TestContract` (021) reading deliveries, 008's repair case |
| 30900 | MinIO | `http://localhost:30900`, the value of `ORIGO_S3_PUBLIC_ENDPOINT` on every node | LFS transfers from the runner (010), the fixture extraction of 017, the `Fault` of 021, a one-node test in a cluster job through `ORIGO_TEST_S3_ENDPOINT` and its sibling variables |

Inside the cluster the nodes reach the stubs and MinIO by their Service
names; the host ports are for the runner. The fixed dev subject of the
stack is `dev`, allowed by the stub authorizer's `-allow *`:
`ORIGO_TEST_ADMIN_TOKEN` unset means a test mints a token for it at
the issuer's host port, valid for the run, so no fixed token value is
checked in (spec 007 refuses a token whose `iat` is older than 24
hours, which rules a fixed one out). Spec 021 references this table
for its defaults.

`test/e2e` gains the scenarios that need a cluster: node kill
mid-push, cache pressure, compaction under load, the degraded-storage
cases of spec 015, the autoscaler and node-removal cases of spec 005,
and the event repair case of spec 008. From spec 021 on the
conformance suite runs on the same stack.

### CI

`verify.yml` gains four jobs beside the gate:

| Job | Runs | Budget |
|---|---|---|
| `integration` | MinIO as a service container, then `make test-tiers`: the `integration` tier and the `e2e` tier's one-node run, `-run 'TestE2E'`, which is every end-to-end test that needs no cluster, starts its own nodes against the bucket, and fits the budget: the tests that push 500 or 1 000 times, import 5 000 commits, or move 500 MiB are in the two cluster jobs below | 25 minutes |
| `e2e` | creates the kind cluster, builds and loads the two images, applies `deploy/examples/kind`, then runs the `e2e` tier's cluster scenarios, `-run 'TestCluster'`, against the three-node overlay, among them the 500 and 1 000 push tests of spec 006 (`TestClusterFiveHundredPushesStayUnder64EntriesAnd6Packs`, `TestClusterCompactionKeepsFetchLatencyFlat`), spec 019's `TestClusterGcBoundsStorage` and `TestClusterImportFixture` (the 5 000-commit import, which starts its own node against the stack's MinIO through the test bucket variables of spec 002, `ORIGO_TEST_S3_ENDPOINT` and its siblings, which the job sets to the host port, because its stub source runs on the runner and spec 016's egress rules keep the stack's nodes from reaching it); from spec 021 on, `TestContract` and `TestSameAnswersOnStubAndStack` of `test/conformance` as a second step against the same stack | 30 minutes |
| `e2e-slow` | the same cluster, then `-run 'TestSlow'`: spec 005's `TestSlowAutoscalerScalesUp` and `TestSlowReplicasScaleReads`, spec 008's `TestSlowEventRepairAfterKill`, spec 004's `TestSlowMaterializeTenThousandEntries`, and spec 010's `TestSlowLFSRoundTripBypassesTheNode` (500 MiB through MinIO's host port), which each wait on a timer or a fixture the others do not | 30 minutes |
| `fuzz` | `make fuzz` on a weekly `schedule` trigger | 60 minutes |

Every test of the tier carries the build tag `e2e`; which job runs it
is its name prefix, given to `go test -run` by the job: `TestE2E` for
the one-node run, `TestCluster` for the cluster scenarios, `TestSlow`
for the slow ones, and `TestMeasure` for the measurement run no job
selects. A one-node test starts `origod` itself from the bucket
variables of spec 002; a cluster or slow test targets the stack the job
applied through `ORIGO_TEST_URL` and `ORIGO_TEST_ADMIN_TOKEN` (spec
002), whose defaults are the ports table's: `http://localhost:30080`,
and a token minted at the issuer's host port for the dev subject when
the token is unset, so the jobs set neither. The two cluster jobs run in parallel on every push
to `main` and every pull request. A job over its budget fails the push;
a scenario that needs more time moves to `e2e-slow`, and one that makes
`e2e-slow` exceed its budget is a spec change, not a budget change. The
shared
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
| `ORIGO_TEST_URL`, `ORIGO_TEST_ADMIN_TOKEN` | the stack a `TestCluster` or `TestSlow` test targets and the token it creates repositories with; the URL defaults to `http://localhost:30080` and the token to one minted for the dev subject at the issuer's host port (the ports table); a cluster test skips when nothing answers at the URL, so the tier runs on a developer's machine without the stack |

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
  job; `test/e2e`, `TestE2EDevStackClones`, which starts `make dev`
  in the background from the test with a distinct `DEV_PROJECT`, waits
  at most 2 minutes for the clone line on its output, clones with it,
  and tears the stack down with `make dev-down` for that project
  whatever happened, never with `make clean`, which would remove the
  `out/` of the checkout under test).
- `kustomize build deploy/examples/kind` succeeds and the applied
  overlay reaches three ready nodes, the stubs, `metrics-server`, and
  Cilium within 5 minutes, with the namespace carrying the restricted
  label and every host port of the ports table answering (proposed:
  `verify.yml`, the `e2e` job's set-up step).
- The `integration`, `e2e`, and `e2e-slow` jobs exist in `verify.yml`
  and each selects its prefix and nothing else: the `go test` line of
  each carries `-run 'TestE2E'`, `-run 'TestCluster'`, or `-run
  'TestSlow'`, and no other job runs the `e2e` tier (proposed:
  `test/e2e`, `TestJobsSelectByPrefix`, which reads
  `.github/workflows/verify.yml` through a test-only constant resolved
  from its own source file, as spec 011 says for a test that reads
  outside its package).
- `tools/docs/run-blocks.sh` runs the `sh` blocks of a fixture
  document in order in one shell, stops at the first failing block,
  and passes `ORIGO_TEST_URL` and `ORIGO_TEST_ADMIN_TOKEN` through
  (proposed: `tools/docs/run_blocks_test.sh`, run through a Go test in
  `tools/docs` the way spec 017 runs its smoke test).
- `make fuzz` runs every fuzz function in the module for 40 seconds and
  the weekly schedule in `verify.yml` calls it (proposed: `Makefile`,
  the `fuzz` target; `verify.yml`, the `fuzz` job on `schedule`).
