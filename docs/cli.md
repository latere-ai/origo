# The origo command

`origo` reads and changes a repository on an Origo installation without
cloning it. It prints lines a shell can filter and takes `--json` on every
read, so finding a file in a three thousand file repository is one command and
one `grep` rather than a checkout.

It is built from the same tag as `origod` and ships in the same release
archives. It runs where you run, not where Origo runs: nothing is deployed and
nothing changes on the installation.

## Get a credential

Every call carries one bearer. The credential to use is a **repository-bound
token**: it is bound to one repository, its scope is the whole of what it can
do, and it expires within the hour, so a leak is bounded.

Minting one is an `admin` action, and `origo` does not do it. That is
deliberate: a command an agent runs in a loop holds no code path that can widen
its own access. Mint it once with `curl`, from a token that may administer the
repository.

```sh
set -eu
ORIGO_URL="${ORIGO_TEST_URL:-http://localhost:30080}"
export ORIGO_URL
ADMIN="$ORIGO_TEST_ADMIN_TOKEN"

# A repository to work in, and a first commit so there is something to read.
ORIGO_REPO=$(uuidgen | tr 'A-Z' 'a-z')
export ORIGO_REPO
curl -sf -X POST "$ORIGO_URL/v1/repos" \
	-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
	-d "{\"id\":\"$ORIGO_REPO\",\"owner\":\"docs\",\"slug\":\"cli-$(echo "$ORIGO_REPO" | cut -c1-8)\"}" >/dev/null
echo "repository $ORIGO_REPO"
```

```sh
# A token bound to that one repository, for an hour, that may write.
ORIGO_TOKEN=$(curl -sf -X POST "$ORIGO_URL/v1/repos/$ORIGO_REPO/tokens" \
	-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
	-d '{"scope":"write","ttl":3600}' | sed 's/.*"token":"\([^"]*\)".*/\1/')
export ORIGO_TOKEN
export ORIGO_AUTHOR="Docs Example <docs@example.com>"
echo "token minted"
```

For an agent that only reads, use `{"scope":"read","ttl":3600}`. The scope is
the gate: Origo refuses a write under a `read` token whatever the command
sends.

**One command does not work under a repository-bound token: `origo repos`.**
That token's decision was made when it was minted, for one repository, so the
installation refuses to answer a question about every repository you may see
and says `forbidden` with `action=list`. This is the model working, not a gap.
Name the repository with `-repo` or `ORIGO_REPO`, which every other command
takes, or use a token from your issuer if you want to browse. The command says
as much when it is refused.

## Set four variables

| Variable | What it is |
|---|---|
| `ORIGO_URL` | the installation, `https://git.example.com`. Plain `http` is accepted only at a loopback address |
| `ORIGO_TOKEN` | the bearer above. Never printed, never logged, never in an error line |
| `ORIGO_REPO` | the repository every command works on: an id, or `owner/slug` |
| `ORIGO_AUTHOR` | `Name <email>`, the author of every commit a write command makes |

`-repo` overrides `ORIGO_REPO` on any command. `ORIGO_AUTHOR` is needed only by
the write commands.

If you set `ORIGO_TOKEN` to the node's `ORIGO_TOKEN_KEY` by mistake, the
command says so and stops rather than sending a private key in a header.

## The commands

Read:

| Command | What it prints |
|---|---|
| `origo repos` | one line per repository this credential may see, `<id>  <owner>/<slug>`. A repository-bound token cannot list; see below |
| `origo info` | the repository in one call: name, default branch, whole head, size, times |
| `origo refs` | `<name> <7 hex>` per line; `-tags`, `-all`, `-prefix` |
| `origo ls [path]` | one line per tree entry, `-r` for the whole tree, `-ref` for another revision |
| `origo cat <path>` | a file's text, windowed by `-n`, `-offset` and `-max-bytes` |
| `origo log` | `<7 hex> <date> <author> <subject>` per commit; `-ref`, `-path`, `-since`, `-until` |
| `origo show <commit>` | one commit: metadata, message, trailers, totals, per-file stat; `-p` adds the patch |
| `origo diff <base> <head>` | one stat line per file and a totals line; `-p` adds the patch |

Write. Each takes `-expect`, the commit the branch is at now, and `-dry-run`
checks a plan without landing it:

| Command | What it does |
|---|---|
| `origo commit <path>...` | a commit from the local files you name; `-delete`, `-file r=l`, `-create`, `-from` |
| `origo merge <source>` | merge a branch or commit into a branch; `-strategy` |
| `origo cherry-pick <sha>...` | replay commits onto a branch; `-mainline` picks a merge's parent |
| `origo revert <sha>...` | replay them backwards; `-mainline` the same |

`origo help` prints the whole surface. `origo version` prints the build
identity.

## What the defaults give you

The default answer is the smallest one that answers the question. The larger
one is one flag away, and the flag is named where the answer was cut.

| Command | Without a flag | For more |
|---|---|---|
| `repos` | 200 rows | `-n 0` |
| `refs` | 100 references | `-n 0` |
| `ls` | 200 entries of one directory | `-r`, `-n 0` |
| `cat` | 800 lines or 32 KiB, whichever comes first | `-offset`, `-n`, `-max-bytes` |
| `log` | 20 commits, subjects only | `-n`, `-n 0` |
| `show` | metadata, message, per-file stat | `-p`, `-path` |
| `diff` | per-file stat and a totals line | `-p`, `-path`, `-max-bytes` |

`0` always means every one of them.

## Reading the two streams

**Data goes to standard output. Everything about the data goes to standard
error.** A file's header, a truncation notice and a staleness notice are all on
standard error, so a pipeline filters only payload and `origo cat x.go > x.go`
writes the file and nothing else.

A cut answer ends with one bracketed line on standard error naming the flag
that continues it:

```
[truncated: lines 0-799 of 3120, 32768 bytes; add -offset 800]
```

**No such line means the answer is whole.** Where the installation cut a body
itself the line says so and names the last entry it saw, and a partial list is
never presented as a complete one.

Exit status is `0` when the command was served, `1` when the installation or
the network refused, and `2` when the command line or the environment is wrong
and no request was made.

## Pipelines

These blocks run under the repository-bound token minted above, so every one
of them names a repository. First a commit, so there is something to read.
`-create` starts the branch; on an empty repository it needs no `-from`:

```sh
mkdir -p /tmp/origo-cli-docs && cd /tmp/origo-cli-docs
printf 'hello from the docs\n' > README.md
origo commit -m 'the first commit' -create -branch main README.md
origo info | grep '^head'
```

Find a file by name, without a clone:

```sh
origo ls -r -n 0 | grep -i 'readme'
```

Read the head, change the file against it, and see the new commit:

```sh
HEAD_SHA=$(origo log -n 1 --json | sed 's/.*"sha":"\([0-9a-f]*\)".*/\1/')
printf 'hello again\n' >> README.md
origo commit -m 'a second line' -expect "$HEAD_SHA" README.md
origo log -n 5
```

Find who changed what. Origo's history route has no author parameter and this
command adds none; the shell is the filter:

```sh
origo log -n 200 | grep 'Docs Example'
```

Look at a change stat first, then at one file's patch:

```sh
NEW_SHA=$(origo log -n 1 --json | sed 's/.*"sha":"\([0-9a-f]*\)".*/\1/')
origo diff "$HEAD_SHA" "$NEW_SHA"
origo diff -p -path README.md "$HEAD_SHA" "$NEW_SHA"
```

Undo a commit. The recovery path is additive: the mistake stays in the history
where an audit can see it.

```sh
origo revert -expect "$NEW_SHA" "$NEW_SHA"
origo log -n 3
```

## For an agent

`skills/origo/SKILL.md` in this repository is a skill document: a short file an
agent reads on demand that teaches it the credential, the four variables, the
defaults, the truncation vocabulary and the pipelines above. Copy it into
wherever your agent host keeps skills. Its cost while unused is its name and
one sentence.

## What it will not do

- **Mint a credential.** `POST /v1/repos/{id}/tokens` is an `admin` action.
  The command takes a bearer; it does not make one.
- **Administer a repository.** No create, delete, rename, transfer, freeze,
  import, export or `gc`. All are `admin`, which is what a bound token
  excludes.
- **Make a commit unreachable.** Every change appends a commit whose parent is
  `-expect` and moves one branch to it. There is no force, no rewrite and no
  reference deletion, so the undo for a mistake is `origo revert`.
- **Run git.** No clone, no worktree, no subprocess. If you want a whole tree
  to work on locally, clone it with git; this command is for the work a clone
  is too expensive for.
- **Write a file.** It reads the files you name on a `commit` and writes none,
  keeps no cache on disk and has no configuration file.
- **Search content.** Origo serves no search endpoint. `origo ls -r -n 0 |
  grep` and `origo log -n 200 | grep` are what the shell gives you instead.

The design and the reasoning behind each of those is
[spec 025](../specs/025-agent-client.md).
