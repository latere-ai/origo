---
title: "Repository scaffold: module, binary, configuration, quality gate, release"
status: complete
track: infra
depends_on:
  - specs/001-architecture.md
affects: [cmd/origod/, internal/config/, internal/version/, Makefile, .lateregate.yaml, Dockerfile, Dockerfile.ci, docker-compose.yml, deploy/, .github/workflows/, tools/smoke/]
effort: small
created: 2026-09-06
updated: 2026-09-10
author: changkun
---

# Repository scaffold

## Overview

A compiling, testable, releasable repository before any protocol code
exists: the Go module, the binary, typed configuration, the quality gate,
the container image, the release pipeline, and the deploy manifests. The
shape is chosen so the repository reads as an ordinary open source Go
service to a newcomer. This spec is also the configuration reference: it
owns every `ORIGO_*` variable the node reads, including the ones later
specs give a meaning to, and lists the variables the test tiers and the
workflows read, so an operator has one table. It also owns the table of
failpoint names.

## Current state

Built in phase 1 and in the tree. `cmd/origod` is the binary,
`internal/config` the typed configuration, `internal/version` the build
identity, `Makefile` the gate's entry point, `Dockerfile` and
`Dockerfile.ci` the images, `docker-compose.yml` the local MinIO,
`deploy/base`, `deploy/prod`, and `deploy/bootstrap` the manifests,
`.github/workflows/verify.yml` and `release.yml` the thin callers of the
shared pipeline in `latere-ai/ci`, and `tools/smoke/release.sh` the
post-deploy smoke. No tag has been cut. The Outcome lists what diverged
from the first draft. The rows the table below gained for later specs
and the Failpoint table are reference entries; the spec named in each
row builds what reads it.

The shared runtime stage of `Dockerfile` and `Dockerfile.ci` is
`debian:bookworm-slim`, whose `git` is 2.39. Spec 017 owns the move to
`debian:trixie-slim` pinned by digest, which ships git 2.47, the floor
`origod check` (spec 018) enforces for spec 020's merge family; both
files are in that spec's affects and change at once so the two stages
stay byte for byte the same. This spec is complete as built.

## Design

### Layout

What exists today is marked as such; the rest lands with the spec named.

```
cmd/origod/             main: configuration, listeners, run group, readiness, the sweeper loop
internal/config/        typed configuration from the environment; every problem in one message
internal/version/       build identity set by -ldflags
internal/contract/      the Origo-Contract header and the error codes (spec 003)
internal/auth/          the verifier over the issuers and the node's key, the authorizer client, the guard, the signer of repository-bound tokens (spec 007)
internal/wal/           the write-ahead log: formats, commit, currency check, metadata, sweeper, the Store (spec 004)
internal/repo/          the local repository cache and the git subprocess wrapper (spec 004, 005)
internal/httpgit/       smart HTTP: info/refs, upload-pack, receive-pack, the pre-receive hook (spec 003, 004)
internal/api/           the JSON API: repositories today (spec 003); reads (spec 009); administration (spec 019)
internal/gittest/       test support over the real git
internal/placement/     rendezvous hashing, gossip, eviction (spec 005)      -- not yet
internal/compact/       compaction (spec 006)                                 -- not yet
internal/events/        push events (spec 008)                                -- not yet
internal/lfs/           the LFS batch API and the presigned transfers (spec 010)
internal/limits/        quotas and rate limits (spec 012)                     -- not yet
internal/metrics/       the one place every metric of spec 011 is registered   -- not yet
test/e2e/               origod as a process against MinIO with the real git (e2e build tag)
test/conformance/       the contract as an importable test package (spec 021) -- not yet
test/stubs/             the stub issuer and authorizer (spec 007, built); the event sink, contract stub, source, binary, and slow proxy (spec 013, 015) -- not yet
tools/smoke/            the post-deploy smoke the release pipeline runs
tools/spike/            the conditional-write probe; its own module
tools/specindex/        the cross-reference table of specs/README.md; its own module
deploy/base/            Deployment, Service, headless gossip Service, Ingress, PodDisruptionBudget, ServiceAccount
deploy/prod/            the overlay the release pipeline applies; namespace origo
deploy/bootstrap/       Namespace and the Secret templates, applied by hand once
deploy/examples/kind/   MinIO, three nodes, and the stubs in a kind cluster (spec 013) -- not yet
```

### Local stack

`make dev` builds the binary, starts MinIO from `docker-compose.yml`
with the bucket, and runs `origod` in the foreground with the variables
of the table below set to local values. In phase 1 the clone line it
prints carries `ORIGO_DEV_TOKEN`. `make test-integration` starts the
same MinIO and runs the `integration` and `e2e` tiers against it. Spec
013 owns what the local stack becomes once spec 007 removes
`ORIGO_DEV_TOKEN`: `make dev` running the stub issuer and authorizer
beside MinIO, generating `ORIGO_TOKEN_KEY` at start with `openssl
ecparam -genkey -name prime256v1` into a file under `out/` (spec 007),
and `make test-tiers`, the target that runs the tiers against the test
bucket variables the environment carries and that CI calls with its
service container.

### Binary and listeners

| Listener | Default address | Serves |
|---|---|---|
| public | `:8080` (`ORIGO_PUBLIC_ADDR`) | `/r/{id}.git/*` and `/{owner}/{slug}.git/*` smart HTTP, `/v1/*`, LFS, plus `GET /readyz` and `GET /version` so the release smoke reaches them through the ingress, and the landing page `GET /` with `GET /favicon.ico` beside it (spec 022) |
| internal | `:8081` (`ORIGO_INTERNAL_ADDR`) | the four probes below |
| gossip | `:7946/udp` (`ORIGO_GOSSIP_ADDR`) | node to node sequence announcements (spec 005); until then datagrams are read and discarded |

The probes are `latere.ai/x/pkg/health`, mounted whole on the internal
listener and path by path on the public one.

| Method | Path | Body |
|---|---|---|
| GET | `/livez` | 200 `ok`, touches no dependency |
| GET | `/readyz` | 200 `ok` when every check passes; 503 `not ready: <check>: <error>` otherwise, and `not ready: draining` during shutdown; text, the developer register. The `storage` check passes while a storage breaker is open, once the bucket has answered this replica at least once since it started, because such a node still serves warm repositories stale and refuses writes with a code, and taking it out of rotation would lose those reads (spec 015) |
| GET | `/version` | `{"version","commit","build_time"}` from `internal/version`, set by `-ldflags` |
| GET | `/metrics` | the `latere.ai/x/pkg/metrics` registry in the Prometheus text format |

Readiness runs two checks with a 2 second budget: `storage` (one listing
of at most one key under `origo/`) and `disk` (create and remove a file
under `ORIGO_DATA_DIR`). Shutdown on `SIGTERM` or `SIGINT`: readiness
answers 503 at once, the node waits a 3 second drain delay, then closes
the HTTP servers with a 60 second grace period, then the gossip socket,
then the background loops. The Deployment's
`terminationGracePeriodSeconds` is 90.

`origod -version` prints `origod <version> (<commit>, <date>)` and exits
0; a bad flag exits 2; a configuration or start-up failure exits 1 with
one line on stderr prefixed `origod:`.

The first argument that does not start with `-` selects a subcommand;
without one the binary serves. Every subcommand reads the same
configuration table and shares the `-version` flag:

| Subcommand | Reads | Does | Spec |
|---|---|---|---|
| `serve` (default) | the node's variables | the listeners and the loops above | this spec |
| `check` | the node's variables | one line per requirement of the installation, exit 1 on any failure | 018 |
| `migrate -manifest <file> -report <file>` | `ORIGO_MIGRATE_URL`, `ORIGO_MIGRATE_TOKEN_ENV`, `ORIGO_MIGRATE_PARALLEL` and none of the node's | drives a batch of imports against an Origo as a client | 014 |

An unknown subcommand is a usage error, exit 2. Spec 014 built the
dispatcher with `serve` and `migrate`; `check` is spec 018's and is an
unknown subcommand until it lands. The node's configuration is loaded
inside `serve`, so a subcommand that reads none of it runs without a
bucket. This spec keeps the table.

### Configuration

Every variable is read once at start-up by `internal/config.Load`, which
collects every problem and fails with one message
`configuration: missing ORIGO_A; missing ORIGO_B; ...` sorted by name.
`Resolve` then creates `ORIGO_DATA_DIR` and derives `ORIGO_CACHE_BYTES`.
Variables a later spec reads are listed here with that spec; a
deployment that sets one before its spec lands is not refused, because
an unknown variable is never an error.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `ORIGO_S3_ENDPOINT`, `ORIGO_S3_REGION`, `ORIGO_S3_BUCKET`, `ORIGO_S3_KEY`, `ORIGO_S3_SECRET` | yes | none | the bucket; the prefix `origo/` is fixed |
| `ORIGO_S3_PATH_STYLE` | no | unset | `1` addresses the bucket as a path segment (MinIO, any endpoint by IP) |
| `ORIGO_S3_PUBLIC_ENDPOINT` | spec 010 | `ORIGO_S3_ENDPOINT` | the bucket endpoint LFS clients reach; presigned URLs are signed against it |
| `ORIGO_PUBLIC_URL` | yes | none | an absolute URL such as `https://git.example.com`; trailing slash removed; used in clone URLs and event payloads |
| `ORIGO_DEV_TOKEN` | no; refused from spec 007 | none | the phase 1 bearer the public listener accepted; spec 007 removed it, and a start-up that sets it fails with `ORIGO_DEV_TOKEN is no longer read; remove it` in the one message; the stub issuer of spec 013 takes its place in `make dev` and `test/e2e` |
| `ORIGO_DATA_DIR` | no | `/var/lib/origo` | the repository cache; `repos/`, `spool/`, and `home/` under it; a local disk, never a network file system |
| `ORIGO_CACHE_BYTES` | no | 80% of the file system holding `ORIGO_DATA_DIR` | eviction ceiling of the cache (spec 005); a positive integer |
| `ORIGO_PUBLIC_ADDR`, `ORIGO_INTERNAL_ADDR`, `ORIGO_GOSSIP_ADDR` | no | `:8080`, `:8081`, `:7946` | listen addresses; a test binds `127.0.0.1:0` |
| `ORIGO_NODE_NAME` | no | the host name, `origod` when unknown | the identity used in gossip and placement (spec 005); the pod name in Kubernetes |
| `ORIGO_GOSSIP_PEERS` | no | unset | the other nodes (spec 005): a comma separated list of `host:port` entries, or one DNS name, which resolves to every node on the port of `ORIGO_GOSSIP_ADDR`; the headless Service `origod-gossip` in Kubernetes, two loopback entries with distinct ports for two local nodes |
| `ORIGO_GOSSIP_SECRET` | from spec 005, when `ORIGO_GOSSIP_PEERS` is set | none | the key of the HMAC-SHA256 every gossip datagram carries (spec 005); at least 32 bytes; the same value on every node of one installation; a single node with no peers runs with neither variable, and the secret without the peers is read and unused |
| `ORIGO_SWEEP_INTERVAL` | no | `10m` | how often the sweeper runs over every repository (spec 004); `0` disables it |
| `ORIGO_SWEEP_MIN_AGE` | no | `1h` | how old an orphan must be before the sweeper deletes it (spec 004) |
| `ORIGO_FAILPOINT` | no | unset | the name of an injected failure from the Failpoint table below, for the end-to-end suite; empty in every deployment |
| `ORIGO_OIDC_ISSUERS` | yes, from spec 007 | none | comma separated issuer URLs whose tokens are accepted |
| `ORIGO_OIDC_INSECURE_ISSUERS` | spec 007 | unset | comma separated issuer URLs from `ORIGO_OIDC_ISSUERS` that may use `http://` on a host other than a loopback address (spec 007); set by the kind overlay for the stub issuer, never in production |
| `ORIGO_AUTHORIZER_URL`, `ORIGO_AUTHORIZER_TOKEN` | yes, from spec 007 | none | the consumer's authorization endpoint and the bearer Origo sends it |
| `ORIGO_TOKEN_KEY` | yes, from spec 007 | none | PEM-encoded ECDSA P-256 private key that signs repository-bound tokens; required in every mode, so a node without it never starts and `POST /v1/repos/{id}/tokens` never runs without a key; `make dev` and the kind overlay of spec 013 generate one at start with `openssl ecparam` |
| `ORIGO_EVENTS_URL`, `ORIGO_EVENTS_SECRET` | spec 008 | unset | the push event sink and the HMAC key; events are off when the URL is unset; the URL without the secret is a start-up failure (spec 008) |
| `ORIGO_REPAIR_UNHEARD` | spec 008 | `5m` | how long a node must be unheard before another node repairs the events its journals name (spec 008) |
| `ORIGO_REPAIR_INTERVAL` | spec 008 | `10m` | how often the event repair sweep runs (spec 008) |
| `ORIGO_STORAGE_TIMEOUT` | spec 015 | `10s` | the deadline of one object storage operation |
| `ORIGO_STALE_MAX` | spec 015 | `5m` | how long a warm repository is served from the local copy while the read breaker is open |
| `ORIGO_MAX_GIT_PROCS` | spec 012 | `64` | concurrent git subprocesses per node |
| `ORIGO_REQUESTS_PER_MINUTE` | spec 012 | `600` | the requests one effective subject may send a node in a minute, and the bucket's burst, unless the authorizer names that subject a `requests_per_minute` of its own (spec 007), which is that subject's rate and burst instead; every response of the public listener carries the figure in force for its own subject as `RateLimit-Limit`, with the tokens it has left beside it as `RateLimit-Remaining`; `0` turns the per-subject limit off; the `kind` overlay of spec 013 sets `6000`, because its scenarios drive one node far harder than any one caller of a live installation |
| `ORIGO_EGRESS_ALLOW` | spec 016 | unset | comma separated hostnames, exact or `*.` wildcards, that server-side fetches (`import`, `verify`) may reach, matched with `latere.ai/x/pkg/hostmatch`; an exact hostname may carry a pinned address as `host=address`, one IP literal that fixes the address the dialer uses for that host, inside `ORIGO_CLUSTER_CIDRS` or outside it, and the only form that admits a source inside the cluster (spec 016); a pinned address that is not an IP literal, or on a wildcard, is a problem in the one start-up message, while an address no listed range contains is not; unset refuses every source |
| `ORIGO_CLUSTER_CIDRS` | spec 016 | unset | comma separated CIDR ranges of the cluster's service and pod networks that a server-side fetch must never reach, added to the well-known refused ranges of spec 016; unset refuses only the well-known ranges |
| `ORIGO_EGRESS_CA_BUNDLE` | spec 016 | unset | the path of a PEM file of CA certificates the egress proxy of spec 016 trusts beside the system roots when it dials an `import` or `verify` source over TLS; unset in production; the kind overlay of spec 013 sets it to the CA the stubs' certificates are signed by |
| `ORIGO_TEST_DROP_CAPABILITY` | spec 021 | unset | one git-controlled capability name the node stops advertising, for the mutation job of spec 021; empty in every deployment |
| `ORIGO_CHECK_SELFTEST` | spec 018 | unset | `1` makes `origod check` run its `conditional-create` line against an in-process store that ignores the header, so the check's own failure path is testable |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_*` | spec 011 | unset | the standard OpenTelemetry exporter variables, read by `latere.ai/x/pkg/otel`; telemetry is off without the endpoint |

Durations are Go durations (`10m`, `500ms`). A malformed value is a
problem in the same start-up message as a missing key.

Variables read by the test tiers and the workflows, never by a serving
node:

| Variable | Read by | Purpose |
|---|---|---|
| `ORIGO_TEST_S3_ENDPOINT`, `ORIGO_TEST_S3_REGION`, `ORIGO_TEST_S3_BUCKET`, `ORIGO_TEST_S3_KEY`, `ORIGO_TEST_S3_SECRET`, `ORIGO_TEST_S3_PATH_STYLE` | the `integration` and `e2e` tiers (spec 013) | the bucket the tiers use; the tiers skip when the endpoint is unset |
| `ORIGO_E2E_MEASURE` | `test/e2e` (spec 013) | `1` runs `TestMeasure`, which prints the measurements spec 004's Outcome records; no threshold depends on it |
| `ORIGO_TEST_URL`, `ORIGO_TEST_ADMIN_TOKEN` | the `e2e` and `e2e-slow` jobs (spec 013), `TestContract` (spec 021) | the base URL of the kind stack and a token with `admin` on every repository; unset, each defaults to the ports table of spec 013 (`http://localhost:30080`, a token minted at the stub issuer's host port for a subject naming the test, so each cluster scenario has a bucket of spec 012's rate limit to itself), so the jobs set neither and a test that targets the stack instead of starting a node knows where it is |
| `ORIGO_LIVE_URL`, `ORIGO_LIVE_TOKEN` | the live conformance run (spec 021) | repository secrets: the installation the run targets after a release and a token with `admin` on its conformance prefix |
| `ORIGO_KUBECONFIG` | the `deploy` job of `release.yml` (spec 017) | a repository secret holding the kubeconfig `kubectl` applies the release with |
| `ORIGO_RELEASE_DEPLOY` | `release.yml` (spec 017) | a repository variable; unset skips the deploy and smoke step, so a tag on a fork publishes artifacts only |
| `ORIGO_IMAGE_NAMESPACE` | `release.yml` and `tools/release/deploy-archive.sh` (spec 017) | a repository variable naming the registry namespace both images are published under; unset it is `ghcr.io/<the repository owner>`, so a fork's tag publishes to the fork's own packages. An image reference is lowercase and no workflow expression folds case, so an owner whose login carries capitals sets it; the `build` job refuses a namespace that is not a lowercase reference prefix before anything is pushed |

Variables another spec's table defines, listed here so the reference is
one page:

| Owner | Name | Purpose |
|---|---|---|
| spec 004 | `ORIGO_HOOK_DIR` | set by the node on `git receive-pack` only: the directory of the two FIFOs the pre-receive hook uses |
| spec 014 | `ORIGO_MIGRATE_URL` | the Origo the `origod migrate` subcommand drives |
| spec 014 | `ORIGO_MIGRATE_TOKEN_ENV` | the name of the variable holding the bearer `origod migrate` presents to Origo |
| spec 014 | `ORIGO_MIGRATE_PARALLEL` | repositories `origod migrate` drives at once |
| spec 018 | `ORIGO_INSTALL_IMAGE` | the image reference the install document's blocks apply, set by the `install` job of `verify.yml` and the `install-release` job of `release.yml` |
| spec 018 | `ORIGO_INSTALL_MANIFESTS` | the path of the manifests those blocks apply, set by the same two jobs |
| spec 017 | `ORIGO_PREVIOUS_RELEASE_FIXTURE` | the path of the previous release's fixture archive `TestPreviousReleaseFixture` uploads and reads, set by the `e2e` job and the release pipeline; unset skips the test |
| spec 024 | `ORIGO_SSH_ADDR` | the SSH listener's address, `:2222` in the deployment; unset turns SSH off and is the default, so a node that sets none of this spec's four SSH variables runs as it does today |
| spec 024 | `ORIGO_SSH_HOST_KEYS` | the ordered list of host key files the SSH listener presents and announces, the same list on every node of one installation |
| spec 024 | `ORIGO_SSH_KEYS_URL` | the operator's endpoint that resolves an offered public key to a subject |
| spec 024 | `ORIGO_SSH_KEYS_TOKEN` | the bearer Origo sends that endpoint |

### Failpoints

`ORIGO_FAILPOINT` names one point at which the node exits at once with
status 3, so the end-to-end suite can kill a node between two writes. Every name the
deck uses is here; the owning spec says what the suite asserts. A
failpoint has no count: the node exits the first time the point is
reached, so a test that needs the point on a later operation of one
node starts the node without it and restarts it under its name and
data directory with it (spec 008's stack test); a conformance run
(spec 021) assumes no count either.

| Failpoint | Reached | Spec |
|---|---|---|
| `commit.before-index` | after the entry is written and before the index object is created | 004 |
| `events.before-enqueue` | after the index object is created and the verdict is delivered, before the event object is written | 008 |

### Quality bar

Bare `make` runs `go tool lateregate`, the gates of `latere.ai/x/ci-gate`
configured in `.lateregate.yaml`: `fmt-check`, `modernize`, `cgo-free`,
`otel-client`, `license` (MIT, holder Latere AI), `spec-lint`, `lint`,
`vuln`, `test`, `race`, `hermetic` (the suite with only the toolchain and
`/usr/bin` on `PATH`, because git is the one binary origod needs beside
itself), `tempdir` (the suite against an empty temporary directory), and
`cover` (90% per package, no exemptions). `make test-integration` runs
the tiers that need MinIO: the store suite (`integration` tag) and the
end-to-end suite (`e2e` tag). Fuzz tests cover every parser that reads
bytes from a client: pkt-line, the receive-pack request, the entry
header, the reference transaction, and the index object. Every fuzz
function runs as a seed-corpus test in the suite on every push; the 40
second run of every fuzz function, `make fuzz`, and its weekly schedule
are spec 013's.

### Release

`.github/workflows/release.yml` runs the shared `service-release.yml` of
`latere-ai/ci` on a `v*` tag: build the binary, package it with
`Dockerfile.ci`, push `ghcr.io/latere-ai/origod:<tag>`, apply
`deploy/prod/`, wait for the rollout, run `tools/smoke/release.sh`
against the public URL (`GET /readyz` answers 200 and `GET /version`
serves the tag), and publish the GitHub release with the
smoke's markdown evidence and the `CHANGELOG.md` section for the tag.
`verify.yml` runs the gate on every push to `main` and every pull
request. Spec 017 fixes the artifacts, the version promise, and the
release evidence beyond the smoke.

### Images

`Dockerfile` builds the binary inside the image for a developer;
`Dockerfile.ci` copies `out/origod` the verify run built. Both share one
runtime stage, byte for byte: Debian slim pinned by digest, with `git`
and `ca-certificates`, user `65532`, `/var/lib/origo` owned by it, ports
`8080`, `8081`, `7946/udp`. The runtime is not distroless because origod
runs git as a subprocess. The tree carries bookworm-slim, whose git is
2.39; spec 017 moves both files to trixie-slim, git 2.47 (Current state).

## Acceptance criteria

- `make` passes on a clean checkout (`verify.yml`, every push).
- `origod` started against MinIO with an empty `ORIGO_DATA_DIR` answers
  `GET /readyz` 200 on the internal listener within 10 seconds
  (`test/e2e`, `startNode` in `harness_test.go`, which every end-to-end
  test goes through; `cmd/origod`, `TestReadyzReportsStorage`).
- Started without the five bucket variables, `origod` exits 1 with one
  stderr line naming every missing key (`cmd/origod`,
  `TestMissingConfigurationIsOneMessage`; `internal/config`,
  `TestLoadNamesEveryMissingKeyInOneMessage`).
- Every optional variable is read with its default from the table and a
  malformed duration or byte count is reported in the same message
  (`internal/config`, `TestLoadAppliesDefaults`,
  `TestLoadReadsEveryOptionalValue`, `TestLoadReportsMalformedValuesTogether`).
- `GET /readyz` answers 503 naming `disk` when `ORIGO_DATA_DIR` is not
  writable and 503 `draining` after `SIGTERM` (`cmd/origod`,
  `TestReadyzFailsWhenTheDiskIsNotWritable`, `TestReadyzReportsDrainingDuringShutdown`).
- Coverage of `internal/config` is 100% (`cover` gate, checked on every
  push).

## Outcome

Phase 1 shipped the scaffold on 2026-09-06. Every criterion above has a
passing test in the tree, so the spec is complete. "A tag on a fork builds
and publishes the image" was a criterion of the first draft; no tag was
cut in phase 1, and the criterion moved to spec 017, which owns the
release and verifies the first tag.

Divergences from the first draft:

- Authentication is a phase 1 stand-in: the public listener accepts one
  static bearer from `ORIGO_DEV_TOKEN` (as a Bearer header, as git's
  basic auth password with any username, or as the basic auth username
  with an empty password) and refuses everything else with 401
  `unauthenticated` and `WWW-Authenticate: Basic realm="origo"`. Every
  admitted request carries the subject `dev`. The code is
  `internal/auth.StaticBearer`; spec 007 replaces it.
- Variables added beyond the first table: `ORIGO_PUBLIC_ADDR`,
  `ORIGO_INTERNAL_ADDR`, `ORIGO_GOSSIP_ADDR`, `ORIGO_SWEEP_INTERVAL`,
  `ORIGO_SWEEP_MIN_AGE`, `ORIGO_FAILPOINT`, `ORIGO_DEV_TOKEN`.
  `ORIGO_CACHE_BYTES` is read and resolved; eviction is spec 005.
  `ORIGO_STORAGE_TIMEOUT`, `ORIGO_STALE_MAX`, `ORIGO_MAX_GIT_PROCS`,
  `ORIGO_TOKEN_KEY`, `ORIGO_S3_PUBLIC_ENDPOINT`, and the `OTEL_*` variables
  are in the table for their specs and are not read yet.
- `/readyz` and `/version` are also served on the public listener;
  `/livez` and `/metrics` stay internal.
- The runtime image is Debian slim with git, not distroless.
- `internal/contract` and `internal/gittest` were added; `internal/metrics`
  existed briefly and moved to `latere.ai/x/pkg/metrics`. The error
  envelope is `httpjson.Error`, the probes `pkg/health`, the cancellable
  sleep and the sweep ticker `pkg/wait`, the S3 client `pkg/s3`.
- `make/` fragments were not adopted; every gate-named target lives in
  the gate.
- The integration tiers are not run by CI: the shared `lateregate.yml`
  has no services step. Spec 013 adds the job.
- Three targets the first draft assigned to this spec after it was
  complete are spec 013's, which lists them in its affects and Current
  state: `make fuzz` with its weekly schedule, `make test-tiers`, and
  the form of `make dev` that runs the stub issuer and authorizer. This
  spec keeps the variable table only.
- `deploy/base` has no HorizontalPodAutoscaler and no PrometheusRule yet;
  they land with specs 005 and 011. The PodDisruptionBudget keeps
  `minAvailable: 1`.
- The subcommand table was the design and the dispatcher was not built
  until spec 014 needed `migrate`. It built the dispatcher of the
  Design above on 2026-09-09: the first argument that does not start
  with a dash names the subcommand, `serve` is the default and is where
  `config.Load` now runs, `migrate` is spec 014's, every subcommand
  shares `-version`, and anything else is exit 2
  (`cmd/origod`, `TestMigrateUsageAndConfiguration`). Spec 018 adds
  `check` to it. This spec keeps the table and the configuration rule
  that every subcommand shares.
- A defect fixed by spec 013 on 2026-09-08: the storage and outbound
  transports of `cmd/origod` had no dial timeout, so a bucket or an
  issuer that drops packets held a request for the operating system's
  connect timeout times the client's retries. Both now dial with a 10
  second bound (`cmd/origod`, `TestTransportsBoundTheDial`); spec 015's
  `ORIGO_STORAGE_TIMEOUT` bounds the whole operation once it lands.
- A second defect fixed by spec 013 on 2026-09-08: the digests pinned
  for `debian:bookworm-slim` in both Dockerfiles and for `minio/mc` in
  `docker-compose.yml` were the `linux/arm64` manifests of a pull on an
  Apple Silicon machine, not the multi-arch indexes, so a build or a run
  on an `amd64` runner failed with `exec format error`. Every pin is now
  the index digest, which resolves to the platform's manifest; the
  `build` and `integration` jobs of `verify.yml` prove it on every push.
