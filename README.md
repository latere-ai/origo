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

Phase 1 runs: one node serves `git clone`, `fetch`, and `push` over smart
HTTP against a repository that lives only in the bucket, with every push
durable before it is acknowledged, verified on MinIO and on DigitalOcean
Spaces. The spec deck under [`specs/`](specs/README.md) describes the
whole system and is the build plan; its index says what is built, what
is next, and what has to be true before the repository goes public.
Operators start at [`docs/install.md`](docs/install.md), which takes a
cluster and a bucket to a first push, and then
[`docs/operations.md`](docs/operations.md).

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
| `deploy/` | Kubernetes manifests: `base/`, the `prod/` overlay, `bootstrap/`, and `examples/` to copy |
| `docs/` | For whoever runs or uses an installation. Start at [`docs/install.md`](docs/install.md). |
| `test/e2e/` | End-to-end suite: origod as a process, MinIO, the real git (`make test-integration`) |
| `CONTRIBUTING.md` | The bar, the spec workflow, when a package belongs in `latere.ai/x/pkg`, and the writing registers |

## Acknowledgements

The storage design follows the approach described by Cursor's engineering
team in [Git at any scale](https://cursor.com/blog/git-at-any-scale): a
write-ahead log in object storage as the source of truth, repositories as a
warm cache, and linearized pushes without a consensus cluster.

## License

MIT. See [`LICENSE`](LICENSE).
