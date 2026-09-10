# Origo

**Git hosting as an infrastructure component.** Every push is an entry in a
write-ahead log in S3 compatible object storage. Repositories on disk are
only a cache. There is no database, no leader, and no consensus cluster.

[![CI](https://github.com/latere-ai/origo/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/origo/actions/workflows/verify.yml)
[![Release](https://img.shields.io/github/v/release/latere-ai/origo)](https://github.com/latere-ai/origo/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/origo)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## The problem

Some platforms need one git repository per unit of work: a repository per
project, a repository per sandbox session, a repository per agent run. A
conventional git server does not fit that shape. It keeps every repository
on a specific disk, so nodes are not interchangeable, growth means
migration, and a million repositories that nobody fetches still cost a
million repositories' worth of storage and backup.

Origo gives each of them a real git remote over HTTPS, and pays for the
ones in use.

## How it works

Object storage is the source of truth, not a backup of it.

- **A push is a log entry.** Origo writes the packfile and the reference
  update to the log in your bucket, then acknowledges. Nothing is
  acknowledged before it is durable.
- **`If-None-Match: *` is the whole lock.** A create-if-absent write on the
  next log slot linearizes concurrent pushes. That is the only coordination
  primitive Origo needs, so there is no Raft group and no leader election to
  operate.
- **Nodes are stateless and hold no routing table.** Any node serves any
  repository. A repository missing from a node's disk is rebuilt from the
  log on the next request and dropped again when it goes idle. Adding,
  replacing, or losing a node needs no migration.
- **Idle repositories cost storage and nothing else.** A busy monorepo gets
  as many warm copies as its reads need; a million quiet ones keep no local
  copy at all.

## Try it in a few minutes

You need Go 1.27 or newer, `git`, `openssl`, and a container engine
(`podman` or `docker`).

```sh
git clone https://github.com/latere-ai/origo.git
cd origo
make dev
```

That starts MinIO with a bucket, a stub identity provider, a stub
authorization endpoint, and an event sink, then runs the node in the
foreground. About half a minute in, it prints a ready-to-paste line:

```
git clone http://x:<token>@localhost:<port>/dev/hello.git
```

Run it in another terminal and you have a git remote. Commit, push, clone
it again somewhere else, and the commit is there. `make dev-down` stops
everything.

## Install it for real

[`docs/install.md`](docs/install.md) goes from a Kubernetes cluster and a
bucket to a first push, in about an hour. Every command on that page is run
against a throwaway cluster by the project's own tests before it is
published, so a command that drifts from the manifests fails the build
rather than your installation.

You supply four things: a bucket at any S3 compatible endpoint that honours
conditional creates, an OIDC issuer, one HTTP endpoint that answers whether
a subject may read, write, or administer a repository, and a hostname.
Origo authenticates and authorizes every request, and it asks you both
questions rather than deciding them itself.

## What you get

- **A git remote.** `git clone`, `fetch`, and `push` over smart HTTP with
  bearer authentication. Shallow and partial clones work at any reachable
  commit. Git LFS is supported.
- **Consistent reads.** Every reference update is one compare-and-swap
  against the log, so a read never sees a half-applied push.
- **A read API.** Refs, log, diff, tree, blob, and an archive of any commit
  over JSON, so a product can show history and diffs without cloning.
- **Server-side git.** Create a commit or merge a branch through the API,
  with no working copy anywhere.
- **Push events.** A signed webhook per reference update, so a build, a
  deploy, or a review starts from a push.
- **Delegation.** A service acts on behalf of a user or an organization
  under an auditable claim, so it commits for its users without holding
  their credentials.

## Documentation

| | |
|---|---|
| [Install](docs/install.md) | from a cluster and a bucket to a first push |
| [Configuration](docs/configuration.md) | every environment variable with its default |
| [API](docs/api.md) | endpoints, headers, and error codes a client relies on |
| [Operations](docs/operations.md) | backup, restore, scaling, and what to do during an outage |
| [Upgrades](docs/upgrades/README.md) | what a version number promises, and how to verify what you install |
| [Migration](docs/migration.md) | moving repositories in from another git host |

[`docs/README.md`](docs/README.md) is the index. The design, and the
reasoning behind it, is in [`specs/`](specs/README.md).

## Status

v0.1.0 shipped on 2026-09-10. A release carries `origod` for Linux and
macOS on amd64 and arm64, container images for both architectures,
Kubernetes manifests pinned to the release, three SPDX bills of materials,
and cosign signatures over the images and the checksums. On every tag the
pipeline installs that release onto a fresh cluster and runs the
conformance suite against the published image.

There is no public Origo installation yet. Origo runs where you install it,
and this paragraph will name a hosted endpoint when one exists. The
conformance suite has been run against a cluster built from the published
release, and not yet against a long-lived one.

The release tag is a semantic version, and it covers the API contract, the
log format, the configuration variables, and the event payloads. See
[Upgrades](docs/upgrades/README.md) for what each bump means and how long a
previous contract stays served.

## Contributing

Issues and pull requests are welcome. [`CONTRIBUTING.md`](CONTRIBUTING.md)
covers the quality gate, how to run it locally, and how a change is
reviewed. [`SECURITY.md`](SECURITY.md) is how to report a vulnerability;
please do not open an issue for one.

## Acknowledgements

The storage design follows the approach described by Cursor's engineering
team in [Git at any scale](https://cursor.com/blog/git-at-any-scale): a
write-ahead log in object storage as the source of truth, repositories as a
warm cache, and linearized pushes without a consensus cluster.

Origo is built by [Latere](https://latere.ai).

## License

MIT. See [`LICENSE`](LICENSE).
