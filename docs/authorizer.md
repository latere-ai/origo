# The authorization endpoint

For whoever writes the endpoint that decides what Origo allows, and the
optional endpoint that resolves SSH keys. Origo stores no user, no
team, no role, and no key. Before every operation it asks your
endpoint, and it adds nothing to the answer. This page is the whole
contract of both calls. The operator's side, where each URL and bearer
is configured and in what order to register and create a repository,
is step 2 of [`install.md`](install.md).

## Without an endpoint

Leave `ORIGO_AUTHORIZER_URL` unset and Origo runs a built-in owner
policy instead of calling anything:

- any authenticated subject may create a repository;
- the subject that created a repository may do anything to it;
- the subjects listed in `ORIGO_ADMIN_SUBJECTS`, each written
  `<issuer>|<sub>`, may do anything to every repository;
- everyone else, and every anonymous caller, is denied;
- the directory lists the repositories a subject created.

That is enough for one person or one team hosting their own
repositories. Write an endpoint when permissions live somewhere else:
organizations, teams, plans, public repositories, or anything that
changes without Origo knowing.

## The request

Origo sends one `POST` per decision it does not hold in its cache:

```
POST <ORIGO_AUTHORIZER_URL>
Authorization: Bearer <ORIGO_AUTHORIZER_TOKEN>
Content-Type: application/json

{
  "subject":  "https://auth.example.com|0f5c1d2e-…",
  "issuer":   "https://auth.example.com",
  "sub":      "0f5c1d2e-…",
  "claims":   {…},
  "action":   "repo.read",
  "resource": {"kind": "Repository", "id": "…", "owner": "…", "slug": "…"},
  "request":  {"id": "…", "ip": "203.0.113.4", "user_agent": "git/2.47"}
}
```

| Field | Value |
|---|---|
| `subject` | the issuer URL with any trailing slash removed, a pipe, and the token's `sub`. Empty for an anonymous request and for the probe described below. Over SSH it is the subject your key endpoint returned |
| `issuer`, `sub` | the two halves apart, so an endpoint keyed by issuer does not split the string. Empty over SSH |
| `claims` | every verified claim of the token, verbatim. Origo reads none of them, so a plan, a team, or a role is read here |
| `action` | `repo.read`, `repo.write`, `repo.admin`, or `repo.list` |
| `resource.kind` | `Repository`, always |
| `resource.id` | the repository id, a lower-case UUID. Empty for a name Origo could not resolve, and absent on `repo.list` |
| `resource.owner`, `resource.slug` | set when the request named the repository by owner and slug, and on a creation; empty when it named the id |
| `request` | the request id Origo logs the call under, the caller's IP, and its user agent, for an endpoint that audits or rate-limits |

Check the bearer first. It is how the endpoint tells Origo from
anything else that reaches it.

## The answer

Answer 200 with a verdict, allow or deny:

```
200 {"allow": true, "ttl": 60, "limits": {"replicas": 1, "quota_bytes": 53687091200, "requests_per_minute": 600}}
200 {"allow": false, "reason": "not a member"}
```

A deny's `reason` reaches the caller as `details.reason` of a 403
`forbidden`, so write it for the person who will read it.

Every field of an allow but `allow` is optional, and an endpoint that
sends `{"allow": true}` alone is complete:

| Field | Omit it when | Send it when |
|---|---|---|
| `ttl` | 60 seconds of revocation lag suits you | you want fewer calls; at most 600 |
| `limits.replicas` | almost always; absent is 1 | a busy repository should be kept warm on more than one node |
| `limits.quota_bytes` | 50 GiB per repository suits you | you sell plans or cap by tenant |
| `limits.requests_per_minute` | your subjects are people; absent is the node's `ORIGO_REQUESTS_PER_MINUTE` | one subject drives many repositories, such as a build fleet under one token |

## Which action each operation asks for

| Action | Operations |
|---|---|
| `repo.read` | clone and fetch, the LFS download, `GET /v1/repos/{id}`, a name lookup, every read route, the import state, `export.bundle`, and `stats` |
| `repo.write` | push, the LFS upload and verify, and the four server-side operations (commits, merge, cherry-pick, revert) |
| `repo.admin` | create, rename, delete, undelete, minting a repository-bound token, transfer, freeze, unfreeze, import, verify, and `gc` |
| `repo.list` | the directory form of `GET /v1/repos`: which repositories this subject may see |

On a creation the resource carries the id, owner, and slug the caller
sent, so the endpoint decides from the name the caller chose.

A request made with a repository-bound token never reaches the
endpoint: the token's scope decides it. The one exception is a write
under such a token, for which Origo asks for `repo.write` under the
minter's subject only to read `quota_bytes`. A deny, or an allow with
no figure, falls back to the 50 GiB default; an endpoint that does not
answer refuses the write, as it would any other.

## The directory

`repo.list` is the one action whose answer is not a verdict. Its
resource carries the kind, and `cursor` and `limit` when the caller
sent them. Answer one of three shapes:

```
200 {"repos": [{"id": "…", "owner": "…", "slug": "…"}, …], "next_cursor": "…"}
200 {"allow": false, "reason": "…"}
200 {"directory": false}
```

The first is a page of the repositories the subject may see, with
`next_cursor` null on the last page. The second refuses the question.
The third says this installation lists no repositories, and callers get
501 `directory_unsupported`. A page is never cached, and Origo drops any
id from it that it no longer holds.

## The five rules

An endpoint that keeps these serves any installation.

1. **Answer 200 for both verdicts.** Anything else, a 500, a timeout, a
   body that does not parse, is a refusal, and the caller sees 503
   `authorizer_unavailable`. There is no fail-open, so an endpoint that
   is down stops reads as well as writes.
2. **Deny the repository id `00000000-0000-0000-0000-000000000001` for
   every subject and every action**, the empty subject included. That
   id is reserved as a probe: `origod check` sends it with an empty
   subject, and an allow tells it the endpoint is not reading the
   request.
3. **Key on the repository id when the answer varies by repository.**
   A request that names the id sends the id alone, with no owner and no
   slug, and that is what most calls use.
4. **Decide without the repository.** An empty or unknown id is the
   ordinary case: it is what a creation sends before the repository
   exists, and what a name that resolved to nothing sends. Answer it
   without revealing which, because Origo asks before it reads anything,
   so that a deny and a missing repository look the same to a caller.
5. **Treat the endpoint's availability as Origo's.** It is on the path
   of every operation that is not answered from the cache. Run it near
   the nodes, answer from memory, and keep nothing slow in front of it.

Rule 4 has a consequence: an endpoint that has never heard of a
repository denies it, and Origo has no call that tells it about one.
Whatever holds your permissions learns about a repository first; then
the repository is created at Origo.

## What Origo does with the answer

An allow is cached per subject, action, and repository id for its
`ttl`, 60 seconds when absent. A deny is cached for 5 seconds. An answer
about an unresolved name is not cached, and neither is a directory page.
So the call rate the endpoint sees follows the cache rather than the
traffic, and a higher `ttl` divides it.

One call, the retry included, has 5 seconds. Origo retries once when
the connection failed before any response arrived: a refused or reset
connection, or a dial timeout. A 5xx, a timeout after the request was
sent, and a body that does not parse are never retried.

## One tenant

An endpoint that answers `{"allow": true}` for a fixed list of
subjects, `{"allow": false}` for everyone else, denies the probe id,
and answers `repo.list` with `{"directory": false}` keeps the contract
in full. Rule 3 does not apply when the answer is the same for every
repository, and every figure may be left to Origo's defaults. That is a
few dozen lines behind the bearer, in any language.

## Writing it in Go

The vocabulary is published so an endpoint does not copy strings from
this page:

- `latere.ai/x/origo/authorizer` declares the four actions, the
  resource kind `Repository`, and the table as a shared vocabulary.
  `authorizer.PageActions()` names `repo.list` as the action answered
  with a page.
- `latere.ai/x/pkg/authz/server` carries the bearer check, the body
  bound, the decoding, the validation against the vocabulary, and the
  failure rules, so an endpoint writes one `Decide` method.
- `latere.ai/x/pkg/authz/conformance` runs one case per row of the
  vocabulary against a running endpoint.

## The SSH key endpoint

Needed only when the installation serves git over SSH. Origo stores no
public key. When a client offers one, Origo asks a second endpoint of
yours which subject the key belongs to, behind its own bearer:

```
POST <ORIGO_SSH_KEYS_URL>
Authorization: Bearer <ORIGO_SSH_KEYS_TOKEN>
Content-Type: application/json

{"fingerprint": "SHA256:HxK…", "type": "ssh-ed25519", "public_key": "ssh-ed25519 AAAAC3Nz…"}

200 {"found": true, "subject": "https://auth.example.com|user_42", "key_id": "k_19", "ttl": 60}
200 {"found": false}
```

`subject` is used as the caller's subject verbatim: it is what your
authorization endpoint receives, what the push is recorded under, and
what the push event names. Return the same `<issuer>|<sub>` string a
request over HTTPS carries for the same person, so one person is one
subject on both transports and the authorization endpoint needs no
second table. With the built-in owner policy this is required: a
repository created over HTTPS is owned by the issuer-qualified subject.

`key_id` is yours and appears in Origo's log line for the session.
`ttl` caches a found key for up to 600 seconds, 60 when absent; a
`{"found": false}` is held for 5 seconds.

The same kind of rules apply:

1. **Answer 200 for both verdicts.** A 500, a timeout, or a body that
   does not parse refuses the connection.
2. **Resolve by fingerprint.** The SSH user name is always `git` and
   Origo does not read it.
3. **One subject per fingerprint, across the installation.** A store
   that lets two people register one key lets the second be credited
   with the first's pushes. Refuse a fingerprint that is already
   registered.
4. **Answer `{"found": false}` without saying why.** Unknown, revoked,
   expired, and registered to someone else look alike; the person sees
   `Permission denied (publickey)` either way.
5. **Treat its availability as Origo's.** It is on the path of every
   SSH connection that is not answered from the cache.

Adding, naming, listing, and removing keys is your product's surface.
For a single team, a file of fingerprints and subjects served behind
the bearer is the whole requirement.
