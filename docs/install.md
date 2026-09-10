# Installing Origo

For whoever runs Origo. It takes about an hour on a cluster you already
have, and this page is the whole of it: what to prepare, what to apply,
how to tell whether it worked, and what to do when it did not.

Origo is one stateless binary. Object storage holds every repository;
local disk is a cache a node rebuilds on demand. So there is no database
to run, no leader to elect, and no volume to back up. What you supply is
a bucket, an identity provider, an endpoint that answers who may do what,
and a hostname.

Every command on this page is run by the project's own tests against a
throwaway cluster before it is published, so a command that drifts from
the manifests fails the build rather than your installation.

## What you need

| | What | Notes |
|---|---|---|
| Kubernetes | 1.29 or newer | any distribution. Pod Security admission at `restricted` on the namespace is supported and recommended. |
| An ingress controller | any | the manifests carry an `Ingress` with no class; your overlay names the controller and its settings. |
| A hostname and a certificate | one name pointed at the ingress | clients only ever see this name. |
| A bucket | any S3 compatible endpoint | it must honour a conditional create, `PUT` with `If-None-Match: *`. That is what linearizes pushes. MinIO, DigitalOcean Spaces, and AWS S3 are known to. `origod check` proves it before you trust it. |
| An OIDC issuer | discovery and a key set over HTTPS | it mints the tokens people and services present. Register one client for people and one for each service that acts on their behalf. |
| An authorization endpoint | one HTTP endpoint you run | Origo asks it, before every repository operation, whether a subject may read, write, or administer a repository. |
| Disk | a default storage class, or nodes with local disk | the cache. Sized for the repositories in active use, not for all of them. |
| On your machine | `kubectl`, `openssl`, `curl`, `uuidgen`, `git` | nothing is installed in the cluster beyond the manifests. |

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

The rest of the page reads these. Set them for your installation; the
defaults install the example overlay on a throwaway cluster.

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
service that will act on a user's behalf. Origo accepts tokens whose
audience is `origo`.

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

Two rules make it safe to run:

- It must deny the repository id `00000000-0000-0000-0000-000000000001`
  for every subject. That id is reserved as a probe, and `origod check`
  uses it to prove your endpoint reads the request rather than answering
  yes to everything.
- Anything other than `200` with an `allow` field is treated as a refusal.
  Origo fails closed.

Everything else is yours: your own permission model, your own answer
caching through `ttl`. The full request and answer are in
[`api.md`](api.md).

If you are trying Origo out, the example overlay runs a stub issuer and a
stub authorizer for you and you can skip this step until you have seen it
work.

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

## 5. Apply

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

## 6. Check

Run the same check by hand and read all seven lines:

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
| `events` | a signed delivery reached your webhook sink, or you have configured none |
| `disk` | the cache directory is writable and the file system is large enough |
| `git` | the git in the image runs and is new enough |

A `fail` line names the requirement and what went wrong, and the command
exits non-zero. Fix that one thing and run it again.

## First clone and push

Wait for your hostname to answer, then get a token from your issuer for a
subject your authorization endpoint allows. A rollout reports ready a
moment before the Service, the ingress, or the load balancer in front of
it routes to the new pods, and a request in that moment is refused or
reset; the loop below is what waits it out, and it gives up rather than
hanging when the address is wrong. `/version` answers the release the
node runs, and opening the same address in a browser shows a small page
naming it; there is no web interface beyond that page. The fallbacks below are the example stack's stub issuer, so
this section runs against a trial installation with nothing set.

```sh
ORIGO_URL="${ORIGO_URL:-${ORIGO_TEST_URL:-http://localhost:30080}}"
n=0
until curl -sf "$ORIGO_URL/version"; do
	n=$((n + 1))
	[ "$n" -lt 60 ] || { echo "$ORIGO_URL does not answer" >&2; exit 1; }
	sleep 1
done
TOKEN="${ORIGO_TOKEN:-$(curl -sf -X POST \
	"${ORIGO_EXAMPLE_ISSUER:-http://localhost:30081}/mint" \
	-d '{"sub":"install-doc"}' | sed 's/.*"token":"\([^"]*\)".*/\1/')}"
test -n "$TOKEN"
```

Create a repository. The id is a UUID you choose, so your own records can
carry it and Origo's name for a repository never changes even when its
owner or slug does:

```sh
ID=$(uuidgen | tr 'A-Z' 'a-z')
curl -sf -X POST "$ORIGO_URL/v1/repos" \
	-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
	-d "{\"id\":\"$ID\",\"owner\":\"install\",\"slug\":\"hello-$$\"}" >/dev/null
echo "created $ID"
```

Push to it and read it back. A repository with no history clones empty,
which is not an error:

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

## Pointing your platform at Origo

Your platform holds the users and the permissions; Origo holds the
repositories. Three things connect them:

- **The authorization endpoint** you wrote in step 2. It is the whole
  permission model, and Origo asks it every time.
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
