---
title: "Push events: signed webhooks per reference update"
status: complete
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/005-placement-and-replication.md
  - specs/007-authentication-and-delegation.md
affects: [internal/events/, internal/httpgit/, internal/api/, internal/wal/, internal/config/, cmd/origod/, test/e2e/]
effort: small
created: 2026-09-06
updated: 2026-09-08
author: changkun
---

# Push events

## Overview

A platform reacts to pushes: a deploy starts, a build runs, a review
opens. Origo tells the consumer about every reference update with one
signed HTTP request per push, delivered at least once, so the consumer
never polls.

## Current state

Built on 2026-09-08 as the Design describes; the Outcome lists the
tests and the interpretations. Before it, spec 003 fixed the payload,
`internal/httpgit` recorded the client's push options in the entry
header (`push_options`), so `origo.event=off` was already in the log,
`ORIGO_EVENTS_URL` and `ORIGO_EVENTS_SECRET` were read by
`internal/config` and unused, `ORIGO_REPAIR_UNHEARD` and
`ORIGO_REPAIR_INTERVAL` were not read, and `internal/events` did not
exist.

Two items were for the builder beyond the package. The receive path in
`internal/httpgit`, in this spec's affects, records
`origo_push_duration_seconds{phase}` of spec 011 (`receive`, `entry`,
`index`, `apply`), which phase 1 attributed to spec 004 and never
recorded; this spec changes that path for the `forced` flag and the
enqueue, so the four observations land with it. `apply` spans the
forced computation, the verdict, git's own reference update, and the
local advance; the enqueue falls outside the four phases, and a
refused push observes `receive` alone. And every event kind
the deck defines goes through the one channel below: the
administration kinds of spec 019 and the `verified` kind of spec 014
are emitted through `Emit`, defined beside `Enqueue`, so one package
owns the key, the id, delivery, retry, dead-letter, the cursor, and
repair for every kind.

## Design

### Event object

The unit of work is one object per event: one per acknowledged push,
and one per administration operation of spec 019 or verification of
spec 014, which have no sequence:

| Key | Content |
|---|---|
| `origo/events/<repo>/<seq>.json` | a `push` event: `{"event": <payload>, "attempts": 0, "next_at": "<RFC 3339>"}`; rewritten unconditionally after each failed delivery; deleted after a 2xx |
| `origo/events/<repo>/a-<id>.json` | an event without a sequence, the same content; `<id>` is the event's id, the UUID v5 of `<repo>:<kind>:<occurred_at>` under the namespace of the payload table, `<occurred_at>` the payload's `at` in RFC 3339 with second precision in UTC, so a repeated `Emit` of one operation writes the same key and is idempotent; a `PUT` of an existing key is a no-op rather than a second event |
| `origo/events/<repo>/cursor` | `{"seq": n, "delivered": [{"id", "at"}]}`: `seq` is the highest `push` sequence delivered, monotonic per writer; `delivered` is the set of administration event ids delivered in the last 24 hours with their `at`, older entries dropped on every rewrite, so a delivery loop or a repair that finds an `a-<id>.json` whose id is in the set deletes it without delivering; written unconditionally |
| `origo/events/dead/<repo>/<seq>.json`, `origo/events/dead/<repo>/a-<id>.json` | an event that exhausted the window, under the key it had |
| `origo/events/nodes/<node>/<date>.log` | the node's journal for one UTC day, `<date>` in Go's `2006-01-02` layout: one line `<repo> <seq>` per `push` entry the node enqueued, in enqueue order; rewritten whole by the node from its in-memory copy, which is seeded at start-up from the node's own objects of today and yesterday so a restart under the same name keeps the lines an earlier process wrote; read by the repair sweep |

The payload is spec 003's, with `kind`:

| Event | Payload |
|---|---|
| `push` | `{"id", "kind": "push", "repo", "seq", "owner", "slug", "pusher": {"sub", "actor"}, "updates": [{"ref", "before", "after", "forced"}], "at", "kind_detail", "operation"}`; `seq` is the entry's sequence; `id` is deterministic: the UUID v5 (RFC 9562, SHA-1) of the name `<repo>:<seq>`, `<seq>` as the 12 digit zero-padded decimal of the log key, under the namespace UUID `7c1f0b6e-4d0a-4b6a-9d3e-2a8f5c1e9b47`, fixed here and in `internal/events`, so the enqueue and the repair sweep produce the same id for one entry and a consumer that deduplicates on `id` sees one event however many times it is delivered; `forced` is true when `before` is not an ancestor of `after`; `at` is the entry header's `at`; `kind_detail` is present only when the push changes no branch or tag and says what it did instead, and it takes one value on the wire: `default_branch`, with `updates` holding the one symbolic update `{"ref": "HEAD", "before": "ref: refs/heads/<old>", "after": "ref: refs/heads/<new>", "forced": false}` (spec 003's `PATCH` of `default_branch` moves `HEAD` through the log). A push entry with an empty transaction is an undelete (spec 004) and its `kind_detail` is `undelete`, but no `push` event is ever emitted for it, by the enqueue or by the repair sweep: an undelete has exactly one event, spec 019's `undeleted`, and the value exists so both paths recognise the entry and skip it. `operation` is present only for a push made by a server-side operation of spec 020 and is its name (`commits`, `merge`, `cherry-pick`, `revert`), copied from the push option `origo.operation=<name>` in the entry header |

Spec 019 defines the administration kinds and spec 014 the `verified`
kind; each carries the shared fields spec 019 lists (`id`, `kind`,
`repo`, `owner`, `slug`, `at`, `pusher`) and no `seq`, and its `id` is
the UUID v5 of the key table above. The `ping` of spec 018 is sent by
`origod check` directly with the headers of the Deliver section and a
UUID v4 id; it is never an object, because the check has no delivery
loop.

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
of spec 004 bounds the cost. A push without a pack (a delete alone)
runs no quarantine in git and the hook reports an empty path; no
update of such a push needs the objects.

### Enqueue

Enqueue is one function, `events.Enqueue(ctx, repo, entry)` in
`internal/events`, `entry` an `events.Entry{Header, Refs, Forced}`: the
committed header, which `wal.Committed` carries so no caller reads the
entry back, the transaction, and the references the receive path found
forced. It is the only path that writes an event object for a `push`
entry. Three callers: `internal/httpgit` after the verdict `ok` is
delivered and `Cache.Advance` recorded the sequence; `internal/api`
after the commit of a `default_branch` change and of an undelete (spec
003); and the server-side operations of spec 020 after their commit.
`Enqueue` appends the line `<repo> <seq>` to the in-memory journal,
then writes the event object with `attempts: 0` and `next_at` now, and
writes no object for an entry whose `push_options` contains
`origo.event=off`, for an undelete (the payload rules above), or when
`ORIGO_EVENTS_URL` is unset. A failed object write is logged and the
push is still acknowledged: the repair sweep writes the event from the
index.

The journal line is written by `Enqueue`, after the verdict, and not
before the index create: a push whose node died between the index
create and `Enqueue` has no line of its own. Repair covers that push
all the same, because the journal names repositories, not pushes: the
sweep reads the newest index of every repository a dead node's
journals name and writes the event of every `push` entry above the
cursor that has no object, so the push is repaired whenever an earlier
push through that node put the repository in today's or yesterday's
journal. That is the case the journal exists for. The one case it does
not cover is a node's first push to a repository in two days that died
before `Enqueue`; the caller of that push saw success, and nothing
names the repository to the sweep. The in-memory journal is flushed to
`origo/events/nodes/<node>/<date>.log` every 10 seconds, and at once
on the first line for a repository in a day; a flush that fails is
logged and retried at the next tick, and the pushes it holds are
covered once it lands. A node loads its own journals of today and
yesterday into memory at start-up, before the start-up repair below,
so the first flush after a restart rewrites the day with the earlier
process's lines; a line already in memory is not taken twice. A day's object is at most one line per push, a
few MiB for the busiest node; the sweep deletes journals older than 2
days.

`Emit` is the second function of the package, for events that have no
entry: `events.Emit(ctx, repo, kind, at, pusher, extra)` takes `at`
and `pusher` as arguments, because the id is derived from `at` and
every kind carries the same `pusher`, keeps `at` to the second in UTC
so the payload's `at` and the id agree, fills the shared fields
(`id` as the key table says, `kind`, `repo`, `owner`, `slug`, `at`,
`pusher`) around the extra fields the caller gives, writes
`origo/events/<repo>/a-<id>.json` with `attempts: 0` and `next_at` now,
and returns; it writes no journal line, because there is no entry to
rebuild the event from. It is called by every administration operation
of spec 019 after its write and before its response, and by `verify`
of spec 014, and does nothing when `ORIGO_EVENTS_URL` is unset. A
failed write is logged and the operation still answers; a caller that
repeats the operation produces a new `at` and a new event, and a
caller that retries `Emit` for one operation produces the same key. A
node that dies between an operation's write and its `Emit` leaves no
event for it: the operation's caller saw no response, and repeats it.

### Deliver

Each node runs a delivery loop over the events it enqueued or emitted
and over what the sweep hands it, one loop for every kind: `POST
<ORIGO_EVENTS_URL>` with the body being the `event` object alone, a 10
second timeout, and these headers:

| Header | Value |
|---|---|
| `Origo-Signature` | `sha256=<hex HMAC-SHA256 over the body with ORIGO_EVENTS_SECRET>` |
| `Origo-Event` | the `kind` |
| `Origo-Delivery` | the event `id` |

A 2xx deletes the object, advances the cursor's `seq` when the
sequence is higher or adds the id to its `delivered` set for an event
without one, and counts `origo_events_delivered_total`. Anything else
increments `attempts`, sets `next_at` from the schedule, and rewrites
the object; the schedule, the window, and the dead-letter move below
apply to every kind alike:

| Attempt | Delay before the next |
|---|---|
| 1 | 1 s |
| 2 | 10 s |
| 3 | 1 min |
| 4 | 10 min |
| 5 | 1 h |
| 6 and later | 1 h |

Once `at` is more than 24 hours in the past the object moves under
`origo/events/dead/<repo>/` under its own key, `origo_events_dead_total`
increments, and the alert of spec 011 fires. The consumer treats
delivery as at least once and keys on `id`: two nodes can deliver one
event when a sweep overlaps a loop.

Deliveries are one at a time per node, in key order, so a repository's
events leave a node in sequence order while the sink answers, and a
slow sink bounds the rate at one delivery per round trip. That is the
rule: a consumer reads a repository's events in order in the common
case, at-least-once means a retry can still arrive after a later
event, and spec 005 spreads repositories over nodes, so a deployment's
delivery rate grows with its nodes. Concurrent deliveries with the
per-repository cursor serialised are a later spec's if a consumer
needs them.

### Repair

A sweep every `ORIGO_REPAIR_INTERVAL` (spec 002, default 10 minutes) on
every node, offset by the node's name so nodes do not sweep together,
does two things over `origo/events/`:

1. Delivers every pending object, of every kind, whose `next_at` is
   more than 1 minute in the past, which is what a dead node left
   behind; an `a-<id>.json` whose id the cursor's `delivered` set holds
   is deleted instead. An object whose modification time in the
   listing is within the minute is skipped without a read, because
   `next_at` is never before the write; every other pending object
   costs one read per sweep.
2. Reads the journals of nodes that are not in the live set of spec 005
   (the `events.Membership` interface, `LastHeard(node)`, which spec
   005 wires; nil until then, so every other node counts as never
   heard since this node started and a two-node harness repairs
   without gossip) and were last heard more than `ORIGO_REPAIR_UNHEARD`
   ago (spec 002, default 5 minutes), or never heard since this node
   started, for today and yesterday. For every repository
   those journals name, it reads the newest index and, for each listed
   `push` entry with a sequence above the cursor and an `at` older than
   1 minute that has no event object, reads the entry head and writes
   the event from it: `updates` from the transaction, `pusher` from the
   header, `forced` false, `operation` from the header, `kind_detail`
   from the transaction as the payload table says, and `id` from
   `repo` and `seq` as the payload table says,
   so a consumer that received the original before the node died and
   the repair after it sees one id. The object is written by
   create-if-absent, so two sweeps over one entry produce one object,
   and an entry whose event sits under `dead/` is skipped, because
   rebuilding it would move it back under `dead/` on every sweep after
   the window (the operator's replay is the move out of `dead/`, Not
   in this spec). This covers a node that died
   between the index create and the enqueue, for a repository the
   journal names (Enqueue, above); an entry whose header carries
   `origo.event=off`, and an undelete, are skipped as the enqueue skips
   them. Such an entry above the cursor is read again on every pass,
   two `HEAD` requests and one entry-head read, because the cursor
   moves only on a delivery; the cost is bounded by the journal's
   life, since this step reads today's and yesterday's journals only:
   the re-read ends at most two days after the dead node's last push
   to the repository, 288 passes at the default interval, and at once
   when the node is heard again. This step rebuilds `push` events only: an event without a
   sequence has no entry to rebuild from, and step 1 is what covers it.
   A node also runs this step
   once at start-up over its own journals, which covers a restart
   under the same name. No sweep lists every repository: the cost is
   one listing of `origo/events/nodes/` and one index read per
   repository a dead node touched. The two durations are variables so
   a test sets them to seconds; a deployment keeps the defaults.

## Not in this spec

The payloads of the kinds other than `push` (specs 014, 018, 019); this
spec owns how every kind is keyed, delivered, retried, and repaired.
Delivery to more than one sink. Replay of dead events; an operator
moves an object back under `origo/events/<repo>/` to retry it.

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
  after the index create and the verdict, on its second push to a
  repository its first push put in its journal, yields one event for
  the second push from another node's repair sweep within one
  `ORIGO_REPAIR_INTERVAL` once the dead node is `ORIGO_REPAIR_UNHEARD`
  unheard, with `updates` equal to the entry's transaction and `id`
  equal to the UUID v5 the payload table defines, and the dead node's
  journal names the repository once. The test starts two nodes of its
  own against the stack's MinIO through `ORIGO_TEST_S3_ENDPOINT` and
  its sibling variables, which spec 013's job fills with the overlay's
  values, with `ORIGO_EVENTS_URL` pointing at the stub sink's host port
  of spec 013's ports table, `ORIGO_GOSSIP_PEERS` and
  `ORIGO_GOSSIP_SECRET` set so the survivor's live set drops the dead
  node, and `ORIGO_REPAIR_UNHEARD` and `ORIGO_REPAIR_INTERVAL` set in
  seconds on both, `5s` and `10s`; one node carries the failpoint and
  is killed by it, the other runs the sweep. A failpoint of spec 002
  has no count, so the test starts the first node without it for the
  first push and restarts it under its name and data directory with
  it before the second (proposed: `test/e2e`,
  `TestSlowEventRepairAfterKill`, in the `e2e-slow` job of spec 013).
- `Emit` called twice for one operation with one `at` writes one
  object under `a-<id>.json` and delivers one event; called for two
  operations of one kind with different `at` values it writes two; a
  sink answering 500 sees the same retry schedule and the same
  dead-letter move for an `a-<id>.json` as for a `<seq>.json`; and an
  `a-<id>.json` left pending by a dead node is delivered by another
  node's step 1 within one `ORIGO_REPAIR_INTERVAL`, unless its id is
  in the cursor's `delivered` set, in which case it is deleted
  (proposed: `internal/events`, `TestEmitIsIdempotent`,
  `TestEmittedEventsRetryAndRepairLikePushes`).
- `origo_push_duration_seconds{phase}` observes the four phases of one
  push, each once, and the sum is within 10% of the push's request
  duration (proposed: `internal/httpgit`, `TestPushPhasesAreObserved`).
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

## Outcome

Built on 2026-09-08 in seven steps: `wal.Committed` carrying the
entry header and the two commit-phase durations, the configuration
rule and the two repair durations, `internal/events`, the receive
path, the repository API, the node wiring, and the stack test. Every
criterion has a passing test in the tree; the stack criterion runs in
the `e2e-slow` job of spec 013 and also against a bare MinIO with an
in-process sink, which is how it was verified on this machine.

| Criterion | Test |
|---|---|
| 1 000 pushes, one delivered event each, signed, `Origo-Event: push`, `Origo-Delivery` equal to the body's `id` | `internal/events`, `TestThousandPushesOneEventEach` |
| the retry schedule on a fake clock, the dead-letter object and `origo_events_dead_total` 24 hours after `at` | `internal/events`, `TestRetryScheduleAndDeadLetter` |
| a node killed at `events.before-enqueue` on its second push, repaired by another node's sweep with the entry's transaction and the UUID v5, the journal naming the repository once | `test/e2e`, `TestSlowEventRepairAfterKill`, in the `e2e-slow` job |
| `Emit` idempotent for one `at`, two events for two `at` values, the same schedule and dead-letter move, step 1 delivering or deleting a pending `a-<id>.json` | `internal/events`, `TestEmitIsIdempotent`, `TestEmittedEventsRetryAndRepairLikePushes` |
| the four phases of `origo_push_duration_seconds`, each once, within 10% of the request | `internal/httpgit`, `TestPushPhasesAreObserved` |
| the sweep reads only unheard nodes' journals and only their repositories' indexes | `internal/events`, `TestRepairReadsOnlyDeadJournals` |
| one id for the enqueue and the repair, two for two entries, equal to the UUID v5 the test computes | `internal/events`, `TestEventIDIsDeterministic` |
| `pusher.sub` and `pusher.actor` from `act` and `sub`, `actor` empty without `act` | `internal/events`, `TestPusherCarriesSubjectAndActor`, from the entry header; the token's `act` and `sub` reaching the header is spec 007's `TestActClaimIsRecordedOnEntryAndAuthorizer` |
| no `push` event for an undelete from either path, `undeleted` once; the `default_branch` change as one `HEAD` update with `kind_detail` | `internal/events`, `TestUndeleteEmitsNoPushEvent`, `TestDefaultBranchChangeIsAHeadUpdate`; the wiring in `internal/api`, `TestLifecycleEventsAreEmitted` |
| the URL without the secret fails the start-up with the one message | `internal/config`, `TestEventsURLNeedsTheSecret` |
| `origo.event=off` suppresses that push alone, from the enqueue and from the sweep | `internal/events`, `TestEventOffSuppressesOnlyThatPush` |
| `forced` computed while the objects are in quarantine, a fast-forward not forced | `internal/httpgit`, `TestForcedFlagIsComputedBeforeTheVerdict` |

The dispatcher runs on the node when `ORIGO_EVENTS_URL` is set
(`cmd/origod`, `TestEventsLoopRunsWithTheSink`). Coverage: `internal/events`
95%, `internal/httpgit` 94%, `internal/api` 93%, `internal/config`
100%, `cmd/origod` 94%.

Divergences and interpretations, all kept:

- `Enqueue(ctx, repo, entry)` takes `events.Entry{Header, Refs,
  Forced}`: the committed header, which `wal.Committed` now carries so
  no caller reads the entry back, the transaction, and the references
  the receive path found forced. `Emit(ctx, repo, kind, at, pusher,
  extra)` takes `at` and `pusher` as arguments rather than inside a
  payload, because the id is derived from `at` and every kind carries
  the same `pusher`; `at` is kept to the second in UTC, the precision
  the id is derived from, so the payload's `at` and the id agree.
- The live set is the `events.Membership` interface (`LastHeard(node)`),
  nil until spec 005 wires its set: every other node then counts as
  never heard since this node started, which the Repair section names,
  so a two-node harness repairs without gossip.
- A node loads its own journals of today and yesterday into memory at
  start-up before it runs the start-up repair, so a restart under the
  same name keeps the lines an earlier process wrote; the Design's
  "rewritten whole from the in-memory copy" would otherwise drop them
  on the first flush, and a push younger than the one-minute lag at
  start-up would lose its only cover. A line already in memory is not
  taken twice.
- The repair sweep writes a rebuilt event by create-if-absent, so two
  sweeps over one entry produce one object, and it skips an entry whose
  event sits under `dead/`, because rebuilding it would move it back to
  `dead/` on every sweep after the window; the operator's replay is a
  move out of `dead/` as the Not in this spec section says.
- Step 1 skips a listed object modified within the lag without reading
  it, since `next_at` is never before the write; every other pending
  object costs one read per sweep.
- The stack test restarts the first node under its name and data
  directory with the failpoint for the second push, because a
  failpoint of spec 002 has no count and would end the first push.
- A push without a pack (a delete alone) runs no quarantine in git and
  the hook reports an empty path; no update of such a push needs the
  objects.
- The `apply` phase spans the forced computation, the verdict, git's
  own reference update, and the local advance; the enqueue falls
  outside the four phases, and the refused push observes `receive`
  alone.
- `TestThousandPushesOneEventEach` commits its entries through
  `Log.Commit` and enqueues them the way the receive path does, because
  a thousand `git push` processes do not fit the unit suite; the
  receive path's own enqueue is asserted by the `forced` test.
- The dispatcher's default HTTP client sets an explicit transport of
  the node's outbound shape; `pkg/otel`'s instrumented client would
  put the OpenTelemetry SDK on the node's build list, which is spec
  011's to add, and 011 wraps this transport when it lands.
- Deliveries are one at a time per node, the one loop the Design
  names; a sink that answers slowly bounds the rate at one delivery per
  round trip.

No criterion is deferred. The twelfth review round decided the two
items the first build left open: deliveries stay one at a time per
node, with the reason in the Deliver section, and the re-read of a
trailing suppressed entry is bounded by the journal's life, stated in
the Repair section.
