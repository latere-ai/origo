---
title: "The OpenAPI document: Origo's surface as a machine-readable contract, generated from the specs"
status: testing
track: infra
depends_on:
  - specs/003-protocol-contract.md
  - specs/018-installation.md
  - specs/029-the-api-at-the-platform-origin.md
affects: [tools/apidoc/openapi.go, tools/apidoc/openapi_test.go, tools/apidoc/main.go, tools/apidoc/page.go, tools/apidoc/page_test.go, tools/apidoc/go.mod, api/openapi.yaml, api/openapi.go, api/openapi_test.go, cmd/origod/node.go, cmd/origod/openapi_test.go, cmd/origod/main_test.go, deploy/prod/ingress.yaml, docs/api.md, Makefile, .github/workflows/verify.yml, CHANGELOG.md, specs/020-server-side-git-operations.md, specs/022-landing-page.md, specs/README.md]
effort: medium
created: 2026-09-20
updated: 2026-09-20
author: changkun
---

# The OpenAPI document

## Overview

### Scope

One document: Origo's HTTP surface as OpenAPI 3.1, generated from the same
spec tables `docs/api.md` is generated from, committed at `api/openapi.yaml`,
and served by the node at `GET /openapi.yaml`. One Ingress rule so Latere's
installation answers it, and one line on `docs/api.md` pointing at it.

Out of scope: a request or response schema per route, because the tables state
bodies in prose and a schema invented from prose would claim types no spec
states; the platform's Repos pages, which are platform spec 68's; any route,
code, header or field change. No endpoint moves and no self-hoster has to do
anything.

### Problem

The maintainer decided on 2026-09-20 that each capability's documentation
renders its API overview from that capability's own OpenAPI document
(latere-ai/platform, `specs/68-docs-over-the-capabilities.md`, option A of
"Where Origo's OpenAPI document comes from"). Arca publishes one at
`arca/api/openapi.yaml` and serves it; Origo publishes none, so the platform's
Repos pages can only link `docs/api.md`, and no client generator, no request
collection and no linter can read Origo's surface without parsing Markdown.

Origo's route truth today is in three places, and they are not equals:

| Where | What it holds | Who reads it |
|---|---|---|
| the endpoint tables of the specs | every method, path and behaviour, one row each, 40 rows over 11 specs | `tools/specindex/specs` parses them; `tools/apidoc` renders `docs/api.md` from them; the deck's own test fails on a name no spec defines |
| `internal/api`, `internal/httpgit`, `internal/lfs`, `cmd/origod` | the registrations, one `mux.HandleFunc` each (`internal/api/api.go:189`, `read.go:66`, `admin.go:36`, `operations.go:92`, `internal/httpgit/handler.go:175`, `internal/lfs/lfs.go:117`, `cmd/origod/node.go:548`) | the router alone |
| `internal/contract` | the 25 error codes, their statuses and their one user sentence each | every handler |

The specs are the published truth: `docs/api.md` says so in its first
paragraph, and it is the page a consumer codes against. The registrations
carry no behaviour text at all, and `internal/contract` carries the codes but
no route. So the document has one honest source and two constraints the design
has to meet: the node must be able to answer it without a parser its build
list does not carry (`.lateregate.yaml`, `depcheck`), and the file a consumer
vendors must be the file the node serves.

## Options

### Where the document is generated from

| | Shape | For | Against |
|---|---|---|---|
| A | `tools/apidoc` gains a second writer over the index it already builds: one operation per endpoint row of the specs | One source for both documents, so `docs/api.md` and the OpenAPI document cannot disagree; the deck's own findings already fail the run, so a table shape neither tool recognizes is caught at `make docs` | The document is only as precise as the tables, which state bodies in prose |
| B | A route table in the main module, as Arca does (`arca/internal/apidocs`, one row per registration) | The document cannot disagree with the router; the node builds it in process | Origo's registrations carry no text, so every summary would be written a second time beside the one the spec already states, and `docs/api.md` would keep its own source: two route truths, which is the fault this document exists to avoid |
| C | Hand-written and committed | Full control of every schema | It drifts the day a route lands, which is what platform spec 68 refused for its own half |

**Recommendation: A.** The row a reader sees on `docs/api.md` and the operation
a generator reads are then one row read twice. What A does not prove, that the
node registers what the tables state, is proved where it is true rather than
asserted here: an acceptance test drives the real node for every path the
document marks as taking no bearer, and for one that takes one.

### How the node answers the document

| | Shape | For | Against |
|---|---|---|---|
| A | The node embeds `api/openapi.yaml` and serves those bytes at `GET /openapi.yaml`, `application/yaml` | One committed document, and the served body is byte for byte the file a consumer vendors, which is a stronger statement than any test over two files can make; no new package on the node's build list | The path differs from arcad's `openapi.json`, so a reader who knows one core guesses wrong once |
| B | The node serves `openapi.json`, from a second file the same run generates in JSON and the node embeds | The family's path, and the encoding a browser tool expects | A second committed copy of one document, whose only reader is the compiler: a build artifact in the tree. The two copies cannot disagree only because a test says so, which is the argument A does not have to make |
| C | The node embeds the YAML and converts it at start-up | One file and the family's path | A YAML parser joins `cmd/origod`'s build list, which `.lateregate.yaml`'s `depcheck` allows only as a recorded decision, and spec 001's seventh invariant is that the node carries what it needs and nothing else. A parser for a constant the build already fixed is work at start-up for no reader |

**Recommendation: A.** The constraint decides it: the node cannot produce JSON
from YAML without a package it should not carry, and the alternative is
committing the same document twice. YAML is what the platform vendors and what
every OpenAPI reader takes, and the one cost, a path unlike arcad's, is a line
in the documentation rather than a fault.

### What the document covers

| | Shape | For | Against |
|---|---|---|---|
| A | Every row of every endpoint table: the JSON API, the git transport and LFS under a `transport` tag, and the probes and the landing page under a `service` tag with `x-scope: operator` | The document describes what a node answers, which is what a self-hoster reading it at their own installation needs; a consumer hides a group by its tag or its scope | A platform page that hides nothing renders six routes no integrator calls |
| B | The `/v1` routes alone | Nothing to hide | The document would answer at a node and not describe that node: the git transport is the product, and a reader who fetched the document from `code.latere.ai` would not find the clone paths in it |
| C | A, but the transport rows excluded | The origin serves `/v1/repos` alone (spec 029), so the document would match the origin | The document is not the origin's; it is the installation's, and Latere's own transport is at `code.latere.ai` |

**Recommendation: A.** One document per installation, covering what that
installation serves, with the marks a consumer needs to show a subset.
Hiding is the consumer's decision and the document gives it two handles.

## Design

### The generator

`tools/apidoc` keeps rendering `docs/api.md` and gains `openapi.go`: the
document as Go values, built from the `specs.Index` the command already
builds, and rendered to JSON by the standard library, then to YAML by
`github.com/goccy/go-yaml`, the converter Arca uses. Both are dependencies of
the tool's own module (`tools/apidoc/go.mod`), which is not the node's.
`make docs` writes both pages and the document in one run:

```
specs/*.md  ──►  tools/specindex/specs.Build  ──►  Index
                                                    │
                          ┌─────────────────────────┴───────────────┐
                          ▼                                         ▼
                    Page(idx)                               Document(idx)
                          │                                         │
                          ▼                                  JSON ──► YAML
                   docs/api.md                                       │
                                                                     ▼
                                                            api/openapi.yaml
                                                                     │
                                                              //go:embed
                                                                     ▼
                                                     GET /openapi.yaml at the node
```

The document is OpenAPI `3.1.0`. `info.title` is Origo, `info.version` is the
contract version of spec 003's `Origo-Contract` row, read by the function
`docs/api.md` already states it with, so a version the deck does not state
renders no document at all rather than a guess. There is no `servers` block:
a document in a repository describes no one installation, and the node serves
the same bytes at whatever address it answers.

One operation per endpoint row:

| Field | Rule |
|---|---|
| `operationId` | the method and the path, one word per segment, a wildcard as `By` and its name: `getV1ReposById`, `postV1ReposByIdCherryPick`. Derived from the route, so rewording a row does not move a generated client's method name. Arca's scheme (`arca/internal/apidocs/apidocs.go`) |
| `summary` | the opening statement of the row's answer column, which is the last column after `Path`, since a table that splits request from response states the answer last and a table with one column states both in it. Cut at the first sentence end, semicolon or colon outside backticks, which is where these rows stop naming the answer and start on its shape and its conditions |
| `description` | every column after `Path`, in the table's order, each under its own header when the table has more than one. The row's whole text, so the document carries what the spec states and the page shows; absent where the row is one statement and the summary already is it |
| `tags` | one tag per group (below); a consumer renders and orders by it |
| `x-scope` | `operator` on the rows a caller writing against the API never sends: spec 002's four probe routes and spec 022's two page routes. Absent on every other row |
| `parameters` | every `{name}` of the path, `in: path`, required; and every `?name=` or `&name=` the row states, `in: query`, optional, because each of those has a default the row also states. `{repo}` carries the sentence `docs/api.md` opens its endpoint section with, the one place it is written |
| `security` | the bearer scheme, except on the eight rows the deck serves without a token, which carry `security: []` |
| the success answers | every 2xx the row states, in the order it states them, or 200 where it states none, or none at all where the answer column opens with a status that is not 2xx, which is how a row states that the route only refuses (`POST /{repo}/info/lfs/locks`). Two on a row is two answers: an operation of spec 020 commits with 201 and answers a dry run 200, and `POST /v1/repos/{id}/gc` answers 200 on the node that compacted and 202 on one that scheduled. Each carries the status text and no schema: these rows answer JSON, raw blob bytes, a tarball, a diff, a bundle and git's own wire format, and a content type the row does not state would be an invention |
| the refusals | one response per code the row names, `$ref`ing the component that code declares, under each status that code's own Status column lists. Two codes of one row sharing a status are one response naming both. Every secured operation also answers `unauthenticated`, `forbidden` and `rate_limited`, which no row states because they are the middleware's and not the handler's: the verifier and the bucket of spec 012 run before the route does. Criterion 6's test is where the first of the three is held to the node |

The row is the unit, which has two consequences worth stating. A status a
row does not state is not in the document: spec 020's table stated its 201
in the paragraph above the rows rather than in them, so the rows gained it
with a dated note, and a row that only ever refuses keeps no success answer.
And a refusal stated for a family of rows rather than in one of them is not
on that operation either; the three the middleware always answers are added
because they are the middleware's, and the rest are each row's own.

`components.schemas.Error` is the envelope of spec 003, `{"error": {"code",
"message", "details"}}`. `components.responses` has one entry per code of the
deck, its status, its name and its user sentence. The codes are the deck's,
which is `internal/contract`'s table read from the other end: a test in the
main module holds the two to each other, so a code added to the package
without a row, or a row without a constant, fails there.

The groups, one tag per row, decided by the path first and the owning spec
second, so a row's tag follows what the row is rather than which spec grew it:

| Tag | Rows |
|---|---|
| `transport` | every path under `/{repo}/`: git's three smart HTTP routes (spec 003) and the three LFS routes (spec 010) |
| `repositories` | the lifecycle and the directory (specs 003, 026) |
| `read` | refs, commits, compare, tree, blob, archive (spec 009) |
| `operations` | the server-side git operations (spec 020) |
| `administration` | transfer, freeze, stats, gc, import, export, verify (specs 019, 014) |
| `tokens` | the repository-bound token and the key set (spec 007) |
| `service` | the probes, the landing page, the favicon and this document (specs 002, 022, 030) |

A spec that defines an endpoint outside `/{repo}/` and has no entry in that
table renders no document: a new spec's routes are a decision about where a
reader finds them, not a fall-through into an unnamed group.

### The file

`api/openapi.yaml`, committed, regenerated by `make docs`, with a comment
header naming what it is and that it is generated. A consumer vendoring it
records the tag it took it from; the document itself names the contract
version, because that is what a client pins, and Origo cannot know its own
tag at the moment it renders.

`tools/apidoc`'s test renders a fresh document from the deck and fails when
the committed file differs, the way `TestAPIDocIsCurrent` already holds
`docs/api.md`. The `specindex` job runs `make docs` and then `git diff
--exit-code`, so a spec table edited without regenerating shows up as a
documentation diff on the same push.

`docs/api.md` gains one line under its opening, pointing at the document and
at the route a node serves it from. The line is in the generator's page
template, since the page is generated.

### The route

Two rows of the document describe the internal listener rather than the
published one: `GET /livez` and `GET /metrics` are spec 002's, and a
request for either at the published address meets the verifier. OpenAPI
has no way to say which listener answers a path, which is part of why
both carry `x-scope: operator`: they are not a caller's surface, and a
consumer that hides that group hides the exception with it. Criterion 6's
test reads each on the listener that serves it.

| Method | Path | Body |
|---|---|---|
| GET | `/openapi.yaml` | 200, this document, `application/yaml`; the bytes of `api/openapi.yaml` as the build embedded them. No token, like `GET /readyz` and `GET /version` (spec 002); unlike those two it is part of the contract, so a client may read it at any installation |

It sits on the public listener beside `GET /version` and the key
set, before the verifier, so it takes no token: a document that says how to
authenticate is not one a caller can be asked to authenticate for.
`api/openapi.go` is `package openapi` in the directory the document lives in,
because `//go:embed` reads no parent directory; it holds the embedded bytes
and the handler, which writes them with `application/yaml` and nothing else.
The bytes are the file, so no rendering happens at start-up or per request.

Latere's installation publishes it at `code.latere.ai` and not at
`api.latere.ai`: one probe surface per host, owned by the core that publishes
that host, which is spec 029's decision for `/readyz` and `/version` and
holds for this route for the same reason. `deploy/prod/ingress.yaml` gains
one path, written as a regular expression anchored at the end like the
favicon's, because that object sets `use-regex` and because the nginx
admission webhook refuses a path holding a dot under `Exact` or `Prefix`.
The base overlay gains nothing: a self-hoster's Ingress takes `/` and reaches
the route already.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `api/openapi.yaml` is what `make docs` renders from the deck, and a spec table changed without regenerating fails | `tools/apidoc`, `TestOpenAPIDocumentIsCurrent`, beside the page's own; verify.yml's generated-documentation step, which diffs `api/` beside `docs/` |
| 2 | Every endpoint the deck defines is one operation of the document, with an `operationId`, a summary, a tag and its path parameters, and the document defines no operation the deck does not | `tools/apidoc`, `TestDocumentCarriesEveryEndpoint` over the built values |
| 3 | The document declares one response per error code of `internal/contract` and no other, under the statuses that code's row lists, with the sentence the node writes | `api/openapi_test.go`, `TestDocumentNamesEveryContractCode`, which reads the committed file against `contract.Codes()`, `contract.Statuses` and two sentences in full, the code-table walk of `internal/contract` allowing a constant and not a variable as the code of a call |
| 4 | `info.version` is the contract version of spec 003 and a deck that states none renders no document | `tools/apidoc`, `TestDocumentStatesTheContractVersion`; `api/openapi_test.go`, the committed file against `contract.Version` |
| 5 | The node answers `GET /openapi.yaml` with no token, `application/yaml`, and the bytes of `api/openapi.yaml` | `cmd/origod`, `TestOpenAPIDocumentIsServed` against a running node |
| 6 | Every route the document serves without a bearer is one the node serves without a bearer, and a route it marks with the bearer scheme refuses an unauthenticated request with `unauthenticated` | `cmd/origod`, `TestTheDocumentsUnauthenticatedRoutesAreTheNodes`, over the public and the internal listener |
| 7 | Latere's installation claims exactly one new path, `/openapi.yaml`, on `code.latere.ai` and nothing on `api.latere.ai`, and the base overlay is unchanged | `cmd/origod`, `TestTheProdIngressPublishesTheDocument` |
| 8 | `docs/api.md` points at the document and at the route it is served from | `tools/apidoc`, `TestPageNamesTheDocument`, and `TestAPIDocIsCurrent` for the committed page |

## Dependencies

[[003-protocol-contract]] for the contract version and the error envelope the
document renders, [[018-installation]] for the generated-page rule this
document joins, and [[029-the-api-at-the-platform-origin]] for the host
decision the route follows. Outside the tree, latere-ai/platform
`specs/68-docs-over-the-capabilities.md`, which vendors `api/openapi.yaml` by
tag as `docs/repos/openapi.yaml` and renders the Repos API page from it.

What the platform gets, stated as it is rather than as it will be: its
renderer groups by the first tag and hides an operation whose `x-scope`
holds `admin` or opens with `platform.`
(`frontend/src/docs/api/model.ts`, `isDeveloperOp`). So Origo's seven
groups render, its `operator` mark hides nothing there yet, and the six
probe and page routes show until that repository extends the filter,
which is its own spec 68's third engine change. Nothing in this tree
waits on it: the mark is in the document and the group is one name.

## State on 2026-09-20

Built on main, green locally. Every criterion has a passing test in the tree.

### What was built

**The generator.** `tools/apidoc/openapi.go` builds the document from the
index the command already reads and renders it to YAML through JSON; the
command writes `api/openapi.yaml` beside `docs/api.md` under one `-write`,
which is what `make docs` runs. `github.com/goccy/go-yaml` is a dependency of
the tool's module alone, so the node's build list is unchanged. The document
is 41 operations over 36 paths, seven groups and 25 declared refusals.

**The file.** `api/openapi.yaml`, committed, with the comment header and no
`servers` block.

**The route.** `api/openapi.go` embeds the file and serves it;
`cmd/origod/node.go` mounts `GET /openapi.yaml` on the public listener before
the verifier. `deploy/prod/ingress.yaml` publishes it at `code.latere.ai`.

**The page.** `docs/api.md` carries one line naming the document and the
route, rendered from the page template.

**Two dated notes and one list.** Spec 020's Result column now opens with
the status each operation answers, the 201 its own Response paragraph
states and the 200 of a dry run, because the document carries what a row
states and nothing else; without it the document claimed 200 for four
routes that answer 201, which is the one thing a generated document must
never do. Spec 022 records that the public listener's unauthenticated
paths are six rather than five. And the route sweep of spec 007,
`TestEveryRouteRequiresAToken` in `cmd/origod`, carries the document
route in its unauthenticated list, which is the list that spec says a new
route belongs in.

### What the tests prove

| Criterion | Test | State |
|---|---|---|
| 1 | `tools/apidoc`, `TestOpenAPIDocumentIsCurrent` | passing |
| 2 | `tools/apidoc`, `TestDocumentCarriesEveryEndpoint` | passing |
| 3 | `api/openapi_test.go`, `TestDocumentNamesEveryContractCode` | passing |
| 4 | `tools/apidoc`, `TestDocumentStatesTheContractVersion`; `api/openapi_test.go`, `TestDocumentStatesTheContractVersion` | passing |
| 5 | `cmd/origod`, `TestOpenAPIDocumentIsServed` | passing |
| 6 | `cmd/origod`, `TestTheDocumentsUnauthenticatedRoutesAreTheNodes` | passing |
| 7 | `cmd/origod`, `TestTheProdIngressPublishesTheDocument` | passing |
| 8 | `tools/apidoc`, `TestAPIDocIsCurrent` and `TestPageNamesTheDocument` | passing |

Each was run once against the tree without the change and once with it, and
each failed for the reason it exists:

| Test | What it was run against | What it said |
|---|---|---|
| `TestOpenAPIDocumentIsCurrent` | the tree with no committed document, and an index one row behind | `open api/openapi.yaml: no such file or directory`, then the stale-row half of the test itself |
| `TestDocumentCarriesEveryEndpoint` | a generator that skips one row | `the document is missing [GET /v1/repos/{id}/stats]` |
| `TestDocumentNamesEveryContractCode` | a document with one response block removed | `the document declares no response for repo_frozen` |
| `TestOpenAPIDocumentIsServed` | the node without the route | the document route answered 401: with no route in front of the verifier, the request reached it and was asked for a token |
| `TestTheDocumentsUnauthenticatedRoutesAreTheNodes` | a document marking the directory route open | the directory answers 401 and the document serves it without a token |
| `TestTheProdIngressPublishesTheDocument` | the overlay without the rule | `deploy/prod/ingress.yaml does not publish /openapi.yaml` |

### What waits for a release

Two things outside the tree, which is why the status is `testing` and not
`complete`. The route answers at `code.latere.ai` when the overlay is applied:
the manifest test reads the rule, and only an apply proves nginx takes it, the
dot in the path being what the admission webhook is particular about. And the
platform vendors `api/openapi.yaml` from a tag, so its Repos API page renders
once Origo is tagged; until then platform spec 68's freshness check skips the
capability, which is what that spec says it does. The Outcome records both,
and the spec is then `complete`.
