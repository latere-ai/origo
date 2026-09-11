# condwrite

A probe for the conditional primitives Origo's write-ahead log (spec 004)
relies on, runnable against any S3 endpoint. It answers, with the wire
status codes as evidence:

- `PUT If-None-Match: *` creates an absent key and is refused with 412 on
  an existing one, without touching the object.
- N writers that race to create one key with `If-None-Match: *` at the
  same instant see exactly one 200 per round, and the stored object is the
  winner's. This is the commit primitive of the immutable-index design.
- `HEAD` answers 404 for an absent key and 200 with the ETag for a present
  one, the currency check of that design.
- `GET If-None-Match: <etag>` answers 304 for the current ETag and 200 with
  the body for a stale one.
- Optional, recorded for the provider table: `PUT If-Match: <etag>`
  succeeds with a new ETag when current and is refused with 412 when
  stale, and N writers firing a CAS on one ETag see exactly one winner.
  Reported as `absent` when the provider refuses it; the run still passes.
- Fallbacks a provider without conditional PUT would need: whether
  `CopyObject` honours `If-Match` and `If-None-Match` on the destination,
  and whether the bucket can version objects.
- Latency of the 304, the `HEAD` 404 and 200, a successful CAS PUT, and
  the unconditional GET and PUT, 200 samples each, reported as min, p50,
  p95, p99, max, mean.

Every object is written under one random prefix, `origo-spike/<random>/`,
and deleted at the end, versions and delete markers included.

This tool is its own Go module so the AWS SDK stays out of Origo's
dependency footprint. Nothing in `origod` imports it.

## Run

Against a local MinIO (the bucket is created and deleted by the run):

```sh
podman run -d --rm --name minio -p 127.0.0.1:9000:9000 quay.io/minio/minio server /data
cd tools/spike/condwrite
S3_KEY=minioadmin S3_SECRET=minioadmin go run . \
  -endpoint http://127.0.0.1:9000 -bucket origo-spike -path-style -create-bucket
```

Against an existing bucket on any provider, with the credentials in the
environment (never on the command line):

```sh
export S3_ENDPOINT=https://fra1.digitaloceanspaces.com S3_REGION=fra1 \
       S3_BUCKET=<bucket> S3_KEY=<key> S3_SECRET=<secret>
cd tools/spike/condwrite && go run . -json /tmp/spaces.json
```

`SPACES_*` and `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` are read as
aliases. Flags: `-samples` (200), `-writers` (16), `-rounds` (20),
`-prefix`, `-keep` to leave the objects for inspection, `-json <file>` for
a machine readable report. Bucket versioning is only enabled on a bucket
the run created.

The exit status is 0 when every primitive the design relies on behaves:
create-if-absent, the create race, `HEAD`, and the conditional `GET`.
Optional rows (`If-Match`) and fallback rows never fail the run.

Findings per provider are recorded in `docs/spikes/`.
