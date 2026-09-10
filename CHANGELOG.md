# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

## v0.1.1 - 2026-09-10

- The install page now gets you from nothing to a pushed commit on your own
  cluster, not only on the throwaway one: it says how to obtain a token from
  your own OIDC issuer and what claims Origo reads, and it says that your
  authorization endpoint has to know a repository before Origo creates one,
  which is the 403 a first push used to end in with nothing to read. The
  endpoint's five rules are stated in full, and so is the fact that a
  single-tenant installation needs no service behind it: a static list of
  subjects that denies the reserved probe id is a complete implementation.
  Every block that falls back to the example stack now says so beside
  itself.
- The API page carries the authorization endpoint: the request and the
  answer, the action sent per operation, the reserved probe id, the caching
  and the one retry, and the optional figures with when to send each. It is
  what to code against when you write the endpoint.
- Opening an installation in a browser now shows a small page instead of a
  username and password box that nothing could satisfy. It says what Origo
  is, that the address is a git remote rather than a website, how to clone,
  that git wants a bearer token as the password, where the documentation
  is, and which version is running. `curl` gets the same page as plain
  text. There is no web interface beyond it and it needs no token.
- The README is a front door: what Origo is, the problem it solves, how the
  design works, a local quick start, and where the install and API pages
  are. It no longer claims a hosted installation, because there is none.
- `SECURITY.md` says what a release actually carries today: three SPDX
  bills of materials and cosign signatures over the images and the
  checksums, and no build provenance while the repository is private.
- A code of conduct, issue templates, and a pull request template.
- The documentation index is grouped by what the reader is doing: running
  Origo, building against it, or changing it.
- The source tree carries source and nothing else. A compiled binary had
  been committed by mistake, so a clone is now smaller and holds only what
  you can read.

## v0.1.0 - 2026-09-10

- The conformance suite (spec 021): `test/conformance` runs the whole
  contract against any base URL, a live Origo, the contract stub, or a
  consumer's own stub, one subtest per row of every table, and deletes
  what it created. `RateLimit-Limit` is what its rate-limit case reads.
  The code table in `internal/contract` carries every status beside
  every sentence, and a test walks the module so no handler sends a
  code under another status or with a sentence of its own. A push the
  log refuses reaches git as `remote: <code>: <sentence>` with the
  table's sentence and nothing else; the reference and the hashes go to
  the node's log. `ORIGO_TEST_DROP_CAPABILITY`, for the mutation job,
  turns one advertised capability off. The contract stub serves push
  events, the limits, and compaction, so a consumer's tests meet the
  whole contract in-process.

- `origod` starts from typed configuration and refuses to start with one
  message that names every missing variable.
- Three listeners: the public surface on `:8080`, `/livez`, `/readyz`,
  `/version`, and `/metrics` on `:8081`, and the gossip port `:7946/udp`.
  `/readyz` and `/version` are also served publicly for the release smoke.
- `make dev` runs MinIO and the node locally; `make test-integration` runs
  the tiers that need MinIO.
- Container images, Kubernetes manifests under `deploy/`, and the release
  pipeline on a `v*` tag.
- Authentication and delegation (spec 007): a request carries a JWT from
  one of `ORIGO_OIDC_ISSUERS` with audience `origo`, a service token may
  carry `act` to act on behalf of a subject, and every request that names
  a repository is authorized by the consumer's endpoint at
  `ORIGO_AUTHORIZER_URL` before the repository is looked up, with the
  answer cached for its `ttl`. `POST /v1/repos/{id}/tokens` mints a
  repository-bound `read` or `write` token signed with `ORIGO_TOKEN_KEY`,
  and `GET /.well-known/jwks.json` serves the key. `ORIGO_DEV_TOKEN` is
  gone: a node that sets it refuses to start, and `ORIGO_OIDC_ISSUERS`,
  `ORIGO_AUTHORIZER_URL`, `ORIGO_AUTHORIZER_TOKEN`, and `ORIGO_TOKEN_KEY`
  are required. The bootstrap Secret is `origod-auth`. `make dev` is out
  of service until spec 013 ships the stub binary; `make test-integration`
  runs the stub issuer and authorizer in-process.
- The authorizer call is retried once when the authorizer closed a
  kept-alive connection before reading the request (net/http's `server
  closed idle connection`), the same as a refused or reset connection;
  it was answered `authorizer_unavailable` before.
- The read API (spec 009), under `/v1/repos/{id}` with the `read`
  action: `refs`, `commits` with exact cursor paging, `commits/{sha}`
  with its stats and trailers, `compare/{base}...{head}` as a diff cut
  at 1 MiB on a file boundary, `tree/{sha}` in pages, `blob/{sha}` with
  `Range`, and `archive/{sha}.tar.gz` streamed as git produces it. A
  full reference name goes in `?ref=`, `?base=`, or `?head=` with the
  segment `-`. Every response carries `Origo-Commit` and an `ETag` of
  the index sequence, so `If-None-Match` answers 304 without git;
  `Origo-Truncated` marks a cut. A subprocess past 30 seconds is 504
  `operation_timeout`, a blob over 50 MiB asked for whole is 413
  `blob_too_large`. `GET /v1/repos/{id}` gains `pushed_at`.
- Every error response carries the fixed user sentence of its code with
  the developer reason in `details`, and every response of the public
  listener, `/readyz` and `/version` included, carries `Origo-Contract`.
- Git LFS (spec 010): `POST /{repo}/info/lfs/objects/batch` answers
  presigned URLs on the bucket, so object bytes never pass through a
  node. An upload URL pins `Content-Length` and
  `Content-Type: application/octet-stream`, and the upload is complete
  when `POST /{repo}/info/lfs/verify` has checked the stored size; a
  download is served only for a verified object. An upload batch past
  the authorizer's `quota_bytes`, the bytes under `lfs/` counted, is
  refused. `ORIGO_S3_PUBLIC_ENDPOINT` is the bucket endpoint LFS
  clients reach and defaults to `ORIGO_S3_ENDPOINT`. Locking is not
  supported and every path under `info/lfs/locks` says so. These
  endpoints answer in the LFS body shape, not the error envelope.
- Test stubs and the kind overlay (spec 013): `origo-stubs`, one binary
  running the stub issuer, authorizer, event sink, and a TLS git source
  with a 5 000-commit fixture, from flags, packaged as
  `ghcr.io/latere-ai/origo-stubs` by `Dockerfile.stubs`; `test/stubs/origo`,
  the whole contract in-process for a consumer's tests; the stub
  authorizer's outage set over HTTP (`PUT /fail`, `POST /hang`,
  `POST /resume`). `make dev` is back: MinIO, the stubs, a generated
  `ORIGO_TOKEN_KEY`, the node, and a clone line with a minted token.
- Push events (spec 008): with `ORIGO_EVENTS_URL` and
  `ORIGO_EVENTS_SECRET` set, every acknowledged push is one signed
  `POST` to the sink with `Origo-Signature` (HMAC-SHA256 over the
  body), `Origo-Event`, and `Origo-Delivery`, delivered at least once
  with an `id` that is the same however often it is delivered, so a
  consumer deduplicates on it. A push made on behalf of a user carries
  `pusher.sub` and `pusher.actor`; a forced update is marked `forced`;
  `git push -o origo.event=off` sends nothing for that push; a
  `default_branch` change is one event with the single `HEAD` update.
  A sink that fails is retried at 1 s, 10 s, 1 min, 10 min, then
  hourly for 24 hours, after which the event sits under
  `origo/events/dead/` and `origo_events_dead_total` counts it; an
  operator moves the object back under `origo/events/<repo>/` to
  retry it. A node that dies between the push and the delivery is
  covered by every other node's repair sweep (`ORIGO_REPAIR_INTERVAL`,
  `ORIGO_REPAIR_UNHEARD`). The URL without the secret refuses to
  start. `origo_push_duration_seconds{phase}` reports the four phases
  of a push.
- Placement and replication (spec 005): nodes keep a live set by
  heartbeat over the gossip port, every datagram signed with
  `ORIGO_GOSSIP_SECRET`, required whenever `ORIGO_GOSSIP_PEERS` is set;
  a push is announced to every peer so a warm copy elsewhere catches up
  before the next request. Every response that names a repository
  carries `Origo-Prefer`, the nodes that hold it warm by rendezvous
  hashing, highest first, as many as the authorizer's `replicas`. The
  cache is bounded by `ORIGO_CACHE_BYTES`: least recently used copies
  are evicted under pressure, copies used in the last 10 minutes are
  kept, and copies idle for 24 hours go. Materialization fetches
  entries with 4 workers and indexes consecutive packs in one
  `index-pack` run, so 1 000 entries land in about a second instead of
  a minute. `deploy/base` gains the HorizontalPodAutoscaler on CPU
  between 2 and 32 replicas and pod anti-affinity; the bootstrap
  Secret template carries `ORIGO_GOSSIP_SECRET`. A warm copy whose
  bucket was reset is rebuilt from the log instead of answering
  `storage_unavailable`, and a catch-up costs one currency check, not
  two. The currency check's histogram carries `result` (`404`, `200`,
  `error`).
  `deploy/examples/kind` runs MinIO, three nodes, and the stubs in a kind
  cluster with Cilium and metrics-server on fixed host ports, through
  `up.sh` and `down.sh` (`make dev-up`, `make dev-down`); `verify.yml`
  runs the integration, cluster, up-script, mutation, and weekly fuzz
  jobs from one image build. The node's storage and outbound transports
  bound each dial to 10 seconds, so a bucket that drops packets answers
  `storage_unavailable` instead of hanging.
- The write-ahead log (spec 004): entries, immutable index objects
  committed by create-if-absent, the `HEAD` currency check, repository
  metadata, and the sweeper, over an S3 client signed by the standard
  library. Repositories materialize from the log into `ORIGO_DATA_DIR`
  and are rebuilt when corrupt.
- Smart HTTP (spec 003): clone, fetch, and push in both URL forms, with
  partial and shallow clones, protocol v2, atomic pushes, push options,
  and per-reference results. A push is acknowledged only after its entry
  and index object are durable.
- The repository lifecycle under `/v1/repos`: create, read, rename,
  default branch, delete with a 7 day hold, undelete.
- `make test-integration` runs the store suite against MinIO and the
  end-to-end suite: push, wipe the disk, clone; two nodes pushing
  different branches at once; a node killed mid-push.
- Compaction (spec 006): a repository that receives many pushes no
  longer serves fetches from thousands of small packs. One node, the
  first the placement header names, repacks it into a few geometrically
  sized packs, uploads them, and records the result as one log entry, so
  every other node downloads packs instead of repacking. It runs in the
  background after a push that crosses 64 entries, 256 MiB of pack
  bytes in them, or a 512 KiB index object, and never delays a push. A
  push that lands while a repack runs wins and the compaction is
  retried; nothing is ever lost to one. A node that is not the
  repository's compaction node records the need instead, and the node
  that owns it acts within ten minutes. Once the packs hold the
  history, the folded entries and the packs they replaced are deleted
  after `ORIGO_SWEEP_MIN_AGE`, so a repository's storage stays
  proportional to its content rather than to how often it is pushed to.

- Three defects of the write-ahead log (spec 004) are fixed. A pack
  produced by compaction or an import is written on disk as
  `pack-<hash>.pack`, the name git reads; it was written under the log
  key's base name, on disk and invisible to git. A local copy fetches
  every listed pack it is missing whatever it already holds, so a copy
  that lost a pack file is restored on its next open instead of served
  from an incomplete object store. `size_bytes` of a repository is what
  the log holds: a compaction sets it to the bytes of its packs and a
  push adds its own, so the figure falls after a compaction and the
  quota and `stats` count bytes that exist.
- A push no longer hangs on macOS about once in a thousand: the
  hand-off between the node and its pre-receive hook used a FIFO open
  rendezvous that loses its wakeup there. The node now holds both ends
  of the hook's FIFOs, and a node that goes away mid-push ends its hook
  and `git receive-pack` instead of leaving them waiting.
- A node that starts before one of its issuers answers serves that
  issuer's tokens from the first request after the issuer is up: the
  first fetch is retried after a second, doubling to the minute, instead
  of `issuer_unavailable` for a minute after a failed start-up fetch.
- Telemetry (spec 011). `GET /metrics` carries every metric the deck
  defines from the first scrape, at 0 until something records it, so a
  dashboard panel is never empty because a series has not appeared yet;
  no label carries a repository, an owner, a subject, a reference, or a
  path. Setting `OTEL_EXPORTER_OTLP_ENDPOINT` exports traces, metrics,
  and log records over OTLP/HTTP: one trace per request on the public
  listener, a span per phase of a push and per object storage call, and
  the repository, subject, and actor as span attributes. Every request
  also writes one JSON line with its route, method, status, duration,
  repository, subject, actor, bytes each way, and trace id, and never a
  credential. That trace id is the `request_id` an LFS failure quotes,
  in place of the fresh UUID it sent before. The ten alerts are
  `deploy/base/prometheusrule.yaml`, applied beside the base where the
  Prometheus operator is installed.
- Degraded storage (spec 015). Every call to the bucket runs under
  `ORIGO_STORAGE_TIMEOUT` (10 seconds) and one of two breakers, reads
  and writes, that open after five failed calls in a row and stay open
  for 30 seconds before one probe. While the read breaker is open a
  repository the node holds is served from its local copy with an
  `Origo-Stale` header, the whole seconds since its last check that
  answered, for up to `ORIGO_STALE_MAX` (5 minutes); a repository the
  node does not hold, or one past that bound, answers 503
  `storage_unavailable` at once with a `Retry-After`. A push is refused
  before the client uploads a pack, as `remote error:
  storage_unavailable: ...` from `git push`, and a pack already
  uploaded waits up to a minute for the write breaker before it is
  refused in the sideband. A pack or entry the log names that is
  missing or corrupt makes that repository alone answer 503
  `repository_unavailable` naming the key, for the operator to restore.
  `origo_storage_ops_total`, `origo_storage_seconds`,
  `origo_storage_breaker_state`, `origo_stale_responses_total`, and
  `origo_log_integrity_errors_total` record it, and the alerts
  `OrigoBreakerOpen`, `OrigoStaleServing`, and `OrigoLogIntegrity`
  fire. A replica stays ready while its breaker is open. The kind
  overlay runs the slow proxy of `origo-stubs` in front of MinIO, with
  its control endpoint on host port 30085.
- Security and threat model (spec 016). A server-side fetch of another
  host, the import and verify to come, reaches only a host named in
  `ORIGO_EGRESS_ALLOW`, exact or `*.` wildcard, and never a loopback,
  link-local, private, or cluster address (`ORIGO_CLUSTER_CIDRS`): the
  node resolves the host once and dials by IP, through a forward proxy
  on a loopback port that git is pointed at, which terminates the
  source's TLS against the system roots plus `ORIGO_EGRESS_CA_BUNDLE`,
  follows redirects itself so every hop is checked, and refuses
  `CONNECT`. An operator pins a source to one address as
  `host=address`: the host is reached at that address and at no other,
  inside the cluster ranges or outside them, which is how a source that
  runs inside the cluster is named. Every repository now runs with `transfer.fsckObjects` and `core.protectHFS` beside
  `receive.fsckObjects` and `core.protectNTFS`, so a pack carrying a
  broken object or a tree entry that names the git directory is refused
  on every transfer. A reference name that is not UTF-8, or whose
  component ends in a dot, is refused. The gossip port admits UDP from
  the `origod` pods alone (`deploy/base/networkpolicy.yaml`), and
  `SECURITY.md` at the root says how to report a vulnerability.
- Repository administration (spec 019). `/v1/repos/{id}` carries the
  operations a repository needs over years: `transfer` moves it to
  another owner without changing its id, `freeze` and `unfreeze` stop
  and resume pushes, `import` mirrors an existing `https` repository in
  with its whole history as one log entry, `export.bundle` streams the
  whole repository as one `git bundle`, `stats` reports `size_bytes`,
  `lfs_bytes`, `packs`, `entries_since_compaction`, `refs`,
  `pushed_at`, and `compacted_at`, and `gc` compacts now. A rename or a
  transfer changes the clone URL at once and the old URL answers 404,
  never a redirect. A push to a frozen repository is refused at
  `info/refs` before the client uploads anything, and git prints
  `remote error: repo_frozen`, while clones go on. An import needs the
  source host on `ORIGO_EGRESS_ALLOW`, runs with `transfer.fsckObjects`
  and a 30 minute budget, and holds the repository until it finishes;
  a node that dies mid-import frees it after 45 minutes, and one that
  restarts under the same name frees it at start-up. A repository whose
  7 day delete hold has passed now answers 410 `gone` on every
  endpoint: its id stays taken forever and its owner and slug are free
  again. Once a week, on Sunday at 03:00 UTC, one node lists the whole
  bucket prefix and reports `origo_orphan_objects` and
  `origo_storage_bytes`, deleting an object nothing names after seven
  days and an unverified LFS object seven days after its upload. Each
  operation sends an event of its own kind: `renamed`, `transferred`,
  `frozen`, `unfrozen`, `deleted`, `undeleted`, `imported`, and
  `compacted`.
- Limits and abuse controls (spec 012). A node accepts
  `ORIGO_REQUESTS_PER_MINUTE` requests a minute per authenticated
  subject, 600 by default and `0` to turn the limit off, names the
  figure on every response as `RateLimit-Limit`, and answers 429
  `rate_limited` with `Retry-After` past that; it runs at most `ORIGO_MAX_GIT_PROCS` git
  subprocesses at once, 64 by default, a request waiting five seconds
  for a slot before the same 429 and a compaction skipping to its next
  sweep. A repository is held to the authorizer's `quota_bytes`, 50 GiB
  when it names none, measured as what the log holds plus the objects
  under `lfs/`: a push past it is refused in git's own output with
  `over_quota` and nothing is written, and an LFS upload batch past it
  is 413. A single push is at most 2 GiB and its body is no longer
  spooled past that. A repository-bound token's push is now held to the
  quota of the subject that minted it rather than to the default.
- Release and versioning (spec 017). A `v*` tag now builds every release
  artifact in this repository instead of calling the shared pipeline:
  `origod` for `linux` and `darwin` on `amd64` and `arm64` with
  `checksums.txt`, `ghcr.io/latere-ai/origod` and
  `ghcr.io/latere-ai/origo-stubs` as multi-architecture images, a
  `deploy-<version>.tar.gz` of `deploy/base` and `deploy/examples` with
  both images pinned, and a `fixture-<version>.tar.gz` the next release
  reads back to prove it serves what this one wrote. The images and the
  checksums are signed with the release workflow's own identity and no
  key held by Latere, and each image carries an SPDX bill of materials
  and build provenance; the pipeline verifies all of it from a clean
  runner before the run ends. The conformance suite runs against the
  published image in a kind stack between the build and the deploy, and
  against the live installation after it. `docs/upgrades/` states what a
  version number promises, how to upgrade and roll back, and how to
  verify a release.
- The runtime image is `debian:trixie-slim` pinned by digest, whose git
  is 2.47, in place of bookworm-slim and its 2.39.
- A node that meets a log object written by a newer release refuses that
  one repository with 503 `repository_unavailable` naming the object and
  a log line that names the upgrade document, instead of a parse error;
  every other repository goes on serving.
- `GET /version` has one source: the pipeline and `make build` both set
  `internal/version`, and `main.version` is gone.
- The release smoke no longer fails on every run: after `GET /readyz`
  answered 200 it grepped standard input, which is closed in a pipeline.
- `TestPushPhasesAreObserved` (spec 008) takes the push's request
  duration on a channel the server sends on after the handler returns,
  the edge it had none of: git exits on the report status the handler
  writes before its last phase is observed.
- Server-side git operations (spec 020), for tooling that changes many
  repositories without cloning any of them. Four `POST` routes under
  `/v1/repos/{id}`, each authorized as a `write`, take the branch and
  the commit the caller expects to find there and answer the commit
  they made: `commits` writes and deletes files, creating the branch
  from another when asked; `merge` fast-forwards or writes a
  two-parent commit; `cherry-pick` and `revert` apply up to 100
  commits, all in one entry so a partial application never lands. Each
  is one entry, one commit, and one `push` event carrying
  `operation`. A stale `expected_head` is 409 `non_fast_forward`, a
  merge git cannot make is 409 `merge_conflict` with the paths, and a
  change the request got wrong is 400 `invalid_change` with its index
  and reason. `dry_run` computes the result and writes nothing at all.
  A repository accepts 60 operations a minute; past that it is 429
  `rate_limited` with `Retry-After`.
- The authorizer's answer may now carry `requests_per_minute` (spec
  007), the rate that subject alone is bucketed at on the node: a tool
  that drives a fleet of repositories takes a figure of its own without
  raising `ORIGO_REQUESTS_PER_MINUTE` for every caller.
- Migration from another git host (spec 014).
  `POST /v1/repos/{id}/verify` compares a source with Origo's copy and
  answers whether every reference matches, which references differ with
  the hash on each side, and how many objects Origo's copy holds; the
  verdict is `verified_at` and `verified_equal` on `GET /v1/repos/{id}`
  and a `verified` webhook event. `origod` now dispatches subcommands:
  `serve` is the default, and `origod migrate -manifest <file> -report
  <file>` drives a whole manifest of repositories from registration to
  mirrored against `ORIGO_MIGRATE_URL` with the bearer
  `ORIGO_MIGRATE_TOKEN_ENV` names, `ORIGO_MIGRATE_PARALLEL` at once,
  writing one JSON line per repository and resuming from Origo's own
  state on a second run. `docs/migration.md` is the runbook, including
  the cut-over that keeps a prior host's clone URLs working.

- Installing Origo (spec 018). `docs/install.md` takes an operator with
  a cluster and a bucket to a first clone and push: what to prepare,
  the bucket and its credentials, the OIDC issuer and the authorization
  endpoint they run, the overlay, the signing key no manifest can
  carry, the apply, the check, and a table of what each failure means.
  Its commands are run against a bare cluster on every push, so a step
  that drifts from the manifests fails the build.
- `origod check` reaches everything a node depends on and prints one
  line per requirement, exiting non-zero on any failure: the bucket,
  the conditional create the log is linearized by, every issuer's
  discovery document and key set, the authorization endpoint's answer
  to the reserved probe id, a signed `ping` to the event sink, the
  cache directory against `ORIGO_CACHE_BYTES`, and git against the 2.40
  floor. It runs as an init container, so a misconfigured pod never
  reports ready.
- `ORIGO_TOKEN_KEY` has one home: the Secret `origod-token-key`, which
  the install document generates and every workload reads by name.
  `deploy/bootstrap/secrets.example.yaml` no longer offers a field for
  it, because a key pasted into a template is not the operator's own.
- `deploy/base` is provider-neutral: no ingress class, no
  certificate-manager annotation, no hostname, and no controller's own
  settings, each of which an overlay supplies. `deploy/examples`
  carries `digitalocean` and `aws` beside `kind`, and `deploy/prod`
  holds the values that used to sit in the base.
- Two generated reference pages: `docs/configuration.md`, every
  variable with its default, its constraints, and the subsystem that
  reads it, from `internal/config`; and `docs/api.md`, the endpoint,
  header, and code tables a consumer codes against. `make docs` writes
  both and a drift in either fails the same push.
