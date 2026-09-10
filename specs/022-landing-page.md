---
title: "Landing page: what a person sees at the root"
status: drafted
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-protocol-contract.md
  - specs/007-authentication-and-delegation.md
  - specs/016-security-and-threat-model.md
affects: [cmd/origod/, tools/smoke/]
effort: small
created: 2026-09-10
updated: 2026-09-10
author: changkun
---

# Landing page

## Overview

Origo has no web surface, so a browser that visits the root of an
installation reaches the catch-all behind the verifier and is answered
401 with `WWW-Authenticate: Basic realm="origo"`. A browser renders that
as a username and password dialog, and nothing a person can type in it
is accepted: Origo takes a bearer token from an OIDC issuer, not a
password. The first thing a stranger sees is a box that cannot be
satisfied.

This spec puts one small page at `GET /`, served without a token, that
says what Origo is, that this address is a git remote rather than a
website, how to clone, how to authenticate, and where the project lives.
It is a sign on a door, not a web interface. There is no HTML anywhere
else in Origo and this spec does not start one.

## Current state

Not built. `cmd/origod/node.go` `publicHandler` registers three
unauthenticated paths on the public listener's mux, `GET /readyz`,
`GET /version`, and `GET /.well-known/jwks.json`, and hands `/` to the
application surface behind the verifier. The route sweep
`TestEveryRouteRequiresAToken` in `cmd/origod` holds the rule that every
other route demands a token, as a maintained list (spec 016).

## Design

### Where the route is registered

The public listener has two levels of mux, and the page goes on the
outer one, in `publicHandler`, beside the probes:

```
public listener
└── outer mux (cmd/origod, publicHandler)
    ├── GET /{$}                 landing page      unauthenticated
    ├── GET /favicon.ico         204               unauthenticated
    ├── GET /readyz              probe             unauthenticated
    ├── GET /version             probe             unauthenticated
    ├── GET /.well-known/jwks.json  key set        unauthenticated
    └── /                        verifier → limits → application mux
                                 ├── /r/{id}/...             git
                                 ├── /{owner}/{slug}/{service...}  git, by name
                                 ├── /v1/...                 API
                                 └── /{repo}/info/lfs/...    LFS
```

Nothing of the git or API surface can be shadowed, for two reasons and
not one. The first is the level: every git, API, and LFS pattern is
registered on the inner application mux, which the outer mux reaches
through a single `/` entry, so a pattern added here is not even a
candidate against them. The second holds even if the two muxes were one.
The `/{$}` pattern matches the single path `/` and nothing else, while the
shortest git or API pattern needs at least two path segments, and
`http.ServeMux` gives the most specific pattern the request, so `/`
takes the page and every longer path takes the catch-all. The wildcard
label route `/{owner}/{slug}/{service...}` of spec 009 is the pattern to
watch, because it is deliberately broad; it needs two segments before
the wildcard, so `/` cannot reach it and it cannot reach `/`.

Anything other than a `GET` or `HEAD` of `/` falls to the catch-all and
is answered as it is today: the verifier refuses it 401, or the
application's unknown-route handler answers 400 `invalid_request`. The
page is a `GET`, so a `POST` of `/` is unchanged.

### Content negotiation

Both audiences reach this URL: a person in a browser, and a person at a
terminal running `curl https://git.example.com`. The body is chosen from
the request's `Accept`:

| `Accept` contains `text/html` | Body |
|---|---|
| yes | `text/html; charset=utf-8`, the page below with an inline stylesheet |
| no | `text/plain; charset=utf-8`, the same facts as lines |

`Vary: Accept` goes on both. The cost is one string test and a second
writer over the same facts; the benefit is that `curl` prints text a
person reads instead of markup they have to look past, and that is the
form half the visitors of a git host's root will ask for. A browser and
a terminal browser both send `text/html` in `Accept` and get the page.

### What the page says

One document, the same for every request except the version. The facts,
in order:

1. Origo, and one sentence: git hosting as an infrastructure component,
   where a push is an entry in a write-ahead log in object storage, so
   any node can serve any repository.
2. This address is a git remote, not a website. There is nothing to
   browse here.
3. How to clone, as a shape rather than a live URL:
   `git clone https://{host}/{owner}/{slug}.git`, with `{host}` named as
   the address being read. A repository also answers at `/r/{id}.git`,
   the form that survives a rename (spec 003).
4. Authentication in one line: when git asks for a password, use any
   username and a bearer token from this installation's identity
   provider. A browser sign-in box cannot accept one.
5. Where the documentation and the project live:
   `https://github.com/latere-ai/origo`.
6. The running version, the same string `GET /version` serves.

The register is the user's (`CONTRIBUTING.md`): no spec numbers, no
package names, no internal vocabulary.

### What the page must not say

The page is readable by anyone who can reach the installation, so it
carries nothing an unauthenticated caller does not already have. Not the
issuer, the authorizer, the bucket, the endpoint, the node name, the
internal listener, the data directory, or any other `ORIGO_*` value; not
a repository count, an owner, or whether any repository exists; not an
operator's name or address. The version is the one released fact on it,
and `GET /version` already serves that without a token.

The rule that makes this checkable rather than a promise: **nothing from
the request or the configuration reaches the body.** The page is a
constant document with the build's version interpolated, so it cannot be
made to reflect a `Host` header, a query string, or a path. That also
means it is identical for every visitor and safe for any cache.

### The favicon

A browser that renders the page then asks for `/favicon.ico` on the same
origin. Left to the catch-all that is a 401 with
`WWW-Authenticate: Basic`, which is the dialog this spec exists to
remove, arriving a moment later from a request the person never made.
Two things close it, and the page carries both because the first is a
bet on browser behaviour that Origo cannot test: the document declares
`<link rel="icon" href="data:,">`, which tells a browser not to ask, and
`GET /favicon.ico` answers 204 without a token for the browsers that ask
anyway. Neither fetches anything from anywhere.

### No assets, no script, no knob

The document is one file: inline `<style>`, no script, no image, no
font, no link to any host but the project's. It has to be legible with
the stylesheet thrown away, so the markup is a heading, paragraphs, a
`<pre>` block, and a footer, in reading order.

The page is fixed. It is not configurable and cannot be turned off,
because there is nothing in it for an operator to correct: it names no
installation and states no policy, so the page a private installation
serves and the page a public one serves are the same page. An operator
who wants a different root has the layer that already exists for it, the
Ingress every installation runs (spec 018), where a rule for `/` reaches
whatever they want to serve without Origo knowing. A variable would buy
that same outcome at the price of a second code path, an operator-
supplied string on an unauthenticated HTML page, and a row in spec 002's
table, for a need no installation has stated.

### Endpoints

| Method | Path | Body |
|---|---|---|
| GET | `/` | 200, the landing page; `text/html; charset=utf-8` when `Accept` contains `text/html`, `text/plain; charset=utf-8` otherwise, `Vary: Accept` on both; no token, no `WWW-Authenticate` |
| GET | `/favicon.ico` | 204, empty, no token; so a browser rendering the page is never asked for credentials it cannot supply |

Both join `GET /readyz`, `GET /version`, and `GET /.well-known/jwks.json`
as the unauthenticated paths of the public listener, and both carry
`Origo-Contract` like every other response of that listener (spec 003).
Neither is part of the contract of spec 003: a provider serving that
contract owes machines an API, not a person a page, and the conformance
suite of spec 021 does not ask for it.

## Not in this spec

- A web interface. No repository browsing, no file view, no login.
- Any HTML under `/v1/` or the git paths. They answer JSON and git.
- A page per repository, or any surface that says whether a repository
  exists.
- Configuration for the page's content. See above.

## Acceptance criteria

- `GET /` without a token answers 200, carries no `WWW-Authenticate`
  header, and its body names the project, the clone shape, the token
  rule, the project URL, and the running version; `Vary: Accept` is set
  (`cmd/origod`, `TestLandingPageAnswersTheRoot`).
- `Accept: text/html` answers `text/html; charset=utf-8` with the same
  facts, an inline stylesheet, the `data:` icon link, no `<script`, and
  no reference to any host but the project's; any other `Accept`
  answers `text/plain; charset=utf-8`
  (`cmd/origod`, `TestLandingPageNegotiatesHTML`).
- The page shadows nothing: with the page registered, `GET /{repo}/info/refs`
  in both URL forms and `GET /v1/repos/{id}` reach their own handlers
  and answer as before, none of the three bodies is the page, and `GET /`
  answers the page rather than any of them
  (`cmd/origod`, `TestLandingPageShadowsNoRoute`).
- The body carries no value the node was configured with: not the
  bucket, the endpoint, the issuer, the authorizer, the node name, the
  data directory, or any listener address
  (`cmd/origod`, `TestLandingPageLeaksNoConfiguration`).
- `GET /favicon.ico` answers 204 with an empty body and no
  `WWW-Authenticate` (`cmd/origod`, `TestFaviconAnswersNoContent`).
- The route sweep gains the two paths as unauthenticated and asserts
  that a `POST` of `/` and of `/favicon.ico` still demands a token
  (`cmd/origod`, `TestEveryRouteRequiresAToken`, the maintained list of
  spec 016).
- The post-deploy smoke checks the root: `GET /` answers 200 and its
  body names the released version, so a release proves the page on the
  installation it deployed (`tools/smoke`, `TestReleaseSmoke`).
