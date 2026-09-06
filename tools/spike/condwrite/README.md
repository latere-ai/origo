# condwrite

A probe for the conditional primitives Origo's write-ahead log (spec 004)
relies on, runnable against any S3 endpoint. It answers, with the wire
status codes as evidence:

- `PUT If-None-Match: *` creates an absent key and is refused with 412 on
  an existing one, without touching the object.
- `PUT If-Match: <etag>` succeeds with a new ETag when the ETag is current
  and is refused with 412, unapplied, when it is stale.
- `GET If-None-Match: <etag>` answers 304 for the current ETag and 200 with
  the body for a stale one.
- N writers that read one ETag and fire a CAS at the same instant see
  exactly one 200 per round, and the stored object is the winner's.
- Fallbacks a provider without conditional PUT would need: whether
  `CopyObject` honours `If-Match` and `If-None-Match` on the destination,
  and whether the bucket can version objects.
- Latency of the 304, of a successful CAS PUT, and of their unconditional
  counterparts, 200 samples each, reported as min, p50, p95, p99, max, mean.

Every object is written under one random prefix, `origo-spike/<random>/`,
and deleted at the end, versions and delete markers included.

This tool is its own Go module so the AWS SDK stays out of Origo's
dependency footprint. Nothing in `origod` imports it.

## Run

Against a local MinIO (the bucket is created and deleted by the run):

```sh
podman run -d --rm --name minio -p 127.0.0.1:9000:9000 docker.io/minio/minio server /data
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

The exit status is 0 when every primary primitive behaves. Fallback rows
are reported as honoured or not honoured and never fail the run.

Findings per provider are recorded in `docs/spikes/`.
