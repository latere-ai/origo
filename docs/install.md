# Installing Origo

For whoever runs Origo. It takes about an hour on a cluster you already
have, and this page is the whole of it: what to prepare, what to apply,
how to tell whether it worked, and what to do when it did not.

Origo is one stateless binary. Object storage holds every repository;
local disk is a cache a node rebuilds on demand. So there is no database
to run, no leader to elect, and no volume to back up. What you supply is
a bucket, an identity provider, an endpoint that answers who may do what,
and a hostname.

Every shell block on this page is run by the project's own tests against
a throwaway cluster before it is published, so a command that drifts from
the manifests fails the build rather than your installation. That
throwaway cluster is why some blocks fall back to the example stack when
you set nothing: each one says so where it does, and the paragraph beside
it says what your installation does instead. The blocks that are not
shell are sketches of a request, to read rather than to paste, because
their shape is your provider's and not Origo's.

## What you need

| | What | Notes |
|---|---|---|
| Kubernetes | 1.29 or newer | any distribution. Pod Security admission at `restricted` on the namespace is supported and recommended. |
| An ingress controller | any | the manifests carry an `Ingress` with no class; your overlay names the controller and its settings. |
| A hostname and a certificate | one name pointed at the ingress | clients only ever see this name. |
| A bucket | any S3 compatible endpoint | it must honour a conditional create, `PUT` with `If-None-Match: *`. That is what linearizes pushes. MinIO, DigitalOcean Spaces, and AWS S3 are known to. `origod check` proves it before you trust it. |
| An OIDC issuer | discovery and a key set over HTTPS | it mints the tokens people and services present. Register one client for people and one for each service that acts on their behalf. |
| An authorization endpoint | one HTTP endpoint you run | Origo asks it, before every repository operation, whether a subject may read, write, or administer a repository. It has to know your repositories before Origo does, and with one tenant it can be a static list of subjects behind an HTTP handler. |
| Disk | a default storage class, or nodes with local disk | the cache. Sized for the repositories in active use, not for all of them. |
| A key resolution endpoint | one HTTP endpoint you run | only if you want git over SSH. It answers which subject an offered public key belongs to; Origo stores no key. A file of fingerprints behind a bearer satisfies it. |
| On your machine | `kubectl`, `openssl`, `curl`, `uuidgen`, `git`, `ssh-keygen` | nothing is installed in the cluster beyond the manifests. |

Two of these are yours to write and have no default: the issuer and the
authorization endpoint. Origo authenticates every request and authorizes
every repository operation, and it asks you both questions. There is no
mode in which it decides them itself.

Trying it out first is reasonable. The example overlay
`deploy/examples/kind` brings its own bucket, issuer, authorizer, and
event sink, so it installs on a throwaway cluster with nothing prepared,
and it is what the commands below install when you set nothing. Nothing
in it belongs in a real installation.

## Where things go

```
deploy/base/          the service: Deployment, Services, Ingress, autoscaler,
                      disruption budget, network policies
deploy/bootstrap/     what you create once by hand: the namespace and the Secrets
deploy/examples/      overlays to copy: kind, digitalocean, aws
```

You never apply `deploy/base` directly. You write an overlay that names
it and supplies what is yours: the hostname, the ingress class and its
settings, the replica bounds, and the Secrets.

## Settings for this page

The rest of the page reads these. Set them for your installation. Every
default here is the example stack's, so a variable you leave unset
installs the throwaway cluster of `deploy/examples/kind` and nothing of
yours: `MANIFESTS` is your overlay from step 3.

```sh
VERSION="${ORIGO_VERSION:-v0.1.0}"
IMAGE="${ORIGO_INSTALL_IMAGE:-ghcr.io/latere-ai/origod:$VERSION}"
MANIFESTS="${ORIGO_INSTALL_MANIFESTS:-deploy/examples/kind}"
NAMESPACE="${ORIGO_NAMESPACE:-origo}"
echo "installing $IMAGE from $MANIFESTS into $NAMESPACE"
```

`VERSION` is the release you are installing. The releases page lists
them, and each release carries `deploy-<version>.tar.gz`, the manifests
of that exact release with both images pinned; unpack it and point
`MANIFESTS` at the overlay inside. What a version number promises and
how to move between two of them is in [`upgrades/`](upgrades/README.md).

## 1. The bucket

Create a bucket at your provider and an access key that can read, write,
and delete in it and nothing else. Origo puts everything under the one
prefix `origo/`, so a bucket may hold other things, but two installations
must never share one bucket and prefix: they would overwrite each other's
log.

Nothing else is needed. Do not turn on object versioning, lifecycle
expiry, or a replication rule that rewrites keys; Origo manages the
objects' whole life itself, and a rule that deletes or rewrites one
corrupts a repository.

## 2. The issuer and the authorization endpoint

**The issuer.** Any OIDC provider with discovery and a key set over
HTTPS. Tokens it mints for people and services are what clients present
to Origo. Register a client for your platform's users, and one client per
service that will act on a user's behalf. Origo accepts a token whose
`iss` is one of the issuers you configure, character for character, and
whose `aud` contains `origo`; how a client asks your provider for that
audience is the provider's own, and the first clone at the end of this
page is where you find out whether you asked correctly.

**The authorization endpoint.** One `POST` endpoint you run. Origo sends
it a subject, an action, and a repository, and it answers whether that is
allowed:

```
POST <your endpoint>
Authorization: Bearer <the value you put in ORIGO_AUTHORIZER_TOKEN>
{"subject": "user_42", "actor": "", "repo": {"id": "…", "owner": "…", "slug": "…"}, "action": "read"}

200 {"allow": true, "ttl": 60}
200 {"allow": false, "reason": "not a member"}
```

Five rules make an endpoint safe to run. An endpoint that keeps them
serves any installation, and one that breaks any of them fails somewhere
you will find hard to read.

1. **Answer `200` for both verdicts.** Anything else, a 500, a timeout,
   a body that does not parse, is a refusal. There is no fail-open, so
   an endpoint that is down is a git plane that is down for reads as
   well as writes.
2. **Deny the repository id `00000000-0000-0000-0000-000000000001` for
   every subject and every action**, the empty subject included. That id
   is reserved as a probe, and `origod check` uses it to prove your
   endpoint reads the request rather than answering yes to everything.
3. **Key on the repository id** when your answer differs per repository.
   A clone by id sends the id alone, with no owner and no slug, and that
   is what most calls use.
4. **Decide without the repository.** An empty or unknown id is
   ordinary, not an error: it is what a creation sends, and what a name
   that resolved to nothing sends. Answer it without revealing which,
   because Origo asks before it reads any metadata, so a deny and a
   repository that does not exist look the same to a caller.
5. **Treat the endpoint's availability as Origo's.** Every repository
   operation waits on it. Put it near the nodes, answer from memory, and
   keep nothing slow in front of the answer.

**What it has to know.** Rule 4 has a consequence worth reading twice:
an endpoint that has never heard of a repository denies it, and Origo
has no call that tells it about one. So whatever holds your permissions
learns about a repository before Origo does. You choose the id, you
register it and who may use it wherever your permissions live, and only
then do you create it at Origo. In the other order the repository exists
and nobody, its owner included, can reach it. The walkthrough at the end
of this page does the two in that order.

**One tenant needs no service at all.** Nothing above asks for a
database, a permission model, or an answer that varies by repository. An
endpoint that answers `{"allow": true}` for a list of subjects,
`{"allow": false}` for everyone else, and always denies the probe id
keeps the contract in full: rule 3 does not apply when the answer is the
same everywhere, and every optional figure may be left out for Origo's
defaults. That is a few dozen lines behind the same bearer, and for a
team hosting its own repositories it is the whole of step 2.

**Where to start reading.** `test/stubs/authorizer` in this repository
is a working endpoint of about that size, and it is what the example
stack runs. Read it as a reference and do not run it in front of
anything you care about: it allows every subject unless a rule says
otherwise, it holds its rules in memory and forgets them when it
restarts, and it carries a control API that a test drives, with no
authentication on it.

Everything else is yours: your own permission model, your own answer
caching through `ttl`. The full request, the action Origo sends per
operation, and the optional figures are in [`api.md`](api.md).

**The key resolution endpoint, if you want SSH.** Origo stores no SSH
public key. It asks a second endpoint of yours which subject an offered
key belongs to, shaped like the one above and behind its own bearer:

```
POST <your key endpoint>
Authorization: Bearer <the value you put in ORIGO_SSH_KEYS_TOKEN>
{"fingerprint": "SHA256:HxK…", "type": "ssh-ed25519", "public_key": "ssh-ed25519 AAAAC3Nz…"}

200 {"found": true, "subject": "user_42", "key_id": "k_19", "ttl": 60}
200 {"found": false}
```

Five rules again, and they are the same shape as the five above.

1. **Answer `200` for both verdicts.** A 500, a timeout, or a body that
   does not parse refuses the connection. Authentication fails closed.
2. **Resolve by fingerprint, not by user name.** The SSH user name is
   always `git` and Origo does not read it.
3. **One subject per fingerprint, across the whole installation.** A
   store that lets two people register one key lets the second be
   credited with the first's pushes. Refuse a fingerprint that is
   already registered.
4. **Answer `{"found": false}` without saying why.** Unknown, revoked,
   expired, and registered to someone else look alike on the wire; the
   person sees `Permission denied (publickey)` either way.
5. **Treat its availability as Origo's.** It is on the path of every SSH
   connection that is not answered from a node's cache.

`subject` is the same string your issuer puts in `sub` for the same
person, so one identity crosses both transports and your authorization
endpoint needs no second table. Adding, naming, listing, and removing
keys is your product surface; Origo has no opinion about it, and the
call itself is how your store learns when a key was last used.

**Where to start reading.** `test/stubs/sshkeys` in this repository is a
working endpoint of about thirty lines of logic, and it is what the
example stack runs. Read it as a reference and do not run it in front of
anything you care about: it holds its table in memory, expires nothing,
and has an unauthenticated control API a test drives. For a single team,
a file of fingerprints served behind the bearer is the whole
requirement; `authorized_keys` is already that table.

If you are trying Origo out, the example overlay runs a stub issuer, a
stub authorizer, and a stub key resolver for you and you can skip this
step until you have seen it work.

## 3. Your overlay

Copy the example closest to your cluster:

```
deploy/examples/digitalocean    DigitalOcean Kubernetes with a Spaces bucket
deploy/examples/aws             EKS with an S3 bucket behind an ALB
deploy/examples/kind            a throwaway cluster that brings its own everything
```

Each is three small files over `deploy/base`, and each names what you
change at the top of its `kustomization.yaml`. In yours you set:

- **the hostname**, in the `Ingress` and in `ORIGO_PUBLIC_URL`. They must
  match: `ORIGO_PUBLIC_URL` goes into every clone URL and signs the
  short-lived tokens Origo mints, so a wrong value fails at the second
  request rather than the first. It has no default, and a node without it
  refuses to start.
- **the ingress class and your controller's settings.** Two matter for a
  git remote: no request body size limit, because a push body is a
  packfile of any size, and a read timeout of several minutes, because a
  clone of a large repository is one long response. The names differ per
  controller; `deploy/examples/kind/patches/ingress-nginx.yaml` is the
  ingress-nginx form and `deploy/examples/aws/ingress.yaml` the ALB form.
- **the replica bounds**, in the autoscaler. It scales on CPU alone and
  needs `metrics-server` in the cluster.
- **the release**, in a kustomize `images:` entry naming
  `ghcr.io/latere-ai/origod` and the tag. The base carries the
  placeholder `unreleased`, which never resolves, so an overlay that
  pins nothing fails to pull rather than running an unknown build. One
  entry covers both containers, because the node and the check in front
  of it are one image. Step 5 sets the same tag imperatively for a
  pipeline; an overlay that pins it is what makes `kubectl apply -k`
  alone reproduce the installation.
- **a registry pull secret**, if your copy of the images is private. The
  base names none, because the released images are public. Add
  `imagePullSecrets` to the pod spec in your overlay and create the
  secret in the namespace yourself.
- **`ORIGO_CACHE_BYTES`**, if the cache volume is bounded. The default is
  80% of the file system holding `ORIGO_DATA_DIR`, and the base mounts an
  `emptyDir` there with a `sizeLimit` of 20Gi: the file system underneath
  is the node's whole disk, so the default lets the cache grow far past
  the limit the kubelet evicts the pod at. Set it below the `sizeLimit`.

Then the two Secrets. Copy `deploy/bootstrap/secrets.example.yaml`, fill
it in, and apply it with `kubectl apply -f`; keep the filled copy out of
version control. It is two objects:

```yaml
# origod-s3: the bucket
ORIGO_S3_ENDPOINT: https://fra1.digitaloceanspaces.com
ORIGO_S3_REGION: fra1
ORIGO_S3_BUCKET: your-bucket
ORIGO_S3_KEY: …
ORIGO_S3_SECRET: …

# origod-auth: identity
ORIGO_OIDC_ISSUERS: https://auth.example.com
ORIGO_AUTHORIZER_URL: https://platform.example.com/internal/origo/authorize
ORIGO_AUTHORIZER_TOKEN: …
ORIGO_GOSSIP_SECRET: …    # openssl rand -hex 32, the same on every node
```

There is a third Secret, `origod-token-key`, and it is not in the
template because its value has to be generated. The next step is that.

Every variable, its default, and what it does is in
[`configuration.md`](configuration.md).

## 4. The signing key

Origo mints short-lived, repository-bound tokens, and this is the key
that signs them. It is the one thing no manifest can carry: it must be
yours, and it must be the same on every node.

First the namespace. Pod Security admission at `restricted` is what the
manifests are written for: the pods run as an unprivileged user with a
read-only root file system and no capabilities, so nothing is given up by
enforcing it.

Origo runs happily in a namespace it shares with other services: every
resource it creates is named `origod` or `origod-`something, and its two
NetworkPolicies select its own pods alone, so they restrict nothing else.
If you install into an existing namespace, set `NAMESPACE` to it, skip
the block below, and do not add those labels to it: `restricted` is
enforced on every pod in a namespace, and a workload already running
there that does not meet it would fail its next admission. Name the
namespace in your overlay's `namespace:` field, not on the command line,
so the overlay is what records where the installation lives.

```sh
kubectl apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/warn: restricted
YAML
```

Then the key.

```sh
KEY=$(mktemp)
openssl ecparam -genkey -name prime256v1 -out "$KEY" 2>/dev/null
kubectl -n "$NAMESPACE" create secret generic origod-token-key \
	--from-file=ORIGO_TOKEN_KEY="$KEY" \
	--dry-run=client -o yaml | kubectl apply -f -
rm -f "$KEY"
```

Keep a copy somewhere you keep secrets. Replacing this key invalidates
every token Origo has minted; losing it costs you nothing else, because
no repository data is encrypted with it.

Do this before the next step. Every pod reads `origod-token-key` by
name, so a rollout that starts without it waits instead of serving.

## 5. Git over SSH

Origo speaks git over HTTPS out of the box. SSH is a second transport in
front of the same repositories and the same durability: people clone
with `git@your-host:owner/slug.git` and carry a key pair instead of a
token. It is off until you turn it on, and an installation that never
sets `ORIGO_SSH_ADDR` runs exactly as it does without this step.

What SSH carries is clone, fetch, and push, and nothing else. The JSON
API and Git LFS are HTTPS. So is a read that must not be stale: over
SSH a node that is serving from its local copy while the bucket is
unreachable says so on the session's error stream, which a git client
cannot act on, so a consumer that must never read a repository that may
be behind uses HTTPS, where the answer carries a header it can refuse.

First the host keys. They are the identity of your installation, the
thing a client remembers in `known_hosts`, and every node must present
the same ones or the second node a person reaches looks like an
impostor. Generate them once, keep a copy where you keep secrets, and
never put them in your bucket.

```sh
SSHKEYS=$(mktemp -d)
ssh-keygen -q -t ed25519 -N "" -C "" -f "$SSHKEYS/ssh_host_ed25519_key"
ssh-keygen -q -t ecdsa -b 256 -N "" -C "" -f "$SSHKEYS/ssh_host_ecdsa_key"
kubectl -n "$NAMESPACE" create secret generic origod-ssh-host-key \
	--from-file=ssh_host_ed25519_key="$SSHKEYS/ssh_host_ed25519_key" \
	--from-file=ssh_host_ecdsa_key="$SSHKEYS/ssh_host_ecdsa_key" \
	--dry-run=client -o yaml | kubectl apply -f -
ssh-keygen -lf "$SSHKEYS/ssh_host_ed25519_key.pub"
```

Publish that fingerprint where your users will look. It is what they
check the first time they connect, and it is what you publish again
before you replace a key; the four-step rotation is in
[`operations.md`](operations.md).

Then four variables on the pods, in your overlay beside the rest:

```yaml
- name: ORIGO_SSH_ADDR
  value: :2222
- name: ORIGO_SSH_HOST_KEYS
  value: /etc/origo/ssh/ssh_host_ed25519_key,/etc/origo/ssh/ssh_host_ecdsa_key
- name: ORIGO_SSH_KEYS_URL
  value: https://auth.example.com/internal/origo/ssh-keys
- name: ORIGO_SSH_KEYS_TOKEN
  valueFrom:
    secretKeyRef:
      name: origod-ssh-keys
      key: ORIGO_SSH_KEYS_TOKEN
```

with the Secret mounted at `/etc/origo/ssh`. All four go together: a
node with `ORIGO_SSH_ADDR` set and any of the other three missing
refuses to start and says which. The example overlay carries all of it,
so on a throwaway cluster there is nothing to add.

Last, the address people clone from. The container listens on 2222 and
never on 22, because the pod runs as an unprivileged user with every
capability dropped and one clone URL is not worth giving it the right
to bind a privileged port. The Service `origod-ssh` publishes 22 in
front of it, and how it reaches the internet is the one choice here:

| What you have | What to do | What people clone |
|---|---|---|
| an L4 load balancer, which every managed Kubernetes has | patch `origod-ssh` to `type: LoadBalancer` with your provider's annotations; the `digitalocean` and `aws` example overlays carry the patch | `git@git.example.com:owner/slug.git` |
| an ingress controller with TCP passthrough | map external 22 to `origod-ssh:22` in its TCP services configuration; ingress-nginx has a `tcp-services` ConfigMap for exactly this | the same |
| neither | `type: NodePort`, or a load balancer on another port | `ssh://git@git.example.com:2222/owner/slug.git` |

The third row works and is not equivalent. `git@host:path` is scp
syntax and has no place to put a port, so any port but 22 puts the
`ssh://` form with the port in it into every user's remote and every CI
configuration you have. Port 22 is what buys the short URL, and that is
the whole trade-off.

## 6. Apply

Apply your overlay, pin the release you are installing, and wait for the
rollout.

```sh
kubectl apply -k "$MANIFESTS"
WORKLOAD=$(kubectl -n "$NAMESPACE" get deployment,statefulset \
	-l app.kubernetes.io/name=origod -o name | head -1)
kubectl -n "$NAMESPACE" set image "$WORKLOAD" "*=$IMAGE"
kubectl -n "$NAMESPACE" rollout status "$WORKLOAD" --timeout=300s
```

`*=` sets every container of the workload, which is the node and the
check that runs in front of it; both are the same image, and a release is
one image.

Each pod runs `origod check` before it starts serving, so a pod that
comes up ready has already reached your bucket, your issuer, and your
authorization endpoint. A pod stuck in `Init:` failed one of them, and
`kubectl -n "$NAMESPACE" logs <pod> -c check` says which.

## 7. Check

Run the same check by hand and read all seven lines. It runs against
your configuration, so this is the first thing that proves your bucket,
your issuer, and your authorization endpoint rather than the example
stack's:

```sh
POD=$(kubectl -n "$NAMESPACE" get pod -l app.kubernetes.io/name=origod \
	-o name | head -1)
kubectl -n "$NAMESPACE" exec "$POD" -c origod -- origod check
```

```
ok bucket
ok conditional-create
ok issuer
ok authorizer
ok events
ok disk
ok git: 2.47.3
```

| Line | What it proved |
|---|---|
| `bucket` | the endpoint, region, credentials, and bucket name are right and reachable |
| `conditional-create` | the store refuses a second create of a key that exists, which is what keeps two nodes from committing the same push |
| `issuer` | every issuer's discovery document and key set were fetched, so a token can be verified |
| `authorizer` | your endpoint answered, and it denied the reserved probe id |
| `events` | a signed delivery reached your webhook sink. With no sink configured the line reads `ok events: not configured`, which is not a failure; push events are optional |
| `disk` | the cache directory is writable and the file system is large enough |
| `git` | the git in the image runs and is new enough |

A `fail` line names the requirement and what went wrong, and the command
exits non-zero. Fix that one thing and run it again.

## First clone and push

Four things stand between a fresh installation and a pushed commit: the
address has to answer, you need a token, your authorization endpoint has
to know the repository, and only then does Origo create it.

**The address.** Set `ORIGO_URL` to your hostname. A rollout reports
ready a moment before the Service, the ingress, or the load balancer in
front of it routes to the new pods, and a request in that moment is
refused or reset; the loop below is what waits it out, and it gives up
rather than hanging when the address is wrong. `/version` answers the
release the node runs, and opening the same address in a browser shows a
small page naming it; there is no web interface beyond that page. The
`http://localhost:30080` below is the example stack's, the port its kind
cluster maps, and it is the default only so this page can be walked on a
throwaway cluster.

```sh
ORIGO_URL="${ORIGO_URL:-${ORIGO_TEST_URL:-http://localhost:30080}}"
n=0
until curl -sf "$ORIGO_URL/version"; do
	n=$((n + 1))
	[ "$n" -lt 60 ] || { echo "$ORIGO_URL does not answer" >&2; exit 1; }
	sleep 1
done
```

**A token.** Origo needs one from an issuer you named in
`ORIGO_OIDC_ISSUERS`, carrying the audience `origo`, for a subject your
authorization endpoint allows. Getting it is your issuer's business and
not Origo's: a service asks for one at the `token_endpoint` of the
issuer's discovery document, and a person gets one from whatever login
your platform already runs. A client credentials grant has this shape,
and it is here for its last line, which is the part that differs between
providers:

```
POST https://auth.example.com/oauth2/token
Authorization: Basic <the client id and secret>
Content-Type: application/x-www-form-urlencoded

grant_type=client_credentials&audience=origo
```

There is no standard parameter for asking for an audience. Some
providers call it `audience`, some `resource`, some derive it from a
scope you request, and some from the client's own configuration in their
console. What Origo reads is the `aud` claim of the token you end up
with, so decode the token and look at it before you go on; a token
without `origo` in `aud` is refused with 401 on every route, and so is
one whose `iss` is not one of your configured issuers.

Put the token in `ORIGO_TOKEN` and the rest of this page uses it. The
fallback below is the example stack's stub issuer, which mints a token
for any subject at `/mint`. No real issuer has that endpoint; it is here
so this page can be walked end to end on a throwaway cluster.

```sh
TOKEN="${ORIGO_TOKEN:-$(curl -sf -X POST \
	"${ORIGO_EXAMPLE_ISSUER:-http://localhost:30081}/mint" \
	-d '{"sub":"install-doc"}' | sed 's/.*"token":"\([^"]*\)".*/\1/')}"
test -n "$TOKEN"
```

**Register the repository before you create it.** Origo asks your
authorization endpoint about a repository before it creates one, sending
the id, the owner, and the slug from the request body with the action
`admin`, and it asks again on every clone and every push afterwards. An
endpoint that has not been told about this repository denies it, so the
create answers 403 carrying whatever reason your endpoint gave, and
nothing you do at Origo fixes that. So: choose the id, tell whatever
holds your permissions about it and grant the subject you hold a token
for, and only then run the next block. How you tell it is yours entirely
and Origo never sees that call.

On the example stack there is nothing to do here, because its stub
authorizer allows every subject on every repository.

Now create it. The id is a UUID you choose, so your own records can carry
it and Origo's name for a repository never changes even when its owner or
slug does. Set `ORIGO_REPO_ID`, `ORIGO_REPO_OWNER`, and `ORIGO_REPO_SLUG`
to the repository you just registered; unset, the block invents an id and
a name, which is what the example stack wants and what no endpoint of
yours would allow:

```sh
ID="${ORIGO_REPO_ID:-$(uuidgen | tr 'A-Z' 'a-z')}"
OWNER="${ORIGO_REPO_OWNER:-install}"
SLUG="${ORIGO_REPO_SLUG:-hello-$$}"
curl -sf -X POST "$ORIGO_URL/v1/repos" \
	-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
	-d "{\"id\":\"$ID\",\"owner\":\"$OWNER\",\"slug\":\"$SLUG\"}" >/dev/null
echo "created $ID"
```

A 403 here is the registration and not the token: the token got you past
authentication, and then your endpoint refused the repository. Its own
reason comes back in `details.reason`.

Push to it and read it back. A repository with no history clones empty,
which is not an error. `http.extraHeader` puts the token in a request
header, so it is exactly as private as the connection: over the plain
`http` address of a throwaway cluster it travels in the clear, and over
the `https` hostname of your installation it does not.

```sh
WORK=$(mktemp -d)
REMOTE="$ORIGO_URL/r/$ID.git"
git -C "$WORK" init -q -b main first
cd "$WORK/first"
echo hello > README.md
git add README.md
git -c user.email=install@example.com -c user.name=install commit -qm "first commit"
git -c http.extraHeader="Authorization: Bearer $TOKEN" push -q "$REMOTE" main
cd - >/dev/null
```

```sh
git -c http.extraHeader="Authorization: Bearer $TOKEN" clone -q "$REMOTE" "$WORK/second"
test "$(cat "$WORK/second/README.md")" = hello
rm -rf "$WORK"
echo "the installation serves a clone and a push"
```

That push is durable: it was written to object storage and acknowledged
only then. You can delete every node and the repository is unchanged.

**And over SSH.** If you did step 5, the same repository clones with a
key pair and no token. Three things have to line up: the person's public
key is registered at your key resolution endpoint under a subject your
authorization endpoint allows, their client trusts the host key you
published, and the address is the one your Service publishes.

Register the key wherever your key store lives. The block below
registers at the example stack's stub, which is not an endpoint any real
installation has; on your own installation this is a call into your own
product and Origo never sees it.

```sh
CLIENTKEY=$(mktemp -d)/id_ed25519
ssh-keygen -q -t ed25519 -N "" -C "" -f "$CLIENTKEY"
FINGERPRINT=$(ssh-keygen -lf "$CLIENTKEY.pub" | awk '{print $2}')
curl -sf -X PUT "${ORIGO_EXAMPLE_SSHKEYS:-http://localhost:30086}/keys" \
	-d "{\"keys\":[{\"fingerprint\":\"$FINGERPRINT\",\"subject\":\"install-doc\"}]}"
echo "registered $FINGERPRINT"
```

Then trust the host key and clone. `ORIGO_SSH_URL` is the address your
Service publishes; the default is the example stack's, whose kind
cluster maps SSH to a host port rather than to 22.

```sh
SSH_URL="${ORIGO_SSH_URL:-ssh://git@localhost:30022}"
SSH_HOST=$(echo "$SSH_URL" | sed 's|.*@||; s|/.*||')
SSH_PORT=$(echo "$SSH_HOST" | sed 's|.*:||')
case "$SSH_HOST" in *:*) SSH_HOST=${SSH_HOST%:*} ;; *) SSH_PORT=22 ;; esac
KNOWN="$CLIENTKEY.known_hosts"
ssh-keyscan -T 20 -p "$SSH_PORT" "$SSH_HOST" > "$KNOWN" 2>/dev/null
test -s "$KNOWN"
GIT_SSH_COMMAND="ssh -i $CLIENTKEY -o IdentitiesOnly=yes \
	-o UserKnownHostsFile=$KNOWN -o GlobalKnownHostsFile=/dev/null \
	-o StrictHostKeyChecking=yes -o BatchMode=yes" \
	git clone -q "$SSH_URL/r/$ID.git" "$CLIENTKEY.clone"
test "$(cat "$CLIENTKEY.clone/README.md")" = hello
echo "the installation serves a clone over SSH"
```

`ssh-keyscan` here reads the key the installation presents, which is
what a person does once before their first clone; compare what it prints
against the fingerprint you published in step 5 rather than trusting it
blind.

## Pointing your platform at Origo

Your platform holds the users and the permissions; Origo holds the
repositories. Three things connect them:

- **The authorization endpoint** you wrote in step 2. It is the whole
  permission model, and Origo asks it every time.
- **The order of a creation.** Choose the id, register the repository
  and its permissions with your endpoint, then `POST /v1/repos`. The
  reverse order leaves a repository at Origo that every caller is denied,
  including its owner, and the repair is to register it and try again.
  Registered but never created is the safer half-failure: the repository
  simply answers 404 until the create succeeds.
- **Repository ids.** Choose the UUID, store it, and address repositories
  by `/r/{id}.git`. That URL survives a rename or a transfer.
- **Repository-bound tokens.** When your platform needs to hand a
  short-lived credential to a build, a bot, or a checkout, it asks Origo
  for one scoped to a single repository rather than passing a user's own
  token around. The call is in [`api.md`](api.md).

Push events are optional and useful: set `ORIGO_EVENTS_URL` and
`ORIGO_EVENTS_SECRET`, and every reference update arrives at your sink as
a signed webhook, so a build starts from a push rather than a poll.

## When something fails

| What you see | What it means | What to do |
|---|---|---|
| the pod stays in `Init:0/1` | `origod check` is failing | `kubectl logs <pod> -c check`; the failing line names the requirement |
| the pod stays in `Init:Error`, and the failing line is a dependency you have not brought up yet | the init container gates the node on every requirement, so an installation whose issuer or authorization endpoint is not serving yet has no ready node at all, even though a running node would start and answer `/readyz` without them | the init container retries with backoff and the pods go ready by themselves the moment the dependency answers; nothing is redeployed. Bring the dependency up rather than removing the gate |
| the pod stays in `ContainerCreating` or `CreateContainerConfigError` | a Secret the pod reads is missing | `kubectl describe pod <pod>` names it: `origod-s3` and `origod-auth` come from step 3, `origod-token-key` from step 4 |
| `configuration: missing …` in the log | a variable is unset or malformed | the message names every problem at once; fix them all and roll again |
| `fail bucket` | the endpoint, region, credentials, or bucket name is wrong, or the network refuses the connection | check the four values in the `origod-s3` Secret, then reach the endpoint from a pod in the namespace |
| `fail conditional-create` | the store accepted a second create of a key that exists | this store cannot host Origo safely. Ask your provider about `If-None-Match: *` on `PUT`, or move the bucket |
| `fail issuer` | the discovery document or the key set did not answer over HTTPS | open `<issuer>/.well-known/openid-configuration` from a pod in the namespace |
| `fail authorizer: probe id allowed` | your endpoint allows the reserved probe id, so it is not reading the request | deny that id for every subject, then run the check again |
| `fail authorizer` with a status | your endpoint refused Origo's bearer or returned an error | check `ORIGO_AUTHORIZER_TOKEN` on both sides |
| `fail disk` | the cache directory is not writable, or `ORIGO_CACHE_BYTES` exceeds the file system | check the volume in your overlay |
| the pod is ready and `/readyz` is 503 | the bucket or the disk stopped answering after start-up | the node's log names the check that fails |
| a push hangs or is cut off | the ingress controller is buffering the body or timing out | the two settings in step 3: no body size limit, and a long read timeout |
| a clone works and a push is refused | your authorization endpoint denies `write` for that subject | its answer carries a `reason`, which Origo passes back to the client |
| every operation on one repository is 403, `POST /v1/repos` included | your authorization endpoint has not been told about this repository, and rule 4 of step 2 makes it deny what it does not know | register the repository there under the id you gave Origo and grant the subject; the endpoint's own `reason` is in `details.reason` on the 403 |
| every operation on every repository is 403 | the endpoint is answering, and refusing this subject | check that the subject in the token is the one you granted: it is the token's `sub`, or its `act` when a service is acting for someone |
| 401 on every request | the token's issuer is not in `ORIGO_OIDC_ISSUERS`, or its audience is not `origo` | decode the token and compare |

Origo's own alert rules, for a cluster running the Prometheus operator,
are `deploy/base/prometheusrule.yaml`; they are not applied by the base,
because they need that operator's custom resource.

## After it works

- [`operations.md`](operations.md): backup and restore, upgrades,
  scaling, and what to do during an outage.
- [`configuration.md`](configuration.md): every variable with its default.
- [`api.md`](api.md): what a consumer codes against.
- [`migration.md`](migration.md): moving repositories in from another
  git host.
- [`upgrades/`](upgrades/README.md): what a version promises and how to
  move between them.
