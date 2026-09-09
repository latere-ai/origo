# Migrating repositories into Origo

For whoever moves repositories from another git host into Origo. The
design is in `specs/`; this page is what to do.

A migration moves one repository at a time through four states, and the
whole batch through the same four:

| State | What happened | What serves the repository |
|---|---|---|
| registered | Origo holds the id, the owner, and the slug | the prior host |
| importing | Origo is mirroring the history from the prior host | the prior host |
| mirrored | every reference is identical on both sides | both |
| cut over | the prior host redirects its clone URLs to Origo | Origo |

Origo never freezes its own copy and never writes to the prior host.
Nothing is irreversible until you cut over, and the cut-over is one
change on the prior host.

## Before you start

You need four things:

- the URL of the Origo you are migrating into;
- a token with the `admin` action on every repository of the migration,
  which is the token the commands below present to Origo;
- for each repository, an HTTPS clone URL on the prior host and a bearer
  that URL accepts;
- the prior host's name on the node's `ORIGO_EGRESS_ALLOW` list. A node
  fetches only from a host that list names, so a source that is not on
  it is refused with `invalid_request` before any connection is opened.

Every command below reads those from the environment. Set them for your
installation; the `ORIGO_TEST_*` fallbacks are what makes this page run
as its own test against an example stack.

```sh
ORIGO_URL="${ORIGO_URL:-${ORIGO_TEST_URL:?the Origo to migrate into}}"
ADMIN="${ORIGO_ADMIN_TOKEN:-${ORIGO_TEST_ADMIN_TOKEN:?a token with admin}}"
SOURCE_URL="${SOURCE_URL:-${ORIGO_TEST_SOURCE_URL:?the clone URL on the prior host}}"
SOURCE_TOKEN="${SOURCE_TOKEN:-${ORIGO_TEST_SOURCE_TOKEN:-}}"
AUTH="Authorization: Bearer $ADMIN"
JSON="Content-Type: application/json"
```

## One repository

Do one repository by hand first. It is four calls, and it tells you
whether the prior host, the token, and the egress list are right before
you run a batch of a thousand.

Register it. The id is a UUID you choose, so the prior host can carry
its own id in its own records and Origo's id never changes again:

```sh
ID=$(uuidgen | tr 'A-Z' 'a-z')
curl -sf -X POST "$ORIGO_URL/v1/repos" -H "$AUTH" -H "$JSON" \
	-d "{\"id\":\"$ID\",\"owner\":\"migration\",\"slug\":\"first-$$\"}" >/dev/null
echo "registered $ID"
```

Import the history. The call answers 202 at once and the import runs on
the node; a push to Origo is refused with `repo_importing` while it
does. The bearer travels in the body, never in a URL or a header you
would find in a log:

```sh
curl -sf -X POST "$ORIGO_URL/v1/repos/$ID/import" -H "$AUTH" -H "$JSON" \
	-d "{\"source\":\"$SOURCE_URL\",\"token\":\"$SOURCE_TOKEN\"}" >/dev/null
STATE=running
for _ in $(seq 1 900); do
	STATE=$(curl -sf "$ORIGO_URL/v1/repos/$ID/import" -H "$AUTH" | jq -r .state || echo unread)
	case "$STATE" in
	done) break ;;
	failed) curl -sf "$ORIGO_URL/v1/repos/$ID/import" -H "$AUTH" | jq . ; break ;;
	esac
	sleep 2
done
test "$STATE" = done
```

A read that does not answer is `unread` and the loop asks again: one
request that fails is a node rolling, not an import that failed. Only
`failed` is a failure, and the state carries git's own message from the
prior host in `error`.

Verify it. This reads both sides and writes to neither: it lists the
prior host's references, lists Origo's, and answers whether the two are
identical, with every reference they disagree on and the reachable
object count of Origo's copy. Compare that count with the prior host's
own figure:

```sh
curl -sf -X POST "$ORIGO_URL/v1/repos/$ID/verify" -H "$AUTH" -H "$JSON" \
	-d "{\"source\":\"$SOURCE_URL\",\"token\":\"$SOURCE_TOKEN\"}" | tee /tmp/verify.json | jq .
jq -e .equal /tmp/verify.json >/dev/null
echo "mirrored $ID"
```

`equal: true` means the repository is mirrored. `equal: false` lists
each reference under `refs.differing` with the hash on each side, an
empty hash for the side that does not hold it. The usual cause is a
push to the prior host after the import started. Fix the cause, then
import again under a **fresh id**: an import refuses a repository that
already has history with `repo_not_empty`, so register a new id, write
it to the prior host's record, and leave the first id in place until you
have looked at it.

Verification is read-only and idempotent, so run it as often as a doubt
asks for it. Each run records `verified_at` and `verified_equal` on the
repository, which `GET /v1/repos/{id}` reports and a batch resumes from.

## A batch

`origod migrate` does the same four calls for every repository of a
manifest, several at a time, and writes one line per repository as it
finishes. It is a client of the API: it reads three variables of its
own and none of a node's, so run it wherever you can reach Origo.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ORIGO_MIGRATE_URL` | yes | none | the Origo the command drives |
| `ORIGO_MIGRATE_TOKEN_ENV` | yes | none | the name of the variable holding the bearer presented to Origo |
| `ORIGO_MIGRATE_PARALLEL` | no | `4` | repositories driven at once |

The token is never on the command line and never in the manifest: both
files name a variable, and the command reads the value out of the
environment.

The manifest is JSON lines, one object per repository. `prior_id` is
yours: Origo never reads it and the report copies it back, which is how
you match a report line to your own record.

```sh
WORK=$(mktemp -d)
for n in 1 2 3 4; do
	printf '{"id":"%s","prior_id":"ws_%s","owner":"migration","slug":"batch-%s-%s","source":"%s","token_env":"SOURCE_TOKEN"}\n' \
		"$(uuidgen | tr 'A-Z' 'a-z')" "$n" "$$" "$n" "$SOURCE_URL" >> "$WORK/manifest.jsonl"
done
cat "$WORK/manifest.jsonl"
```

Run it. The command exits 0 when every repository is mirrored, 1 when
any is not, and 2 on a manifest or a command line it cannot use, so a
pipeline reads the exit code and a person reads the report:

```sh
export ORIGO_MIGRATE_URL="$ORIGO_URL"
export ORIGO_MIGRATE_TOKEN_ENV=ORIGO_MIGRATE_TOKEN
export ORIGO_MIGRATE_TOKEN="$ADMIN"
export ORIGO_MIGRATE_PARALLEL=4
export SOURCE_TOKEN
origod migrate -manifest "$WORK/manifest.jsonl" -report "$WORK/report.jsonl"
cat "$WORK/report.jsonl"
test "$(jq -s 'map(select(.state != "mirrored")) | length' "$WORK/report.jsonl")" = 0
echo "the batch is mirrored"
```

Each report line carries the id, your `prior_id`, the owner and slug,
the `state` (`mirrored`, `skipped`, or `failed`), the reference and
object counts, the seconds it took, and `error` naming the failing step
and Origo's error code when it failed.

A run is resumable. It reads each repository's state from Origo and
nothing else, so re-running the same manifest after a failure imports
nothing that already imported and reports every finished repository as
`skipped`. Fix the failures, run it again, and read the report.

While it runs, Origo emits an `imported` event per repository when the
history lands and a `verified` event per verification, both to the
webhook sink your installation configured, so a prior host can advance
its own records without polling.

## Cutting over

A repository is ready to cut over when it is `mirrored`. The cut-over
is the prior host's step, in this order:

1. Freeze the repository on the prior host so it takes no more writes.
2. Verify once more, now that no write can arrive. This is the check
   that matters: it proves nothing was written between the import and
   the freeze.
3. Point the old clone URLs at Origo. Answer HTTP 308 from
   `<old>/info/refs`, `<old>/git-upload-pack`, and
   `<old>/git-receive-pack` to the same paths under Origo's URL, or
   proxy them. Git follows the redirect and rewrites its base for the
   rest of the session, so a clone and a push against the old URL keep
   working with no change on any developer's machine.

   Put a token for Origo in the redirect target as the URL's user
   info, `https://x:<token>@<origo>/r/<id>.git/info/refs`: every route
   of Origo is authenticated, and git's HTTP client drops an
   `Authorization` header on a redirect that changes the host, so a
   client that carries no Origo credential of its own would be asked
   for one. Minting that token is the prior host's step, as minting
   the import bearer was.
4. Leave the redirect up for at least 30 days, then remove it.
5. Point anything that mounts or clones the repository at Origo.

From step 3 on, Origo's copy is the only writable one. Nothing on
Origo's side has to change: a mirrored repository already accepts
writes.

## If something goes wrong

| What you see | What it means | What to do |
|---|---|---|
| 400 `invalid_request` with `details.reason: egress` | the source host is not on `ORIGO_EGRESS_ALLOW`, or it resolves to an address the node refuses | add the host to the list on every node, then retry |
| 400 `invalid_request` naming `source` | the source did not answer `ls-remote`: a wrong URL, a wrong bearer, or a repository the prior host does not serve | check the URL and the bearer with `git ls-remote` from your own machine |
| 409 `repo_not_empty` | the repository already has history, so an import would drop it | register a fresh id and import into that |
| 409 `repo_importing` | an import is running for this repository | wait for `GET /v1/repos/{id}/import` to leave `running` |
| `import` state `failed` | the mirror did not finish; `error` carries git's own message | fix the cause on the prior host and import again under a fresh id |
| `equal: false` | the two sides differ | read `refs.differing`; a push after the import started is the usual cause |

Migrating LFS objects is separate: run `git lfs fetch --all` on the
prior host and `git lfs push --all` against Origo once the repository is
mirrored.
