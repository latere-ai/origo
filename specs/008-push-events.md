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
updated: 2026-09-06
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

The payload is spec 003's, with `kind`:

| Event | Payload |
|---|---|
| `push` | `{"id", "kind": "push", "repo", "owner", "slug", "pusher": {"sub", "actor"}, "updates": [{"ref", "before", "after", "forced"}], "at"}`; `id` is a UUID v4 drawn once per event; `forced` is true when `before` is not an ancestor of `after`, computed on the node's copy after the apply; `at` is the entry header's `at` |

Spec 019 adds the administration kinds on the same channel.

### Enqueue

In `internal/httpgit`, after `Log.Commit` returns and before the hook's
verdict is `ok`, the node writes the event object with `attempts: 0` and
`next_at` now, unless `push_options` contains `origo.event=off`. A failed
write is logged and the push is still acknowledged: the repair sweep
writes the event from the index. Enqueue is skipped when
`ORIGO_EVENTS_URL` is unset.

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
2. For every repository (`Log.Repos`), reads the newest index and, for
   each listed `push` entry with a sequence above the cursor and an `at`
   older than 1 minute that has no event object, reads the entry head
   and writes the event from it: `updates` from the transaction,
   `pusher` from the header, `forced` false. This covers a node that
   died between the index create and the enqueue; an entry whose header
   carries `origo.event=off` is skipped.

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
- A node killed at a failpoint `events.before-enqueue` after the index
  create yields one event from the repair sweep within one sweep
  interval, with `updates` equal to the entry's transaction (proposed:
  `test/e2e`, `TestEventRepairAfterKill`).
- `git push -o origo.event=off` delivers nothing for that push and the
  repair sweep writes nothing for it, while a following push without the
  option delivers (proposed: `internal/events`, `TestEventOffSuppressesOnlyThatPush`).
- A `forced` update is reported as such and a fast-forward as not
  (proposed: `internal/events`, `TestForcedFlag`).
