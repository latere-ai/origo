---
name: origo
description: Read and change a repository on an Origo git host without cloning it, using the origo command.
---

# Working on an Origo repository

`origo` reaches a repository over HTTP. Reading one file costs one file, not a
checkout. It prints lines, so a shell filters an answer before it reaches you.

## Before the first command

You need four variables. If they are already set, skip to the pipelines.

```sh
export ORIGO_URL=https://git.example.com
export ORIGO_TOKEN=<a bearer>
export ORIGO_REPO=<a repository id, or owner/slug>
export ORIGO_AUTHOR="Your Name <you@example.com>"    # writes only
```

`ORIGO_TOKEN` should be a **repository-bound token**: bound to one repository,
scoped to what you may do, and valid for at most an hour. `origo` will not mint
one, because a command running in a loop should hold no path that widens its
own access. Mint it with one call, from a token that may administer the
repository:

```sh
ORIGO_TOKEN=$(curl -sf -X POST "$ORIGO_URL/v1/repos/$ORIGO_REPO/tokens" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"scope":"read","ttl":3600}' | jq -r .token)
export ORIGO_TOKEN
```

Use `"scope":"read"` unless you are going to commit; then use `"write"`. The
scope is the gate. A `read` token is refused a write by the installation
whatever you type.

A repository-bound token names one repository, so `origo repos` does not work
under it: the installation answers `forbidden` with `action=list`, because
there is no subject-wide question a credential for one repository can ask.
That is expected. Work with `ORIGO_REPO` set, which every other command uses.

If a command answers `unauthenticated: ... reason=expired`, the hour ran out.
Mint another and set the variable again.

## The four pipelines

**Where is the file that handles this?** There is no search endpoint. Walk the
tree once and filter it in the shell.

```sh
origo ls -r -n 0 | grep -i handler
```

**What has this person changed lately?** There is no author parameter either.

```sh
origo log -n 200 | grep 'Ada Lovelace'
```

**What changed between two commits?** Stat first, then the one file you care
about. The whole patch of a large change is far past your budget and the stat
is one line per file.

```sh
origo diff <base> <head>
origo diff -p -path internal/api/read.go <base> <head>
```

**Read the head, commit against it, undo it.** A write is a statement about a
branch you have read, so `-expect` is required and never filled in for you. A
branch that moved answers `non_fast_forward` with the actual head instead of
clobbering it.

```sh
HEAD_SHA=$(origo log -n 1 --json | jq -r '.commits[0].sha')
origo commit -m 'fix the handler' -expect "$HEAD_SHA" internal/api/read.go
NEW_SHA=$(origo log -n 1 --json | jq -r '.commits[0].sha')
origo revert -expect "$NEW_SHA" "$NEW_SHA"
```

`origo commit` reads the local files you name, at the same path in the
repository, and writes nothing to disk. `-dry-run` checks a plan without
landing it. Add `-delete <path>` to remove one, `-file <repo path>=<local
path>` to map one.

## What you get without a flag

Each default is the smallest answer to the question. The flag for more is named
on standard error where the answer was cut, so you never have to guess it.

| Command | Without a flag | For more |
|---|---|---|
| `origo repos` | 200 rows | `-n 0` |
| `origo info` | the repository, one call | the other commands |
| `origo refs` | 100 branches | `-tags`, `-all`, `-n 0` |
| `origo ls [path]` | 200 entries of one directory | `-r`, `-n 0` |
| `origo cat <path>` | 800 lines or 32 KiB | `-offset`, `-n`, `-max-bytes` |
| `origo log` | 20 commits, subjects only | `-n`, `-n 0` |
| `origo show <commit>` | metadata, message, per-file stat | `-p`, `-path` |
| `origo diff <a> <b>` | per-file stat and totals | `-p`, `-path`, `-max-bytes` |

`0` always means all of them. Every read command takes `--json` and prints the
API's own shape, which is what the `jq` above reads.

## Reading the answer

**Standard output is the data. Standard error is everything about the data.** A
file's header, a truncation notice and a staleness notice are all on standard
error, so `origo ls -r | grep` filters only paths and `origo cat x.go > x.go`
writes only the file.

A cut answer ends with one bracketed line naming the flag that continues it:

```
[truncated: lines 0-799 of 3120, 32768 bytes; add -offset 800]
```

**If there is no bracketed line, the answer is whole.** Two other lines are
worth knowing:

- `[truncated: the installation cut the comparison at its 1 MiB cap; the files
  above end at <file> ...]` means the list is partial and the installation, not
  the command, cut it. Narrow with `-path`.
- `[stale: the installation answered from a copy it last checked N seconds
  ago]` means a head you read may have moved. If a write then answers
  `non_fast_forward`, that is why: re-read and retry with the actual head.

Exit status `0` is served, `1` is refused by the installation or the network,
`2` is a wrong command line or environment with no request made. A refusal is
one line: the code, a sentence, and the fields that say what to do next.

## What it will not do

No clone, no git subprocess, no writing files, no content search, and no
administration: create, delete, rename, transfer, freeze, import, export and
`gc` all need `admin`, which is exactly what a repository-bound token excludes.
Nothing here can make a commit unreachable, because every change appends a
commit and moves one branch; the undo for a mistake is `origo revert`.
