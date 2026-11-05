# Running it locally

`origod` on your own machine, from a clone: MinIO for the object
storage, a stand-in issuer, a stand-in authorizer and a stand-in event
receiver, and requests that show a repository being pushed to and read
back. One command does the setup, and a second terminal sends the
requests.

Everything here is for trying Origo. The stand-in issuer signs a token
for anyone who asks, the stand-in authorizer allows the made-up person
you get a token for, and the repositories live in a container volume
that `make clean` deletes. [Install](install.md) is the page for a real
installation.

## What you need

- Go 1.27 or newer, to build `origod` and the stand-ins.
- `git`, `curl` and `openssl`. `openssl` makes the key that signs
  repository tokens.
- Docker or Podman, with its `compose` command, for MinIO.

`make` uses `docker` when it finds one on your path and `podman`
otherwise. To pick one yourself, set `DEV_ENGINE`:

```sh
make dev DEV_ENGINE=podman
```

With Podman, point `DOCKER_HOST` at Podman's socket first, so the
Compose commands find the engine:

```sh
export DOCKER_HOST="unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')"
```

## Start it

In one terminal, from the root of a clone:

```sh
git clone https://github.com/latere-ai/origo.git
cd origo
make dev
```

`make dev` stays in the foreground and prints the node's log. It does
these things, in this order:

1. **Builds** `out/origod`, the node, and `out/origo-stubs`, the
   stand-ins.
2. **Starts MinIO** from `docker-compose.yml`, with a one-shot container
   that creates the bucket `origo` and exits. It waits until MinIO
   answers and the bucket exists:

   ```
   MinIO is ready at http://127.0.0.1:22182 with bucket origo
   ```

3. **Starts the stand-ins** in the background, once per checkout:

   ```
   stubs are ready: issuer http://localhost:22184, authorizer http://127.0.0.1:22185, sink http://127.0.0.1:22186
   ```

   The issuer signs a token for whoever asks at its `/mint` address and
   serves the keys that verify it. The authorizer allows the made-up
   person `dev`, and refuses the one reserved repository every
   authorizer must refuse. The sink is the receiver of push events: it
   checks each event's signature, answers 200, and keeps every event it
   got for you to read. [Writing an authorizer](authorizer.md) describes
   the real endpoint, and [the API](api.md#push-events) the real events.

4. **Makes the token key.** The first run writes an EC key to
   `out/dev/origo/token-key.pem`, which the node signs
   repository-bound tokens with. Later runs reuse it.
5. **Starts the node.** `make dev` prints the line it runs, with every
   setting the node reads, and then the node's log, one JSON line for
   each event:

   ```
   {"time":"2026-11-05T20:29:52.36863+01:00","level":"INFO","msg":"serving","service":"origod","version":"v0.16.9","public":"[::]:22180","internal":"[::]:22181", ...}
   {"time":"2026-11-05T20:29:52.372594+01:00","level":"INFO","msg":"issuer keys fetched","service":"origod","issuer":"http://localhost:22184", ...}
   ```

   The version is that of your checkout, so yours differs.
6. **Creates a repository and prints a clone line.** Once the node
   answers its readiness check, `make dev` gets a token for `dev` from
   the issuer, creates the repository `dev/hello` if it does not exist,
   and prints the line to clone it. The token is good for one hour:

   ```
   git clone http://x:<token>@localhost:22180/dev/hello.git
   ```

   Log lines can come before and after it, because the node keeps
   running. The line in your terminal holds a real token.

**The ports.** Every port counts up from a base that `make` computes from
the name of the directory you cloned into. For a checkout named `origo`
it is 22180, and the ports above are that base plus 0 for git and the
API, 1 for the probes (`/readyz`, `/livez`, `/metrics`), 2 for MinIO, 3
for its console, 4 for the issuer, 5 for the authorizer and 6 for the
event sink. A directory with another name gets other ports, which lets
two clones run side by side, so read your own from the printed lines.
The stand-ins and MinIO answer on `127.0.0.1`. The node itself listens
on every interface of the machine, and the stand-in issuer will sign a
token for anyone who reaches it, so run it on a machine you trust, or
behind a firewall. To pick the base yourself, set `DEV_PORT_BASE`.

## Send requests

Open a second terminal, in any directory outside the clone. Paste the
clone line from the first one: it makes an empty repository, and its
remote already carries the token.

```sh
git clone http://x:<token>@localhost:22180/dev/hello.git
cd hello
```

```
Cloning into 'hello'...
warning: You appear to have cloned an empty repository.
```

If git does not know who you are, tell it, for this repository only:

```sh
git config user.name "Ada Example"
git config user.email ada@example.com
```

### Push a commit

```sh
git commit --allow-empty -m first
git push origin HEAD:main
git ls-remote origin
```

```
[main (root-commit) 6de089f] first
To http://localhost:22180/dev/hello.git
 * [new branch]      HEAD -> main
6de089f4e9b2ad31da97ac0adea323d5c2fa1459	HEAD
6de089f4e9b2ad31da97ac0adea323d5c2fa1459	refs/heads/main
```

Your commit ids differ from these, because a commit id covers your name and the
time. Add a file and push again, so there is something to read:

```sh
echo "hello, origo" > hello.txt
git add hello.txt
git commit -m "add hello.txt"
git push origin HEAD:main
```

```
To http://localhost:22180/dev/hello.git
   6de089f..3c59f9a  HEAD -> main
```

### Read it through the API

The same repository is readable without a clone. Put the token in a
header variable, and the repository's id in another. The id of
`dev/hello` is fixed by `make dev`:

```sh
TOKEN=<token>
A="Authorization: Bearer $TOKEN"
URL=http://localhost:22180
R=0d5e7a1c-6f2b-4c3d-9e8f-1a2b3c4d5e6f
```

Look the repository up by name:

```sh
curl -sS -H "$A" "$URL/v1/repos?owner=dev&slug=hello"
```

```json
{"id":"0d5e7a1c-6f2b-4c3d-9e8f-1a2b3c4d5e6f","owner":"dev","slug":"hello","default_branch":"main","size_bytes":409,"head":"3c59f9aadae0d560f32759533e937ec1223f0430","updated_at":"2026-11-05T19:29:52.986787Z","pushed_at":"2026-11-05T19:30:25.688749Z","frozen_at":null,"verified_at":null,"verified_equal":null}
```

Its references, its history (newest commit first) and the files of the head:

```sh
curl -sS -H "$A" "$URL/v1/repos/$R/refs"
curl -sS -H "$A" "$URL/v1/repos/$R/commits"
curl -sS -H "$A" "$URL/v1/repos/$R/tree/main"
```

```json
[{"name":"refs/heads/main","sha":"3c59f9aadae0d560f32759533e937ec1223f0430","peeled":null}]
```

```json
{"commits":[{"sha":"3c59f9aadae0d560f32759533e937ec1223f0430","parents":["6de089f4e9b2ad31da97ac0adea323d5c2fa1459"],"author":{"name":"Ada Example","email":"ada@example.com","at":"2026-11-05T20:30:25+01:00"},"committer":{"name":"Ada Example","email":"ada@example.com","at":"2026-11-05T20:30:25+01:00"},"message":"add hello.txt\n","trailers":[]},{"sha":"6de089f4e9b2ad31da97ac0adea323d5c2fa1459","parents":[],"author":{"name":"Ada Example","email":"ada@example.com","at":"2026-11-05T20:30:24+01:00"},"committer":{"name":"Ada Example","email":"ada@example.com","at":"2026-11-05T20:30:24+01:00"},"message":"first\n","trailers":[]}],"next_cursor":null}
```

```json
{"entries":[{"path":"hello.txt","mode":"100644","type":"blob","sha":"da92e74c6967fcf093bfc8969177bd3b62426309","size":13}],"next_cursor":null}
```

The file's bytes come from its object id, the `sha` in the tree:

```sh
curl -sS -H "$A" "$URL/v1/repos/$R/blob/da92e74c6967fcf093bfc8969177bd3b62426309"
```

```
hello, origo
```

### Create a second repository and commit to it

A repository is created with an id you choose, an owner and a name:

```sh
curl -sS -X POST -H "$A" -H "Content-Type: application/json" "$URL/v1/repos" -d '{"id":"7c1b9a30-5d2e-4f81-a6b3-0e9d8c7b6a54","owner":"dev","slug":"notes"}'
```

```json
{"id":"7c1b9a30-5d2e-4f81-a6b3-0e9d8c7b6a54","owner":"dev","slug":"notes","default_branch":"main","size_bytes":0,"head":"","updated_at":"2026-11-05T19:30:26.066569Z","pushed_at":null,"frozen_at":null,"verified_at":null,"verified_equal":null}
```

Instead of pushing, ask the node to make the first commit. In an empty
repository the branch is created from nothing, so `create_branch` is
`true` and both `from` and `expected_head` are `null`. A file's content
is base64, here `a note` and a newline:

```sh
curl -sS -X POST -H "$A" -H "Content-Type: application/json" "$URL/v1/repos/7c1b9a30-5d2e-4f81-a6b3-0e9d8c7b6a54/commits" -d '{"branch":"main","create_branch":true,"from":null,"expected_head":null,"author":{"name":"Ada Example","email":"ada@example.com"},"message":"add a note","changes":[{"path":"notes.txt","content":"YSBub3RlCg=="}]}'
```

```json
{"commit":"bf80b4964a08476caf4fea1aa22c90375d7043e4","branch":"refs/heads/main","entry_seq":1,"tree":"ff83d2c8897b6d5fc65113a1e77fd65f28edb0e1"}
```

Git sees the commit too:

```sh
cd ..
git clone "http://x:$TOKEN@localhost:22180/dev/notes.git"
cat notes/notes.txt
```

```
Cloning into 'notes'...
a note
```

On a branch that exists, the same route needs `expected_head` set to the
commit the branch is at, and answers 409 if the branch has moved.
[The API](api.md#writing-without-a-clone) lists the other write routes.

### See the events the push made

The node posts one signed event for every push, and for the commit above,
to the stand-in sink. The sink keeps them, and its port is the base plus
6:

```sh
curl -sS http://127.0.0.1:22186/deliveries | jq -c '.[-1] | {kind, verified, status, body}'
```

```json
{"kind":"push","verified":true,"status":200,"body":{"id":"ad232bb0-7f47-5765-96f0-286379884d21","kind":"push","repo":"7c1b9a30-5d2e-4f81-a6b3-0e9d8c7b6a54","seq":1,"owner":"dev","slug":"notes","pusher":{"sub":"http://localhost:22184|dev"},"updates":[{"ref":"refs/heads/main","before":"0000000000000000000000000000000000000000","after":"bf80b4964a08476caf4fea1aa22c90375d7043e4","forced":false}],"at":"2026-11-05T19:30:26.513193Z","operation":"commits"}}
```

`verified` says the sink checked the signature against the shared
secret, and `operation` names the route that made the commit. Without
`jq`, drop the pipe: the answer is the whole list, with each event's
headers.

### Use the `origo` command

[`origo`](cli.md) reads a repository from a shell without a clone. It
takes a token bound to one repository, which a token with `admin` on
that repository mints. From the root of your Origo clone, build it, mint a read token for `dev/hello`, and
set the variables it reads:

```sh
go build -o out/origo ./cmd/origo
curl -sS -X POST -H "$A" -H "Content-Type: application/json" "$URL/v1/repos/$R/tokens" -d '{"scope":"read","ttl":3600}'
```

```json
{"expires_at":"2026-11-05T20:29:52Z","token":"<token>"}
```

```sh
export ORIGO_URL=$URL ORIGO_TOKEN=<that token> ORIGO_REPO=dev/hello
out/origo info
out/origo log
out/origo cat hello.txt
```

```
repo     0d5e7a1c-6f2b-4c3d-9e8f-1a2b3c4d5e6f
name     dev/hello
branch   main
head     3c59f9aadae0d560f32759533e937ec1223f0430
size     409
updated  2026-11-05T19:29:52Z
pushed   2026-11-05T19:30:25Z
```

```
3c59f9a 2026-11-05 Ada Example add hello.txt
6de089f 2026-11-05 Ada Example first
```

```
hello.txt@da92e74  13 bytes  1 lines
hello, origo
```

A repository token ends within the hour and never outlives the token
that minted it. It cannot list repositories: `origo repos` answers
`forbidden`, and the stand-in authorizer has no directory to list from
either, so `GET /v1/repos` without a name answers
`directory_unsupported`. Everything above names its repository.

## Stop it

In the first terminal, press Ctrl-C. The node finishes its requests in
a few seconds and exits, and the stand-ins stop with it. MinIO keeps
running, so the next `make dev` starts quickly and finds your
repositories: it prints a new clone line, and cloning it gives back the
commits you pushed.

If you stop only the `make` process by some other means, the node and
the stand-ins can outlive it. `make dev-down` stops the stand-ins; the
node is the process started as `out/origod`.

- `make dev-down` stops the stand-ins if they still run, and removes the
  MinIO containers and the network. It keeps the volume and `out/`, so
  the next `make dev` finds the same repositories.
- `make clean` removes the containers and the volume, which holds every
  repository, and deletes `out/`, which holds the binaries, the token
  key and the node's local copy under `out/dev/`. Run it to start over
  from nothing.

## What next

- [Install](install.md): a real installation, with your own bucket,
  issuer and hostname.
- [Configuration](configuration.md): every setting `origod` reads, and
  the ones `make dev` sets.
- [The API](api.md): every route, including merges, transfers and
  imports.
- [Writing an authorizer](authorizer.md): deciding who may do what.
- [The origo command](cli.md): reading and committing from a shell, and
  from an agent.
- [How it works](internals/architecture.md) and
  [testing](internals/testing.md): the log in object storage, and the
  tiers that run against this same stack.
