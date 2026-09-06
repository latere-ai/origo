# Spike: conditional writes on S3 compatible object storage

Date: 2026-09-06. Tool: [`tools/spike/condwrite/`](../../tools/spike/condwrite/README.md).
Raw report of the recorded MinIO run: [`2026-09-06-conditional-writes.minio.json`](2026-09-06-conditional-writes.minio.json).

## Question

Spec 004 linearizes pushes with one `PUT` on the index object guarded by
`If-Match: <etag>`, creates the index with `If-None-Match: *`, and makes
consistent reads cheap with a conditional `GET` that answers 304. Does the
object store honour these, and what do they cost?

For each provider the tool checks, with the wire status code as evidence:

| Primitive | Required behaviour |
|---|---|
| `PUT If-None-Match: *`, key absent | 200 with an ETag |
| `PUT If-None-Match: *`, key present | 412 and the object is untouched |
| `PUT If-Match: <current>` | 200 with a new ETag, the object replaced |
| `PUT If-Match: <stale>` | 412 and the object is untouched |
| `GET If-None-Match: <current>` | 304 |
| `GET If-None-Match: <stale>` | 200 with the body |
| 16 writers, one ETag, one instant, 20 rounds | exactly one write applied per round, the rest 412 |

Fallbacks, probed so a provider without conditional `PUT` is still
characterised: `CopyObject` with `If-Match` / `If-None-Match` on the
destination, and bucket versioning.

## Providers

### MinIO (local, RELEASE.2025-09-07T16-13-09Z)

Started with `podman run docker.io/minio/minio:latest server /data` on a
free localhost port, default credentials, single drive. The tool created
the bucket and deleted it afterwards.

| Primitive | Expected | Got | Result |
|---|---|---|---|
| `PUT If-None-Match: *` on absent key | 200 + ETag | 200 | pass |
| `PUT If-None-Match: *` on existing key | 412, content unchanged | 412, unchanged | pass |
| `PUT If-Match: <current>` | 200 + new ETag, applied | 200, new ETag, GET agrees | pass |
| `PUT If-Match: <stale>` | 412, content unchanged | 412, unchanged | pass |
| `GET If-None-Match: <current>` | 304 | 304 | pass |
| `GET If-None-Match: <stale>` | 200 + body | 200 | pass |
| `PUT If-Match: <any>` on absent key | informational | 404, nothing created | recorded |
| Concurrent CAS race, 20 rounds x 16 writers | 1 applied per round | 20 x 200, 300 x 412, 0 transport errors | pass |
| Concurrent CAS race, 50 rounds x 32 writers | 1 applied per round | 50 x 200, 1550 x 412, 0 transport errors | pass |
| `CopyObject If-None-Match: *` on existing destination | 412 | 200, destination overwritten | not honoured |
| `CopyObject If-Match: <stale>` on destination | 412 | 200, destination overwritten | not honoured |
| `CopyObject If-Match: <current>` on destination | 200 | 200 | honoured, but vacuous given the two rows above |
| Bucket versioning | informational | enabled; each `PUT` returns a version id; `ListObjectVersions` marks the last write `IsLatest` | recorded |

Latency, 200 samples per operation, index-sized body (8 KiB), three
consecutive runs. MinIO runs inside the podman virtual machine, so these
are the client path's floor, not a production figure.

| Operation | Run | min ms | p50 ms | p95 ms | p99 ms | max ms | mean ms |
|---|---|---|---|---|---|---|---|
| `GET If-None-Match` -> 304 | 1 | 1.09 | 1.50 | 3.11 | 4.58 | 7.01 | 1.73 |
| | 2 | 1.04 | 1.83 | 5.68 | 7.59 | 17.76 | 2.41 |
| | 3 | 0.46 | 0.64 | 0.95 | 1.21 | 1.50 | 0.67 |
| `GET` unconditional -> 200 | 1 | 1.31 | 1.91 | 3.87 | 5.89 | 7.33 | 2.21 |
| | 2 | 1.28 | 2.09 | 5.66 | 10.69 | 12.29 | 2.67 |
| | 3 | 0.56 | 0.71 | 0.91 | 1.12 | 1.16 | 0.74 |
| `PUT If-Match` -> 200 (CAS) | 1 | 4.66 | 7.40 | 17.13 | 27.42 | 35.45 | 8.78 |
| | 2 | 2.39 | 5.01 | 14.93 | 51.43 | 70.19 | 6.94 |
| | 3 | 1.75 | 2.52 | 4.21 | 5.96 | 6.89 | 2.73 |
| `PUT` unconditional -> 200 | 1 | 4.10 | 6.35 | 12.56 | 19.69 | 29.31 | 7.20 |
| | 2 | 2.53 | 5.23 | 16.41 | 21.37 | 27.27 | 6.93 |
| | 3 | 1.92 | 2.85 | 5.69 | 7.03 | 8.74 | 3.18 |

No sample failed. The conditional forms cost the same as the unconditional
ones: the 304 saves the body transfer and nothing else at this size, and a
CAS `PUT` is a plain `PUT` plus an ETag comparison.

One observation from an earlier run of the same build: two of 320 race
writers got `http: server closed idle connection` from the Go HTTP client
before the request was sent, while the store still showed exactly one
applied write per round. A writer whose CAS dies in transport does not
know whether it was applied. The tool now settles a round from the stored
object, and the spec gains the same rule: a node that loses the response
to its index `PUT` reads the index back and treats its own entry being
listed as success.

### DigitalOcean Spaces

Not tested. No Spaces credentials were on this machine: `drive/.env` and
`sandbox/.env` do not exist, the `SPACES_*` variables in the one local
`.env` that has them point at a local MinIO, and the environment carries
none. Run the tool against the production bucket before spec 004 is
validated:

```sh
export S3_ENDPOINT=https://<region>.digitaloceanspaces.com S3_REGION=<region> \
       S3_BUCKET=<bucket> S3_KEY=<key> S3_SECRET=<secret>
cd tools/spike/condwrite && go run . -json /tmp/spaces.json
```

The run writes only under `origo-spike/<random>/` and deletes it. If
Spaces refuses `If-Match` on `PUT`, the CopyObject rows of the report say
whether the copy-then-condition substitute exists there, and the
versioning row says whether the bucket can order writes at all.

### AWS S3

Not tested. `aws sts get-caller-identity` could not run: no `aws` binary
and no `~/.aws` on this machine. AWS documents `If-None-Match` and
`If-Match` on `PutObject` since 2024, and the tool takes
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` with `-endpoint
https://s3.<region>.amazonaws.com` when credentials exist.

## Recommendation for spec 004

Proceed as designed. On MinIO every primitive spec 004 relies on behaves
exactly as the design assumes, including the property the whole log rests
on: writers racing on one ETag produce exactly one applied write, and the
losers are refused with 412 without side effects. Keep the index CAS on
`PUT If-Match` and index creation on `PUT If-None-Match: *`.

Do not plan a `CopyObject` fallback. MinIO accepts `If-Match` and
`If-None-Match` on a `CopyObject` destination and ignores them, which is
worse than refusing: a substitute built on it would look correct and lose
writes. Do not plan a versioning fallback either. Versioning records the
order of unconditional writes but refuses none of them, so a loser learns
it lost only after its push was acknowledged, which breaks invariant 1 of
spec 001.

Two additions the spike forces on the spec:

1. The retry path after a `PUT` whose response is lost reads the index
   and checks for its own entry before retrying, since the write may have
   been applied.
2. A conformance check, the tool itself or its checks lifted into
   `test/e2e`, runs against every bucket Origo is pointed at before that
   bucket is used, because a provider that silently ignores a condition
   is indistinguishable from one that honours it until two pushes race.

Spaces and AWS remain to be run with the same tool; the spec's status
stays drafted until the production endpoint passes.
