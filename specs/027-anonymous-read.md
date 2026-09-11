---
title: "Anonymous read: a node may serve a repository the authorizer opens to a caller with no credential"
status: testing
track: infra
depends_on:
  - specs/007-authentication-and-delegation.md
  - specs/009-read-api-and-archive.md
  - specs/012-limits-and-abuse.md
  - specs/016-security-and-threat-model.md
affects: [internal/auth/, internal/limits/, internal/config/, cmd/origod/, docs/install.md, docs/api.md, specs/016-security-and-threat-model.md]
effort: small
created: 2026-09-11
updated: 2026-09-11
author: changkun
---

# Anonymous read

## Overview

Spec 016 says an operator who wants public repositories gets them
"through the authorizer answering allow for an anonymous subject". That
path does not exist. `Verifier.Middleware` answers 401 for a request with
no credential before the guard runs, so the authorizer is never asked and
the allow is unreachable.

This spec builds the path and corrects that sentence. It is off by
default. An installation that does not set `ORIGO_ANONYMOUS_READ` behaves
exactly as it does today, byte for byte, including every reason token.

Origo still holds no visibility and no ownership. Whether a repository is
public is the authorizer's answer, not a fact the node stores. What the
node owns is the two things the authorizer cannot do: admitting a request
that carries no credential, and paying for it.

## Current state

Built on 2026-09-11, off in every installation. `internal/auth/anonymous.go`
holds the route set and matches a request against it with an
`http.ServeMux` over the same patterns; `Verifier.Middleware` admits a
credential-less request on one of them with `Principal{}` when
`VerifierOptions.AnonymousRead` is set; `WriteRefusal` renders every
refusal of an empty-subject principal as the 401 with `reason: "missing"`
and the `Basic` challenge; `Limits.Middleware` buckets an empty subject at
the sentinel `AnonymousBucket` and refills it at
`ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE`; `cmd/origod` reads both variables
and wires them. Every criterion below has a green test, and spec 016's
"Transport" paragraph is corrected.

Not yet: a release carrying the switch, and a live installation with it
set. The status stays `testing` until a tag ships it and the consumer
that decides visibility (auth's spec 077) is deployed against it. Nothing
in this spec is reachable before an operator sets the variable, so the
release that carries it changes nothing for an installation that does
not, and `TestTheSwitchChangesNothingForARefusedCaller` is what says so.

## Design

### The switch

| Variable | Default | Meaning |
|---|---|---|
| `ORIGO_ANONYMOUS_READ` | `false` | when true, a request with no credential on a route of the set below is admitted with an empty subject and decided by the authorizer like any other |
| `ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE` | `60` | the refill of the single anonymous bucket, per node. `0` turns the limit off, which an operator should not do |

`ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE` is read only when the switch is on.

### Admission

The verifier admits a credential-less request when all three hold: the
switch is on, the request carries no credential in any of the three forms
`Credential` accepts, and the route is in the set below. It is admitted
with `Principal{}`: empty subject, empty actor, no bound scope. Every
other refusal of the verifier is unchanged, so a **present but bad**
credential is still 401 with its own reason and is never silently
downgraded to anonymous.

### The route set

A maintained list in `internal/auth/anonymous.go`, matched by an
`http.ServeMux` holding the same patterns, so Go's own routing decides
and not a hand-rolled prefix test. A read-marked route added elsewhere in
the tree does not become anonymous by being added; it becomes anonymous
by being added here.

```
GET  /r/{id}/info/refs                      service=git-upload-pack only
POST /r/{id}/git-upload-pack
GET  /{owner}/{slug}/info/refs              service=git-upload-pack only
POST /{owner}/{slug}/git-upload-pack
GET  /v1/repos/{id}
GET  /v1/repos/{id}/refs
GET  /v1/repos/{id}/commits
GET  /v1/repos/{id}/commits/{sha}
GET  /v1/repos/{id}/compare/{range}
GET  /v1/repos/{id}/tree/{sha}
GET  /v1/repos/{id}/blob/{sha}
GET  /v1/repos/{id}/archive/{file}
```

`info/refs` with `service=git-receive-pack` is not in the set, so a push
gets the 401 and the `Basic` challenge that makes git ask for a
credential. The pretty form is registered as two exact patterns rather
than through `/{owner}/{slug}/{service...}`, because that wildcard covers
`git-receive-pack` as well.

Not in the set, and why: `GET /v1/repos/{id}/stats` (the one read that
touches LFS accounting as well as the index, and no clone or browser
needs it); the LFS batch, whose download answer is a presigned bucket URL
that would outlive the request and the bucket; `GET
/v1/repos/{id}/export.bundle` (an unbounded server-side build);
`GET /v1/repos/{id}/import`; `GET /v1/repos` (the directory); every
write; every operation of spec 019. Consumer-side reasoning for each is
in auth's spec 077.

### The refusal is the 401 that already exists

`WriteRefusal` renders a refusal of an **empty-subject** principal as
401 `unauthenticated`, `details.reason: "missing"`, with
`WWW-Authenticate: Basic realm="origo"`. Not 403.

Two reasons converge, and both are load-carrying:

1. **Existence hiding.** Spec 007 requires authorization before lookup so
   a refused caller cannot tell a repository they may not read from one
   that is not there. A private repository, a name that resolves to
   nothing, an unknown id, and a public repository asked for a write must
   all answer the same. Answering the same 401 the node already answers
   to every credential-less request makes the anonymous refusal identical
   to the refusal of an installation with the switch off. Nothing about
   the registry leaks, including whether the feature is on.
2. **Git.** A git client prompts for a credential on 401 with
   `WWW-Authenticate` and gives up on 403. A 403 here would break every
   person cloning a private repository over HTTPS.

An authorizer outage on an anonymous request renders the same 401, for
the same reason: the shape of the answer must not depend on anything the
node learned about the repository.

One path reaches the bucket before the decision, and it is not a
refusal. The owner/slug form resolves the name to an id before it asks,
because an authorizer keys on the id (spec 007). A bucket that cannot
answer that resolve produces the ordinary `storage_unavailable`, which is
the same answer for every name and the same answer an authenticated
caller gets, so it discloses nothing about the registry. The work it
costs is one resolve per request, inside the anonymous bucket.

### Paying for it

One token bucket for all anonymous traffic on a node, keyed by a sentinel
that no subject can equal, refilled at
`ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE`. `RateLimit-Limit` and
`RateLimit-Remaining` are answered as for any subject.

The consequence is stated and not hidden: anonymous callers share one
bucket per node, so a scraper degrades other anonymous readers. It cannot
degrade an authenticated subject, which holds a bucket of its own that
the anonymous one cannot draw from. A bucket per client address would
need a trusted `X-Forwarded-For` chain, which spec 012 has not solved.

The other limits of spec 012 apply unchanged: the subprocess semaphore,
the body caps, and the deadlines are per node and do not read a subject.

### Threat model rows, for spec 016

| Threat | Control |
|---|---|
| Reading a private repository with no credential | the authorizer decides every anonymous request exactly as it decides an authenticated one; the node opens nothing on its own. With the switch off no anonymous request is admitted at all |
| Learning that a private repository exists | every anonymous refusal is one 401 with `reason: "missing"`, identical across a private repository, an unresolvable name, an unknown id, a refused action, and an authorizer outage, and identical to the switch-off installation |
| Writing with no credential | no write route is in the set, so the verifier refuses the request before a handler sees it; and an authorizer that allowed a write to an empty subject would still find no route that admitted the request |
| Escalating a bad credential into an anonymous one | only an **absent** credential is admitted; a present credential that does not verify is refused with its own reason |
| Denial of service by an anonymous caller | one bucket per node at 60 a minute by default, which cannot draw from an authenticated subject's bucket |
| An expensive anonymous request | `export.bundle` and the LFS batch are out of the set; `archive` is in it and is the figure the bucket default is chosen against |

Spec 016's "Transport" paragraph is corrected in the same change: the
sentence "There is no anonymous read in v1" is replaced by a pointer
here.

## Acceptance criteria

| Criterion | Test |
|---|---|
| with the switch off, every route of the public listener is 401 for a request with no credential | `cmd/origod`, `TestEveryRouteRequiresAToken` (extended to run in both switch states) |
| with the switch on, only the routes of the set admit a credential-less request; every other route is still 401 | `cmd/origod`, `TestEveryRouteRequiresAToken` |
| a present but unverifiable credential is never downgraded to anonymous | `internal/auth`, `TestBadCredentialIsNotAnonymous` |
| the set admits every read route and withholds every route named above | `internal/auth`, `TestAnonymousSetAdmitsTheReadRoutes`, `TestAnonymousSetWithholdsTheRest` |
| the anonymous rate is below the per-subject rate and defaults to 60 | `internal/limits`, `TestAnonymousRateDefaults` |
| an anonymous deny, an unresolvable name, an unknown id, and an authorizer outage are the identical 401 with `reason: "missing"` and a `Basic` challenge | `internal/auth`, `TestAnonymousDenialIsTheSame401Everywhere`; over the git routes, `internal/httpgit`, `TestAnonymousIsRefusedWithTheOne401` |
| `info/refs?service=git-receive-pack` is never anonymous, in either URL form | `internal/auth`, `TestAnonymousSetExcludesReceivePack` |
| all anonymous traffic shares one bucket, and it cannot draw from an authenticated subject's | `internal/limits`, `TestAnonymousShareOneBucket` |
| an anonymous clone of a repository the authorizer allows succeeds, in both URL forms | `internal/httpgit`, `TestAnonymousClone` |
| with the switch off and with it on, a refused caller gets the same status, headers and body, byte for byte, on the anonymous routes and on the withheld ones alike | `cmd/origod`, `TestTheSwitchChangesNothingForARefusedCaller` |

The sweep runs the two owner/slug rows in the switch-off state only: the
fake bucket it runs on answers a storage error to every resolve, so with
the switch on those two never reach the decision. The route set's own
tests cover both URL forms.

## Out of scope

Anonymous LFS. Anonymous writes, ever. A per-address bucket. Any notion
of visibility stored on the node.
