---
title: "Push events: signed webhooks per reference update"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
affects: [internal/events/, internal/httpgit/, internal/wal/, cmd/origod/]
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
by `internal/config` and unused. `internal/events` does not exist.

## Design

### Event object

The unit of work is one object per acknowledged push:

| Key | Content |
|---|---|
| `origo/events/<repo>/<seq>.json` | `{"event": <payload>, "attempts": 0, "next_at": "<RFC 3339>"}`; rewritten unconditionally after each failed delivery; deleted after a 2xx |
| `origo/events/<repo>/cursor` | `{"seq": n}`: the highest sequence delivered, written unconditionally, monotonic per writer |
| `origo/events/dead/<repo>/<seq>.json` | an event that exhausted the window |
| `origo/events/nodes/<node>/<date>.log` | the node's journal for one UTC day: one line `<repo> <seq>` per entry the node committed, in commit order; rewritten whole by the node from its in-memory copy; read by the repair sweep |

The payload is spec 003's, with `kind`:

| Event | Payload |
|---|---|
| `push` | `{"id", "kind": "push", "repo", "owner", "slug", "pusher": {"sub", "actor"}, "updates": [{"ref", "before", "after", "forced"}], "at", "kind_detail", "operation"}`; `id` is a UUID v4 drawn once per event; `forced` is true when `before` is not an ancestor of `after`; `at` is the entry header's `at`; `kind_detail` is present only when `updates` is empty and says why the push entry carries no reference change: `undelete` (spec 004's undelete commits a push entry with an empty transaction); `operation` is present only for a push made by a server-side operation of spec 020 and is its name (`commits`, `merge`, `cherry-pick`, `revert`), copied from the push option `origo.operation=<name>` in the entry header |

Spec 019 adds the administration kinds and spec 018 the `ping` probe on
the same channel.

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

In `internal/httpgit`, after the verdict `ok` is delivered and
`Cache.Advance` recorded the sequence, the node writes the event object
with `attempts: 0` and `next_at` now, unless `push_options` contains
`origo.event=off`, and appends the line to its journal. A failed write
is logged and the push is still acknowledged: the repair sweep writes
the event from the index. Enqueue is skipped when `ORIGO_EVENTS_URL` is
unset.

The journal line is appended in memory before the index create, so it
is there whatever happens after. The in-memory journal is flushed to
`origo/events/nodes/<node>/<date>.log` every 10 seconds, and at once,
before the index create, the first time in a day the node commits to a
repository, so every repository a node committed to on a day is named
by that day's journal before the commit exists. A day's object is at
most one line per commit, a few MiB for the busiest node; the sweep
deletes journals older than 2 days.

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

A sweep every 10 minutes on every node, offset by the node's name so
nodes do not sweep together, does two things over `origo/events/`:

1. Delivers every pending object whose `next_at` is more than 1 minute
   in the past, which is what a dead node left behind.
2. Reads the journals of nodes that are not in the live set of spec 005
   and were last heard more than 5 minutes ago, or never heard since
   this node started, for today and yesterday. For every repository
   those journals name, it reads the newest index and, for each listed
   `push` entry with a sequence above the cursor and an `at` older than
   1 minute that has no event object, reads the entry head and writes
   the event from it: `updates` from the transaction, `pusher` from the
   header, `forced` false, `kind_detail` and `operation` from the
   header. This covers a node that died between the index create and
   the enqueue; an entry whose header carries `origo.event=off` is
   skipped. A node also runs this step once at start-up over its own
   journals, which covers a restart under the same name. No sweep
   lists every repository: the cost is one listing of
   `origo/events/nodes/` and one index read per repository a dead node
   touched.

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
  node's repair sweep within one sweep interval once the dead node is 5
  minutes unheard, with `updates` equal to the entry's transaction, and
  the dead node's journal names the repository (proposed: `test/e2e`,
  `TestEventRepairAfterKill`).
- The repair sweep reads only the journals of nodes not heard for 5
  minutes and reads the index of only the repositories they name, with
  a fake clock and a store that counts reads (proposed:
  `internal/events`, `TestRepairReadsOnlyDeadJournals`).
- An undelete produces a `push` event with `updates: []` and
  `kind_detail: "undelete"` (proposed: `internal/events`,
  `TestEmptyTransactionCarriesKindDetail`).
- `git push -o origo.event=off` delivers nothing for that push and the
  repair sweep writes nothing for it, while a following push without the
  option delivers (proposed: `internal/events`, `TestEventOffSuppressesOnlyThatPush`).
- A `forced` update is reported as such and a fast-forward as not, with
  the flag computed while the objects are still in quarantine, asserted
  by a hook that leaves the quarantine in place (proposed:
  `internal/httpgit`, `TestForcedFlagIsComputedBeforeTheVerdict`).
