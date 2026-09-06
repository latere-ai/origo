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
- Phase 1 authentication is one static bearer from `ORIGO_DEV_TOKEN`.
