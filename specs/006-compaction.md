---
title: "Compaction: primary-only repacks and log truncation"
status: drafted
track: infra
depends_on:
  - specs/004-write-ahead-log.md
affects: [internal/compact/, internal/wal/]
effort: medium
created: 2026-09-06
updated: 2026-09-06
author: changkun
---

# Compaction

## Overview

A repository that receives many pushes accumulates many small entries.
Serving a fetch from thousands of thin packs is slow and materializing
from them slower. Compaction repacks on one node and records the result as
a log entry, so every other node downloads packs instead of repacking.

## Current state

Spec 004 stores entries and lists packs in the index; nothing folds one
into the other.

## Design

### Trigger

The node that holds the top rendezvous score for a repository is its
compaction primary. It compacts when `len(index.entries) > 64` or when the
entries' bytes exceed 256 MiB or the index exceeds 512 KiB, checked after
each push it serves and by a sweep every 10 minutes for repositories it
holds. No other node compacts; a node that is not primary only downloads
packs.

### Procedure

1. Conditional GET the index; apply any missing entries locally.
2. `git repack -d --geometric=2 --write-midx --write-bitmap-index`, which
   folds small packs into geometrically sized ones and leaves large packs
   alone.
3. Upload every new pack with its `.idx` to `packs/<hash>.pack`; packs
   already listed are not re-uploaded.
4. Write a `compact` entry whose refs section is empty and whose pack
   section is absent, listing `packs` after and `compacted_through`.
5. `PUT index If-Match`: replace `packs`, drop entries at or below
   `compacted_through`, keep refs. On 412, restart from step 1; the work
   done is still valid unless a push landed, in which case the repack
   includes it next time.
6. After success, delete entries at or below `compacted_through` older
   than one hour, and packs no longer listed by any index older than one
   hour, so a node mid-materialization on the previous index still
   completes.

### Invariants

Compaction never changes a reference and never drops a reachable object,
proven by `git fsck --connectivity-only` on the repacked repository before
the index write. It runs under the same compare-and-swap as pushes, so a
push and a compaction never interleave in the index.

### Cost

Compaction is CPU on one node and upload bandwidth once; replicas trade
bandwidth for CPU. The tuning target is a repository receiving 100 pushes
per minute staying under 100 entries and its fetch latency flat.

## Acceptance criteria

- After 500 pushes to one repository, the index lists under 64 entries and
  at most 6 packs; `git fsck` passes on every node's copy.
- A push that lands during compaction is preserved and present in the
  next compaction.
- Deleting old entries never breaks a node that started materializing on
  the previous index (test with a delayed node).
- Fetch latency of a 1 GiB repository after 1 000 pushes is within 10% of
  its latency after 10 pushes.
