# Testing

For whoever changes Origo: what each test tier proves, what it needs,
and how to run it locally. Every tier runs in CI; the table says when.

| Tier | Needs | Run it with | In CI |
|---|---|---|---|
| the gate | the Go toolchain and git | `make` | every push and pull request |
| store and one-node end-to-end | a bucket (MinIO) | `make test-integration` | every push and pull request |
| cluster | a container engine and kind | `make dev-up`, then `go test -tags=e2e ./test/e2e/... -run TestCluster` | every tag, and on demand |
| slow cluster | the same, and `git-lfs` | `go test -tags=e2e ./test/e2e/... -run TestSlow` | every tag, and Monday mornings |
| conformance | an installation | `go test -tags=e2e ./test/conformance/... -run TestContract` | every tag, against the stack and against the live installation |
| mutation | a bucket | see below | every tag |
| install document | a bare kind cluster | `tools/docs/run-blocks.sh docs/install.md` | every push, and against the published artifacts on every tag |
| fuzz | the Go toolchain | `make fuzz` | Sunday mornings, and on demand |

## The gate

`make` runs `go tool lateregate`, the shared quality bar pinned in
`go.mod`, followed by the spec index test. `go tool lateregate list`
prints every check with whether it runs here and why; `go tool
lateregate <name>` runs one. The ones that decide most changes:

- `test`, `race`: `go vet` and the whole suite, with and without the
  race detector.
- `hermetic`: the suite with only the Go toolchain, `/usr/bin`, and
  `/bin` on `PATH`. `origod` runs git and nothing else, and this proves
  no test reaches for another binary.
- `tempdir`: the suite leaves nothing under `TMPDIR`.
- `cover`: every package at 90% statement coverage or more, measured
  per package rather than as an average. A package exempt from it is
  listed in `.lateregate.yaml` with the reason.
- `depcheck`: the build of `origod` and of `origo` reaches only the
  modules `.lateregate.yaml` admits.
- `spec-lint`: every spec has its frontmatter and sections, and the
  index agrees with the tree.
- `fmt-check`, `modernize`, `lint`, `vuln`, `license`, `otel-client`,
  `identity`: formatting, the shared linter configuration, known
  vulnerabilities, the SPDX notice on every file, a tracing transport on
  every outbound HTTP client, and the repository's declared role.

Unit tests run against the real git binary and against `wal.MemStore`,
an in-memory bucket that honors the conditional create exactly as the
S3 adapter expects. `internal/gittest` builds fixture histories and
packfiles with git itself, so a test never hand-writes pack bytes.

## Store and one-node end-to-end

```sh
make test-integration
```

starts MinIO with a bucket through compose (podman or docker, whichever
`make dev` found), then runs two suites against it:

- `go test -race -tags=integration ./internal/wal/...`: the log against
  a real S3 endpoint, including the conditional create and concurrent
  writers.
- `go test -race -tags=e2e ./test/e2e/... -run TestE2E`: built `origod`
  processes against MinIO with the real git client. Pushes and clones
  across two nodes, a node killed mid-push, a hundred concurrent pushes,
  the read API, the archive, quotas, the drain on shutdown, and the
  commands of `docs/cli.md`, which `TestE2EOrigoDocCommandsRun` runs
  block by block.

Against a bucket of your own, set the `ORIGO_TEST_S3_*` variables and
run `make test-tiers` instead. Without them, both tiers skip.

## The cluster tiers

```sh
make dev-up      # builds both images and starts a three-node kind stack
go test -count=1 -tags=e2e ./test/e2e/... -run TestCluster
go test -count=1 -tags=e2e ./test/e2e/... -run TestSlow
make dev-down
```

`make dev-up` builds `origod` and `origo-stubs` as images and hands
them to `deploy/examples/kind/up.sh`, which creates a kind cluster with
Cilium, MinIO, the stub issuer, authorizer, SSH key store, event sink,
and git source, and three `origod` pods as a StatefulSet. The cluster
tests drive that stack: placement and node removal under load,
compaction bounds, degraded storage through the slow proxy, SSH, the
import fixture, the pod security context, and the commands of
`docs/migration.md`. The slow tier covers ten thousand entries, LFS
round trips, event repair after a kill, and the autoscaler.

## Conformance

`test/conformance` is the contract as executable tests: `Run` drives the
real git binary and `net/http` against any base URL, one subtest per
rule, and deletes every repository it created by id at the end. Groups
that need something the target does not offer (an issuer, a
controllable authorizer, a git source, a fault injector) skip by name.

| Variable | Target |
|---|---|
| `ORIGO_TEST_URL`, `ORIGO_TEST_ADMIN_TOKEN` | the kind stack, by default its balanced port and a token minted at its stub issuer |
| `ORIGO_LIVE_URL`, `ORIGO_LIVE_TOKEN` | a live installation, run by the release workflow after a deploy |
| `ORIGO_PREVIOUS_RELEASE_FIXTURE` | the fixture archive the previous release attached, which `TestPreviousReleaseFixture` materializes on this build to prove the log format still reads |

`TestSameAnswersOnStubAndStack` runs the same cases against the
in-process contract stub of `test/stubs/origo` and against the stack,
so the stub a consumer tests against cannot drift from the node.

## Mutation

The conformance suite is only worth something if it notices a missing
capability. For each capability `ORIGO_TEST_DROP_CAPABILITY` accepts
(`filter`, `allow-tip-sha1-in-want`, `allow-reachable-sha1-in-want`,
`atomic`, `push-options`), `TestMutation` starts a node that stops
advertising it and passes only when the suite fails on that
capability's cases alone.

```sh
ORIGO_TEST_DROP_CAPABILITY=atomic go test -count=1 -tags=e2e ./test/e2e/... -run TestMutation
```

## Executable documents

The shell blocks of `docs/install.md`, `docs/cli.md`, and
`docs/migration.md` are tests. `tools/docs/run-blocks.sh` runs the
fenced `sh` blocks of a page in order in one shell and fails on the
first that fails. Blocks fenced with no language are shown and never
run, which is how a page carries a command a test cannot run, such as
downloading a release. Each block falls back to the example stack when
the reader sets nothing, so the same page is walked by CI and followed
by an operator.

At push time, `go test ./tools/docs/` checks what needs no cluster:
every `sh` block of the install page parses, every path and link it
names exists, the block that first reaches the installation waits for
it, the version the install page downloads and the one `SECURITY.md`
names are the newest release, the hand-written `docs/api.md` names
every path of the OpenAPI document and every error code, and no user
page cites a spec by number.

## Generated pages

`make docs` regenerates `docs/configuration.md` from `internal/config`,
`docs/internals/contract.md` from the specs' tables, and
`api/openapi.yaml` from the same tables. CI runs it and fails on any
diff, so a change to a variable or a spec table ships with its
documentation. `make specindex` checks the cross-reference table at the
end of `specs/README.md`, and CI also renders every overlay with
`kubectl kustomize` and checks the alert rules with `promtool`.

## Fuzz

```sh
make fuzz
```

runs every `Fuzz` function of the module for 40 seconds each: the
parsers of entry headers, reference transactions, index objects,
pkt-lines, receive-pack commands, and tokens, the reference name, label,
and path validators, and the operation body decoder. The seed corpora
run in the ordinary suite.

## Measurements

`ORIGO_E2E_MEASURE=1` runs `TestMeasure`, which prints the pushes a
second one node sustains, the latency of the currency check, the time to
materialize a thousand entries, the time to the first byte of a large
archive, and clone latency before and after a thousand pushes, and
asserts nothing. Use it to compare a change
that might affect performance, before and after.

## The stubs

`test/stubs/cmd/origo-stubs` is one binary serving the issuer, the
authorizer, the sink, and the SSH key store, each on its own port, plus
the git source and the slow proxy when their flags are set. The example
stack runs it as the `origo-stubs` image. Each stub has a control API a
test drives over HTTP; the `origo` stub is a Go package only.

| Stub | Stands in for |
|---|---|
| `issuer` | an OIDC issuer, minting a token for any subject at `/mint` |
| `authorizer` | the authorization endpoint, allowing every subject unless a rule says otherwise, with deny, fail, and hang switches |
| `sshkeys` | the SSH key endpoint, a table from fingerprint to subject |
| `sink` | an event sink that verifies each signature and records every delivery |
| `source` | a git host over TLS behind a bearer, for import and verify |
| `slowproxy` | a slow bucket, holding each connection's first bytes for a set delay |
| `origo` | the node's own git and API handlers in-process over an in-memory bucket, for a consumer's integration tests |

None of them authenticates its control API. They exist for tests and
the example stack, and nothing about them belongs in an installation.

## Variables the tests read

| Variable | What it does |
|---|---|
| `ORIGO_TEST_S3_ENDPOINT`, `_REGION`, `_BUCKET`, `_KEY`, `_SECRET`, `_PATH_STYLE` | the bucket the store and end-to-end tiers use; unset, they skip |
| `ORIGO_TEST_URL`, `ORIGO_TEST_ADMIN_TOKEN` | the installation the cluster and conformance tiers target |
| `ORIGO_LIVE_URL`, `ORIGO_LIVE_TOKEN` | a live installation for the conformance run after a release |
| `ORIGO_PREVIOUS_RELEASE_FIXTURE` | the previous release's fixture archive |
| `ORIGO_TEST_DROP_CAPABILITY` | the capability a mutation node stops advertising |
| `ORIGO_FAILPOINT` | a named point at which a node exits at once, for kill tests |
| `ORIGO_CHECK_SELFTEST` | makes `origod check` test its own failure path against a store that ignores the conditional create |
| `ORIGO_E2E_MEASURE` | runs the measurement tests |
| `ORIGO_INSTALL_IMAGE`, `ORIGO_INSTALL_MANIFESTS` | the image and manifests the install walk applies |

[`configuration.md`](../configuration.md#testing-and-the-pipeline)
carries the same list with defaults, generated from the code.
