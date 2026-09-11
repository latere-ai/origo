// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/s3/s3test"

	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/test/stubs/sink"
)

// repoID is the fixture repository of the telemetry tests, named once so
// a test can look for it in a label.
const (
	repoID    = "0d5e7a1c-6f2b-4c3d-9e8f-1a2b3c4d5e6f"
	repoOwner = "dev"
	repoSlug  = "hello"
)

// servingEnv is a node configuration against an in-process bucket, the
// stub issuer and authorizer, and the stub sink, so a test drives a real
// push, clone, and event through it.
func servingEnv(t *testing.T) (map[string]string, *identity) {
	t.Helper()
	env, id := newEnv(t)
	bucket := s3test.New(t, "origo")
	events := sink.New(t)
	env["ORIGO_S3_ENDPOINT"] = bucket.URL()
	env["ORIGO_S3_REGION"] = s3test.Region
	env["ORIGO_S3_KEY"] = s3test.Key
	env["ORIGO_S3_SECRET"] = s3test.Secret
	env["ORIGO_S3_PATH_STYLE"] = "1"
	env["ORIGO_EVENTS_URL"] = events.URL()
	env["ORIGO_EVENTS_SECRET"] = events.Secret()
	return env, id
}

// fixture drives the load spec 011's vocabulary criterion names: create,
// push, clone, and a refused push. It returns the token it used.
func fixture(t *testing.T, base string, id *identity) string {
	t.Helper()
	token := id.token()
	body := `{"id":"` + repoID + `","owner":"` + repoOwner + `","slug":"` + repoSlug + `"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/repos", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}

	remote := strings.Replace(base, "http://", "http://x:"+token+"@", 1) + "/" + repoOwner + "/" + repoSlug + ".git"
	src := gittest.NewSource(t)
	src.Commit("a.txt", "a", "base")
	gittest.Run(t, src.Dir, nil, "remote", "add", "origin", remote)
	gittest.Run(t, src.Dir, nil, "push", "-q", "origin", "HEAD:refs/heads/main")

	// A clone of the pushed history, then a push the node refuses: the
	// repository it names does not exist, so the label form carries a
	// name nothing resolves and the answer is repo_not_found.
	clone := filepath.Join(t.TempDir(), "clone")
	gittest.Run(t, t.TempDir(), nil, "clone", "-q", remote, clone)
	absent := strings.Replace(remote, "/"+repoSlug+".git", "/nope.git", 1)
	gittest.Run(t, src.Dir, nil, "remote", "add", "absent", absent)
	if _, err := gittest.Try(src.Dir, nil, "push", "absent", "HEAD:refs/heads/main"); err == nil {
		t.Fatal("a push to a repository that does not exist was accepted")
	}
	return token
}

func scrape(t *testing.T, internal string) string {
	t.Helper()
	code, body := probe(t, "http://"+internal+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics: %d", code)
	}
	return body
}

// TestMetricsVocabulary is spec 011's first criterion on the node: every
// name of the table is on /metrics before anything is recorded, and
// after the fixture load no label value is a repository id, a subject, a
// reference, or a path.
func TestMetricsVocabulary(t *testing.T) {
	env, id := servingEnv(t)
	n, stop := startNode(t, env)
	defer func() { _ = stop() }()
	_, internal, _ := n.addrs()
	public, _, _ := n.addrs()

	first := scrape(t, internal)
	for _, name := range metrics.Names() {
		if !strings.Contains(first, "# TYPE "+name+" ") {
			t.Errorf("%s is missing from the first scrape", name)
		}
	}
	for _, want := range []string{
		"origo_pushes_total 0",
		"origo_wal_commits_total 0",
		`origo_evictions_total{reason="pressure"} 0`,
		`origo_gossip_packets_total{direction="sent"} 0`,
		`origo_storage_ops_total{op="get",result="ok"} 0`,
		"origo_requests_in_flight 0",
		"origo_cache_bytes 0",
		`origo_storage_breaker_state{class="read"} 0`,
	} {
		if !strings.Contains(first, want+"\n") {
			t.Errorf("%q is not in the first scrape", want)
		}
	}

	token := fixture(t, "http://"+public, id)
	after := scrape(t, internal)
	if !strings.Contains(after, "origo_pushes_total 1\n") || strings.Contains(after, "origo_fetches_total 0\n") {
		t.Errorf("the fixture load was not counted:\n%s", after)
	}
	for _, want := range []string{
		`origo_requests_total{route="/{owner}/{slug}/{service...}",status_class="2xx"}`,
		`origo_requests_total{route="/{owner}/{slug}/{service...}",status_class="4xx"}`,
		`origo_request_duration_seconds_count{route="/v1/repos",status_class="2xx"}`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("the request series lack %s:\n%s", want, after)
		}
	}
	forbidden := []string{repoID, repoOwner + "/" + repoSlug, "dev", "nope", "refs/heads/main", "main", "a.txt", token}
	for line := range strings.SplitSeq(after, "\n") {
		open := strings.IndexByte(line, '{')
		if strings.HasPrefix(line, "#") || open < 0 {
			continue
		}
		labels := line[open:]
		for _, bad := range forbidden {
			if strings.Contains(labels, `"`+bad+`"`) {
				t.Errorf("a label value is %q: %s", bad, line)
			}
		}
	}
}

// TestRequestLogRedactsCredentials is spec 011's log criterion: the line
// a push writes carries the listed fields and no credential, though the
// push authenticated with basic auth.
func TestRequestLogRedactsCredentials(t *testing.T) {
	env, id := servingEnv(t)
	var out lockedBuffer
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	n, err := newNode(cfg, slog.New(slog.NewJSONHandler(&out, nil)))
	if err != nil {
		t.Fatal(err)
	}
	n.drainDelay = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.run(ctx) }()
	public, _, _ := n.addrs()
	token := fixture(t, "http://"+public, id)
	cancel()
	<-done

	var push map[string]any
	for line := range strings.SplitSeq(out.String(), "\n") {
		var rec map[string]any
		if line == "" || json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["msg"] == "request" && rec["route"] == "/{owner}/{slug}/{service...}" && rec["bytes_in"].(float64) > 0 {
			push = rec
		}
	}
	if push == nil {
		t.Fatalf("no request line for the push:\n%s", out.String())
	}
	for _, field := range []string{"route", "method", "status", "duration_ms", "repo", "subject", "actor", "bytes_in", "bytes_out", "trace_id"} {
		if _, ok := push[field]; !ok {
			t.Errorf("the line has no %s: %v", field, push)
		}
	}
	if push["repo"] != repoOwner+"/"+repoSlug || push["subject"] != "dev" {
		t.Errorf("repo %v, subject %v", push["repo"], push["subject"])
	}
	if push["bytes_out"].(float64) <= 0 || push["status"].(float64) != 200 {
		t.Errorf("bytes_out %v, status %v", push["bytes_out"], push["status"])
	}
	if strings.Contains(out.String(), token) || strings.Contains(strings.ToLower(out.String()), "authorization") {
		t.Error("a credential reached the log")
	}
}

// TestPushTrace is spec 011's trace criterion: one push against an
// in-memory OTLP receiver produces one trace carrying the five spans of
// the write path, with the repository id as an attribute.
func TestPushTrace(t *testing.T) {
	receiver := newOTLPReceiver(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL())
	// The default sampler keeps one root trace in five; the test needs
	// the one it makes.
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")

	env, id := servingEnv(t)
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger, flush := bootstrap(ctx, io.Discard)
	n, err := newNode(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	n.flushTelemetry, n.drainDelay = flush, 0
	done := make(chan error, 1)
	go func() { done <- n.run(ctx) }()
	public, _, _ := n.addrs()
	fixture(t, "http://"+public, id)
	// The drain flushes the batcher, whose own timeout is five seconds.
	cancel()
	<-done

	traces := map[string][]otlpSpan{}
	for _, s := range receiver.spans() {
		traces[s.trace] = append(traces[s.trace], s)
	}
	want := []string{"receive", "entry.put", "index.create", "apply", "event.enqueue"}
	for _, spans := range traces {
		names := map[string]otlpSpan{}
		for _, s := range spans {
			names[s.name] = s
		}
		if _, ok := names["receive"]; !ok {
			continue
		}
		missing := []string{}
		for _, w := range want {
			if _, ok := names[w]; !ok {
				missing = append(missing, w)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("the push trace is missing %v; it has %v", missing, keys(names))
		}
		if got := names["receive"].attrs["origo.repo"]; got != repoID {
			t.Fatalf("the receive span names the repository %q, want %q", got, repoID)
		}
		return
	}
	t.Fatalf("no trace carries a receive span; %d traces arrived", len(traces))
}

func keys(m map[string]otlpSpan) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lockedBuffer is a bytes.Buffer a running node and the test both touch.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// otlpReceiver is an in-memory OTLP/HTTP endpoint: it answers every
// export with an empty message and keeps the spans of /v1/traces.
type otlpReceiver struct {
	srv *httptest.Server

	mu   sync.Mutex
	seen []otlpSpan
}

func newOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	r := &otlpReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if req.URL.Path == "/v1/traces" {
			r.mu.Lock()
			r.seen = append(r.seen, decodeSpans(body)...)
			r.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *otlpReceiver) URL() string { return r.srv.URL }

func (r *otlpReceiver) spans() []otlpSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]otlpSpan(nil), r.seen...)
}

// otlpSpan is what one exported span says: its trace, its name, and its
// string attributes.
type otlpSpan struct {
	trace string
	name  string
	attrs map[string]string
}

// decodeSpans reads an ExportTraceServiceRequest off the wire. The three
// fields it walks are the message's own shape, so the OTLP protobuf
// packages stay off this module's dependency list; a field it does not
// know is skipped by its wire type.
func decodeSpans(body []byte) []otlpSpan {
	var out []otlpSpan
	for _, resourceSpans := range submessages(body, 1) {
		for _, scopeSpans := range submessages(resourceSpans, 2) {
			for _, span := range submessages(scopeSpans, 2) {
				s := otlpSpan{attrs: map[string]string{}}
				for _, id := range submessages(span, 1) {
					s.trace = hex.EncodeToString(id)
				}
				for _, name := range submessages(span, 5) {
					s.name = string(name)
				}
				for _, kv := range submessages(span, 9) {
					var key, value string
					for _, k := range submessages(kv, 1) {
						key = string(k)
					}
					for _, any := range submessages(kv, 2) {
						for _, v := range submessages(any, 1) {
							value = string(v)
						}
					}
					s.attrs[key] = value
				}
				out = append(out, s)
			}
		}
	}
	return out
}

// submessages returns the payload of every length-delimited field of b
// with the given number. A malformed remainder ends the walk.
func submessages(b []byte, want int) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return out
		}
		b = b[n:]
		number, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return out
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return out
			}
			b = b[8:]
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < size {
				return out
			}
			if number == want {
				out = append(out, b[n:n+int(size)])
			}
			b = b[n+int(size):]
		case 5:
			if len(b) < 4 {
				return out
			}
			b = b[4:]
		default:
			return out
		}
	}
	return out
}

// TestRequestCountersWrapTheResponse covers the two counters the log
// line reads: the writer forwards the status, the flush, and the real
// ResponseWriter, and the reader survives a request with no body.
func TestRequestCountersWrapTheResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &countingWriter{ResponseWriter: rec, status: http.StatusOK}
	w.Flush() // writes the implicit 200
	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	w.WriteHeader(http.StatusTeapot) // after the final status, ignored
	w.Flush()
	if w.status != http.StatusOK || w.n != 4 {
		t.Fatalf("status %d, %d bytes", w.status, w.n)
	}
	// An informational status is forwarded and is not the final one.
	informational := httptest.NewRecorder()
	early := &countingWriter{ResponseWriter: informational, status: http.StatusOK}
	early.WriteHeader(http.StatusContinue)
	early.WriteHeader(http.StatusCreated)
	if early.status != http.StatusCreated {
		t.Fatalf("after 100 the status is %d", early.status)
	}
	if w.Unwrap() != http.ResponseWriter(rec) {
		t.Error("the real writer is not reachable")
	}
	if !rec.Flushed {
		t.Error("the flush did not reach the real writer")
	}

	empty := &countingReader{}
	if n, err := empty.Read(make([]byte, 4)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("a request with no body read %d, %v", n, err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	body := &countingReader{from: io.NopCloser(strings.NewReader("four"))}
	if _, err := io.ReadAll(body); err != nil || body.n != 4 {
		t.Fatalf("%d bytes, %v", body.n, err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDetailsAreEmptyOffThePublicListener holds the fallback: a request
// that never passed the public listener's outermost wrapper, an internal
// probe, has no details and asks for none.
func TestDetailsAreEmptyOffThePublicListener(t *testing.T) {
	if d := detailsFrom(context.Background()); d == nil || d.route != "" || d.repo != "" {
		t.Fatalf("details off the listener: %+v", d)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/repos/"+repoID, nil)
	r.SetPathValue("id", repoID)
	if got := repoOf(r); got != repoID {
		t.Fatalf("repo %q", got)
	}
	if got := routeOf(r); got != "" {
		t.Fatalf("route of an unmatched request %q", got)
	}
}

// TestReadTrace is spec 009's trace criterion and the read half of spec
// 011's span table: a read against an in-memory OTLP receiver produces
// one trace carrying index.check, materialize, and a git.<command>
// span, with the repository id on the two spans the cache owns.
//
// The read runs against a second node with a data directory of its own
// against the same bucket, because materialize is what a node does for
// a repository it does not hold: the node that took the push already
// has the copy, and a read through it would carry the currency check
// and no materialization.
func TestReadTrace(t *testing.T) {
	env, id := servingEnv(t)
	pusher, stopPusher := startNode(t, env)
	public, _, _ := pusher.addrs()
	token := fixture(t, "http://"+public, id)
	if err := stopPusher(); err != nil {
		t.Fatal(err)
	}

	receiver := newOTLPReceiver(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL())
	// The default sampler keeps one root trace in five; the test needs
	// the one it makes.
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")

	// The same bucket and the same identity, an empty disk of its own.
	reader := maps.Clone(env)
	reader["ORIGO_DATA_DIR"] = t.TempDir()
	cfg, err := config.Load(getenv(reader))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger, flush := bootstrap(ctx, io.Discard)
	n, err := newNode(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	n.flushTelemetry, n.drainDelay = flush, 0
	done := make(chan error, 1)
	go func() { done <- n.run(ctx) }()
	readAddr, _, _ := n.addrs()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+readAddr+"/v1/repos/"+repoID+"/commits?limit=10", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read: %d %s", resp.StatusCode, body)
	}
	// The drain flushes the batcher, whose own timeout is five seconds.
	cancel()
	<-done

	traces := map[string][]otlpSpan{}
	for _, s := range receiver.spans() {
		traces[s.trace] = append(traces[s.trace], s)
	}
	for _, spans := range traces {
		names := map[string]otlpSpan{}
		var git []string
		for _, s := range spans {
			names[s.name] = s
			if strings.HasPrefix(s.name, "git.") {
				git = append(git, s.name)
			}
		}
		if _, ok := names["materialize"]; !ok {
			continue
		}
		if _, ok := names["index.check"]; !ok {
			t.Fatalf("the read trace has no index.check span; it has %v", keys(names))
		}
		if len(git) == 0 {
			t.Fatalf("the read trace carries no git.<command> span; it has %v", keys(names))
		}
		for _, name := range []string{"index.check", "materialize"} {
			if got := names[name].attrs["origo.repo"]; got != repoID {
				t.Fatalf("the %s span names the repository %q, want %q", name, got, repoID)
			}
		}
		return
	}
	t.Fatalf("no trace carries a materialize span; %d traces arrived", len(traces))
}
