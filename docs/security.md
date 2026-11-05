# Security

What an installation of Origo guarantees the people who use it, what it
can see, and what whoever runs it must keep safe. The commitments, each
with the test that holds it, what is not defended, and how to report a
vulnerability are in [`SECURITY.md`](../SECURITY.md).

## What Origo guarantees

Every request carries a token from an issuer you listed, unless you turned
anonymous reads on, and then only the read routes accept none. Nothing of
a repository is read before your authorizer, or the built-in owner
policy, has decided, and a decision that cannot be given is a refusal.
A denied caller is answered the same whether the repository exists or
not. A pushed pack is checked by git before it is written to the log, and
a server-side fetch goes only to a host you listed and never to a private,
cluster or loopback address. [What is
protected](../SECURITY.md#what-is-protected) names the test behind each.

## What Origo sees

Origo keeps data in your bucket and on the disk of each node, and writes
the logs, traces and metrics a process writes. It has no database. A
reader of one place sees what the table says and nothing from the others.

| What | Kept in | Who can read it |
|---|---|---|
| Repository contents and history | the bucket, under the installation's `origo/` prefix: each push as an entry holding the pack as the client sent it, and the packs compaction folds them into. Git LFS objects are in the bucket under the repository's `lfs/` prefix. Origo does not encrypt them | whoever holds the bucket or its credentials, and any client the authorizer allows |
| Warm copies | each node's `ORIGO_DATA_DIR`, one bare repository per repository the node has served. It holds nothing the bucket does not, and a copy idle for 24 hours is removed | whoever can read the node's disk |
| Repository metadata | the bucket: a repository's id, owner, slug and creation and update times, and a name object that maps owner and slug to the id | whoever holds the bucket; callers through the API as the authorizer allows |
| Who pushed | the bucket: the header of each log entry names the subject that pushed, an issuer and a `sub` joined by `\|`. Origo keeps no name, address or profile for a subject | whoever holds the bucket |
| SSH public keys | none. Origo stores no key. It posts the fingerprint a client offers to your key endpoint and keeps the answer in the node's memory for the answer's `ttl` | your key endpoint; the node's process memory |
| Push events and webhooks | with `ORIGO_EVENTS_URL` set, each push or administration change is an object in the bucket under `origo/events/` until it is delivered, or after 24 hours of refusals, under `origo/events/dead/`. It carries the repository id, owner, slug, the pusher's `sub`, and each reference with its before and after commit. Each delivery is signed with `ORIGO_EVENTS_SECRET` in the `Origo-Signature` header | whoever holds the bucket, and your sink |
| What the authorizer is told | with `ORIGO_AUTHORIZER_URL` set, each question carries the subject, every verified claim of the token, the action, the repository's id, owner and slug, and the request's id, the address the connection came from and the user agent. Behind an ingress that address is the ingress's. The question goes over the URL you set, so use `https` | the authorizer you run |
| Request logs | JSON on standard error: one line per request with the route pattern, the method, the status, the duration, the repository, the subject, the bytes each way and the trace id. No token, basic auth password or object bytes is a field of the line | whoever reads the process's standard error. With a collector set, the lines also go there |
| Traces | with `OTEL_EXPORTER_OTLP_ENDPOINT` set, one trace per request, with a span per phase of a push and per bucket call, and the repository and subject as span attributes | whoever reads your collector |
| Metrics | `/metrics` on the internal listener: counts and timings under fixed labels. No repository, subject, reference or path is a label | whoever can reach the internal listener |

Logs, traces and metrics are described in [Operating
Origo](operations.md#traces-and-logs). A change that logs a new field is a
change to what is kept here.

## How long it keeps things

The log is the history: it is kept until the repository is deleted, and
[Storage](operations.md#storage) says how compaction bounds it. A deleted
repository is held for seven days and then its objects are removed, as
[Deleting and its hold](operations.md#deleting-and-its-hold) states. An
LFS object nobody verified is removed seven days after its upload, and a
weekly sweep removes objects nothing names. An event is retried for 24
hours and then kept under `origo/events/dead/` until you move or remove it
([Push events](operations.md#push-events)). A warm copy goes by the rules
of the cache in [Scale](operations.md#scale), sized by `ORIGO_CACHE_BYTES`.
The intervals and ages you can set are
[Storage behavior and housekeeping](configuration.md#storage-behavior-and-housekeeping)
and [Push events](configuration.md#push-events). How long a request line, a
trace or a metric lives is your collector's setting, and Origo has none of
its own. How long an allow is cached is the `ttl` your authorizer sends,
at most 600 seconds ([the authorization endpoint](authorizer.md)).

## What it does not defend

Origo does not encrypt repository contents. Whoever holds your bucket
reads every repository, its LFS objects, the owners and names, and who
pushed. A node that is compromised holds the bucket credentials and the
token signing key. A bearer token works until it expires, a repository-bound
token until its own expiry even if the permission behind it is revoked, and
an allow outlives its revoke by the authorizer's `ttl`. The public listener
serves plain HTTP, and Origo follows whatever your authorizer and issuers
decide. [What is not defended](../SECURITY.md#what-is-not-defended) lists
each limit and who can use it. Origo has had no external security audit.

## What an operator keeps safe

Every variable is in [Configuration](configuration.md).

| What | Why |
|---|---|
| The bucket credentials, `ORIGO_S3_KEY` and `ORIGO_S3_SECRET` | they read, write and delete every object of the installation. Keep them in a Secret, and give them access to this bucket and nothing beside it |
| The token signing key, `ORIGO_TOKEN_KEY` | it signs the repository-bound tokens Origo mints, and the cursors of repository listings are encrypted under a key derived from it. Whoever holds it can mint a token for any repository. Every node holds the same key, and replacing it invalidates every outstanding token |
| The webhook signing secret, `ORIGO_EVENTS_SECRET` | it is how your sink tells a real delivery from a forged one. Set with `ORIGO_EVENTS_URL`, and a URL without it stops the node from starting |
| The gossip secret, `ORIGO_GOSSIP_SECRET` | it signs every datagram the nodes send each other, at least 32 bytes, the same on every node. A holder can announce itself as a node |
| The authorizer's bearer, `ORIGO_AUTHORIZER_TOKEN` | it is what your endpoint takes as Origo's own. Whoever holds it can put questions to your endpoint as Origo. It is required when `ORIGO_AUTHORIZER_URL` is set |
| The authorizer itself, `ORIGO_AUTHORIZER_URL` | it decides every action. One that allows everything allows everything. `origod check` asks it a question it must deny and fails on an allow, so run it after every change to the endpoint |
| The issuer list, `ORIGO_OIDC_ISSUERS`, and the audience, `ORIGO_OIDC_AUDIENCE` | an issuer that signs a token for the wrong subject lets its holder act as that person. List only issuers you run or trust to that degree |
| `ORIGO_OIDC_INSECURE_ISSUERS` | it admits an `http://` issuer that is not on loopback, so the key set is fetched over a channel that can be rewritten. It is for a test stack only; leave it unset |
| Administrator subjects, `ORIGO_ADMIN_SUBJECTS` | each subject listed, written `<issuer>\|<sub>`, may do anything to every repository. It is read only when no authorizer endpoint is set |
| Anonymous reads, `ORIGO_ANONYMOUS_READ` | set to `1` it admits a request with no credential on the read routes listed in [Anonymous read](configuration.md#anonymous-read), and your authorizer decides what such a caller may read. Leave it unset and nothing is anonymous. `ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE` bounds what anonymous callers share |
| The hosts a server-side fetch may reach, `ORIGO_EGRESS_ALLOW`, with `ORIGO_CLUSTER_CIDRS` | an import or a verify fetches from a host a caller names. Unset, every source is refused. List only the hosts you mean, name your cluster's own ranges in `ORIGO_CLUSTER_CIDRS`, and pin an address with `host=address` only for a source you run inside the cluster on purpose |
| The key endpoint's bearer, `ORIGO_SSH_KEYS_TOKEN`, and the SSH host keys, `ORIGO_SSH_HOST_KEYS` | the first is what your key endpoint takes as Origo's own. The host keys identify the installation to every SSH client, and a stolen one is a machine in the middle until it is replaced. Keep both in a Secret, and never in the bucket |
| TLS in front of the public listener | `ORIGO_PUBLIC_ADDR` serves plain HTTP, and every request carries a bearer token or a password. End TLS at your ingress, and set `ORIGO_PUBLIC_URL` to an `https` origin |
| The internal listener, `ORIGO_INTERNAL_ADDR` | it serves the probes and `/metrics`. Do not publish it: no Service or ingress should name its port |
| The network policies | the base admits the client ports from every peer and the gossip port from the nodes only. They need a network plugin that enforces them. See [the install guide](install.md) before you widen them |
