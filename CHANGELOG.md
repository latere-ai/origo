# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

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
- Every error response carries the fixed user sentence of its code with
  the developer reason in `details`, and every response of the public
  listener, `/readyz` and `/version` included, carries `Origo-Contract`.
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
