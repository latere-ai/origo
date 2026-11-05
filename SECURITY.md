# Security

Origo stores source code and takes bytes from anyone who can reach it.
This file is how to tell us when it fails at that, what the project
commits to, what it does not defend, and what has and has not been
checked. [`docs/security.md`](docs/security.md) is the same for the people
who run Origo and build on it, with what it can see. The full
[threat model](specs/016-security-and-threat-model.md) names every threat
with its control.

## Reporting a vulnerability

Report it through
[GitHub private vulnerability reporting](https://github.com/latere-ai/origo/security/advisories/new),
or by email to security@latere.ai. Do not open a public issue for it.
The [organization's security policy](https://github.com/latere-ai/.github/blob/main/SECURITY.md)
says when you will hear back, how a fix is disclosed, and the safe harbor
for research that follows it.

Fixes go to the two most recent minor release series.
v0.16.9 is the current release.

These are the reports we most want: anything that lets a caller read or
push to a repository without the authorizer's allow, or with a token for
another audience, from an unlisted issuer, or carrying a delegation; a
denied request that answers differently for a repository that exists and
one that does not; a decision the authorizer could not give that ends in
an allow; an anonymous request that reaches a route beside the read routes,
or any route on an installation that did not turn anonymous reads on; a
server-side fetch that reaches a host the operator did not list, or an
address in a private, cluster or loopback range; a pushed pack that
writes outside a repository or runs code on a node; a token, a
credential or a secret of the configuration in a log line or a metric
label; a way for one repository to read another's objects on a node;
and a forged gossip datagram, or an SSH client that gets anything beyond
`git-upload-pack` and `git-receive-pack`. What
[is not defended](#what-is-not-defended) is not a vulnerability, but a
report that one of those limits is wider than written here is.

## What is protected

The commitments the design makes: every request on the public listener
carries a token from an issuer the operator listed, except the three
paths that serve no repository state and the read routes of an
installation that turned anonymous reads on; nothing of a repository is
read before the authorizer has decided, and a decision that cannot be
given is a refusal; a pushed pack is checked by git before a log entry
is written; and a server-side fetch goes only where the operator allowed.

Each row is who could attack, what Origo commits to against them, and
the tests in this repository that hold the commitment. A control without
a test is a claim, and no claim is listed here. The threat model lists
more controls than these.

| Who | What Origo commits to | Held by |
|---|---|---|
| a caller with no token, a token for another service, an expired one, or one carrying a delegation | every route of the public listener refuses a missing token, a token for another audience and an expired one, except three paths that serve no repository state; a token with an `act` claim is refused before the authorizer is asked | `TestEveryRouteRequiresAToken` (`cmd/origod`), `TestAudienceAtTheDoor` (`internal/auth`), `TestActClaimIsRefused` (`internal/httpgit`) |
| an issuer reached over plain HTTP | an `http://` issuer off loopback stops the node from starting unless the operator lists it in `ORIGO_OIDC_INSECURE_ISSUERS` | `TestInsecureIssuersNeedTheList` (`internal/config`) |
| a caller reaching a repository it may not | on the routes that name a repository the authorizer is asked once, before anything of the repository is read, and a denied caller is answered 403 whether the repository exists or not, so a refused caller cannot tell a private repository from a missing one. The same holds over SSH | `TestDenyBeforeLookup` (`internal/api`), `TestSSHDenyBeforeLookup` (`internal/sshd`) |
| any caller, when the authorizer cannot answer | a decision that cannot be given is a refusal, never an allow, and a write under a repository-bound token is refused the same way | `TestAuthorizerOutageDeniesAndRecovers`, `TestBoundTokenWriteFailsClosedDuringAuthorizerOutage` (`internal/auth`) |
| a holder of a repository-bound token | the token is scoped to one repository and one scope, and never outlives the token that minted it | `TestRepositoryBoundTokenScope`, `TestABoundTokenNeverOutlivesTheTokenThatMintedIt` (`internal/auth`) |
| a caller with no credential | with `ORIGO_ANONYMOUS_READ` unset nothing is anonymous. With it set, only the listed read routes are, a push is never, every other route stays behind a credential, a credential that does not verify is refused and not downgraded to anonymous, and the authorizer decides what an anonymous caller may read. Every refusal of an anonymous caller is the same 401 | `TestAnonymousReadOffAdmitsNothing`, `TestAnonymousSetAdmitsTheReadRoutes`, `TestAnonymousSetExcludesReceivePack`, `TestAnonymousSetWithholdsTheRest`, `TestBadCredentialIsNotAnonymous`, `TestAnonymousDenialIsTheSame401Everywhere` (`internal/auth`), `TestAnonymousIsRefusedWithTheOne401` (`internal/httpgit`) |
| a caller who names a host to import from or verify against | a fetch goes only to a host on `ORIGO_EGRESS_ALLOW`, and never to a loopback, private, link-local, unique-local or `ORIGO_CLUSTER_CIDRS` address. The address is resolved inside the dial, so a name that changes between the check and the connection cannot redirect it, every redirect hop is checked the same way, and a tunnel is refused. A production configuration admits no loopback source | `TestEgressDialerHonorsTheAllowList`, `TestEgressDialerAdmitsOnlyThePinnedClusterAddress`, `TestEgressDialerPinsTheResolvedAddress`, `TestEgressProxyFollowsRedirectsAndRefusesConnect`, `TestAllowLoopbackIsSetOnlyByTests` (`internal/api`), `TestEgressAdmitsNoLoopbackInProduction` (`cmd/origod`) |
| a pusher of hostile objects | git's object checks are on in every repository a node creates, and a pack holding a `.git` entry, an NTFS or HFS+ reserved name or a broken object is refused with git's message and leaves no log entry | `TestBareRepositoryConfiguration` (`internal/repo`), `TestMaliciousPackWritesNothing` (`internal/httpgit`) |
| a caller who chooses a reference, owner or slug | a name with a shell metacharacter, a control character or a form git refuses is rejected before any subprocess sees it | `TestValidRefName`, `TestValidLabel` (`internal/wal`) |
| a repository's bytes reaching a git process | a git subprocess sees exactly seven environment variables and nothing of the node's own, its secrets included | `TestSubprocessEnvironment` (`internal/repo`) |
| anyone who damages or removes an object in the log | an object the log names that is missing or fails its digest makes that repository answer 503, and every other repository is served | `TestMissingPackIsAnIntegrityError` (`internal/repo`), `TestReadIndexReportsACorruptObjectAsIntegrity` (`internal/wal`) |
| a flood from one caller | each subject has a request budget, a node runs a bounded number of git subprocesses, anonymous callers share one budget, and a push over its repository's quota writes nothing | `TestPerSubjectTokenBucket`, `TestSubprocessCap`, `TestAnonymousShareOneBucket` (`internal/limits`), `TestPushOverQuotaWritesNothing` (`internal/httpgit`) |
| a sender of forged gossip | a datagram with no tag, a tag under another secret or a changed payload is dropped without entering the live set, and a flood of valid ones causes at most one catch-up per repository | `TestGossipDropsABadMAC`, `TestGossipCatchUpIsRateLimited` (`internal/placement`) |
| an SSH client | it gets `git-upload-pack` and `git-receive-pack` and nothing else, and before it authenticates the connection is bounded. An unknown key, or a key endpoint that does not answer, is refused, and a revoked key stops working within the answer's `ttl` | `TestSSHRefusesEverythingButTheTwoServices`, `TestSSHUnauthenticatedConnectionIsBounded`, `TestSSHKeyResolverFailsClosedAndRecovers`, `TestSSHRevokedKeyStopsWithinTTL` (`internal/sshd`) |
| a reader of the logs | the request log line carries a fixed set of fields and no credential, though a push authenticated with basic auth, and the token of an import or a verify is in no log line, process argument or URL | `TestRequestLogRedactsCredentials` (`cmd/origod`), `TestSourceTokenIsNeverLogged` (`internal/api`) |
| a compromised node | the pod runs as user 65532, not root, on a read-only root file system with every capability dropped, privilege escalation off and no service account token, with only the cache and `/tmp` writable. A pod asking for more is refused by Pod Security admission, and the gossip port admits only the installation's own nodes | `TestClusterPodSecurityContext` (`test/e2e`) |

`TestClusterPodSecurityContext` runs against a node of the example stack
on a kind cluster, and is built only with the `e2e` tag.

## What is not defended

- **Whoever holds the bucket can read what is in it.** Origo does not
  encrypt repository contents, at rest or end to end. The log holds each
  push's pack as the client sent it, so whoever holds the bucket or its
  credentials reads every repository's history and every Git LFS
  object, along with the owner and name of every repository and the
  subject of each push. Encryption at rest is the bucket's to provide.
- **A compromised node.** It holds the bucket credentials and the token
  signing key, so it reaches every repository those credentials reach.
  One installation serves one trust domain, and its bucket prefix is its
  own.
- **An authorizer or issuer that answers wrongly.** Origo follows the
  decision it is given. An authorizer that allows everything allows
  everything; `origod check` asks it a question it must deny and reports an
  allow, and is the only guard. An issuer that signs a token for the wrong
  person lets its holder act as that person.
- **A revoked permission stays honored for a short while.** An allow is
  cached for the `ttl` the authorizer gave, 60 seconds by default and at
  most 600, and a key the SSH endpoint resolved for the `ttl` of its
  answer. A repository-bound token is decided by its own scope and is not
  checked against the authorizer again, so it works until it expires.
- **A token is a bearer credential.** Whoever holds a bearer token, a
  repository-bound token or a basic auth password can use it until it
  expires.
- **The wire.** TLS ends at the ingress, and the public listener is plain
  HTTP inside the cluster, so TLS in front of it is the operator's to
  provide. A git subprocess on an import reaches its source through a
  proxy on the node that applies the allow list, and trusts the source for
  the bytes git checks and nothing else.
- **A machine in the middle of SSH.** One host key set serves the whole
  installation, and a stolen host key is a machine in the middle for
  every client until the key is replaced.
- **Denial of service.** Origo limits requests per subject and per node,
  bounds git subprocesses and repository sizes, and stops waiting on a
  slow bucket, and nothing more. Each limit is per node, not per
  installation. A flood can still use up what the bucket and the nodes
  can serve.
- **A node that holds the gossip secret.** It can announce itself as a
  member of the installation, at the cost of one catch-up per repository
  per second to its peers.
- **The network policies.** They admit the client ports from every peer
  in the cluster and the gossip port from the nodes, and a cluster whose
  network plugin does not enforce a policy gets nothing from them. The
  tokens and the MAC are the controls; the policies are a second layer.
- **No audit trail beyond the log and the request lines.** Origo has no
  audit export. The log records who pushed, and a read is recorded only
  in the request line a node writes.
- **Who may do what.** Origo holds no policy of its own beyond the
  built-in owner policy of an installation with no authorizer. Which
  subject may read, push or administer a repository is the authorizer's.

## Audit

Origo has had no external security audit. What exists instead is the
[threat model](specs/016-security-and-threat-model.md), which names each
control and the test that holds it, and the checks that run on every
push, among them a check of dependencies for known vulnerabilities. They
show that the controls above hold as written, not that the design is
sound. Weigh that before relying on Origo where a failure would put
someone at risk.

## Supply chain

Dependencies are checked for known vulnerabilities on every push. A release is built from a tagged commit
whose checks passed, through the shared release stages in
[latere-ai/ci](https://github.com/latere-ai/ci). It carries an SPDX bill
of materials for each image, `checksums.txt` over the archives and the
deploy manifests, a cosign signature over each image and over
`checksums.txt`, and an attestation on each image. The signatures name the
stage that made them, and the commands below hold them to this
repository. With `TAG` the release's tag, such as `vX.Y.Z`:

```sh
gh release download "$TAG" -R latere-ai/origo -D origo-release \
  --pattern 'checksums.txt*' --pattern 'deploy-*.tar.gz' --pattern 'origo*_*.tar.gz'
cd origo-release
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-publish\.yml@' \
  --certificate-github-workflow-repository latere-ai/origo checksums.txt
grep -q " deploy-$TAG.tar.gz$" checksums.txt
sha256sum -c --ignore-missing checksums.txt

cosign verify "ghcr.io/latere-ai/origod:$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-build\.yml@' \
  --certificate-github-workflow-repository latere-ai/origo
gh attestation verify "oci://ghcr.io/latere-ai/origod:$TAG" -R latere-ai/origo \
  --signer-workflow latere-ai/ci/.github/workflows/image-build.yml
gh attestation verify "oci://ghcr.io/latere-ai/origod:$TAG" -R latere-ai/origo \
  --signer-workflow latere-ai/ci/.github/workflows/image-build.yml \
  --predicate-type https://spdx.dev/Document/v2.3
```

The bills of materials are release assets, one per image.
[Upgrades](docs/upgrades/README.md#verifying-what-you-install) has the
rest, including how to check a release signed before the shared stages.
