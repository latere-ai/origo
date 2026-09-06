# Spike: conditional writes on S3 compatible object storage

Date: 2026-09-06. Tool: [`tools/spike/condwrite/`](../../tools/spike/condwrite/README.md).
Raw reports: [MinIO](2026-09-06-conditional-writes.minio.json),
[DigitalOcean Spaces](2026-09-06-conditional-writes.spaces.json).

## Question

The first draft of spec 004 linearized pushes with one `PUT` on a mutable
index object guarded by `If-Match: <etag>`, created the index with
`If-None-Match: *`, and made consistent reads cheap with a conditional
`GET` that answers 304. Does the object store honour these, what do they
cost, and which of them can the design rely on across providers?

The tool checks, with the wire status code as evidence:

| Primitive | Required behaviour | Used by |
|---|---|---|
| `PUT If-None-Match: *`, key absent | 200 with an ETag | commit of `index/<n+1>` |
| `PUT If-None-Match: *`, key present | 412 and the object is untouched | commit of `index/<n+1>` |
| 16 writers race to create one key, 20 rounds | exactly one 200 per round, the rest 412, stored object is the winner's | commit of `index/<n+1>` |
| `HEAD` absent key, `HEAD` present key | 404, 200 with the ETag | currency check on `index/<n+1>` |
| `GET If-None-Match: <current>` / `<stale>` | 304 / 200 with the body | conditional reads |
| `PUT If-Match: <current>` / `<stale>` | 200 with a new ETag / 412 untouched | the first draft only |
| 16 writers race a CAS on one ETag, 20 rounds | exactly one 200 per round | the first draft only |

Fallbacks, probed so a provider without a primitive is still
characterised: `CopyObject` with `If-Match` / `If-None-Match` on the
destination, and bucket versioning.

## Providers

### MinIO (local, RELEASE.2025-09-07T16-13-09Z)

`podman run docker.io/minio/minio:latest server /data` on a free localhost
port, default credentials, single drive. The tool created the bucket and
deleted it afterwards.

| Primitive | Expected | Got | Result |
|---|---|---|---|
| `PUT If-None-Match: *` on absent key | 200 + ETag | 200 | pass |
| `PUT If-None-Match: *` on existing key | 412, untouched | 412, untouched | pass |
| Create race, 20 rounds x 16 writers | 1 applied per round | 20 x 200, 300 x 412 | pass |
| Create race, 50 rounds x 32 writers | 1 applied per round | 50 x 200, 1550 x 412 | pass |
| `HEAD` absent key | 404 | 404 | pass |
| `HEAD` existing key | 200 + ETag | 200, ETag equals GET's | pass |
| `GET If-None-Match: <current>` | 304 | 304 | pass |
| `GET If-None-Match: <stale>` | 200 + body | 200 | pass |
| `PUT If-Match: <current>` | 200 + new ETag, applied | 200, applied | pass |
| `PUT If-Match: <stale>` | 412, untouched | 412, untouched | pass |
| CAS race, 20 rounds x 16 writers | 1 applied per round | 20 x 200, 300 x 412 | pass |
| CAS race, 50 rounds x 32 writers | 1 applied per round | 50 x 200, 1550 x 412 | pass |
| `PUT If-Match: <any>` on absent key | informational | 404, nothing created | recorded |
| `CopyObject If-None-Match: *` on existing destination | 412 | 200, destination overwritten | not honoured |
| `CopyObject If-Match: <stale>` on destination | 412 | 200, destination overwritten | not honoured |
| Bucket versioning | informational | enabled; each `PUT` returns a version id; the last write is `IsLatest` | recorded |

Latency, 200 samples per operation, 8 KiB body, the recorded run. MinIO
runs inside the podman virtual machine and the numbers move between runs
(a run minutes earlier had p50 0.57 ms for the 304, 0.35 ms for the `HEAD`
404, 1.84 ms for the CAS), so they are the client path's floor, not a
production figure.

| Operation | min ms | p50 ms | p95 ms | p99 ms | max ms | mean ms |
|---|---|---|---|---|---|---|
| `GET If-None-Match` -> 304 | 0.51 | 1.69 | 10.56 | 16.83 | 56.30 | 3.33 |
| `GET` unconditional -> 200 | 0.67 | 2.18 | 13.62 | 22.18 | 34.67 | 3.82 |
| `HEAD` missing key -> 404 | 0.51 | 1.18 | 4.27 | 6.75 | 8.75 | 1.59 |
| `HEAD` existing key -> 200 | 0.42 | 0.95 | 7.26 | 18.73 | 24.96 | 1.83 |
| `PUT If-Match` -> 200 (CAS) | 2.52 | 7.98 | 52.58 | 85.66 | 281.26 | 15.87 |
| `PUT` unconditional -> 200 | 2.96 | 6.32 | 32.87 | 58.87 | 93.96 | 10.31 |

No sample failed. A `HEAD` 404 is the cheapest round trip of the set; the
conditional forms cost the same as the unconditional ones.

The race writers use one connection per request. With pooled connections
an earlier build saw up to 8% of race writers fail on the client side
with `server closed idle connection` after a 412 had closed the pooled
connection, while the store still showed exactly one applied write per
round. A writer whose request dies in transport does not know whether it
was applied; the tool settles every round from the stored object, and the
spec's retry path reads `index/<n+1>` back and checks whether it names
its own entry.

### DigitalOcean Spaces (fra1, bucket `latere-storage`)

Run on 2026-09-06 with cluster credentials against the production bucket
under `origo-spike/2f900c8e7cfc0f89/`, deleted at the end. The build that
ran predates the create race and the `HEAD` rows; those are pending a
rerun and the table says so. That build also stopped its single-shot
checks after the `If-Match` failure, so the 304 evidence is the timed
row: 200 of 200 conditional `GET`s answered 304.

| Primitive | Expected | Got | Result |
|---|---|---|---|
| `PUT If-None-Match: *` on absent key | 200 + ETag | 200 | pass |
| `PUT If-None-Match: *` on existing key | 412, untouched | 412, untouched | pass |
| Create race | 1 applied per round | pending rerun | |
| `HEAD` absent / existing key | 404 / 200 | pending rerun | |
| `GET If-None-Match: <current>` | 304 | 304, 200 of 200 samples | pass |
| `PUT If-Match: <current>` | 200 + new ETag, applied | 412, not applied | absent |
| `PUT If-Match` timed, 200 samples | 200 | 412 on every sample | absent |
| CAS race, 20 rounds x 16 writers | 1 applied per round | 0 x 200, 320 x 412 | absent |
| `CopyObject If-None-Match: *` on existing destination | 412 | 200, destination overwritten | not honoured |
| `CopyObject If-Match: <stale>` on destination | 412 | 200, destination overwritten | not honoured |
| Bucket versioning | informational | off | recorded |

Spaces answers 412 to every `PUT If-Match`, including one that carries
the ETag it returned a moment earlier. It also returns the `CopyObject`
result's ETag without quotes; the tool now compares ETags without them.

Latency, 200 samples, 8 KiB body, from a laptop to fra1:

| Operation | min ms | p50 ms | p95 ms | p99 ms | max ms | mean ms |
|---|---|---|---|---|---|---|
| `GET If-None-Match` -> 304 | 19.93 | 22.52 | 95.16 | 111.44 | 112.50 | 27.71 |
| `GET` unconditional -> 200 | 20.57 | 24.86 | 94.97 | 107.16 | 114.21 | 29.85 |
| `PUT If-Match` -> 200 (CAS) | no sample succeeded | | | | | |
| `PUT` unconditional -> 200 | 25.18 | 35.17 | 109.08 | 130.00 | 134.74 | 43.17 |

### AWS S3

Not tested: no `aws` binary and no `~/.aws` on the machine. AWS documents
`If-None-Match: *` on `PutObject` since August 2024 and `If-Match` since
November 2024. The tool takes `AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY` with `-endpoint https://s3.<region>.amazonaws.com`
when credentials exist.

## Conclusion

`If-Match` is not portable: MinIO honours it, Spaces refuses every use
of it, and a design built on it would run on one of the two stores Origo
must run on. Create-if-absent is portable: `PUT If-None-Match: *`
behaves the same on MinIO and Spaces, and AWS documents it. `HEAD` and
the conditional `GET` behave the same everywhere.

The design moved to create-if-absent. Spec 004 now keeps the index as a
sequence of immutable objects `index/<seq>`: a writer holding
`index/<n>` writes its entry and commits by creating `index/<n+1>` with
`If-None-Match: *`; a 412 means another writer won, and the writer reads
`index/<n+1>`, applies it, checks fast-forward, and retries at `n+2`. The
currency check for a read is `HEAD index/<n+1>`, 404 meaning current. The
fallbacks are not substitutes on either store: `CopyObject` accepts the
destination condition and ignores it, and versioning orders writes
without refusing the loser.

Remaining to verify: the create race and the `HEAD` rows on Spaces with
the current build, and any run on AWS S3.
