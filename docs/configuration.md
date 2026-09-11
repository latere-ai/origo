# Configuration

Every environment variable `origod` reads, what it does, and what it
does without. This page is generated from the code by `make docs`, so a
default here is the default in the binary.

A node reads its whole configuration once at start-up. Anything missing
or malformed stops it with one message that names every problem at once,
so a deployment is fixed in one round rather than one variable at a
time. `origod check` goes further: it uses the same configuration to
reach the bucket, the issuers, the authorization endpoint, and the event
sink, and says which of them answered. Run it before you trust an
installation, and again whenever one of them changes.

Durations are written as `10m`, `500ms`, `1h30m`. Sizes are plain
integers of bytes.

## The bucket

Object storage is the source of truth. Every node of one installation reads and writes one bucket under the prefix `origo/`, and two installations never share a prefix. The endpoint must honour a conditional create (`PUT` with `If-None-Match: *`); `origod check` proves it before you trust it.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_S3_ENDPOINT` | yes | none | the bucket's API endpoint, an absolute URL such as `https://fra1.digitaloceanspaces.com`. |
| `ORIGO_S3_REGION` | yes | none | the bucket's region, as the provider names it. |
| `ORIGO_S3_BUCKET` | yes | none | the bucket. It must exist; Origo never creates one. |
| `ORIGO_S3_KEY` | yes | none | the access key id. Give it read, write, and delete on this bucket and nothing else. |
| `ORIGO_S3_SECRET` | yes | none | the secret access key. Keep it in a Secret, never in a manifest. |
| `ORIGO_S3_PATH_STYLE` | no | unset | `1` addresses the bucket as the first path segment instead of a subdomain. MinIO and any endpoint reached by IP need it; most cloud providers do not. |
| `ORIGO_S3_PUBLIC_ENDPOINT` | no | `ORIGO_S3_ENDPOINT` | the endpoint Git LFS clients can reach, when it differs from the one the nodes use. LFS transfers are signed against it, so a client that cannot resolve the internal name still uploads and downloads. |

## The service

What clients see and where the node keeps its cache. The cache is disposable: it holds nothing that is not in the bucket, and a node rebuilds it on demand.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_PUBLIC_URL` | yes | none | the absolute URL clients use, such as `https://git.example.com`. It appears in clone URLs and event payloads and signs repository-bound tokens, so every node of one installation sets the same value and changing it invalidates outstanding tokens. |
| `ORIGO_DATA_DIR` | no | `/var/lib/origo` | the repository cache, on a local disk. Never a network file system: git runs against it. |
| `ORIGO_CACHE_BYTES` | no | 80% of the file system holding `ORIGO_DATA_DIR` | the size the cache is kept under, in bytes. A positive integer. Set it when the disk holds something else as well. |
| `ORIGO_PUBLIC_ADDR` | no | `:8080` | the address the client-facing listener binds: git over HTTP, the JSON API, and LFS. |
| `ORIGO_INTERNAL_ADDR` | no | `:8081` | the address the probes and `/metrics` bind. Do not publish it. |
| `ORIGO_GOSSIP_ADDR` | no | `:7946` | the UDP address nodes announce to each other on. |
| `ORIGO_NODE_NAME` | no | the host name | the name this node is known by to the others. The pod name in Kubernetes. It must be unique in the installation. |

## Identity and authorization

Every request carries a bearer token, and every request that names a repository is authorized by an endpoint you run. You supply both: an OIDC issuer that mints the tokens and an authorization endpoint that answers what a subject may do. Neither has a default, and a node without them does not start.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_OIDC_ISSUERS` | yes | none | comma separated issuer URLs whose tokens are accepted. Each must serve OpenID discovery and a key set over HTTPS. |
| `ORIGO_OIDC_INSECURE_ISSUERS` | no | unset | issuers from the list above that may use `http://` on a host that is not a loopback address. For a test stack only; never set it in production. |
| `ORIGO_AUTHORIZER_URL` | yes | none | your authorization endpoint, an absolute `http` or `https` URL. Origo asks it before every repository operation and caches the answer briefly. |
| `ORIGO_AUTHORIZER_TOKEN` | yes | none | the bearer Origo presents to that endpoint, so it can tell Origo from anything else that reaches it. |
| `ORIGO_TOKEN_KEY` | yes | none | a PEM-encoded ECDSA P-256 private key, the whole `openssl ecparam -genkey -name prime256v1` output. It signs the repository-bound tokens Origo mints. Every node of one installation holds the same key; replacing it invalidates every outstanding repository-bound token. |
| `ORIGO_DEV_TOKEN` | no; refused | none | an early fixed bearer, removed. A node that has it set refuses to start, so a deployment that still carries it is found rather than silently trusted. |

## More than one node

Nodes are stateless and interchangeable. They tell each other which repositories they hold warm, over UDP, and every datagram is authenticated. One node needs neither variable.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_GOSSIP_PEERS` | no | unset | the other nodes: comma separated `host:port` entries, or one DNS name that resolves to all of them, which is the headless Service in Kubernetes. |
| `ORIGO_GOSSIP_SECRET` | with `ORIGO_GOSSIP_PEERS` | none | the key every gossip datagram is signed with, at least 32 bytes, the same on every node. `openssl rand -hex 32` produces one. |

## Push events

Origo can post a signed webhook for every reference update, so a build or a deploy starts from a push. Leave the URL unset and no events are produced.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_EVENTS_URL` | no | unset | the endpoint deliveries are posted to. |
| `ORIGO_EVENTS_SECRET` | with `ORIGO_EVENTS_URL` | none | the key each delivery is signed with, so your sink can tell a real delivery from anything else. A URL without a secret is a start-up failure. |
| `ORIGO_REPAIR_INTERVAL` | no | `10m` | how often a node looks for deliveries a node that went away left behind. |
| `ORIGO_REPAIR_UNHEARD` | no | `5m` | how long a node must be silent before another node takes over its undelivered events. |

## Storage behaviour and housekeeping

What a node does when the bucket is slow, and how often it tidies up after itself. The defaults suit a healthy bucket in the same region.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_STORAGE_TIMEOUT` | no | `10s` | the deadline of one object storage call. Above zero. Raise it for a bucket in another region. |
| `ORIGO_STALE_MAX` | no | `5m` | how long a node keeps serving a repository from its local copy while the bucket is unreachable. Reads in that window carry a header saying how old they are; writes are refused throughout. |
| `ORIGO_SWEEP_INTERVAL` | no | `10m` | how often a node removes objects no log entry names. `0` turns the sweep off. |
| `ORIGO_SWEEP_MIN_AGE` | no | `1h` | how old such an object must be before the sweep removes it. The margin that keeps a push in flight from being swept. |

## Limits

Two ceilings protect a node from one caller and from itself. Both are per node, not per installation.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_MAX_GIT_PROCS` | no | `64` | git subprocesses this node runs at once. A positive integer. Requests wait for a slot rather than failing. |
| `ORIGO_REQUESTS_PER_MINUTE` | no | `600` | the requests one subject may send this node in a minute, and the burst it may spend at once. Every response carries the figure in force. `0` turns the limit off. Raise it for a fleet of tooling that shares one token. |

## Anonymous read

A repository can be readable without a credential, if you decide it is.
Origo holds no such decision. With `ORIGO_ANONYMOUS_READ` set, a request
that carries no credential at all is admitted on the routes below and sent
to your authorization endpoint with an empty subject. Your endpoint
answers, exactly as it answers for a person. Leave the variable unset and
every request needs a token, which is how a node behaves without this
section.

A repository your endpoint does not open answers `401` with
`WWW-Authenticate: Basic`. That is the same answer a request with no
credential gets on a node that never set the variable, so a caller cannot
tell a repository they may not read from one that is not there, and `git`
still asks them for a credential.

```
GET  /r/{id}/info/refs?service=git-upload-pack
POST /r/{id}/git-upload-pack
GET  /{owner}/{slug}/info/refs?service=git-upload-pack
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

Nothing else is ever anonymous. Not a push. Not the Git LFS batch, whose
answer is a signed bucket URL that would outlive the request. Not
`/stats`, not `/export.bundle`, not the repository list, and no
administrative route. A public repository that uses Git LFS can be cloned;
its LFS files need a credential to download.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_ANONYMOUS_READ` | no | unset | `1` admits a request with no credential on the routes above and asks your authorization endpoint about it with an empty subject. Unset admits none. |
| `ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE` | no | `60` | the requests every anonymous caller of this node shares in a minute. They share one bucket, so one scraper slows other anonymous readers and never slows a caller with a token. Used only when `ORIGO_ANONYMOUS_READ` is set; a malformed value is a start-up problem either way. |

## Git over SSH

SSH is off until you set an address for it. It carries `git clone`, `git fetch`, and `git push` and nothing else: the JSON API, Git LFS, and reads that must not be stale are HTTPS. Origo stores no public key. It asks an endpoint you run which subject an offered key belongs to, the way it asks your authorization endpoint what that subject may do, so adding, naming, and removing keys stays yours. A file of fingerprints served behind a bearer is the whole requirement.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_SSH_ADDR` | no | unset | the address the SSH listener binds, such as `:2222`. Unset opens no SSH listener and the node runs exactly as it does without it. Bind an unprivileged port and publish 22 in front of it. |
| `ORIGO_SSH_HOST_KEYS` | with `ORIGO_SSH_ADDR` | none | an ordered comma separated list of paths to OpenSSH private host key files, the same list on every node. `ssh-keygen -t ed25519` writes one. The first key of each algorithm is the one presented and every key in the list is announced, which is what makes replacing a key an overlap rather than a warning every client sees. Keep them in a Secret, never in the bucket, and never in a manifest. |
| `ORIGO_SSH_KEYS_URL` | with `ORIGO_SSH_ADDR` | none | your key resolution endpoint, an absolute `http` or `https` URL. Origo posts the offered key's fingerprint to it before every connection it cannot answer from its cache, and a reply it cannot read refuses the connection. |
| `ORIGO_SSH_KEYS_TOKEN` | with `ORIGO_SSH_ADDR` | none | the bearer Origo presents to that endpoint, so it can tell Origo from anything else that reaches it. |

## Fetching from another host

Importing a repository, or verifying one against its prior host, makes the node fetch from a URL a caller supplies. Nothing is fetched from a host you have not listed, so these three are what open that door and how wide.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_EGRESS_ALLOW` | no | unset | comma separated hostnames a fetch may reach, exact or `*.` wildcards. Unset refuses every source. An exact hostname may carry a pinned address as `host=address`, one IP literal that fixes where that name resolves for this node. |
| `ORIGO_CLUSTER_CIDRS` | no | unset | comma separated CIDR ranges of your cluster's own networks, which a fetch must never reach. Loopback, link-local, and metadata addresses are refused whether or not you list them. |
| `ORIGO_EGRESS_CA_BUNDLE` | no | unset | the path of a PEM file of certificate authorities to trust beside the system roots when a fetch dials a source over TLS. For a source with a private certificate. |

## Telemetry

Metrics are always served on the internal listener at `/metrics`. Traces and logs go out only when an endpoint is set.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | unset | the OpenTelemetry collector traces and logs are exported to. Unset, nothing leaves the process. |
| `OTEL_*` | no | unset | the rest of the standard OpenTelemetry environment variables are read as they are defined by that specification. |

## Set by the node

One variable is passed by the node to a subprocess it starts. It is listed so it is not mistaken for a knob.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_HOOK_DIR` | never set by hand | none | the node sets it on the git process of a push, naming the directory the receive hook talks back through. |

## The migrate subcommand

`origod migrate` drives a batch of imports against an installation. It is a client: it reads these three and none of the node's, so run it from anywhere that can reach Origo.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_MIGRATE_URL` | yes | none | the Origo the batch is driven against. |
| `ORIGO_MIGRATE_TOKEN_ENV` | yes | none | the name of the variable holding the bearer presented to Origo, so the token is never on a command line. |
| `ORIGO_MIGRATE_PARALLEL` | no | `4` | repositories driven at once. |

## Testing and the pipeline

None of these is read by a serving node in an installation. They belong to the test suites, the example stack, and the release pipeline, and are listed so the reference is one page.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_CHECK_SELFTEST` | no | unset | `1` makes `origod check` run its conditional-create line against an in-process store that ignores the header, so the check's own failure path can be proved. |
| `ORIGO_FAILPOINT` | no | unset | names a point at which the node exits at once, for a test that needs a node killed between two writes. Empty in every deployment. |
| `ORIGO_TEST_DROP_CAPABILITY` | no | unset | one git capability the node stops advertising, for the test that proves the conformance suite notices. Empty in every deployment. |
| `ORIGO_TEST_S3_ENDPOINT` | no | unset | the bucket the test tiers use; unset, those tiers skip. |
| `ORIGO_TEST_S3_REGION` | no | unset | its region. |
| `ORIGO_TEST_S3_BUCKET` | no | unset | its bucket. |
| `ORIGO_TEST_S3_KEY` | no | unset | its access key id. |
| `ORIGO_TEST_S3_SECRET` | no | unset | its secret access key. |
| `ORIGO_TEST_S3_PATH_STYLE` | no | unset | `1` for a path-style test endpoint. |
| `ORIGO_TEST_URL` | no | the example stack's balanced port | the installation the cluster tests and the conformance suite target. |
| `ORIGO_TEST_ADMIN_TOKEN` | no | a token minted at the example stack's issuer | a token with the `admin` action on every repository the suite touches. |
| `ORIGO_LIVE_URL` | no | unset | an installation the conformance suite runs against after a release. Unset, that run is skipped. |
| `ORIGO_LIVE_TOKEN` | no | unset | a token with `admin` on the prefix that run uses. |
| `ORIGO_E2E_MEASURE` | no | unset | `1` runs the measurement tests, which print figures and assert no threshold. |
| `ORIGO_PREVIOUS_RELEASE_FIXTURE` | no | unset | the previous release's fixture archive, which the compatibility test materializes on this build. Unset, it skips. |
| `ORIGO_INSTALL_IMAGE` | no | the released image | the image reference the install document applies, so the same document is walked against a candidate build and against a release. |
| `ORIGO_INSTALL_MANIFESTS` | no | `deploy/examples/kind` | the manifests the install document applies. |
| `ORIGO_KUBECONFIG` | no | unset | the kubeconfig the release pipeline applies a release with. |
| `ORIGO_RELEASE_DEPLOY` | no | unset | unset, a tag publishes artifacts and deploys nothing. |
| `ORIGO_IMAGE_NAMESPACE` | no | `ghcr.io/` and the repository owner | the registry namespace a tag publishes both images under, so a fork publishes its own. An image reference is lowercase, so an owner whose login carries capitals sets it. |
