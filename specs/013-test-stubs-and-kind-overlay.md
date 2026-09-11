---
title: "Test stubs and the kind overlay"
status: complete
track: infra
depends_on:
  - specs/002-repository-scaffold.md
  - specs/007-authentication-and-delegation.md
affects: [test/stubs/sink/, test/stubs/origo/, test/stubs/source/, test/stubs/cmd/, Dockerfile.stubs, test/e2e/, test/e2e/cluster/, test/e2e/testdata/, deploy/examples/kind/, Makefile, .github/workflows/, .lateregate.yaml, internal/config/, tools/docs/]
effort: medium
created: 2026-09-06
updated: 2026-09-10
author: changkun
---

# Test stubs and the kind overlay

## Overview

Every spec after 007 needs the same three things beside the node to be
tested: an OIDC issuer that mints any token, an authorizer whose answer
a test chooses, and an event sink that records what it received. This
spec fixes the control API of every stub and builds the sink, the
contract stub that is the node's own code served in-process, the
source stub that serves a repository over TLS to an `import` or a
`verify`, the binary that runs the stubs, the `kind` overlay that runs
them and three nodes beside MinIO, the helper a cluster test drives
the cluster with, the test tiers and the targets that run them, and
the CI jobs that give the tiers a budget. Spec 007 builds the issuer
and the authorizer packages, because its own criteria need them, to
the table below; that is the whole split. It is built with spec 007
because the stubs are what replaces `ORIGO_DEV_TOKEN`. The conformance
suite that runs on this stack is spec 021.

## Current state

`test/e2e` (build tag `e2e`) runs a built `origod` against MinIO with
the real git: `TestPushThenCloneFromAnEmptyDisk`,
`TestConcurrentPushesToDifferentBranchesOnTwoNodes`, `TestKillMidPush`,
and `TestMeasure`. `internal/wal`'s `TestS3Suite` (build tag
`integration`) runs the store suite against the same MinIO. Both run
through `make test-integration` and not in CI: the shared
`lateregate.yml` has no services step and Origo's `verify.yml` runs
only the gate and the spec cross-reference test. `test/stubs/issuer`
and `test/stubs/authorizer` exist, built by spec 007 to the table
below; the rest of `test/stubs`, `deploy/examples/kind`, and
`Dockerfile.stubs` do not. The unit
suites cover every package at 90% or more on the in-process store and
the real git. Built on 2026-09-08 as the Design describes; the Outcome
records what diverged.

Three targets spec 002 assigned after it was complete are this spec's,
for the builder: `make fuzz`, which runs every fuzz function in the
module for 40 seconds (`go test -run=^$ -fuzz=<name> -fuzztime=40s`,
one package at a time, the list from `go test -list '^Fuzz'`) and the
`fuzz` job in `verify.yml` that calls it weekly from a `schedule`
trigger; `make test-tiers`, which runs the `integration` and `e2e`
tiers against the test bucket variables the environment carries
without starting compose; and the form of `make dev` that runs the
stub issuer and authorizer beside MinIO. Every fuzz function the deck
names (specs 004, 007, 009, 016, 020) runs under `make fuzz`.

`test/e2e/testdata/stub-ca.pem` is written by `up.sh` at run time, the
CA of the source stub's certificate for that cluster, and is ignored
by git: `.gitignore` gains the path with this spec, while the fault
manifests beside it are checked in.

One more item for the builder: the CI jobs below select tests by a
name prefix, so the three phase 1 scenarios are renamed
`TestE2EPushThenCloneFromAnEmptyDisk`,
`TestE2EConcurrentPushesToDifferentBranchesOnTwoNodes`, and
`TestE2EKillMidPush`, the names specs 001 and 004 now carry;
`TestMeasure` keeps its name because `ORIGO_E2E_MEASURE` selects it
and no job regex does.

And one gate, for the builder: spec 001's last criterion, that the
build list of `./cmd/origod` reaches no cloud SDK and no Kubernetes
client, is the `depcheck` gate of `latere.ai/x/ci-gate`, which this
spec configures in `.lateregate.yaml` as part of the test tooling:
`depcheck.packages` names `./cmd/origod` with an allow list of
`latere.ai/x/pkg` and the standard library. Spec 001 stays at
`testing` until the gate runs on every push.

## Design

### The stubs

`test/stubs/` holds five importable packages and one binary. Each
package has a `New(t testing.TB, ...) *Server` that starts an
`httptest.Server` and stops it with the test, a control API a test
drives, and no dependency beyond the standard library and
`latere.ai/x/pkg`. Spec 007 builds `test/stubs/issuer` and
`test/stubs/authorizer`; this spec builds `test/stubs/sink`,
`test/stubs/origo`, `test/stubs/source`, and the binary
`test/stubs/cmd/origo-stubs`, which runs the issuer, the authorizer,
the sink, and the source from flags for `make dev` and as pods in the
`kind` overlay; `Dockerfile.stubs` packages it as
`ghcr.io/latere-ai/origo-stubs`, built by `verify.yml` and loaded into
kind on every tag, and published per release beside `origod` with the
same tag, signed the same way (spec 017's artifact table), because
spec 018's `install-release` job and an operator's first installation
run the stub authorizer from it.

| Package | Serves | Control |
|---|---|---|
| `test/stubs/issuer` | an OIDC issuer: the discovery document at `/.well-known/openid-configuration` with `jwks_uri`, the public keys at `/jwks`, one ES256 key generated at start or read from `-key <pem>`; a POST to `/mint` with `{"sub", "act", "aud", "exp", "nbf", "iat", "kid", "alg"}` answers `{"token"}`, every claim optional with defaults that verify, so a test mints the token for each row of spec 007's table by setting one field wrong; a POST to `/rotate` adds a key and drops the oldest; a POST to `/hang` makes discovery and JWKS never answer, for spec 007's `issuer_unavailable` case, and a POST to `/resume` ends it; it serves plain HTTP, which is why the node lists it in `ORIGO_OIDC_INSECURE_ISSUERS` (spec 007) wherever its host is not a loopback address | `Mint(claims) string`, `Rotate()`, `Hang()`, `Resume()`, `URL()` |
| `test/stubs/authorizer` | spec 007's endpoint: a POST to its root with the bearer `-authorizer-token` answers from a rule table keyed by `(subject, actor, repo id or owner/slug, action)` with a default of allow for every subject in `-allow <subjects>` (`*` for all); the probe id `00000000-0000-0000-0000-000000000001` is always denied, the rule spec 007's authorizer contract states and spec 018's check relies on; `ttl`, `replicas`, and `quota_bytes` are per rule; a PUT to `/rules` with `{"rules": [{"subject", "actor", "repo", "action", "allow", "reason", "ttl", "replicas", "quota_bytes"}]}` replaces the table, `subject`, `actor`, `repo`, and `action` each `*` or a value, `repo` an id or `owner/slug`, `allow` a boolean and the rest optional; a GET of `/requests` lists every request seen in order, a DELETE of it clears the list; `-fail <status>` makes every answer that status and `-hang` makes it never answer, for the outage cases, and the same three states are set over HTTP so spec 021's stack run drives the outage through the host port: a PUT to `/fail` with `{"status": <int>}` (0 clears it), a POST to `/hang`, and a POST to `/resume`, three paths this spec adds to the package spec 007 built, each calling the method of the same name; spec 026's `list` action is answered from a directory of its own, set by a PUT to `/directory` with `{"supported": <bool>, "repos": [{"id", "owner", "slug"}]}` and unset by default, so an unconfigured stub answers `{"directory": false}` and the stack sees an installation with no directory, and a set directory is filtered through the same rule table a read is, for the requesting subject and actor, and paged at the request's `limit` | `Allow(rule)`, `Deny(rule, reason)`, `SetDirectory(supported, repos...)`, `Requests()`, `Fail(status)`, `Hang()`, `Resume()` |
| `test/stubs/sink` | spec 008's sink: a POST to its root verifies `Origo-Signature` with `-secret`, records the headers and body, and answers the configured status (200 by default; a PUT to `/status` with `{"status": <int>, "body": <json>, "count": <int>}` changes it, answering `status` with `body` for the next `count` deliveries, every one when `count` is 0, then 200 again); a GET of `/deliveries` with `repo` and `kind` parameters lists deliveries in order, a DELETE of it clears them | `Deliveries(repo, kind)`, `Fail(n, status)`, `Wait(repo, kind, n, timeout)` |
| `test/stubs/origo` | the contract stub a consumer's tests target: the real handlers of `internal/httpgit` and `internal/api`, and `internal/lfs` once spec 010 lands, wired through the S3 adapter `wal.NewS3` to an in-process `s3test.Server` of `latere.ai/x/pkg/s3/s3test` (one bucket behind an `httptest` listener that verifies every request and every presigned URL the way a provider does), a temporary cache directory, and an in-process issuer, authorizer, and sink from the packages above; the bucket is a real endpoint rather than `wal.MemStore` so the presigned transfers of spec 010 resolve and the LFS rows of spec 021 run against the stub with nothing skipped, and its `Fail` and the adapter's `Delete` are what spec 021's `Fault` uses on the stub; `New(t)` returns the base URL, a minting function, and the authorizer and sink handles; it speaks the whole contract because it is the node's own code, which spec 021 proves by running `TestContract` against it | `URL()`, `Token(sub, act)`, `Authorizer()`, `Sink()` |
| `test/stubs/sshkeys` | spec 024's key resolution endpoint: a POST to its root with the bearer `-sshkeys-token` answers `{"found": true, "subject", "key_id", "ttl"}` or `{"found": false}`, always 200, from a map keyed by the SHA-256 fingerprint; a PUT to `/keys` with `{"keys": [{"fingerprint", "subject", "key_id", "ttl"}]}` replaces the map and a DELETE of `/keys/{fingerprint}` revokes one key, which is how a test makes an authenticating key stop authenticating; a GET of `/requests` lists every request seen in order and a DELETE of it clears the list; `/fail`, `/hang`, and `/resume` are the authorizer stub's three outage paths, so a stack run drives a key store outage through the host port. It is the reference implementation of the contract and is explicitly not for production: it holds its table in memory, expires nothing on its own, and authenticates only the bearer | `Register(key)`, `Revoke(fingerprint)`, `SetKeys(keys)`, `Requests()`, `Fail(status)`, `Hang()`, `Resume()` |
| `test/stubs/source` | a git source for `import` (spec 019) and `verify` (spec 014): `git http-backend` behind a TLS listener that requires the bearer `-source-token`, serving the fixture repository `/fixture.git` unpacked at start from a bundle the package embeds (`testdata/fixture.bundle`, 5 000 commits, produced by `go generate` in the package from `internal/gittest` and checked in), and any repository a test adds; the certificate is signed by a CA the binary generates at start or reads from `-ca <pem>` and `-ca-key <pem>`, and a GET of `/ca.pem` serves it; a POST to `/commit` with `{"repo", "branch"}` adds one commit on that branch, which is the "late write" of spec 014's cut-over test; a POST to `/repos` with `{"name", "bundle"}` (base64) adds a repository; a GET of `/requests` lists every request seen with its path and whether it carried the bearer, never the bearer itself, and a DELETE of it clears the list | `URL()`, `CA() []byte`, `Commit(repo, branch)`, `AddRepo(name, bundle)`, `Requests()` |

A consumer imports `test/stubs/origo` to run its integration tests
against Origo in-process. The slow proxy of spec 015
(`test/stubs/slowproxy`) lives beside these and is that spec's.

The binary `test/stubs/cmd/origo-stubs` runs the stubs from these
flags. Each listen flag defaults to the stub's fixed port in the stack
on every interface, so the overlay's Deployment passes no listen
address, and each stub has its own flags, so no two stubs share a
name (a bare `-token` would be the authorizer's and the source's at
once):

| Flag | Default | Meaning |
|---|---|---|
| `-issuer-listen` | `0.0.0.0:8081` | the issuer's listen address |
| `-authorizer-listen` | `0.0.0.0:8082` | the authorizer's listen address |
| `-sink-listen` | `0.0.0.0:8083` | the sink's listen address |
| `-source-listen` | `0.0.0.0:8443` | the source's TLS listen address; the source starts only when `-source-token` is set, because a source without a bearer would serve to anyone, and the `test-source` component is what sets it, so the overlay alone runs no source |
| `-slowproxy-listen` | `0.0.0.0:8085` | the slow proxy's control endpoint, host port 30085 of the ports table; the proxy is spec 015's and runs from this binary |
| `-slowproxy-target <host:port>` | none | MinIO's Service, what the slow proxy forwards to; the proxy starts only when `-slowproxy-target` is set, so the overlay's `origo-stubs` Deployment runs no proxy |
| `-slowproxy-data` | `0.0.0.0:8086` | the in-cluster data listener of the slow proxy, what `ORIGO_S3_ENDPOINT` on every node names when the `slowproxy` row is in the overlay |
| `-sshkeys-listen` | `0.0.0.0:8087` | the key resolver's listen address, host port 30086 of the ports table; the resolver is spec 024's and runs from this binary, always, because a key store that answers `{"found": false}` for every fingerprint is what a stack without SSH configured wants |
| `-sshkeys-token` | `stub-sshkeys-token` | the bearer the key resolver requires, the value of `ORIGO_SSH_KEYS_TOKEN` on every node of the stack |
| `-issuer-url` | the issuer's listen address as `http://<address>` | the value the issuer writes as `iss` in every token and as `issuer` in its discovery document; in the stack it is `http://origo-stubs.origo.svc:8081`, the value the nodes carry in `ORIGO_OIDC_ISSUERS`, while a test mints through the host port `localhost:30081`, which is why the URL is a flag of its own and not the listen address; the issuer package spec 007 builds takes it through its `WithIssuer` option |
| `-authorizer-token` | none | the bearer the authorizer requires; `stub-authorizer-token` in the stack |
| `-source-token` | none | the bearer the source requires; `stub-source-token` in the stack, from the component |
| `-allow`, `-secret`, `-key`, `-fail`, `-hang`, `-ca`, `-ca-key` | as the table above | the per-stub flags the table above names |

`make dev` runs the same binary on the loopback interface at ports
derived from the Makefile's `DEV_PORT_BASE` (spec 002's local stack:
`20000 + (cksum of DEV_PROJECT mod 300) * 10`), whose scheme gives the
checkout's node and compose stack `DEV_PORT_BASE` to `DEV_PORT_BASE + 3`, so
the stubs take the next four: `-issuer-listen 127.0.0.1:<base + 4>`,
`-authorizer-listen 127.0.0.1:<base + 5>`, `-sink-listen
127.0.0.1:<base + 6>`, `-issuer-url http://localhost:<base + 4>`, and
`-authorizer-token stub-authorizer-token`, no `-source-token`, so no
source; the Makefile names them `DEV_ISSUER_PORT`,
`DEV_AUTHORIZER_PORT`, and `DEV_SINK_PORT`, and the node gets
`ORIGO_OIDC_ISSUERS` and `ORIGO_OIDC_INSECURE_ISSUERS` set to the
issuer URL and `ORIGO_AUTHORIZER_URL` to `http://127.0.0.1:<base +
5>`, so two checkouts run side by side as they do for MinIO.

### Local stack

`make dev` (spec 002) builds the binary, starts MinIO from
`docker-compose.yml`, and from this spec on also runs
`test/stubs/cmd/origo-stubs` beside it, points `ORIGO_OIDC_ISSUERS`,
`ORIGO_OIDC_INSECURE_ISSUERS`, and `ORIGO_AUTHORIZER_URL` at the stubs,
and prints a clone line with a token the stub issuer minted.
`make dev-up`, added by this spec, runs `deploy/examples/kind/up.sh`
(The stack, below) for the cluster `origo-<DEV_PROJECT>`, so a
developer has the three-node stack the cluster tiers target. `make
dev-down`, added by this spec, stops what `make dev` or `make dev-up`
started for the checkout's `DEV_PROJECT`: the compose stack and the
stubs, and the kind cluster through `down.sh`; each half is a no-op
when nothing of it runs, and `out/` stays in place; `make clean` (spec
002) removes `out/` as well.
`make test-tiers` runs the `integration` and `e2e` tiers against
whatever values of the test bucket variables the environment carries,
which is what CI calls with its service container; `make
test-integration` is the developer's form that starts compose and
calls the same target.

### Documents as tests

`tools/docs/run-blocks.sh <document>`, owned by this spec as test
tooling, extracts the fenced `sh` blocks of one Markdown document and
runs them in order in one shell with `set -e`, with `ORIGO_TEST_URL`
and `ORIGO_TEST_ADMIN_TOKEN` in the environment as the stack's values,
so a document is the test of its own commands and a step that drifts
from the tree fails. Spec 014 runs `docs/migration.md` through it and
spec 018 runs `docs/install.md`.

### The stack

`deploy/examples/kind`, which spec 018 lists as one of its three
example overlays, applies MinIO, three `origod` pods from the
candidate image, and `origo-stubs` running the issuer, the authorizer,
the sink, and the source, to a kind cluster created from `kind.yaml`
in this directory. The directory carries two scripts. `up.sh [-name
<name>] [-port-offset <n>] <origod.tar> <origo-stubs.tar>` takes the
two image tarballs, `docker save` of the candidate `origod` and
`origo-stubs` images, as its arguments: it creates the cluster from
`kind.yaml` (name `origo` by default, `origo-<name>` with `-name`;
`-port-offset <n>` adds `n` to every host port of the ports table, so
a second cluster runs beside the first: the checked-in `kind.yaml` is
the configuration at offset 0, which spec 018's `install` job uses as
it is, and `up.sh` renders `out/kind/<name>/kind.yaml` from it with
one `awk` expression that adds the shell variable `PORT_OFFSET`, set
from `-port-offset` and 0 by default, to the value of every `hostPort`
line, the one place the offset is applied), loads the two tarballs
into it with `kind load image-archive` after the cluster exists and
before anything is applied, installs Cilium and `metrics-server` at
the versions `deploy/examples/kind/versions.env` pins (the Cilium and
`metrics-server` rows below), generates the two
Secrets below (`origod-token-key` holding `ORIGO_TOKEN_KEY`, the one
Secret the overlay does not carry, and the source stub's CA) and the
ConfigMap that carries the CA certificate, applies the overlay together
with the `test-source` component (the `origo-stubs` row) with `kubectl
apply -k` of a kustomization it writes under `out/kind/<name>/` naming
both, and waits until every pod of the tables below is ready and every
host port of the ports table answers, at most 5 minutes; `down.sh
[-name <name>]` deletes the cluster. The `e2e`, `e2e-slow`, and
`up-script` jobs and `make dev-up` call `up.sh`, the jobs with the
tarballs of the `candidate-images` artifact and `make dev-up` with
tarballs it saves from a local build; `make dev-down` calls `down.sh`.
Spec 018's install jobs apply the overlay without the component and
without the script, generating `ORIGO_TOKEN_KEY` through the install
document's own block, the way an operator does. The stack has no ingress
controller: every address the runner reaches is a host port
`kind.yaml` maps to a NodePort, listed in the ports table below. Every
capability the stack has beyond a plain cluster is one row here with
the spec that needs it, so a later spec that needs another adds a row
rather than a step in a job:

| Row | Provides | Needed by |
|---|---|---|
| MinIO | one in-cluster bucket, path style, on the host port of the ports table so the runner reaches it too, with fixed values: bucket `origo-test`, key `minioadmin`, secret `minioadmin`, region `us-east-1`, path style on, endpoint `http://localhost:30900` from the runner; `ORIGO_S3_PUBLIC_ENDPOINT=http://localhost:30900` on every node, which is what a presigned URL and the harness use from outside the cluster; the `e2e` and `e2e-slow` jobs export those values as `ORIGO_TEST_S3_ENDPOINT`, `ORIGO_TEST_S3_REGION`, `ORIGO_TEST_S3_BUCKET`, `ORIGO_TEST_S3_KEY`, `ORIGO_TEST_S3_SECRET`, and `ORIGO_TEST_S3_PATH_STYLE` (spec 002), which is how a test that starts its own node in a cluster job, spec 017's fixture harness, and the `Fault` of spec 021 reach the bucket | every spec; 008 and 021 for a node or a fault of their own, 010 for LFS transfers from the runner, 017 for the fixture the harness extracts |
| three `origod` pods | the StatefulSet `origod` with 3 replicas, `origod-0`, `origod-1`, `origod-2`, replacing the base's Deployment with the same labels so the Service, the PodDisruptionBudget, the NetworkPolicy, and the HorizontalPodAutoscaler (its `scaleTargetRef` patched to the StatefulSet, spec 005) apply unchanged; the candidate image with `ORIGO_NODE_NAME` the pod name, `ORIGO_GOSSIP_PEERS` on the headless Service, `ORIGO_GOSSIP_SECRET` a fixed value (spec 005), `ORIGO_OIDC_ISSUERS=http://origo-stubs.origo.svc:8081` and `ORIGO_OIDC_INSECURE_ISSUERS` naming the same URL (spec 007), `ORIGO_AUTHORIZER_URL=http://origo-stubs.origo.svc:8082` with `ORIGO_AUTHORIZER_TOKEN=stub-authorizer-token` (spec 007), `ORIGO_EVENTS_URL=http://origo-stubs.origo.svc:8083` with `ORIGO_EVENTS_SECRET=stub-sink-secret` (spec 008), `ORIGO_STALE_MAX=30s` so spec 015's cluster scenario waits 30 seconds and not 5 minutes for stale serving to end, `ORIGO_PUBLIC_URL=http://localhost:30080` on all three pods, because the configuration requires it (spec 002) and a repository-bound token carries it as `iss` (spec 007), so a token minted through one node verifies on another only when the value is the same on every pod, and once spec 018 moves the Latere values out of the base this overlay is the only place the value is set, and `ORIGO_TOKEN_KEY` from the Secret `origod-token-key`, which `up.sh` generates with `openssl ecparam -genkey -name prime256v1` and the install document's block of spec 018 generates the same way (spec 007); `ORIGO_SSH_ADDR=:2222` with `ORIGO_SSH_HOST_KEYS` naming the two keys mounted from the Secret `origod-ssh-host-key`, which `up.sh` generates with `ssh-keygen` once for the whole stack so every pod presents the same key, and `ORIGO_SSH_KEYS_URL=http://origo-stubs.origo.svc:8087` with `ORIGO_SSH_KEYS_TOKEN=stub-sshkeys-token` (spec 024); from the `test-source` component, as a patch on the StatefulSet: `ORIGO_EGRESS_ALLOW=origo-stubs.origo.svc=10.96.0.42`, the host pinned to the stubs' Service `clusterIP` as spec 016 requires for a cluster address, `ORIGO_CLUSTER_CIDRS` naming kind's service and pod ranges, and `ORIGO_EGRESS_CA_BUNDLE=/etc/origo/stub-ca.pem`, the CA the source stub's certificate is signed by, mounted from the ConfigMap `origo-stub-ca` (spec 016), so a stack applied without the component carries none of the three and refuses every source; one balanced Service for the public listener and one for SSH, and, per pod, one public, one internal, and one SSH Service selecting on `statefulset.kubernetes.io/pod-name`, each on its host port of the ports table, so a test can push through one node, read that node's `/metrics`, clone through another, and read one named node's SSH host key | every spec; 005, 006, 015 for per-node addresses |
| `origo-stubs` | one Deployment `origo-stubs` running the binary with the issuer (port 8081), the authorizer with `-allow *` and `-authorizer-token stub-authorizer-token` (8082), and the sink with `-secret stub-sink-secret` (8083), behind the one Service `origo-stubs` whose manifest fixes `clusterIP: 10.96.0.42` inside kind's default service range `10.96.0.0/16`, so the nodes' pinned `ORIGO_EGRESS_ALLOW` entry names an address that never changes, each control endpoint on its host port of the ports table so `TestContract` drives them from the runner (spec 021); the values are the ones the pods row sets on the nodes. The source is the kustomize component `deploy/examples/kind/test-source/`, which `up.sh` includes and spec 018's install jobs omit: it patches the Deployment to also run the source with `-source-token stub-source-token` and `-ca` and `-ca-key` from the mount of the Secret `origo-stubs-ca`, adds port 8443 to the Service, so the source serves TLS in-cluster at `https://origo-stubs.origo.svc:8443`, and patches the nodes with the three egress variables and the CA mount the pods row lists; the CA certificate and key are generated by `up.sh` with `openssl` into `origo-stubs-ca`, the certificate carrying the SANs `origo-stubs.origo.svc` and `localhost` so the same certificate verifies in-cluster and through the host port; the certificate alone is copied into the ConfigMap `origo-stub-ca` the nodes mount for `ORIGO_EGRESS_CA_BUNDLE`, and written to `test/e2e/testdata/stub-ca.pem` on the runner (ignored by git), so a test on the runner trusts the host port through the file. An installation from the overlay alone runs the issuer, the authorizer, and the sink and no source | 007, 008, 021; 014 and 019 for an in-cluster HTTPS source through the component |
| `slowproxy` | `test/stubs/slowproxy` as a pod in front of MinIO, the `origo-stubs` binary run with `-slowproxy-target` naming MinIO's Service, `ORIGO_S3_ENDPOINT` on every node pointing at its Service on the data port 8086, delay 0 until its control endpoint, on its host port of the ports table, sets one; the proxy and this row are added by spec 015, which owns the package, so until it lands `ORIGO_S3_ENDPOINT` names MinIO's Service and the ports table's 30085 answers nothing | 015 for the slow-bucket case |
| `metrics-server` | the resource metrics API a CPU-target HorizontalPodAutoscaler reads: `kubectl apply` of the release manifest `https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml`, downloaded and checked with `sha256sum -c` against the digest `versions.env` records beside the version, with the image reference rewritten to its digest form from the same file and `--kubelet-insecure-tls` added to the container arguments, the flag that accepts kind's kubelet certificates | 005 |
| Cilium | the CNI, installed in place of kindnet (`disableDefaultCNI` in `kind.yaml`), so NetworkPolicy is enforced: the Helm chart `cilium/cilium` from `https://helm.cilium.io` at version `1.18.0`, `helm install cilium` into `kube-system` with `ipam.mode=kubernetes` (kind allocates the pod CIDRs) and the chart's default `image.useDigest=true`, which pins every Cilium image by digest; the chart version is the one `versions.env` records, and spec 018's `install` job installs Cilium from the same file | 015 for the unreachable-bucket policy, 016 for the gossip policy |
| `versions.env` | `deploy/examples/kind/versions.env`, the one file that pins what the stack installs beyond the overlay: the Cilium chart version, the `metrics-server` version, the sha256 of its manifest, and the digest of its image; `up.sh` and spec 018's `install` job source it, so a bump is a one-line change the `up-script` and `install` jobs prove | 005, 015, 016, 018 |
| Pod Security admission | the label `pod-security.kubernetes.io/enforce=restricted` on the `origo` namespace | 016 |
| HPA scale-down window | a patch setting the stabilization window to 60 seconds, which spec 005 adds to this overlay | 005 |

#### Ports

`kind.yaml` maps each host port to the NodePort of the Service named,
so every address a test or a document uses is fixed and the jobs set
neither `ORIGO_TEST_URL` nor `ORIGO_TEST_ADMIN_TOKEN`. A criterion of
another spec names a node by its row here, "node 1 of the ports
table", and never by a pod name a Kubernetes version might change:

| Host port | Service | Reached as | Used by |
|---|---|---|---|
| 30080 | `origod` public listener, all three pods behind one balanced Service | `http://localhost:30080`, the default of `ORIGO_TEST_URL` | every `TestCluster` and `TestSlow` test that does not care which node answers, `TestContract` (021), the documents of 014 and 018 |
| 30180 | node 1, the pod `origod-0`, public listener | `http://localhost:30180` | a test that pushes or clones through one named node (005, 006, 015) |
| 30181 | node 2, the pod `origod-1`, public listener | `http://localhost:30181` | same |
| 30182 | node 3, the pod `origod-2`, public listener | `http://localhost:30182` | same |
| 30022 | `origod-ssh`, all three pods behind one balanced Service | `ssh://git@127.0.0.1:30022/` | a test that pushes or clones over SSH without caring which node answers (024) |
| 30122 | node 1, the pod `origod-0`, SSH listener | `ssh://git@127.0.0.1:30122/` | a test that reads one named node's host key (024) |
| 30123 | node 2, the pod `origod-1`, SSH listener | `ssh://git@127.0.0.1:30123/` | same |
| 30124 | node 3, the pod `origod-2`, SSH listener | `ssh://git@127.0.0.1:30124/` | same |
| 30190 | node 1, internal listener, `GET /metrics` | `http://localhost:30190/metrics` | a test that reads one node's counters (005, 006, 015) |
| 30191 | node 2, internal listener | `http://localhost:30191/metrics` | same |
| 30192 | node 3, internal listener | `http://localhost:30192/metrics` | same |
| 30081 | the stub issuer, its discovery, JWKS, and control endpoints | `http://localhost:30081` | the harness minting tokens, `TestContract` (021) |
| 30082 | the stub authorizer, its endpoint and control endpoints | `http://localhost:30082` | `TestContract` (021) flipping allow and deny |
| 30083 | the stub sink, its endpoint and control endpoints | `http://localhost:30083` | `TestContract` (021) reading deliveries, 008's repair case |
| 30084 | the stub source, its git endpoint over TLS and its control endpoints | `https://localhost:30084`, trusted through `test/e2e/testdata/stub-ca.pem` | 014's and 019's cluster tests adding a commit or reading the request list; inside the cluster the nodes reach it as `https://origo-stubs.origo.svc:8443` |
| 30085 | the slow proxy's control endpoint | `http://localhost:30085` | 015's slow-bucket case |
| 30086 | the stub key resolver, its endpoint and control endpoints | `http://localhost:30086` | 024's cluster tests registering and revoking a public key |
| 30900 | MinIO | `http://localhost:30900`, the value of `ORIGO_S3_PUBLIC_ENDPOINT` on every node | LFS transfers from the runner (010), the fixture extraction of 017, the `Fault` of 021, a test that starts a node of its own in a cluster job (008, 021) through `ORIGO_TEST_S3_ENDPOINT` and its sibling variables |

The SSH rows name the loopback address where every other row names
`localhost`. `kind` publishes a host port on IPv4, and `ssh` and
`ssh-keyscan` ask the first address a name resolves to and do not try
the next one, where `curl` and Go's HTTP client walk the whole list. On
a machine that answers `localhost` with `::1` first, every HTTP row of
this table is reachable by name and no SSH row is.

Inside the cluster the nodes reach the stubs and MinIO by their Service
names; the host ports are for the runner. The fixed dev subject of the
stack is `dev`, allowed by the stub authorizer's `-allow *`:
`ORIGO_TEST_ADMIN_TOKEN` unset means a test mints a token for it at
the issuer's host port, valid for the run, so no fixed token value is
checked in (spec 007 refuses a token whose `iat` is older than 24
hours, which rules a fixed one out). Spec 021 references this table
for its defaults.

#### Driving the cluster

A cluster test that changes the cluster, rather than only talking to
it, does so through `test/e2e/cluster`, a helper package of the `e2e`
tier that shells out to `kubectl` from `PATH` under the kubeconfig
`kind` wrote for the job (`KUBECONFIG`, or the default path), in the
namespace `origo`; no test runs `kubectl` on its own and no test
imports a Kubernetes client, which keeps spec 001's dependency rule.
Every function takes the `testing.TB`, fails the test on a non-zero
exit with `kubectl`'s stderr, and is named by the criterion that uses
it:

| Function | Runs | Used by |
|---|---|---|
| `DeletePod(t, name)` | `kubectl delete pod <name>` and waits until the StatefulSet's replacement is ready | 005's node removal |
| `ApplyManifest(t, path)` | `kubectl apply -f <path>` of a manifest under `test/e2e/testdata/`, and registers `kubectl delete -f <path>` to run when the test ends, so a fault lasts exactly one test | 015's and 021's unreachable bucket (`cut-storage.yaml`, a NetworkPolicy denying the nodes egress to MinIO), 005's replica counts (`hpa-2.yaml`, `hpa-4.yaml`, `hpa-8.yaml`, each the HorizontalPodAutoscaler named `origod`, replacing the overlay's; this spec writes `hpa-2.yaml`, the fixture of its own helper test below, and spec 005 writes `hpa-4.yaml` and `hpa-8.yaml`) |
| `ApplyManifestExpectRefusal(t, path)` | `kubectl apply -f <path>` of a manifest under `test/e2e/testdata/` that admission must refuse: fails the test when the apply succeeds, and returns `kubectl`'s stderr, the refusal text, for the test to assert on; registers no cleanup because nothing was created | 016's refused pod (`privileged-pod.yaml`) |
| `HPAStatus(t, name)` | `kubectl get hpa <name> -o json` and returns the current and desired replica counts | 005's autoscaler and replica cases |
| `Apply(t, overlay)` | `kubectl apply -k <overlay>` and waits for the rollout, which restores the stack after a test changed it | 005 after its replica cases, any test that applied a manifest the cleanup of `ApplyManifest` cannot undo |
| `Get(t, kind, name)` | `kubectl get <kind> <name> -o json` in the namespace and returns the bytes, for a test that asserts on an object the overlay applied rather than on the stack's behaviour | 016's pod security context and its gossip NetworkPolicy `origod-gossip` |

`test/e2e/testdata/` holds every fault manifest, one file per fault,
and the CA file `up.sh` writes; a cluster criterion of another spec
names the function and the manifest it uses.

`test/e2e` gains the scenarios that need a cluster: node kill
mid-push, cache pressure, compaction under load, the degraded-storage
cases of spec 015, the autoscaler and node-removal cases of spec 005,
and the event repair case of spec 008. From spec 021 on the
conformance suite runs on the same stack.

### CI

`verify.yml` gains the jobs of the table beside the gate:

| Job | Runs | Budget |
|---|---|---|
| `integration` | MinIO as a service container, then `make test-tiers`: the `integration` tier and the `e2e` tier's one-node run, `-run 'TestE2E'`, which is every end-to-end test that needs no cluster, starts its own nodes against the bucket, and fits the budget: the tests that push 500 or 1 000 times, import 5 000 commits, or move 500 MiB are in the two cluster jobs below | 25 minutes |
| `build` | builds the two images `origod` and `origo-stubs` once from the push's tree and uploads them as `docker save` tarballs in one `actions/upload-artifact` named `candidate-images`; the `e2e`, `e2e-slow`, and `up-script` jobs below and spec 018's `install` job each `needs` it and download the artifact with `actions/download-artifact`, so no job builds an image of its own and every job of the push tests the same bytes | 15 minutes |
| `e2e` | downloads the `candidate-images` artifact, then `deploy/examples/kind/up.sh` with the two tarballs, which creates the cluster, loads them, and applies the overlay with the `test-source` component, exports the MinIO values of the overlay table as the `ORIGO_TEST_S3_ENDPOINT` family, then runs the `e2e` tier's cluster scenarios, `-run 'TestCluster' -skip 'TestClusterUpScript'`, against the three-node overlay with `kubectl` on `PATH` for `test/e2e/cluster`, among them the 500 and 1 000 push tests of spec 006 (`TestClusterFiveHundredPushesStayUnder64EntriesAnd6Packs`, `TestClusterCompactionKeepsFetchLatencyFlat`), spec 019's `TestClusterGcBoundsStorage` and `TestClusterImportFixture` (the 5 000-commit import from the in-cluster source stub, `https://origo-stubs.origo.svc:8443/fixture.git`, into the stack), and spec 014's `TestClusterMigrationCatchesALateWrite`; from spec 021 on, `TestContract` and `TestSameAnswersOnStubAndStack` of `test/conformance` as a second step against the same stack, and from spec 017 on `TestPreviousReleaseFixture` with `ORIGO_PREVIOUS_RELEASE_FIXTURE` set to the fixture archive the job downloaded from the latest release with `gh release download`, unset when no release exists | 30 minutes |
| `e2e-slow` | the same set-up from the same artifact, with `git-lfs` installed on the runner for spec 010, then `-run 'TestSlow'`: spec 005's `TestSlowAutoscalerScalesUp` and `TestSlowReplicasScaleReads`, spec 008's `TestSlowEventRepairAfterKill`, spec 004's `TestSlowMaterializeTenThousandEntries`, and spec 010's `TestSlowLFSRoundTripBypassesTheNode` (500 MiB through MinIO's host port), which each wait on a timer or a fixture the others do not | 30 minutes |
| `up-script` | downloads the `candidate-images` artifact, then `-run 'TestClusterUpScript'` and nothing else: this spec's `TestClusterUpScript`, which runs `up.sh -name up-test -port-offset 1000` with the two tarballs, creating a cluster `origo-up-test` of its own whose host ports are the ports table's plus 1000, checks it, and runs `down.sh -name up-test` whatever happened; a job of its own, on no stack, because a second cluster's creation and the script's 5 minute wait do not fit beside the `e2e` job's scenarios | 15 minutes |
| `mutation` | spec 021's job: MinIO as a service container, like `integration`, the `ORIGO_TEST_S3_ENDPOINT` family exported, and `-run 'TestMutation'` once per capability of spec 021's set with `ORIGO_TEST_DROP_CAPABILITY` set to it; `TestMutation` of `test/e2e` starts one node of its own carrying the variable, the way spec 008's repair case starts nodes, runs `conformance.Run` against it, and expects the run to fail on exactly the dropped capability; spec 021 says what it asserts, this table gives it its budget | 20 minutes |
| `fuzz` | `make fuzz` on a weekly `schedule` trigger | 60 minutes |

Every test of the tier carries the build tag `e2e`; which job runs it
is its name prefix, given to `go test -run` by the job: `TestE2E` for
the one-node run, `TestCluster` for the cluster scenarios (with
`-skip 'TestClusterUpScript'`, the one test of that prefix the
`up-script` job runs alone by its full name), `TestSlow`
for the slow ones, `TestMutation` for the mutation job, and
`TestMeasure` for the measurement run no job
selects. A one-node test starts `origod` itself from the bucket
variables of spec 002; a cluster or slow test targets the stack the job
applied through `ORIGO_TEST_URL` and `ORIGO_TEST_ADMIN_TOKEN` (spec
002), whose defaults are the ports table's: `http://localhost:30080`,
and a token minted at the issuer's host port for a subject naming the test, one bucket of spec 012's rate limit per scenario, when
the token is unset, so the jobs set neither. A test that starts a node
of its own (008's repair case beside the stack, 021's `TestMutation`
with no stack) reads the bucket from the
`ORIGO_TEST_S3_ENDPOINT` family the job exported, and one that changes
the cluster does so through `test/e2e/cluster`. The two cluster jobs
run in parallel on every push
to `main` and every pull request. A job over its budget fails the push;
a scenario that needs more time moves to `e2e-slow`, and one that makes
`e2e-slow` exceed its budget is a spec change, not a budget change. The
shared
pipeline in `latere-ai/ci` gains an optional `services` input so any
service with a storage tier can run the `integration` job the same
way; until that input exists the jobs are plain jobs in Origo's own
`verify.yml`.

### Variables of the test tiers

Defined in spec 002's table with every other variable; what the tiers
do with them:

| Test variable | Purpose |
|---|---|
| `ORIGO_TEST_S3_ENDPOINT`, `ORIGO_TEST_S3_REGION`, `ORIGO_TEST_S3_BUCKET`, `ORIGO_TEST_S3_KEY`, `ORIGO_TEST_S3_SECRET`, `ORIGO_TEST_S3_PATH_STYLE` | the bucket the `integration` and `e2e` tiers use; the tiers skip when the endpoint is unset; in the two cluster jobs the values are the overlay's MinIO row (`http://localhost:30900`, `us-east-1`, `origo-test`, `minioadmin`, `minioadmin`, `1`), exported by the job, and what a test that starts its own node, spec 017's fixture harness, and spec 021's `Fault` read |
| `ORIGO_E2E_MEASURE` | `1` runs `TestMeasure`, which prints the measurements spec 004's Outcome records and asserts no threshold; the one check it carries is spec 005's monotonicity of clones per second over 2, 4, and 8 replicas, which no job runs; every threshold a spec names is a plain test of the `e2e` tier that runs without it |
| `ORIGO_TEST_URL`, `ORIGO_TEST_ADMIN_TOKEN` | the stack a `TestCluster` or `TestSlow` test targets and the token it creates repositories with; the URL defaults to `http://localhost:30080` and the token to one minted at the issuer's host port for a subject naming the test (the ports table), so each scenario has a bucket of spec 012's rate limit to itself; a cluster test skips when nothing answers at the URL, so the tier runs on a developer's machine without the stack |

## Not in this spec

The conformance suite, the code table, what the mutation job asserts,
and the run against the live service (spec 021). The issuer and the
authorizer packages (spec 007, to the table above). The slow proxy
(spec 015). A stub of a provider other than Origo.

## Acceptance criteria

- The stub issuer mints a token spec 007's verifier accepts and one
  refused token per row of its verification table, and stops answering
  on `Hang`; the stub authorizer answers its rule table, denies the
  probe id, and records requests in order (both built by spec 007 to
  the table above: `test/stubs/issuer`, `TestMintsEachFailure`;
  `test/stubs/authorizer`, `TestRulesAndProbe`); the stub sink refuses
  a bad signature and answers the configured failures then recovers
  (proposed: `test/stubs/sink`, `TestSignatureAndFailures`).
- The stub source serves `/fixture.git` over TLS to `git clone` with
  the bearer and refuses a clone without it, `Commit` adds one commit
  the next `ls-remote` shows, `Requests` lists the clone without the
  bearer's value, and the embedded bundle unpacks to 5 000 commits
  (proposed: `test/stubs/source`, `TestSourceServesTheFixtureOverTLS`).
- The contract stub serves a clone, a push, and the lifecycle table of
  spec 003 to the real git and `net/http` in-process (proposed:
  `test/stubs/origo`, `TestStubServesTheContract`; spec 021's
  `TestStubConforms` is the full proof).
- `make dev` prints a clone line whose token the node accepts, and
  `make test-tiers` with the test bucket variables set runs both tiers
  without compose (proposed: `Makefile`, exercised by the `integration`
  job; `test/e2e`, `TestE2EDevStackClones`, which starts `make dev`
  in the background from the test with a distinct `DEV_PROJECT`, waits
  at most 2 minutes for the clone line on its output, clones with it,
  and tears the stack down with `make dev-down` for that project
  whatever happened, never with `make clean`, which would remove the
  `out/` of the checkout under test).
- `kustomize build deploy/examples/kind` succeeds and `up.sh` reaches
  the three ready pods `origod-0`, `origod-1`, `origod-2`, the stubs,
  `metrics-server`, and Cilium within 5 minutes (and the slow proxy
  once spec 015 adds its row), with
  the namespace carrying the restricted label, every host port of the
  ports table answering (each per-node public port answering `GET
  /version`, each internal port `GET /metrics`), the nodes carrying the
  variables of the overlay table (`ORIGO_STALE_MAX=30s` among them),
  the Secret `origo-stubs-ca` holding a certificate with both SANs,
  Cilium and `metrics-server` running at the versions `versions.env`
  pins, and `test/e2e/testdata/stub-ca.pem` written; `down.sh` then removes
  the cluster (proposed: `test/e2e`, `TestClusterUpScript`, in the
  `up-script` job under its 15 minute budget: it runs `up.sh -name
  up-test -port-offset 1000` with the two tarballs of the
  `candidate-images` artifact, which creates the cluster
  `origo-up-test` with every host port of the ports table offset by
  1000, checks the list above against that cluster with its own
  kubeconfig and the offset ports, and runs `down.sh -name up-test`
  whatever happened, skipped when `kind` is not on `PATH`; the `e2e`
  and `e2e-slow` jobs' own set-up step is the same script on the
  cluster their tests use).
- `test/e2e/cluster` runs each of its six functions against the
  stack: `DeletePod` of node 2 returns once the replacement pod is
  ready, `ApplyManifest` of `test/e2e/testdata/cut-storage.yaml` makes
  a node answer 503 `storage_unavailable` and its cleanup restores the
  answer, `ApplyManifestExpectRefusal` of `privileged-pod.yaml`
  returns the refusal text and leaves no pod behind, `HPAStatus`
  reports the counts `kubectl` shows, `Get` of the StatefulSet
  `origod` returns JSON naming three replicas, and `Apply` of the overlay
  restores the replica count after an `hpa-2.yaml`
  (proposed: `test/e2e`, `TestClusterHelperDrivesKubectl`).
- The `build`, `integration`, `e2e`, `e2e-slow`, `up-script`, and
  `mutation` jobs exist in `verify.yml` with the budgets of the table,
  the two cluster jobs export the six `ORIGO_TEST_S3_ENDPOINT` family
  values of the overlay table, `e2e-slow` installs `git-lfs`, the
  `build` job is the only job with a `docker build` step and uploads
  the `candidate-images` artifact, the `e2e`, `e2e-slow`, and
  `up-script` jobs each `needs` it and download it, and each of the
  five test jobs selects its prefix and nothing
  else: the `go test` line of each carries `-run 'TestE2E'`, `-run
  'TestCluster' -skip 'TestClusterUpScript'`, `-run 'TestSlow'`, `-run
  'TestClusterUpScript'`, or `-run 'TestMutation'`, and no other job
  runs the `e2e` tier (proposed:
  `test/e2e`, `TestE2EJobsSelectByPrefix`, which reads
  `.github/workflows/verify.yml` through a test-only constant resolved
  from its own source file, as spec 011 says for a test that reads
  outside its package).
- `tools/docs/run-blocks.sh` runs the `sh` blocks of a fixture
  document in order in one shell, stops at the first failing block,
  and passes `ORIGO_TEST_URL` and `ORIGO_TEST_ADMIN_TOKEN` through
  (proposed: `tools/docs/run_blocks_test.sh`, run through a Go test in
  `tools/docs` the way spec 017 runs its smoke test).
- `make fuzz` runs every fuzz function in the module for 40 seconds and
  the weekly schedule in `verify.yml` calls it (proposed: `Makefile`,
  the `fuzz` target; `verify.yml`, the `fuzz` job on `schedule`).
- The build list of `./cmd/origod` reaches no package under
  `github.com/aws/`, `cloud.google.com/`, `github.com/Azure/`, or
  `k8s.io/`, spec 001's criterion, which this spec owns: the `depcheck`
  gate runs on every push with `depcheck.packages` in `.lateregate.yaml`
  naming `./cmd/origod` and an allow list of `latere.ai/x/pkg` and the
  standard library (proposed: `.lateregate.yaml`, the `depcheck` gate,
  checked by bare `make`).

## Outcome

Built on 2026-09-08 in eleven steps: the three outage paths on the
authorizer stub, the sink, the source with its embedded fixture, the
contract stub, the binary and `Dockerfile.stubs`, the `test/e2e/cluster`
helper with the fault manifests, the overlay with `up.sh` and
`down.sh`, the e2e tests, the Makefile targets, `tools/docs`, the
`depcheck` gate, and the jobs of `verify.yml`.

| Criterion | Test |
|---|---|
| the issuer and the authorizer (spec 007), the sink refuses a bad signature and answers the configured failures then recovers | `test/stubs/issuer`, `TestMintsEachFailure`; `test/stubs/authorizer`, `TestRulesAndProbe` and `TestOutageIsSetOverHTTP` for the three paths this spec adds; `test/stubs/sink`, `TestSignatureAndFailures` |
| the source serves `/fixture.git` over TLS with the bearer and refuses without, `Commit`, `Requests` without the bearer's value, 5 000 commits | `test/stubs/source`, `TestSourceServesTheFixtureOverTLS` |
| the contract stub serves a clone, a push, and the lifecycle table in-process | `test/stubs/origo`, `TestStubServesTheContract` |
| `make dev` prints a clone line the node accepts; `make test-tiers` runs both tiers without compose | `test/e2e`, `TestE2EDevStackClones`; the `integration` job runs `make test-tiers` |
| the overlay builds, `up.sh` reaches every pod and host port within 5 minutes with the list of the criterion, `down.sh` removes the cluster | `test/e2e`, `TestClusterUpScript`, in the `up-script` job |
| the six helper functions against the stack | `test/e2e`, `TestClusterHelperDrivesKubectl`, in the `e2e` job; `test/e2e/cluster`, `TestEveryFunctionRunsItsCommand` and `TestFailuresCarryStderr` check the commands against a recorded `kubectl` without a cluster |
| the jobs, budgets, exports, `git-lfs`, one `docker build`, `candidate-images`, one prefix per job | `test/e2e`, `TestE2EJobsSelectByPrefix` |
| `run-blocks.sh` runs the `sh` blocks in order in one shell, stops at the first failure, passes the two variables through | `tools/docs`, `TestRunBlocks` running `run_blocks_test.sh` |
| `make fuzz` and its weekly schedule | `Makefile`, `fuzz`; `verify.yml`, `fuzz` on `schedule` |
| the build list of `./cmd/origod` | `.lateregate.yaml`, `depcheck`, run by bare `make` and the `gate` job |

Divergences and interpretations, all kept:

- The binary's `-authorizer-token` has no default and the binary refuses
  to start without it: an authorizer with an empty bearer would admit a
  request that carries none. `-ca` and `-ca-key` go together. The source's
  repositories live under a temporary directory the binary creates,
  removed at exit; a pod mounts `/tmp` writable for it.
- The binary gains `-slowproxy-target <host:port>` beside
  `-slowproxy-listen`, and `-slowproxy-data 0.0.0.0:8086` for the data
  listener `ORIGO_S3_ENDPOINT` points at: the proxy starts only when the
  target is set, its control endpoint answers a GET of its root with the target and
  a delay of 0, and the data listener forwards TCP with no delay. That
  shell lives in `test/stubs/cmd/origo-stubs/slowproxy.go`; spec 015
  builds `test/stubs/slowproxy` with the fault behaviour and replaces it.
  The overlay passes no target, so `30085` answers nothing until 015.
- The sink records every delivery, a refused one included, with
  `Verified` and the status it answered, so spec 008's test sees an
  unsigned delivery arrive; `Fail(n, status)` is `SetStatus(status, nil,
  n)`, `Fail(0, 0)` restores 200, and `Wait` returns the deliveries with
  whether the count was reached.
- The source's CA carries the two SANs as well as the leaf it signs, so
  the criterion's "a certificate with both SANs" holds on the Secret's
  `ca.crt`; the leaf also carries `127.0.0.1` for the in-process tests.
  The key is read in the SEC 1 form `openssl ecparam` writes and the
  PKCS #8 form `openssl req -newkey ec` writes, which `up.sh` uses. The
  fixture is generated by `TestGenerateFixture` under the `generate`
  build tag from one `git fast-import` stream with fixed dates, so the
  1.1 MB bundle is byte for byte reproducible; `go generate` in the
  package rewrites it.
- The helper has a seventh function, `GetIn(t, namespace, kind, name)`,
  `Get` being `GetIn` in `origo`, because `TestClusterUpScript` asserts
  on Cilium and `metrics-server` in `kube-system`. `DeletePod` waits with
  `rollout status` on the pod's owner. The helper's package carries no
  build tag, so the `cover` gate measures it through the recorded
  `kubectl`; only its callers carry `e2e`.
- `up.sh` writes the rendered kustomization (the overlay, the component,
  and the generated Secrets) under `out/kind/<name>/` and the name of
  the cluster it brought up in `out/kind/current`, which
  `TestClusterHelperDrivesKubectl` reads to find the directory
  `cluster.Apply` restores the stack from: applying the checked-in
  overlay would drop the component's patches and roll the nodes.
  `down.sh` removes both.
- The overlay owns the Namespace `origo` with the restricted label,
  because the label is a row of its table and the production Namespace
  stays in `deploy/bootstrap` (spec 018); the base's Deployment and
  Ingress are removed with `$patch: delete`, and the base's Service is
  patched to a NodePort. The autoscaler `origod` is a resource of the
  overlay held at three replicas with the 60 second scale-down window,
  not a patch, because `deploy/base` has no autoscaler until spec 005
  adds it; `hpa-2.yaml` carries a scale-down window of 0 so the two
  replicas are reached within one reconcile. The gossip NetworkPolicy
  `origod-gossip` is spec 016's in `deploy/base` and is not built here.
- MinIO for the `integration` and `mutation` jobs is a `docker run`
  step, not a `services` container: the image takes `server /data` as
  its command, which the `services` key cannot pass. The `mutation` job
  loops over the job's own `MUTATION_CAPABILITIES`, empty until spec 021
  fills it, so its `go test` line runs once with the variable unset and
  selects nothing. The `build` job builds from `Dockerfile` and
  `Dockerfile.stubs` and tags both images `candidate`, the tag the
  overlay's `images` field names.
- `TestClusterUpScript` reads the tarballs from `out/images/origod.tar`
  and `out/images/origo-stubs.tar`, where the `up-script` job downloads
  the artifact, and skips without them or without `kind`, `kubectl`, and
  `helm`.
- `.lateregate.yaml` admits `github.com/google/uuid` beside
  `latere.ai/x/pkg` for `./cmd/origod`: `pkg/httpjson` reaches it, so an
  allow list of `pkg` alone cannot pass; it is not a cloud SDK.
- `run-blocks.sh` runs the blocks with `eval` in its own shell rather
  than piping them to a second `sh`, so it runs under the `hermetic`
  gate, whose `PATH` holds no `/bin`.
- A defect found in the tree and fixed at the root, recorded in spec
  002's Outcome: the storage and outbound transports of `cmd/origod`
  had no dial timeout, so a bucket that drops packets held a request
  for the operating system's connect timeout times the client's
  retries; both now dial with a 10 second bound
  (`TestTransportsBoundTheDial`), which is what lets
  `cut-storage.yaml` produce `storage_unavailable` within the helper
  test's patience until spec 015's `ORIGO_STORAGE_TIMEOUT` bounds the
  whole operation.
- Spec 001's `TestE2E` renames and its `depcheck` criterion, and spec
  007's `make fuzz`, are done here, and both specs' Outcomes record it.

- Two races the first stack runs showed, fixed in `up.sh` and the
  helper test: every host port answering does not mean the nodes hold
  the issuer's keys, because a node that started before the stubs
  fetched nothing and retries once a minute (spec 007), so `up.sh` ends
  by waiting until a token minted at the issuer's host port reaches the
  authorizer through the nodes, which denies the probe id with 403; and
  a pod `DeletePod` replaced is ready before its NodePort routes to it,
  so the test polls node 2 for up to a minute.
- A third race, found by spec 005's cluster run and fixed in `up.sh`:
  the identity wait checked the balanced port, which answers from
  whichever node fetched the issuer's keys first, so a test that names
  a node, or `TestClusterHelperDrivesKubectl` deleting the one node
  that had them, found the others answering `issuer_unavailable` for
  up to a minute. The wait now holds for the balanced port and each of
  the three node ports.
- A second defect found in the tree and fixed at the root, recorded in
  spec 002's Outcome: the digests pinned for `debian:bookworm-slim` and
  `minio/mc` were `arm64` manifests, not multi-arch indexes, so the
  `build` and `integration` jobs failed with `exec format error` on the
  `amd64` runners until every pin became the index digest.

- The four SSH rows of the ports table read `127.0.0.1` where every
  other row reads `localhost`, changed after spec 018's install job
  stopped on them. `ssh-keyscan` asks the first address a name resolves
  to and does not try the next, so the document's wait found nothing at
  `localhost:30022` while `up.sh`, which has always written the
  address, reached the same port of the same cluster in the same run.
  The job is the proof: red on the name in runs 34524148220 and
  34525855273, green on the address in run 34526990408.

The job's budget was tested on 2026-09-11, when
`TestSlowMaterializeTenThousandEntries`, the scenario this table had
named since it was written, finally reached the tree. It writes 10 000
entries through `Log.Commit` before it materializes them, which is the
longest fixture any scenario here builds. On run 34629785911 the test
took 416.83 s of a job that took 12 m 20 s, so the 30 minute row
stands unchanged and the rule above, that a scenario making `e2e-slow`
exceed its budget is a spec change and not a budget change, was not
reached. The figure is worth keeping: the job was 5 m 25 s before the
scenario landed, so the ceiling scenario more than doubled it and a
sixth scenario of that size would need the rule.

Verified on this machine against a live stack: `make dev` and
`TestE2EDevStackClones` on podman compose, and `make test-integration`
(the store suite and the `TestE2E` tier). The kind stack could not run
here: a rootless podman machine cannot mount the BPF file system Cilium
needs, and the 2 GiB machine starved the API server on a kindnet
attempt. The overlay, `up.sh`, `down.sh`, the Cilium and
`metrics-server` rows, `cut-storage.yaml`, `TestClusterUpScript`, and
`TestClusterHelperDrivesKubectl` are verified by the `up-script`,
`e2e`, and `e2e-slow` jobs of `verify.yml`, green on `main` at
`c54c711` (run 34208209981), where `up.sh` brings the stack up in
about 90 seconds.
