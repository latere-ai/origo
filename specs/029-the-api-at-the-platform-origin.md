---
title: "The API at the platform origin: /v1/repos answers at api.latere.ai beside code.latere.ai"
status: testing
track: infra
depends_on:
  - specs/007-authentication-and-delegation.md
  - specs/026-repository-directory.md
  - specs/027-anonymous-read.md
  - specs/028-authorizer-contract-2.md
affects: [deploy/prod/ingress-api.yaml, deploy/prod/audience.yaml, deploy/prod/kustomization.yaml, deploy/base/deployment.yaml, internal/auth/verifier.go, internal/auth/conformance_test.go, internal/config/config.go, internal/config/document.go, cmd/origod/node.go, cmd/origod/manifests_test.go, docs/configuration.md, docs/install.md, specs/007-authentication-and-delegation.md, specs/028-authorizer-contract-2.md, .lateregate.yaml]
effort: small
created: 2026-09-20
updated: 2026-09-20
author: changkun
---

# The API at the platform origin

## Overview

### Scope

Two changes to Latere's installation: a second Ingress rule serving `/v1/repos`
at `api.latere.ai`, and a second accepted audience so a token minted for the
origin verifies. No route is renamed, and a self-hoster running `deploy/base`
gets nothing new.

Out of scope: the console's Repos screens (platform spec 67, ps-02); SSH keys and
the registry, platformd's; ps-04; ps-11; any route, field or reason rename.

### Problem

The family decided on 2026-09-20 that `api.latere.ai/v1` is partitioned by
capability prefix, one prefix per core, and that Origo's prefix is `repos`
(latere-ai/specs, `decisions/2026-09-20-origin-capability-prefixes.md` and the
"Revision, 2026-09-20" of `infrastructure/platform/ps-01-one-origin.md`). Origo
owes no restructuring for it: the JSON API is already mounted at `/v1/repos`
(`internal/api/api.go:189`, `read.go:66`, `admin.go:36`, `operations.go:92`), so
the capability prefix and the resource collection are one path. Missing are the
route and the audience. The origin's Ingress rules are arcad's alone, so
`api.latere.ai/v1/repos` answers 404 from nginx, and the node verifies the
single audience `origo` (`internal/auth/verifier.go:325`,
`internal/config/config.go:280`), so a personal access token or a platform key
addressed to `api.latere.ai` is refused with `audience` even once the route
exists. Open cores' "one audience" sentence was amended the same day: a core
accepts exactly two, its own name and the platform origin, and the boundary
between cores is the authorizer's.

### The two hosts

```
code.latere.ai                          api.latere.ai
 origod-tls, on origod's object          api-latere-ai-tls, on arcad's object
 /{owner}/{slug}/info/refs               /v1/repos            ─┐ origod,
 /{owner}/{slug}/git-upload-pack         /v1/repos/{id}        │ no rewrite,
 /{owner}/{slug}/git-receive-pack        /v1/repos/{id}/refs   │ no base path
 /{owner}/{slug}/info/lfs/…              …every route the mux ─┘
 git over SSH, TCP 22                     already registers
 /v1/…  unchanged until ps-04            /v1/storage/…  arcad
 /readyz, /version                       /v1/environments/…, /v1/models/… as
 /.well-known/jwks.json                   each core arrives
 /  origo-web
```

Transport stays `code.latere.ai`: clone, LFS and SSH URLs name that host
whichever host a JSON request arrived on.

## Options

### How the rule on the shared host is carried

| | Shape | For | Against |
|---|---|---|---|
| A | A second host block inside `deploy/prod/ingress.yaml` | One file and one object for the installation's routing | That object carries `use-regex: "true"` (`deploy/prod/ingress.yaml:39`), which turns every path in it into a regular expression, and an unanchored `/v1/repos` matches anywhere in a path. Its own comment says regular expressions are tried before any prefix (`deploy/prod/ingress.yaml:24`) |
| B | A second object in `deploy/prod`, `origod-api`, host `api.latere.ai`, no `use-regex` | The regex annotation stays with the host that needs it and the origin rule is a plain `pathType: Prefix`. It is the shape arcad already runs on this host | A second file and a second object to keep in step |
| C | The base Ingress grows the rule | Nothing, for Latere | The base names no host and `TestBaseIngressIsControllerNeutral` (`cmd/origod/manifests_test.go:41`) enforces that; `api.latere.ai` is Latere's address, not an operator's |

**Recommendation: B.** The annotation decides it: A widens either the git paths
or the new one, and nginx rejects a whole document rather than one rule, so that
failure lands on a release apply.

### The audience variable's shape

| | Shape | For | Against |
|---|---|---|---|
| A | A second variable, the plural of the existing name | No change to a variable an operator already sets | Two variables for one fact, and two rows in `internal/config/document.go:77` |
| B | `ORIGO_OIDC_AUDIENCE` becomes a comma-separated list whose first entry is the primary, following Cella (`cella/internal/config/identity.go:100`, `audiences_test.go`) | One variable, one row, and the sibling core's shape unchanged. A single value keeps its present meaning exactly | The name is singular and holds a set |
| C | The verifier accepts `api.latere.ai` unconditionally | No configuration at all | A self-hoster would accept a Latere name, and the cores hold no Latere value |

**Recommendation: B.** The primary is what the node mints with, a distinction a
bare set cannot carry, and Cella has the shape already.

### The probe surface at the shared host

| | Shape | For | Against |
|---|---|---|---|
| A | `/readyz`, `/version` and `/.well-known/jwks.json` are not routed at `api.latere.ai` | The key set must resolve under `ORIGO_PUBLIC_URL`, which is `https://code.latere.ai` and does not move; the release smoke reads the probes at that host (`.github/workflows/release.yml:472`) | A reader who knows arcad's object carries four probes finds the hosts inconsistent |
| B | The three are exposed under `/v1/repos/...` | The prefix stays honest | Three new routes for a caller that does not exist, and `/version` under a collection is not a version of the collection |

**Recommendation: A.** A repository-bound token's `iss` is `ORIGO_PUBLIC_URL`
(`cmd/origod/node.go:259`, `:280`), so its key set is resolvable at exactly one
host by construction, and probes follow the address a deployment publishes: one
probe surface per host, owned by the core that publishes that host.

## Design

### The Ingress object

`deploy/prod/ingress-api.yaml`, a new `resources` entry of
`deploy/prod/kustomization.yaml:22` and not a patch, since the patches list
merges into the base and this object has no counterpart there. It carries `host:
api.latere.ai`, `/v1/repos` at `pathType: Prefix`, backend `origod:http`, no
`use-regex`, no `rewrite-target`:

- No `tls` block and no `cert-manager.io/cluster-issuer`: the origin's
  certificate is arcad's (`arca/deploy/prod/ingress.yaml:49`, `:60`, secret
  `api-latere-ai-tls`), and a second issuer annotation on one host is a second
  claim on one certificate.
- `proxy-body-size: "0"`, `proxy-read-timeout: "600"` and `proxy-send-timeout:
  "600"`, or one request behaves differently per host: an operation body is
  bounded at 64 MiB by the node (`internal/api/operations.go:39`) against a
  controller default of 1 MiB, so `POST /v1/repos/{id}/merge` would take a
  controller 413 at the origin and the node's readable error at
  `code.latere.ai`. `archive` and `export.bundle` stream for minutes.

Two nginx constraints govern the rule. The admission webhook refuses a path
holding a dot under `Exact` or `Prefix` and rejects the whole document rather
than the one rule (`arca/deploy/prod/ingress.yaml:142`); `/v1/repos` holds no dot,
so a plain `Prefix` applies. And the controller does not refuse two Ingresses
claiming one path on one host: what keeps `/v1/repos` Origo's is one prefix per
core plus each repository's own deploy test, ps-01 criterion 6. nginx also
renders `Prefix` as a prefix location, not the segment-bounded match the
Kubernetes type defines, so `/v1/reposX` reaches origod and 404s in its mux; no
other origin prefix begins with `/v1/repos`, so nothing is shadowed.

No base path is needed, and that is why this leaf is small. The decision's "what
it costs" paragraph has every other core rewrite `/v1/<prefix>/(.*)` to `/v1/$1`
or mount under a configured base, because its resources sit below a prefix that is
not one of them. Origo's prefix *is* its one collection, so published and
registered path are one string: no rewrite, no base-path variable, no change to
`ORIGO_PUBLIC_URL`, the root for a self-hoster.

### The audience list

`ORIGO_OIDC_AUDIENCE` becomes a comma-separated list. The first entry is the
primary; entries are trimmed, and an empty or repeated entry is a start-up
failure, the way `CELLA_OIDC_AUDIENCE` reads one. Unset stays
`auth.DefaultAudience`, `origo`. `Config.OIDCAudience` keeps its meaning as the
primary and gains `Config.OIDCAudiences`, the accepted set
(`internal/config/config.go:86`); `VerifierOptions.Audience` becomes `Audiences
[]string` and `Verifier.audience` a set. The check stays by hand at
`internal/auth/verifier.go:325` and is *not* moved into `jwt.Config.Audiences`:
the package checks `aud` after `exp`, spec 007's table checks it before, and
`internal/auth/verifier.go:143` says so in as many words. Handing the set to the
package would reorder two refusal reasons a client reads.

Latere's manifests name `origo,api.latere.ai` in both containers, set in
`deploy/prod` because the second name is Latere's address; the base keeps
`origo` (`deploy/base/deployment.yaml:83`, `:155`). The value carries no scheme:
ci-gate's audience rule refuses a value beginning with `http` or holding `,http`
(`ci-gate/internal/identity/deploy.go:66`), and an address is not an audience.
`.lateregate.yaml:125` keeps the scalar `audience: origo`, the primary, because
the gate's block reads one string (`ci-gate/internal/config/identity.go:78`) and
reading a list for the core role is ps-01's work there.
`TestEveryNodeNamesItsAudience` (`cmd/origod/manifests_test.go:230`) compares
the manifest value against
`auth.DefaultAudience` and fails on a pair; it becomes a check that the *first*
entry is that constant.

Rule R4 does not move. The signer mints with the primary
(`cmd/origod/node.go:280`), so a repository-bound token still carries `iss` =
`ORIGO_PUBLIC_URL` and `aud: ["origo"]` as `007:318` fixes, is verified against
the local key with no fetch, and is accepted by this node alone. A token naming
`act` is still refused (`internal/auth/verifier.go:361`). Specs 007 and 028 gain
a dated note, not a rewrite: 028's configuration row (`028:68`) and its "default
`origo`" sentences (`028:133`, `028:187`) say the variable is a list whose first
entry is the primary, and 007's `aud` row (`007:103`) reads "contains one of the
configured audiences".

### What the second host serves and says

`ORIGO_ANONYMOUS_READ` is on here (`deploy/prod/anonymous-read.yaml`). The route
set is a list, not a rule, and eight of its ten entries are `/v1/repos/{id}/...`
reads (`internal/auth/anonymous.go:46`), so those reads become anonymous at
`api.latere.ai` too. That is intended and not a widening: the authorizer still
decides each one, and the directory `GET /v1/repos` is withheld from the list, so
an unauthenticated directory request answers 401 at either host. The anonymous
bucket is keyed by the sentinel subject inside the node, so the hosts share one
`ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE` budget.

No `/v1/repos` response embeds an absolute URL built from `ORIGO_PUBLIC_URL`:
the `Repository` representation carries none (`internal/api/api.go:201`),
`archive` streams the tarball rather than redirecting, and the variable is read
only as the local issuer and the signer's `iss`. A body served at `api.latere.ai`
is byte for byte the body served at `code.latere.ai`. The absolute URLs Origo
does write are transport URLs and keep naming `code.latere.ai`: LFS hrefs are
presigned bucket URLs plus a verify href from the request's own host
(`internal/lfs/lfs.go:486`), and LFS is not routed at the origin.

### Cutover

Additive. `code.latere.ai/v1/repos` keeps serving until ps-04 retires the host's
API paths. The no-compatibility-windows decision governs contracts between
services and is not weakened: no contract has two shapes, one address gains a
second name, the distinction ps-04 already draws for a hostname. The Ingress
object and the audience list ship in one release, because the route without the
second audience serves 401 to every key-minted caller. origo-web needs no change,
since `ORIGOWEB_ORIGO_URL` is the in-cluster Service
(`origo-web/deploy/prod/settings.yaml:37`); latere-cli needs none, since it names
`code.latere.ai` only as a git credential host. platformd's console Repos section
is a new caller, with its own leaf in the platform deck (spec 67).

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `GET https://api.latere.ai/v1/repos` answers the directory for a person's actor token, `aud=origo`, and for a PAT-minted token, `aud=api.latere.ai` | two requests after the release, recorded in the Outcome |
| 2 | A token addressed to a third audience is 401 with the package's `audience` reason | `internal/auth/conformance_test.go` runs `conformance.Run` a second time with `Service{Audience: "api.latere.ai"}` over a verifier holding the full list; `RefusesOtherAudience` is that case |
| 3 | `ORIGO_OIDC_AUDIENCE` reads a list, first entry primary, and refuses an empty or repeated entry | `internal/config`, a table test in the shape of Cella's `TestConfiguredAudienceSet` |
| 4 | A repository-bound token still carries `aud: ["origo"]`, verifies locally, and `aud` is still weighed before `exp` | `internal/auth`, the signer test and the existing reason-order test, both with the list configured |
| 5 | The prod overlay claims exactly `/v1/repos` on `api.latere.ai`, with no `tls` block, no cert-manager annotation and no `use-regex`, and both containers name a list whose first entry is the core's own audience | a deploy test beside `cmd/origod/manifests_test.go` reading `deploy/prod/ingress-api.yaml`; `TestEveryNodeNamesItsAudience`, amended |
| 6 | A 64 MiB operation body succeeds at both hosts | one request per host after the release |
| 7 | No `/v1/repos` response body names `code.latere.ai`, and origo-web and latere-cli are unchanged | `internal/api`, a sweep of the representation and the read answers for the host string; `git grep` for `code.latere.ai/v1` over both trees |

## Dependencies

[[028-authorizer-contract-2]] for the configurable audience this widens,
[[027-anonymous-read]] for the route set the second host inherits,
[[026-repository-directory]] for the collection route criterion 1 calls, and
[[007-authentication-and-delegation]] for the verification table whose `aud` row
moves. Outside the tree, latere-ai/specs
`infrastructure/platform/ps-01-one-origin.md`, which this is Origo's half of.

## State on 2026-09-20

Built on main, green locally. Every criterion a local run can prove is
proved; the two that are requests at the origin wait for the release,
because the route does not exist until `deploy/prod` is applied.

### What was built

**The route.** `deploy/prod/ingress-api.yaml`, the object `origod-api`:
`ingressClassName: nginx`, host `api.latere.ai`, one rule, `/v1/repos` at
`pathType: Prefix`, backend `origod:http`. No `tls` block, no
`cert-manager.io/cluster-issuer`, no `use-regex`, no `rewrite-target`;
`proxy-body-size: "0"`, `proxy-read-timeout: "600"` and
`proxy-send-timeout: "600"` are the three annotations it carries. It is a
`resources` entry of `deploy/prod/kustomization.yaml`, not a patch, as
the Design says.

**The audience list.** `ORIGO_OIDC_AUDIENCE` is comma separated, entries
trimmed, an empty or repeated entry a start-up failure, unset still
`auth.DefaultAudience`. `Config.OIDCAudience` keeps its meaning as the
primary and `Config.OIDCAudiences` is the accepted set;
`VerifierOptions.Audience` became `Audiences []string` and
`Verifier.audience` a set read by `Verifier.addressed`. The check is
still by hand and still before `exp`. The signer still mints with the
primary (`cmd/origod/node.go`), so a repository-bound token carries
`aud: ["origo"]` unchanged.

**Latere's values.** `deploy/prod/audience.yaml` sets
`origo,api.latere.ai` on the node and on the check; `deploy/base`
keeps `origo`, so a self-hoster gets nothing new.
`.lateregate.yaml` is untouched and still names the scalar `origo`, the
primary. `docs/configuration.md` is regenerated from
`internal/config/document.go`, and `docs/install.md` says the variable
reads a list and what its first entry is. Specs 007 and 028 carry a
dated note each.

### What the tests prove

| Criterion | Test | State |
|---|---|---|
| 2 | `internal/auth`, `TestConformance`, the family's suite run once per configured audience over a verifier holding both; `RefusesOtherAudience` is the case | passing |
| 3 | `internal/config`, `TestConfiguredAudienceSet`, table-driven over the default, a list, a reordered list, a trimmed list, and the four malformed values | passing |
| 4 | `internal/auth`, `TestSignerMintsAndServesItsKey` (the minted token names the primary alone) and `TestVerifierAcceptsTwoIssuersAndRefusesEachFailure` (the reason order, now over a verifier holding the list) | passing |
| 5 | `cmd/origod`, `TestTheOriginIngressClaimsTheRepositoryPrefix` (the resource entry, one host, one path, `/v1/repos` at `Prefix` to `origod:http`, and the absence of `tls`, `secretName`, `cert-manager.io/`, `use-regex` and `rewrite-target`) and `TestEveryNodeNamesItsAudience`, amended to read the first entry of the list and extended to the prod patch | passing |
| 7, the half a tree can answer | no file of `internal/api` or `internal/lfs` names `code.latere.ai`; `git grep code.latere.ai/v1` finds nothing in origo outside this document and nothing in origo-web, and latere-cli names `code.latere.ai` only as a git credential host and in clone URLs | passing |

Each was run once against the tree without the change and once with it:
the ingress test fails on a `tls` block, a `secretName` and a
`use-regex` annotation; the audience test fails on a list whose primary
is the origin; `TestConformance/api.latere.ai` fails with `audience` on
a verifier holding one name; and `TestConfiguredAudienceSet` fails on
every malformed value with the entry check removed.

### What waits for the release

Criteria 1 and 6 are requests at `https://api.latere.ai`, and the host
answers 404 from nginx until this overlay is applied: the directory for a
person's actor token and for a PAT-minted token addressed to
`api.latere.ai`, and a 64 MiB operation body at both hosts. The object
and the audience list ship in one release, because the route without the
second audience serves 401 to every key-minted caller. Record both in the
Outcome, then the spec is `complete`.
