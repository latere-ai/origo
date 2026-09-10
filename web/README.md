# origoweb

A read-only web interface for an [Origo](../README.md) installation:
repositories, branches and tags, the commit log, a commit's diff, the file
tree, a file. In the spirit of cgit and sourcehut.

No comments, no reviews, no stars, no forks, no issues. It is a window onto a
git repository for people who already know which repository they want.

## What it is

One binary. It serves HTML, and it is a client of Origo's read API and
nothing else: it never runs git, never reads object storage, and never
decides who may see what. It sends your own token to the installation and
shows you what comes back, so what you can see here is exactly what you can
clone.

Every page works with JavaScript disabled, because there is no JavaScript.
Every control is a link, a form, or a native `<details>`. Light and dark
follow your system setting.

## Screens

| Screen | Address |
|---|---|
| sign in | `/sign-in` |
| repositories | `/` |
| overview: clone lines, the reference selector, the root tree, the readme | `/r/{id}` |
| branches and tags | `/r/{id}/refs` |
| commit log, filterable by path | `/r/{id}/log` |
| a commit, its message, its parents and its diff | `/r/{id}/commit/{sha}` |
| any two revisions compared | `/r/{id}/compare?base=&head=` |
| the file tree | `/r/{id}/tree/{path}` |
| a file, with line anchors | `/r/{id}/blob/{path}` |
| the bytes of a file | `/r/{id}/raw/{path}` |

A branch or a tag is chosen with `?ref=`, which is what the selector on the
overview submits, so any screen at any revision is a URL you can send to
someone.

**Repositories are addressed by identifier, not by name.** Origo's JSON API
answers questions about a repository you already name by its identifier and
has no way to resolve `owner/name` or to list what you may see. Until it
does, the home page is a box you paste an identifier into and a list of what
you have opened in this session, and the names are shown on every page even
though the address is the identifier. See [Limits](#limits).

## Run it

You need an Origo installation, a second OIDC client registered for the
browser flow whose tokens carry Origo's audience, and a cookie key.

```sh
export ORIGOWEB_ORIGO_URL=https://git.example.com
export ORIGOWEB_PUBLIC_URL=https://code.example.com
export ORIGOWEB_AUTH_URL=https://auth.example.com
export ORIGOWEB_AUTH_CLIENT_ID=origoweb
export ORIGOWEB_AUTH_CLIENT_SECRET=...
export ORIGOWEB_AUTH_AUDIENCE=origo
export ORIGOWEB_AUTH_COOKIE_KEY=$(openssl rand -hex 32)
make build && out/origoweb
```

Then point a hostname at it and open it. Nothing about your Origo
installation changes, and an operator who skips all of this has an
installation that works exactly as before.

**Give it its own hostname.** Origo's git surface claims
`/{owner}/{name}/...` for clone and push, which is the shape a browsing
interface wants; one hostname cannot serve both. `code.example.com` in front
of this and `git.example.com` in front of Origo.

### Settings

| Setting | Default | What it does |
|---|---|---|
| `ORIGOWEB_ADDR` | `:8080` | the address it listens on |
| `ORIGOWEB_ORIGO_URL` | required | the Origo installation, the one address it talks to |
| `ORIGOWEB_PUBLIC_URL` | required | its own address, used to build the sign-in redirect |
| `ORIGOWEB_CLONE_HOST` | `ORIGOWEB_ORIGO_URL` | what the clone lines say, when your git host differs from your API address |
| `ORIGOWEB_SSH_CLONE_HOST` | unset | the host in the SSH clone line; unset shows the HTTPS line alone |
| `ORIGOWEB_ISSUER_NAME` | unset | what the sign-in button calls your identity provider |
| `ORIGOWEB_KEYS_URL` | unset | a key management surface, if you have one; see [Limits](#limits) |
| `ORIGOWEB_AUTH_URL` | `https://auth.latere.ai` | your OIDC issuer, which must be one of Origo's `ORIGO_OIDC_ISSUERS` |
| `ORIGOWEB_AUTH_CLIENT_ID` | required | the client registered for the browser flow |
| `ORIGOWEB_AUTH_CLIENT_SECRET` | unset | a confidential client's secret; a public client uses PKCE alone |
| `ORIGOWEB_AUTH_AUDIENCE` | the issuer | must be the audience Origo verifies, normally `origo` |
| `ORIGOWEB_AUTH_COOKIE_KEY` | required | 32 bytes of hex; rotating it signs everyone out |

`deploy/` holds a Kubernetes base: a Deployment of two stateless replicas, a
Service, an Ingress, and the three secrets above.

## Signing in

You sign in with the account your organisation already gave you. There is no
password here and no sign-up. Your access token lives in one encrypted
cookie, is sent to Origo unchanged, and is in no page, no address bar and no
log line. A session lasts twelve hours from sign-in; refreshing your token
does not extend it.

What you may read is Origo's decision and not this interface's. A repository
you cannot see and a repository that does not exist read the same, because
that is a difference Origo deliberately refuses to make.

## Limits

Three things are missing because no component can answer them yet, not
because they were left out.

- **A list of your repositories.** Origo answers questions about one
  repository at a time, and the service that decides who may see what
  answers one yes or no at a time, with no way to enumerate. The home page
  degrades to a box and a per-session history until both grow the question.
- **Public repositories.** Origo requires a credential on every read, so a
  signed-out visitor sees the sign-in page and nothing else. Every read here
  is already issued without a credential when there is none, so the day
  Origo answers one, the same page shows the repository.
- **SSH keys.** Public keys are held outside Origo, behind a resolver Origo
  only reads, and no component owns a place to add or remove one. The screen
  and its navigation entry appear only when `ORIGOWEB_KEYS_URL` names a
  surface, which is never by default.

Two more, on purpose: there is no syntax highlighting, and no blame. Neither
is needed to read a diff, and both are a large dependency over file content
nobody vetted.

## Where it lives

This directory is a Go module of its own, `github.com/latere-ai/origo-web`,
sitting inside the Origo repository until its own exists. Nothing here can
import Origo's internals, which is the point: it speaks the same published
API a third-party client would.

MIT, like Origo. The typeface is Inter under the SIL Open Font License,
served from the binary; the monospace stack is your system's.
