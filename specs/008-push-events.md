---
title: "Push events: signed webhooks per reference update"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
affects: [internal/events/, internal/httpgit/, internal/api/, internal/wal/, internal/config/, cmd/origod/, test/e2e/]
effort: small
created: 2026-09-06
updated: 2026-09-07
author: changkun
---

# Push events

## Overview

A platform reacts to pushes: a deploy starts, a build runs, a review
opens. Origo tells the consumer about every reference update with one
signed HTTP request per push, delivered at least once, so the consumer
never polls.

## Current state

Spec 003 fixes the payload. `internal/httpgit` records the client's push
options in the entry header (`push_options`), so `origo.event=off` is
already in the log. `ORIGO_EVENTS_URL` and `ORIGO_EVENTS_SECRET` are read
by `internal/config` and unused; `ORIGO_REPAIR_UNHEARD` and
`ORIGO_REPAIR_INTERVAL` are not read. `internal/events` does not exist.

## Design

### Event object

The unit of work is one object per acknowledged push:

| Key | Content |
|---|---|
| `origo/events/<repo>/<seq>.json` | `{"event": <payload>, "attempts": 0, "next_at": "<RFC 3339>"}`; rewritten unconditionally after each failed delivery; deleted after a 2xx |
| `origo/events/<repo>/cursor` | `{"seq": n}`: the highest sequence delivered, written unconditionally, monotonic per writer |
| `origo/events/dead/<repo>/<seq>.json` | an event that exhausted the window |
| `origo/events/nodes/<node>/<date>.log` | the node's journal for one UTC day, `<date>` in Go's `2006-01-02` layout: one line `<repo> <seq>` per entry the node committed, in commit order; rewritten whole by the node from its in-memory copy; read by the repair sweep |

The payload is spec 003's, with `kind`:

| Event | Payload |
|---|---|
| `push` | `{"id", "kind": "push", "repo", "seq", "owner", "slug", "pusher": {"sub", "actor"}, "updates": [{"ref", "before", "after", "forced"}], "at", "kind_detail", "operation"}`; `seq` is the entry's sequence; `id` is deterministic: the UUID v5 (RFC 9562, SHA-1) of the name `<repo>:<seq>`, `<seq>` as the 12 digit zero-padded decimal of the log key, under the namespace UUID `7c1f0b6e-4d0a-4b6a-9d3e-2a8f5c1e9b47`, fixed here and in `internal/events`, so the enqueue and the repair sweep produce the same id for one entry and a consumer that deduplicates on `id` sees one event however many times it is delivered; `forced` is true when `before` is not an ancestor of `after`; `at` is the entry header's `at`; `kind_detail` is present only when the push changes no branch or tag and says what it did instead, and it takes one value on the wire: `default_branch`, with `updates` holding the one symbolic update `{"ref": "HEAD", "before": "ref: refs/heads/<old>", "after": "ref: refs/heads/<new>", "forced": false}` (spec 003's `PATCH` of `default_branch` moves `HEAD` through the log). A push entry with an empty transaction is an undelete (spec 004) and its `kind_detail` is `undelete`, but no `push` event is ever emitted for it, by the enqueue or by the repair sweep: an undelete has exactly one event, spec 019's `undeleted`, and the value exists so both paths recognise the entry and skip it. `operation` is present only for a push made by a server-side operation of spec 020 and is its name (`commits`, `merge`, `cherry-pick`, `revert`), copied from the push option `origo.operation=<name>` in the entry header |

Spec 019 adds the administration kinds and spec 018 the `ping` probe on
the same channel; those carry no `seq` and draw a UUID v4 per event.

`ORIGO_EVENTS_URL` set without `ORIGO_EVENTS_SECRET` is a start-up
failure in the one message of spec 002 (`ORIGO_EVENTS_SECRET is
required with ORIGO_EVENTS_URL`), because an unsigned delivery is one
the sink cannot trust. Both unset turns events off.

### The `forced` flag

`forced` needs the pushed objects, which git holds in quarantine until
the hook's verdict. The node computes it after `Log.Commit` succeeded
and before it writes `ok` to the verdict FIFO: one `git merge-base
--is-ancestor --end-of-options <before> <after>` per update with
`GIT_ALTERNATE_OBJECT_DIRECTORIES` set to the quarantine path the hook
reported on the `updates` FIFO (spec 004); exit 0 is a fast-forward, 1
is forced, a create or a delete is never forced. The verdict is
therefore `ok` only after the flag is known; the git subprocess deadline
of spec 004 bounds the cost.

### Enqueue

Enqueue is one function, `events.Enqueue(ctx, repo, entry)` in
`internal/events`, given the committed entry's header and transaction,
and it is the only path that writes an event object for a `push`
entry. Three callers: `internal/httpgit` after the verdict `ok` is
delivered and `Cache.Advance` recorded the sequence; `internal/api`
after the commit of a `default_branch` change and of an undelete (spec
003); and the server-side operations of spec 020 after their commit.
`Enqueue` writes the event object with `attempts: 0` and `next_at`
now, appends the line to the journal, and does nothing for an entry
whose `push_options` contains `origo.event=off`, for an undelete (the
payload rules above), or when `ORIGO_EVENTS_URL` is unset. A failed
write is logged and the push is still acknowledged: the repair sweep
writes the event from the index.

The journal line is appended in memory before the index create, so it
is there whatever happens after. The in-memory journal is flushed to
`origo/events/nodes/<node>/<date>.log` every 10 seconds, and at once,
before the index create, the first time in a day the node commits to a
repository, so every repository a node committed to on a day is named
by that day's journal before the commit exists. A flush that fails
before the index create is logged and the push proceeds: the journal
is what repair reads, not what a push needs, and a repository whose
line never reached the bucket is the one case repair does not cover,
which the next successful flush closes for every later push. A day's
object is at most one line per commit, a few MiB for the busiest node;
the sweep deletes journals older than 2 days.

### Deliver

Each node runs a delivery loop over the events it enqueued and over what
the sweep hands it: `POST <ORIGO_EVENTS_URL>` with the body being the
`event` object alone, a 10 second timeout, and these headers:

| Header | Value |
|---|---|
| `Origo-Signature` | `sha256=<hex HMAC-SHA256 over the body with ORIGO_EVENTS_SECRET>` |
| `Origo-Event` | the `kind` |
| `Origo-Delivery` | the event `id` |

A 2xx deletes the object, advances the cursor when the sequence is
higher, and counts `origo_events_delivered_total`. Anything else
increments `attempts`, sets `next_at` from the schedule, and rewrites
the object:

| Attempt | Delay before the next |
|---|---|
| 1 | 1 s |
| 2 | 10 s |
| 3 | 1 min |
| 4 | 10 min |
| 5 | 1 h |
| 6 and later | 1 h |

Once `at` is more than 24 hours in the past the object moves to
`origo/events/dead/<repo>/<seq>.json`, `origo_events_dead_total`
increments, and the alert of spec 011 fires. The consumer treats
delivery as at least once and keys on `id`: two nodes can deliver one
event when a sweep overlaps a loop.

### Repair

A sweep every `ORIGO_REPAIR_INTERVAL` (spec 002, default 10 minutes) on
every node, offset by the node's name so nodes do not sweep together,
does two things over `origo/events/`:

1. Delivers every pending object whose `next_at` is more than 1 minute
   in the past, which is what a dead node left behind.
2. Reads the journals of nodes that are not in the live set of spec 005
   and were last heard more than `ORIGO_REPAIR_UNHEARD` ago (spec 002,
   default 5 minutes), or never heard since this node started, for
   today and yesterday. For every repository
   those journals name, it reads the newest index and, for each listed
   `push` entry with a sequence above the cursor and an `at` older than
   1 minute that has no event object, reads the entry head and writes
   the event from it: `updates` from the transaction, `pusher` from the
   header, `forced` false, `operation` from the header, `kind_detail`
   from the transaction as the payload table says, and `id` from
   `repo` and `seq` as the payload table says,
   so a consumer that received the original before the node died and
   the repair after it sees one id. This covers a node that died
   between the index create and the enqueue; an entry whose header
   carries `origo.event=off`, and an undelete, are skipped as the
   enqueue skips them. A node also runs this step
   once at start-up over its own journals, which covers a restart
   under the same name. No sweep lists every repository: the cost is
   one listing of `origo/events/nodes/` and one index read per
   repository a dead node touched. The two durations are variables so
   a test sets them to seconds; a deployment keeps the defaults.

## Not in this spec

Event kinds other than `push` (spec 019). Delivery to more than one
sink. Replay of dead events; an operator moves an object back under
`origo/events/<repo>/` to retry it.

## Acceptance criteria

- Every push in a 1 000 push load produces exactly one delivered event
  with a valid signature, `Origo-Event: push`, and `Origo-Delivery`
  equal to the body's `id`, in a sink stub that records deliveries
  (proposed: `internal/events`, `TestThousandPushesOneEventEach`).
- A sink answering 500 sees retries at 1 s, 10 s, 1 min, 10 min, 1 h,
  and hourly with a fake clock, and a dead-letter object plus an
  increment of `origo_events_dead_total` 24 hours after `at` (proposed:
  `internal/events`, `TestRetryScheduleAndDeadLetter`).
- A node killed at `ORIGO_FAILPOINT=events.before-enqueue` (spec 002)
  after the index create and the verdict yields one event from another
  node's repair sweep within one `ORIGO_REPAIR_INTERVAL` once the dead
  node is `ORIGO_REPAIR_UNHEARD` unheard, both set to seconds, with
  `updates` equal to the entry's transaction and `id` equal to the
  UUID v5 the payload table defines, and the dead node's journal names
  the repository (proposed: `test/e2e`, `TestSlowEventRepairAfterKill`, in
  the `e2e-slow` job of spec 013).
- The repair sweep reads only the journals of nodes not heard for
  `ORIGO_REPAIR_UNHEARD` and reads the index of only the repositories
  they name, with a fake clock and a store that counts reads (proposed:
  `internal/events`, `TestRepairReadsOnlyDeadJournals`).
- The enqueue and the repair of one entry produce the same `id`, two
  entries of one repository produce two, and the id equals the UUID v5
  computed by the test from the namespace and `<repo>:<seq>` (proposed:
  `internal/events`, `TestEventIDIsDeterministic`).
- A push with a service token carrying `act` delivers `pusher.sub` equal
  to `act` and `pusher.actor` equal to the token's `sub`, and one
  without `act` delivers `actor` empty (proposed: `internal/events`,
  `TestPusherCarriesSubjectAndActor`).
- An undelete produces no `push` event from the enqueue and none from
  the repair sweep run over its entry, while spec 019's `undeleted` is
  delivered once, and a `PATCH` of `default_branch` produces one `push`
  event with the single `HEAD` update and `kind_detail: "default_branch"`
  (proposed: `internal/events`, `TestUndeleteEmitsNoPushEvent`,
  `TestDefaultBranchChangeIsAHeadUpdate`).
- `ORIGO_EVENTS_URL` set without `ORIGO_EVENTS_SECRET` fails the
  start-up with the one message (proposed: `internal/config`,
  `TestEventsURLNeedsTheSecret`).
- `git push -o origo.event=off` delivers nothing for that push and the
  repair sweep writes nothing for it, while a following push without the
  option delivers (proposed: `internal/events`, `TestEventOffSuppressesOnlyThatPush`).
- A `forced` update is reported as such and a fast-forward as not, with
  the flag computed while the objects are still in quarantine, asserted
  by a hook that leaves the quarantine in place (proposed:
  `internal/httpgit`, `TestForcedFlagIsComputedBeforeTheVerdict`).
