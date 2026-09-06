# Origo

Origo hosts git repositories as an infrastructure component. It gives every
project, sandbox, or agent a real git remote over HTTPS, stores each push as
an entry in a write-ahead log in object storage, and keeps repositories on
local disk only as a cache. Nodes are stateless and carry no routing table,
pushes are linearized without a consensus cluster, and an idle repository
costs nothing.

Origo is built by [Latere](https://latere.ai) for its own platform, where
it serves `git.latere.ai`, and is designed to be run by anyone with a
Kubernetes cluster and an S3 compatible bucket. Origo is the component's
name; the hostname is the operator's.

## Status

Phase 1 is being built: one node that serves clone, fetch, and push with
the log as the source of truth (specs 002, 003, 004). The spec deck under
[`specs/`](specs/README.md) describes the whole system and is the build
plan. What runs today: `git clone`, `fetch`, and `push` over smart HTTP
against a repository that lives in the bucket, the repository lifecycle
under `/v1/repos`, the write-ahead log with its create-if-absent commit,
the repository cache that materializes from the log and is rebuilt when
corrupt, the sweeper, the three listeners with `/livez`, `/readyz`,
`/version`, and `/metrics`, the quality gate, the container images, and
the release pipeline. Phase 2 adds identity and delegation (spec 007)
and many nodes with gossip (spec 005).

Authentication in phase 1 is a single static bearer read from
`ORIGO_DEV_TOKEN`; the public listener accepts it and refuses everything
else. Spec 007 replaces it with OIDC and the consumer's authorizer.

## Run it

Requirements: Go 1.27, git, and a container engine for MinIO.

```sh
make            # the whole quality gate
make dev        # MinIO in a container, then origod in the foreground
```

`make dev` prints the ports it chose. Then, with the token it prints:

```sh
curl -X POST -H "Authorization: Bearer dev-token" http://localhost:$PORT/v1/repos \
  -d '{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"acme","slug":"app"}'
git clone http://x:dev-token@localhost:$PORT/acme/app.git
```

The id form `/r/<id>.git` always works and is what a consumer stores.
The node reads its configuration from the environment; spec 002 lists
every variable. The required ones:

| Variable | Value |
|---|---|
| `ORIGO_S3_ENDPOINT`, `ORIGO_S3_REGION`, `ORIGO_S3_BUCKET`, `ORIGO_S3_KEY`, `ORIGO_S3_SECRET` | the bucket; `ORIGO_S3_PATH_STYLE=1` for MinIO |
| `ORIGO_PUBLIC_URL` | the origin clients see, for example `https://git.example.com` |
| `ORIGO_DEV_TOKEN` | the phase 1 bearer |

A start with anything missing fails with one message that names every
missing variable. `make test-integration` runs the tiers that need MinIO
beside them. Kubernetes manifests are under [`deploy/`](deploy/); apply
[`deploy/bootstrap/`](deploy/bootstrap/README.md) once by hand, and a
`v*` tag releases through `deploy/prod/`.

## What it does

- **A git remote for every repository.** `git clone`, `fetch`, and `push`
  over smart HTTP with bearer authentication. Shallow and partial clones
  work at any reachable commit. Git LFS is supported.
- **Durable before acknowledged.** A push is written to the write-ahead log
  in object storage and acknowledged only then. The reference update is one
  compare-and-swap, so every push is linearized and every read is consistent.
- **Stateless nodes.** Any node serves any repository. A repository missing
  from a node's disk is materialized from the log on the next request and
  garbage collected when idle. Adding or replacing a node needs no migration.
- **Cheap at both ends.** A busy monorepo gets as many replicas as its reads
  need. A million small repositories that are mostly idle keep no local copy
  at all.
- **Server-side operations.** Refs, log, diff, tree, blob, and an archive of
  any commit over a JSON API, so a platform can show history and diffs
  without cloning.
- **Push events.** A signed webhook per reference update, so a deploy, a
  build, or a review can start from a push.
- **Delegation.** A service acts on behalf of a user or an organization with
  an auditable claim, so a platform can commit for its users without holding
  their credentials.

## Where things live

| Path | Purpose |
|---|---|
| `specs/` | Design specs and the build plan. Read [`specs/README.md`](specs/README.md) first. |
| `cmd/origod` | The server binary: configuration, listeners, run group |
| `internal/` | The packages behind it, one per spec |
| `deploy/` | Kubernetes manifests: `base/`, the `prod/` overlay, and `bootstrap/` |
| `test/e2e/` | End-to-end suite: origod as a process, MinIO, the real git (`make test-integration`) |

## Acknowledgements

The storage design follows the approach described by Cursor's engineering
team in [Git at any scale](https://cursor.com/blog/git-at-any-scale): a
write-ahead log in object storage as the source of truth, repositories as a
warm cache, and linearized pushes without a consensus cluster.

## License

MIT. See [`LICENSE`](LICENSE).
