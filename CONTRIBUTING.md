# Contributing

Origo is Latere's git hosting service: a write-ahead log in object
storage as the source of truth, repositories as a warm cache. This file
is for people and agents changing it; users read the README, and the
design lives in [`specs/`](specs/README.md).

## The bar

Bare `make` runs the quality gate (`go tool lateregate`): format, lint,
modernize, per-package coverage at 90% or more, the suite with only the
toolchain and git on `PATH`, the suite against an empty temporary
directory, the licence notice, and the spec tree. `make test-integration`
runs the store suite and the end-to-end suite against MinIO; run it
before a push that touches the log. A change that lowers a threshold or
adds a waiver needs the reason in `.lateregate.yaml`. A bug fix carries a
test that fails without it.

## Specs first

A feature starts as a spec with acceptance criteria that are testable
sentences. Implementation follows the spec; a divergence is recorded in
the spec's Outcome section, not left in the code. Names in a spec (error
codes, environment variables, metrics, keys) are the names the code uses.

## Before writing a package

Check [`latere.ai/x/pkg`](https://github.com/latere-ai/pkg#before-writing-a-package)
first: a generic package with a plausible second consumer is written
there, at that module's bar, and consumed from there, while `internal/`
holds only what is specific to Origo. The S3 client, the metrics
registry, the error envelope, and the probes moved there once a second
consumer existed; the error codes, the `Origo-Contract` header, and the
`Store` interface stayed.

## Three registers

Every sentence is written for one reader, and the register follows the
reader: the user in API `message` fields and git's sideband, the
contributor in specs, this file, package docs, and commit messages, the
developer in logs, `/readyz`, and error details. The rule and the review
checklist are
[`docs/writing/registers.md`](https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md)
in pkg.

## Commits

One logical change per commit, staged explicitly, in the imperative,
saying what changed for whoever reads the log. Push to `main`; the
pipeline runs the gate on every push and a tag `v*` releases.
