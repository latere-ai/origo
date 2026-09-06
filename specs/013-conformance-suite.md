---
title: "Conformance suite: the contract as executable tests"
status: drafted
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/008-push-events.md
  - specs/009-read-api-and-archive.md
affects: [test/conformance/, test/e2e/, .github/workflows/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Conformance suite

## Overview

The contract in spec 003 is only worth something if it is checked. This
spec turns it into a Go test package that runs against any base URL: a
live Origo, a consumer's stub, or a future provider. Origo's own release
is gated on it, and consumers run it against their stubs so their
integration tests and the real service agree.

## Design

`test/conformance` is an importable package: `conformance.Run(t,
conformance.Target{URL, Token, Authorizer, EventsSink})`. It drives the
real `git` binary and `net/http` against the target and asserts every
table in spec 003: repository lifecycle, each advertised capability,
shallow fetch at any reachable hash, atomic pushes, non-fast-forward
rejection, the read endpoints against a fixture it pushes itself, archive
reproducibility, delegation with `act`, repository-bound tokens, push
event delivery and signature, and every error code.

`test/e2e` runs the suite against a kind stack: MinIO, three `origod`
replicas, a stub issuer, a stub authorizer and event sink, plus the
failure scenarios of specs 004 to 008 that need a real cluster (node kill
mid-push, cache pressure, compaction under load). Runs on every push to
`main` with a 20 minute budget and after every production rollout
against the live service with a dedicated repository, whose timings are
attached to the release.

A consumer keeps a stub of the contract for its own tests; the suite is
what keeps that stub honest.

## Acceptance criteria

- The suite passes against the kind stack and against the live service
  after a release.
- Removing any capability from the server fails at least one test in the
  suite, checked by a mutation run in CI for the capabilities table.
- A consumer's stub (the hosting product's `gitstub`) passes the suite.
