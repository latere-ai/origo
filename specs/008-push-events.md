---
title: "Push events: signed webhooks per reference update"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
  - specs/007-authentication-and-delegation.md
affects: [internal/events/, internal/wal/]
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

Spec 003 fixes the payload. The index update in spec 004 is the moment an
update is durable and the right place to enqueue.

## Design

### Enqueue

In the same code path that acknowledges a push, after the index CAS
succeeds, the node writes `origo/events/<repo>/<seq>.json` with the
payload of spec 003 and the push options. The write is part of the
acknowledged path so an event is never lost with an acknowledged push;
if the event object write fails the push is still acknowledged and the
sweeper repairs from the index (below).

### Deliver

Each node runs a delivery loop over events it wrote: `POST
ORIGO_EVENTS_URL` with `Origo-Signature: sha256=<hmac(secret, body)>`,
`Origo-Event: push`, `Origo-Delivery: <event id>`, 10 second timeout.
A 2xx deletes the object. Anything else retries with backoff 1s, 10s,
1m, 10m, 1h, hourly to 24 hours, then the object is moved to
`origo/events/dead/` and `origo_events_dead_total` increments and alerts.
The consumer treats delivery as at least once and keys on `id`.

### Repair

A sweep every 10 minutes lists `origo/events/` and delivers anything a
dead node left behind. A second sweep compares each repository's index
`entries` against delivered sequences for the last hour and writes a
missing event, which covers a node that died between the CAS and the
event write.

### Suppression

`git push -o origo.event=off` suppresses the event for that push, for
consumers that push on a user's behalf and already know.

## Acceptance criteria

- Every push in a 1 000 push load test produces exactly one delivered
  event with a valid signature, in a consumer stub that records deliveries.
- A consumer returning 500 sees retries at the documented intervals and a
  dead-letter object after the window.
- Killing the node between the index write and the event write yields
  one event from the repair sweep within 10 minutes.
- `origo.event=off` suppresses delivery and nothing else.
