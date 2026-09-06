# Specs

Design specs for Origo, git hosting as an infrastructure component. One
spec covers one module. Each spec states the problem, the design with
enough precision to build from, and acceptance criteria that are testable
statements. Spec 001 fixes the architecture every other spec assumes; read
it first. Spec 003 is the contract a consumer codes against; it is the one
document a platform integrating Origo needs.

## Layout

Flat files `specs/NNN-name.md` in one number space with `track: infra` in
the frontmatter. Numbers are stable identifiers and are never reused. Open
specs sit here and are the work queue. A terminal spec moves to
`specs/.archive/` keeping its number so `depends_on` paths keep resolving.

## Lifecycle

```mermaid
stateDiagram-v2
  [*] --> drafted
  drafted --> validated: review passes
  validated --> testing: implementation lands
  testing --> complete: verified, Outcome written
  drafted --> stale
  validated --> stale
  vague --> drafted: scoped
```

## Index

| # | Spec | Effort | Status |
|---|---|---|---|
| [001](001-architecture.md) | Architecture: components, storage model, flows, invariants | medium | drafted |
| [002](002-repository-scaffold.md) | Repository scaffold: module, binary, configuration, gate, release | small | testing |
| [003](003-protocol-contract.md) | Protocol contract: what a consumer relies on | medium | in-progress |
| [004](004-write-ahead-log.md) | Write-ahead log: entries, immutable index, create-if-absent commit, materialization | large | testing |
| [005](005-placement-and-replication.md) | Placement and replication: rendezvous hashing, gossip, consistent reads, cache eviction | medium | drafted |
| [006](006-compaction.md) | Compaction: primary-only repacks, log truncation | medium | drafted |
| [007](007-authentication-and-delegation.md) | Authentication and delegation: issuers, authorizer, acting on behalf | medium | drafted |
| [008](008-push-events.md) | Push events: signed webhooks per reference update | small | drafted |
| [009](009-read-api-and-archive.md) | Read API and archive: refs, log, diff, tree, blob, tarball | medium | drafted |
| [010](010-lfs.md) | Git LFS: batch API and presigned object transfer | small | drafted |
| [011](011-observability.md) | Observability: metrics, traces, logs, alerts | small | drafted |
| [012](012-limits-and-abuse.md) | Limits and abuse controls | small | drafted |
| [013](013-conformance-suite.md) | Conformance suite: the contract as executable tests | medium | drafted |
| [014](014-drive-migration.md) | Migration of Drive's hosted repositories (cross-repo) | medium | vague |

## Dependency graph

Edges point from a spec to the specs it builds on.

```mermaid
flowchart LR
  subgraph F[Foundation]
    S001[001 architecture]
    S002[002 scaffold]
    S003[003 contract]
  end
  subgraph S[Storage]
    S004[004 write-ahead log]
    S005[005 placement + replication]
    S006[006 compaction]
  end
  subgraph A[Access]
    S007[007 auth + delegation]
    S008[008 push events]
    S009[009 read API + archive]
    S010[010 LFS]
  end
  subgraph H[Hardening]
    S011[011 observability]
    S012[012 limits]
    S013[013 conformance]
  end
  subgraph M[Adoption]
    S014[014 drive migration]
  end
  S002 --> S001
  S003 --> S001
  S004 --> S002
  S005 --> S004
  S006 --> S004
  S007 --> S002
  S007 --> S003
  S008 --> S004
  S008 --> S007
  S009 --> S004
  S009 --> S007
  S010 --> S004
  S010 --> S007
  S011 --> S004
  S011 --> S005
  S012 --> S007
  S012 --> S004
  S013 --> S003
  S013 --> S008
  S013 --> S009
  S014 --> S013
```

## Build order

| Phase | Specs | Outcome |
|---|---|---|
| 1 | 002, 003, 004 | A single node serves clone, fetch, and push with the log as the source of truth |
| 2 | 007, 005 | Authenticated, delegated access; many nodes, consistent reads |
| 3 | 008, 009, 006 | Push events, the read API and archive, compaction under load |
| 4 | 010, 011, 012, 013 | LFS, telemetry, limits, and the conformance suite gating releases |
| 5 | 014 | Existing repositories migrate from Drive |

## Conventions

- Every spec has the frontmatter fields `title`, `status`, `track`,
  `depends_on`, `affects`, `effort`, `created`, `updated`, `author`.
- Diagrams are Mermaid. Tables carry exact values so an implementer never
  has to guess a number.
- Error codes, metric names, environment variables, and paths named in a
  spec are the names the code uses.
- Acceptance criteria are the test list. A spec is `complete` when every
  criterion has a passing test and the Outcome section records any
  divergence.
- Wording is for a reader outside Latere. A Latere hostname or value is an
  example or a default, never the only option.
