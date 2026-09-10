# Contributing

Thanks for looking. This file is for people and agents changing Origo.
Users read the [README](README.md) and [`docs/`](docs/README.md); the
design and the reasoning behind it live in [`specs/`](specs/README.md).

## Getting set up

You need Go 1.27 or newer, `git`, `openssl`, and a container engine
(`podman` or `docker`). Then:

```sh
make dev      # MinIO, the stubs, and a node you can push to
make dev-down # stops all of it
make          # the quality gate
```

`make` needs only the Go toolchain and git. Everything it pins comes from
public modules, so it runs the same on your machine as in CI.

Install the hooks once with `git config core.hooksPath .githooks`. They run
formatting and licence checks before a commit and the linter before a push,
so you see a finding before CI does.

## Sending a change

Fork the repository, work on a branch, and open a pull request. Keep one
logical change per commit, stage the files explicitly, and write the
subject in the imperative, saying what changed for whoever reads the log.
Maintainers push to `main` directly; the pipeline runs the gate on every
push and pull request, and a `v*` tag cuts a release.

If you are planning something large, open an issue first. A design that
lands without a spec is harder to review than one that arrives with the
reasoning attached.

## The bar

`make` runs the whole gate (`go tool lateregate`), fourteen checks:
formatting, the linter, modernization, known vulnerabilities, the suite
with and without the race detector, per-package coverage at 90% or more,
the suite with only the toolchain and git on `PATH`, the suite against an
empty temporary directory, the licence notice, the dependency allow list,
and the spec tree. `go tool lateregate list` names them and
`go tool lateregate <name>` runs one. `make test-integration` runs the
store suite and the end-to-end suite against MinIO; run it before a change
that touches the log.

A bug fix carries a test that fails without it. A change that lowers a
threshold or adds a waiver records the reason in `.lateregate.yaml`, so
the exception is reviewable rather than invisible.

## Specs first

A feature starts as a spec with acceptance criteria that are testable
sentences. The implementation follows the spec, and a divergence is
recorded in the spec's Outcome section rather than left in the code.
Names in a spec (error codes, environment variables, metrics, keys) are
the names the code uses. `make specindex` checks that the spec index and
the specs agree.

Small fixes do not need a spec. Anything that changes the contract, the
log format, or a configuration variable does.

## Where a package belongs

`internal/` holds what is specific to Origo. A generic package with a
plausible second consumer belongs in
[`latere.ai/x/pkg`](https://github.com/latere-ai/pkg), the shared library
Origo already depends on, so both consumers get the fixes. The S3 client,
the metrics registry, the error envelope, and the probes moved there once
a second consumer existed; the error codes, the `Origo-Contract` header,
and the `Store` interface stayed here because nothing else would use them.
If you are unsure which side a package falls on, put it in `internal/` and
say so in the pull request.

## Three registers

Every sentence is written for one reader, and the register follows the
reader: the user in API `message` fields and git's sideband, the
contributor in specs, this file, package documentation, and commit
messages, the developer in logs, `/readyz`, and error details. The rule
and the review checklist are in
[`docs/writing/registers.md`](https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md).

## Reporting a vulnerability

Do not open an issue. [`SECURITY.md`](SECURITY.md) says where to send it.
