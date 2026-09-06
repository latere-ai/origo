// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command condwrite probes an S3 endpoint for the conditional primitives
// spec 004 builds on: create-if-absent (PUT If-None-Match: *), HEAD as the
// currency check, and the conditional GET that answers 304; and for the
// compare-and-swap (PUT If-Match: <etag>) the design no longer relies on,
// so a provider table says what each store lacks. It records whether each
// primitive behaves, races concurrent writers to create one key and to CAS
// one key to confirm exactly one winner, measures the latency of each
// operation, and probes the fallbacks (CopyObject with a destination
// condition, bucket versioning). Everything it writes lives under one
// random prefix that it deletes at the end.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// check is the outcome of one primitive probe. Expect and Got are HTTP
// status codes or short phrases; Pass says whether Got is acceptable for
// the design, and Note carries the evidence a reader needs.
type check struct {
	Name   string `json:"name"`
	Expect string `json:"expect"`
	Got    string `json:"got"`
	Pass   bool   `json:"pass"`
	Note   string `json:"note,omitempty"`
	// Fallback marks a probe of a substitute primitive. Its outcome is
	// recorded but does not decide the exit status: the design needs the
	// primary primitives, and a fallback matters only when those are absent.
	Fallback bool `json:"fallback,omitempty"`
	// Optional marks a primitive the design no longer relies on (If-Match
	// on PUT, which not every provider honours). It is recorded so the
	// provider table stays complete and never decides the exit status.
	Optional bool `json:"optional,omitempty"`
}

// latency summarises N timed samples of one operation.
type latency struct {
	Name string  `json:"name"`
	N    int     `json:"n"`
	Min  float64 `json:"min_ms"`
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
	Mean float64 `json:"mean_ms"`
	Fail int     `json:"failures"`
}

type report struct {
	Endpoint  string    `json:"endpoint"`
	Bucket    string    `json:"bucket"`
	Prefix    string    `json:"prefix"`
	Server    string    `json:"server_header"`
	Started   time.Time `json:"started"`
	Checks    []check   `json:"checks"`
	Latencies []latency `json:"latencies"`
}

type options struct {
	endpoint, region, bucket, key, secret, prefix, jsonOut string
	pathStyle, createBucket, keep                          bool
	samples, writers, rounds                               int
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "condwrite:", err)
		os.Exit(1)
	}
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("condwrite", flag.ContinueOnError)
	fs.StringVar(&o.endpoint, "endpoint", env("S3_ENDPOINT", "SPACES_ENDPOINT"), "S3 endpoint URL (env S3_ENDPOINT or SPACES_ENDPOINT)")
	fs.StringVar(&o.region, "region", env("S3_REGION", "SPACES_REGION", "AWS_REGION"), "signing region; us-east-1 when empty")
	fs.StringVar(&o.bucket, "bucket", env("S3_BUCKET", "SPACES_BUCKET"), "bucket that already exists, or that -create-bucket makes")
	fs.StringVar(&o.key, "key", env("S3_KEY", "SPACES_KEY", "AWS_ACCESS_KEY_ID"), "access key (env S3_KEY, SPACES_KEY, AWS_ACCESS_KEY_ID)")
	fs.StringVar(&o.secret, "secret", env("S3_SECRET", "SPACES_SECRET", "AWS_SECRET_ACCESS_KEY"), "secret key (env S3_SECRET, SPACES_SECRET, AWS_SECRET_ACCESS_KEY)")
	fs.StringVar(&o.prefix, "prefix", "", "key prefix for every object written; origo-spike/<random>/ when empty")
	fs.StringVar(&o.jsonOut, "json", "", "write the report as JSON to this file")
	fs.BoolVar(&o.pathStyle, "path-style", env("S3_PATH_STYLE", "SPACES_PATH_STYLE") == "1", "path-style addressing (MinIO)")
	fs.BoolVar(&o.createBucket, "create-bucket", false, "create the bucket first and delete it at the end (local MinIO only)")
	fs.BoolVar(&o.keep, "keep", false, "leave the objects under the prefix in place")
	fs.IntVar(&o.samples, "samples", 200, "latency samples per operation")
	fs.IntVar(&o.writers, "writers", 16, "concurrent writers per race round")
	fs.IntVar(&o.rounds, "rounds", 20, "race rounds")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.endpoint == "" || o.bucket == "" || o.key == "" || o.secret == "" {
		return o, errors.New("endpoint, bucket, key, and secret are required")
	}
	if o.region == "" {
		o.region = "us-east-1"
	}
	if o.prefix == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return o, err
		}
		o.prefix = "origo-spike/" + hex.EncodeToString(b[:]) + "/"
	}
	if !strings.HasSuffix(o.prefix, "/") {
		o.prefix += "/"
	}
	return o, nil
}

func env(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// serverRecorder is the transport under the SDK: it keeps the Server
// header of the last response so the report names the implementation.
type serverRecorder struct {
	next   http.RoundTripper
	server atomic.Pointer[string]
}

func (t *serverRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err == nil {
		if s := resp.Header.Get("Server"); s != "" {
			t.server.Store(&s)
		}
	}
	return resp, err
}

func run(ctx context.Context, args []string, out io.Writer) error {
	o, err := parse(args)
	if err != nil {
		return err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	rec := &serverRecorder{next: tr}
	newClient := func(rt http.RoundTripper) *s3.Client {
		return s3.New(s3.Options{
			Region:                     o.region,
			BaseEndpoint:               aws.String(o.endpoint),
			UsePathStyle:               o.pathStyle,
			Credentials:                credentials.NewStaticCredentialsProvider(o.key, o.secret, ""),
			HTTPClient:                 &http.Client{Transport: rt},
			RetryMaxAttempts:           1,
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		})
	}
	// The races open one connection per write. A pooled connection that
	// the server closed after a 412 fails the next write on the client side
	// before it is sent, which measures the client's pool, not the store.
	// The timed operations keep the pool: that is the path a node runs.
	raceTr := tr.Clone()
	raceTr.DisableKeepAlives = true
	p := &probe{c: newClient(rec), rc: newClient(raceTr), bucket: o.bucket, prefix: o.prefix, o: o}
	rep := &report{Endpoint: o.endpoint, Bucket: o.bucket, Prefix: o.prefix, Started: time.Now().UTC()}

	if o.createBucket {
		if _, err := p.c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &o.bucket}); err != nil {
			return fmt.Errorf("create bucket: %w", err)
		}
	}
	if !o.keep {
		defer func() {
			if err := p.cleanup(context.WithoutCancel(ctx)); err != nil {
				fmt.Fprintln(os.Stderr, "condwrite: cleanup:", err)
			}
		}()
	}

	rep.Checks = append(rep.Checks, p.primitives(ctx)...)
	rep.Checks = append(rep.Checks, p.createRace(ctx), p.casRace(ctx))
	rep.Checks = append(rep.Checks, p.copyFallback(ctx)...)
	// Timing runs before the versioning probe, which may change the
	// bucket's write path on a bucket this run created.
	rep.Latencies = p.latencies(ctx)
	rep.Checks = append(rep.Checks, p.versioning(ctx)...)
	if s := rec.server.Load(); s != nil {
		rep.Server = *s
	}

	print(out, rep)
	if o.jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(o.jsonOut, b, 0o644); err != nil {
			return err
		}
	}
	for _, c := range rep.Checks {
		if !c.Pass && !c.Fallback && !c.Optional {
			return errors.New("at least one primitive did not behave; see the table")
		}
	}
	return nil
}

type probe struct {
	c      *s3.Client // pooled connections: primitives, timing, cleanup
	rc     *s3.Client // one connection per request: the races
	bucket string
	prefix string
	o      options
}

func (p *probe) key(name string) string { return p.prefix + name }

// status extracts the HTTP status of a failed SDK call, 200 for success.
func status(err error) int {
	if err == nil {
		return 200
	}
	var sc interface{ HTTPStatusCode() int }
	if errors.As(err, &sc) {
		return sc.HTTPStatusCode()
	}
	return 0
}

func got(err error) string {
	if err == nil {
		return "200"
	}
	if s := status(err); s != 0 {
		return fmt.Sprint(s)
	}
	return "error: " + err.Error()
}

func (p *probe) put(ctx context.Context, key string, body []byte, ifMatch, ifNoneMatch string) (string, error) {
	return putWith(ctx, p.c, p.bucket, key, body, ifMatch, ifNoneMatch)
}

func putWith(ctx context.Context, c *s3.Client, bucket, key string, body []byte, ifMatch, ifNoneMatch string) (string, error) {
	in := &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))}
	if ifMatch != "" {
		in.IfMatch = &ifMatch
	}
	if ifNoneMatch != "" {
		in.IfNoneMatch = &ifNoneMatch
	}
	res, err := c.PutObject(ctx, in)
	if err != nil {
		return "", err
	}
	return aws.ToString(res.ETag), nil
}

func (p *probe) get(ctx context.Context, key, ifNoneMatch string) ([]byte, string, error) {
	in := &s3.GetObjectInput{Bucket: &p.bucket, Key: &key}
	if ifNoneMatch != "" {
		in.IfNoneMatch = &ifNoneMatch
	}
	res, err := p.c.GetObject(ctx, in)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return b, aws.ToString(res.ETag), err
}

func (p *probe) head(ctx context.Context, key string) (string, error) {
	res, err := p.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &p.bucket, Key: &key})
	if err != nil {
		return "", err
	}
	return aws.ToString(res.ETag), nil
}

// body returns an index-sized payload that differs per tag.
func body(tag string) []byte {
	return []byte(fmt.Sprintf(`{"v":1,"tag":%q,"pad":%q}`, tag, strings.Repeat("x", 8<<10)))
}

func (p *probe) primitives(ctx context.Context) []check {
	var cs []check
	key := p.key("index")
	add := func(name, expect, g string, pass bool, note string) {
		cs = append(cs, check{Name: name, Expect: expect, Got: g, Pass: pass, Note: note})
	}

	// 1. create-if-absent on an absent key.
	e1, err := p.put(ctx, key, body("a"), "", "*")
	add("PUT If-None-Match:* on absent key", "200 + ETag", got(err), err == nil && e1 != "", "etag "+e1)
	if err != nil {
		return cs
	}

	// 2. create-if-absent on an existing key must be refused and not applied.
	_, err = p.put(ctx, key, body("b"), "", "*")
	b, _, gerr := p.get(ctx, key, "")
	unchanged := gerr == nil && bytes.Equal(b, body("a"))
	add("PUT If-None-Match:* on existing key", "412, content unchanged", got(err), status(err) == 412 && unchanged, fmt.Sprintf("content unchanged: %v", unchanged))

	// 3. CAS with the current ETag succeeds with a new ETag. Optional: the
	// design no longer relies on it, and a provider may refuse it.
	e2, err := p.put(ctx, key, body("c"), e1, "")
	b, e2get, gerr := p.get(ctx, key, "")
	applied := gerr == nil && bytes.Equal(b, body("c")) && e2get == e2
	add("PUT If-Match:<current>", "200 + new ETag, applied", got(err), err == nil && e2 != "" && e2 != e1 && applied, fmt.Sprintf("etag %s -> %s, GET agrees: %v", e1, e2, applied))
	cs[len(cs)-1].Optional = true
	cur, want := e2, body("c")
	if err != nil {
		cur, want = e1, body("a")
	}

	// 4. CAS with a stale ETag is refused and not applied.
	stale := `"0123456789abcdef0123456789abcdef"`
	_, err = p.put(ctx, key, body("d"), stale, "")
	b, e3get, gerr := p.get(ctx, key, "")
	unchanged = gerr == nil && bytes.Equal(b, want) && e3get == cur
	add("PUT If-Match:<stale>", "412, content unchanged", got(err), status(err) == 412 && unchanged, fmt.Sprintf("content unchanged: %v", unchanged))
	cs[len(cs)-1].Optional = true

	// 5. Conditional GET with the current ETag answers 304.
	_, _, err = p.get(ctx, key, cur)
	add("GET If-None-Match:<current>", "304", got(err), status(err) == 304, "")

	// 6. Conditional GET with a stale ETag answers 200 with the body.
	b, e4, err := p.get(ctx, key, stale)
	add("GET If-None-Match:<stale>", "200 + body", got(err), err == nil && bytes.Equal(b, want) && e4 == cur, "")

	// 7. HEAD on an absent and on a present key, the currency check of the
	// immutable-index design.
	_, err = p.head(ctx, p.key("absent"))
	add("HEAD absent key", "404", got(err), status(err) == 404, "")
	h, err := p.head(ctx, key)
	add("HEAD existing key", "200 + ETag", got(err), err == nil && h == cur, fmt.Sprintf("etag agrees with GET: %v", h == cur))

	// 8. Informational: If-Match on an absent key. AWS documents 404; the
	// design never issues this, so any refusal passes.
	absent := p.key("absent")
	_, err = p.put(ctx, absent, body("e"), `"0123456789abcdef0123456789abcdef"`, "")
	_, _, gerr = p.get(ctx, absent, "")
	add("PUT If-Match:<any> on absent key", "412 or 404, nothing created", got(err), err != nil && status(gerr) == 404, "informational")
	return cs
}

// createRace has writers race to create one key with If-None-Match: * at
// the same instant, for several rounds, one key per round. Exactly one 200
// per round with the stored object being the winner's is the property the
// immutable-index design of spec 004 commits with.
func (p *probe) createRace(ctx context.Context) check {
	c := p.contend(ctx, "c", func(round int) string { return p.key(fmt.Sprintf("create/%012d", round)) },
		func(ctx context.Context, key, _ string, b []byte) (string, error) {
			return putWith(ctx, p.rc, p.bucket, key, b, "", "*")
		})
	c.Name = "concurrent create race (If-None-Match:*)"
	return c
}

// casRace has writers read one ETag and fire a CAS on it at the same
// instant, for several rounds on one key. Optional: it documents whether
// the provider has a working If-Match, which the design no longer needs.
func (p *probe) casRace(ctx context.Context) check {
	key := p.key("race")
	if _, err := p.put(ctx, key, body("seed"), "", "*"); err != nil {
		return check{Name: "concurrent CAS race (If-Match)", Expect: "1 applied per round", Got: got(err), Note: "seed write failed", Optional: true}
	}
	c := p.contend(ctx, "r", func(int) string { return key },
		func(ctx context.Context, key, prev string, b []byte) (string, error) {
			return putWith(ctx, p.rc, p.bucket, key, b, prev, "")
		})
	c.Name, c.Optional = "concurrent CAS race (If-Match)", true
	return c
}

// contend runs rounds of writers that each fire one write at the same
// instant and settles every round from the stored object. write receives
// the key for the round, the ETag read back after the previous round (the
// seed's on the first), and the writer's body.
func (p *probe) contend(ctx context.Context, tag string, keyFor func(round int) string, write func(ctx context.Context, key, prev string, b []byte) (string, error)) check {
	var bad []string
	winners, losers, ambiguous, ackLost, other := 0, 0, 0, 0, 0
	etag, _ := p.head(ctx, keyFor(0))
	for round := range p.o.rounds {
		key := keyFor(round)
		type result struct {
			etag string
			err  error
			id   int
		}
		results := make([]result, p.o.writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range p.o.writers {
			wg.Go(func() {
				<-start
				e, err := write(ctx, key, etag, body(fmt.Sprintf("%s%dw%d", tag, round, i)))
				results[i] = result{e, err, i}
			})
		}
		close(start)
		wg.Wait()
		// The store decides the round, not the clients: the object that is
		// there afterwards names the winner. A writer whose request died in
		// transport (status 0) does not know whether it was applied; that
		// ambiguity is the case the design's retry path must resolve by
		// reading the index, so it is recorded, not counted as a failure,
		// as long as the store still shows exactly one writer applied.
		var won []result
		var lost []int
		for _, r := range results {
			switch {
			case r.err == nil:
				won = append(won, r)
				winners++
			case status(r.err) == 412:
				losers++
			case status(r.err) == 0:
				lost = append(lost, r.id)
				ambiguous++
			default:
				other++
				bad = append(bad, fmt.Sprintf("round %d writer %d: %v", round, r.id, r.err))
			}
		}
		b, e, gerr := p.get(ctx, key, "")
		if gerr != nil {
			bad = append(bad, fmt.Sprintf("round %d: read back: %v", round, gerr))
			continue
		}
		stored := -1
		for i := range p.o.writers {
			if bytes.Equal(b, body(fmt.Sprintf("%s%dw%d", tag, round, i))) {
				stored = i
			}
		}
		switch {
		case len(won) > 1:
			bad = append(bad, fmt.Sprintf("round %d: %d writers got 200", round, len(won)))
		case len(won) == 1 && (stored != won[0].id || e != won[0].etag):
			bad = append(bad, fmt.Sprintf("round %d: stored object is not the winner's", round))
		case len(won) == 0 && stored >= 0 && slices.Contains(lost, stored):
			ackLost++
		case len(won) == 0:
			bad = append(bad, fmt.Sprintf("round %d: no writer got 200 and the stored object is writer %d's", round, stored))
		}
		etag = e
	}
	note := fmt.Sprintf("%d rounds x %d writers: %d x 200, %d x 412, %d transport errors of which %d were applied without an acknowledgement, %d other",
		p.o.rounds, p.o.writers, winners, losers, ambiguous, ackLost, other)
	if len(bad) > 0 {
		note += "; " + strings.Join(bad, "; ")
	}
	return check{Expect: "exactly 1 applied per round, rest 412", Got: fmt.Sprintf("%d applied / %d rounds", winners+ackLost, p.o.rounds), Pass: len(bad) == 0 && winners+ackLost == p.o.rounds, Note: note}
}

// copyFallback asks whether CopyObject honours If-Match and If-None-Match
// on the destination, the substitute a provider without conditional PUT
// would need: write the candidate to a scratch key, then copy it over the
// index under a condition.
func (p *probe) copyFallback(ctx context.Context) []check {
	var cs []check
	dst, src := p.key("copy-index"), p.key("copy-src")
	e0, err := p.put(ctx, dst, body("copy0"), "", "")
	if err != nil {
		return []check{{Name: "CopyObject fallback", Expect: "setup", Got: got(err), Fallback: true}}
	}
	if _, err := p.put(ctx, src, body("copy1"), "", ""); err != nil {
		return []check{{Name: "CopyObject fallback", Expect: "setup", Got: got(err), Fallback: true}}
	}
	source := p.bucket + "/" + src
	cp := func(ifMatch, ifNoneMatch string) (string, error) {
		in := &s3.CopyObjectInput{Bucket: &p.bucket, Key: &dst, CopySource: &source, MetadataDirective: types.MetadataDirectiveReplace}
		if ifMatch != "" {
			in.IfMatch = &ifMatch
		}
		if ifNoneMatch != "" {
			in.IfNoneMatch = &ifNoneMatch
		}
		res, err := p.c.CopyObject(ctx, in)
		if err != nil {
			return "", err
		}
		return aws.ToString(res.CopyObjectResult.ETag), nil
	}

	_, err = cp("", "*")
	b, _, _ := p.get(ctx, dst, "")
	cs = append(cs, check{Name: "CopyObject If-None-Match:* on existing dst", Expect: "412, dst unchanged", Got: got(err), Pass: status(err) == 412 && bytes.Equal(b, body("copy0")), Note: fmt.Sprintf("dst unchanged: %v", bytes.Equal(b, body("copy0")))})

	_, err = cp(`"0123456789abcdef0123456789abcdef"`, "")
	b, _, _ = p.get(ctx, dst, "")
	cs = append(cs, check{Name: "CopyObject If-Match:<stale> on dst", Expect: "412, dst unchanged", Got: got(err), Pass: status(err) == 412 && bytes.Equal(b, body("copy0")), Note: fmt.Sprintf("dst unchanged: %v", bytes.Equal(b, body("copy0")))})

	e1, err := cp(e0, "")
	b, eget, _ := p.get(ctx, dst, "")
	// Some providers return the copy result's ETag without quotes.
	same := strings.Trim(eget, `"`) == strings.Trim(e1, `"`)
	cs = append(cs, check{Name: "CopyObject If-Match:<current> on dst", Expect: "200, dst replaced", Got: got(err), Pass: err == nil && bytes.Equal(b, body("copy1")) && same, Note: fmt.Sprintf("etag %s -> %s", e0, e1)})
	for i := range cs {
		cs[i].Fallback = true
	}
	return cs
}

// versioning reports whether the bucket can version objects, the last
// resort for a provider with no conditional write: PUT unconditionally and
// let a later ListObjectVersions decide who came first. It enables
// versioning only on a bucket this run created.
func (p *probe) versioning(ctx context.Context) []check {
	res, err := p.c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &p.bucket})
	if err != nil {
		return []check{{Name: "bucket versioning", Expect: "informational", Got: got(err), Pass: true, Note: "GetBucketVersioning failed"}}
	}
	state := string(res.Status)
	if state == "" {
		state = "off"
	}
	if res.Status != types.BucketVersioningStatusEnabled && !p.o.createBucket {
		return []check{{Name: "bucket versioning", Expect: "informational", Got: state, Pass: true, Note: "not enabled; the run does not change a bucket it did not create"}}
	}
	if res.Status != types.BucketVersioningStatusEnabled {
		_, err := p.c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &p.bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
		if err != nil {
			return []check{{Name: "bucket versioning", Expect: "informational", Got: "enable: " + got(err), Pass: true, Note: err.Error()}}
		}
	}
	key := p.key("versioned")
	in := &s3.PutObjectInput{Bucket: &p.bucket, Key: &key, Body: bytes.NewReader(body("v1"))}
	r1, err1 := p.c.PutObject(ctx, in)
	in.Body = bytes.NewReader(body("v2"))
	r2, err2 := p.c.PutObject(ctx, in)
	if err1 != nil || err2 != nil {
		return []check{{Name: "bucket versioning", Expect: "informational", Got: got(errors.Join(err1, err2)), Pass: true}}
	}
	lv, err := p.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &p.bucket, Prefix: &key})
	if err != nil {
		return []check{{Name: "bucket versioning", Expect: "informational", Got: "list: " + got(err), Pass: true}}
	}
	var ids []string
	for _, v := range lv.Versions {
		ids = append(ids, fmt.Sprintf("%s latest=%v", aws.ToString(v.VersionId), aws.ToBool(v.IsLatest)))
	}
	note := fmt.Sprintf("PUT version ids %q, %q; ListObjectVersions: %s", aws.ToString(r1.VersionId), aws.ToString(r2.VersionId), strings.Join(ids, ", "))
	return []check{{Name: "bucket versioning", Expect: "informational", Got: "enabled, " + fmt.Sprint(len(lv.Versions)) + " versions listed", Pass: true, Note: note}}
}

func (p *probe) latencies(ctx context.Context) []latency {
	key := p.key("latency")
	etag, err := p.put(ctx, key, body("l0"), "", "")
	if err != nil {
		return nil
	}
	n := p.o.samples
	var out []latency

	out = append(out, timeIt("GET If-None-Match -> 304", n, func(i int) error {
		_, _, err := p.get(ctx, key, etag)
		if status(err) != 304 {
			return fmt.Errorf("sample %d: %s", i, got(err))
		}
		return nil
	}))
	out = append(out, timeIt("GET unconditional -> 200", n, func(i int) error {
		_, _, err := p.get(ctx, key, "")
		return err
	}))
	missing := p.key("missing")
	out = append(out, timeIt("HEAD missing key -> 404", n, func(i int) error {
		_, err := p.head(ctx, missing)
		if status(err) != 404 {
			return fmt.Errorf("sample %d: %s", i, got(err))
		}
		return nil
	}))
	out = append(out, timeIt("HEAD existing key -> 200", n, func(i int) error {
		_, err := p.head(ctx, key)
		return err
	}))
	out = append(out, timeIt("PUT If-Match -> 200 (CAS)", n, func(i int) error {
		e, err := p.put(ctx, key, body(fmt.Sprintf("l%d", i+1)), etag, "")
		if err != nil {
			return fmt.Errorf("sample %d: %s", i, got(err))
		}
		etag = e
		return nil
	}))
	out = append(out, timeIt("PUT unconditional -> 200", n, func(i int) error {
		e, err := p.put(ctx, key, body(fmt.Sprintf("u%d", i)), "", "")
		if err != nil {
			return err
		}
		etag = e
		return nil
	}))
	return out
}

func timeIt(name string, n int, op func(i int) error) latency {
	samples := make([]time.Duration, 0, n)
	fail := 0
	for i := range n {
		t := time.Now()
		if err := op(i); err != nil {
			fail++
			fmt.Fprintln(os.Stderr, "condwrite:", name+":", err)
			continue
		}
		samples = append(samples, time.Since(t))
	}
	return summarize(name, samples, fail)
}

func summarize(name string, samples []time.Duration, fail int) latency {
	l := latency{Name: name, N: len(samples), Fail: fail}
	if len(samples) == 0 {
		return l
	}
	slices.Sort(samples)
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	var sum time.Duration
	for _, d := range samples {
		sum += d
	}
	l.Min, l.Max = ms(samples[0]), ms(samples[len(samples)-1])
	l.P50, l.P95, l.P99 = ms(percentile(samples, 50)), ms(percentile(samples, 95)), ms(percentile(samples, 99))
	l.Mean = ms(sum / time.Duration(len(samples)))
	return l
}

// percentile is the nearest-rank percentile of sorted samples.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func (p *probe) cleanup(ctx context.Context) error {
	var objs []types.ObjectIdentifier
	lv := s3.NewListObjectVersionsPaginator(p.c, &s3.ListObjectVersionsInput{Bucket: &p.bucket, Prefix: &p.prefix})
	for lv.HasMorePages() {
		page, err := lv.NextPage(ctx)
		if err != nil {
			// Providers without versioning support may refuse the call;
			// fall back to a plain listing.
			objs = nil
			lo := s3.NewListObjectsV2Paginator(p.c, &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: &p.prefix})
			for lo.HasMorePages() {
				page, err := lo.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, o := range page.Contents {
					objs = append(objs, types.ObjectIdentifier{Key: o.Key})
				}
			}
			break
		}
		for _, v := range page.Versions {
			objs = append(objs, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, d := range page.DeleteMarkers {
			objs = append(objs, types.ObjectIdentifier{Key: d.Key, VersionId: d.VersionId})
		}
	}
	for chunk := range slices.Chunk(objs, 1000) {
		if _, err := p.c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &p.bucket, Delete: &types.Delete{Objects: chunk, Quiet: aws.Bool(true)}}); err != nil {
			return err
		}
	}
	// Confirm nothing is left under the prefix.
	left, err := p.c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: &p.prefix, MaxKeys: aws.Int32(1)})
	if err != nil {
		return err
	}
	if aws.ToInt32(left.KeyCount) != 0 {
		return fmt.Errorf("prefix %s still holds objects", p.prefix)
	}
	if p.o.createBucket {
		_, err = p.c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &p.bucket})
	}
	return err
}

func print(w io.Writer, r *report) {
	fmt.Fprintf(w, "endpoint %s bucket %s prefix %s server %q\n\n", r.Endpoint, r.Bucket, r.Prefix, r.Server)
	fmt.Fprintln(w, "| Primitive | Expected | Got | Result | Evidence |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, c := range r.Checks {
		res := "pass"
		switch {
		case c.Fallback && c.Pass:
			res = "honoured"
		case c.Fallback:
			res = "not honoured"
		case c.Optional && !c.Pass:
			res = "absent"
		case !c.Pass:
			res = "FAIL"
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n", strings.TrimSpace(c.Name), c.Expect, c.Got, res, c.Note)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Operation | n | min ms | p50 ms | p95 ms | p99 ms | max ms | mean ms | failures |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|")
	for _, l := range r.Latencies {
		fmt.Fprintf(w, "| %s | %d | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %d |\n", l.Name, l.N, l.Min, l.P50, l.P95, l.P99, l.Max, l.Mean, l.Fail)
	}
}
