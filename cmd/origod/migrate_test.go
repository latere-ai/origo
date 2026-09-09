// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/s3/s3test"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/source"
)

// sourceHost is the name the migrate test reaches its in-process source
// under. A single-label name such as localhost is not a valid
// ORIGO_EGRESS_ALLOW entry (spec 016), and the Go resolver answers every
// name under .localhost with the loopback address and no DNS query.
const sourceHost = "origo-source.localhost"

// migrateStack is one node with an in-process bucket, a source stub the
// node is allowed to reach, and the two tokens the command carries.
type migrateStack struct {
	url    string
	stub   *source.Server
	bundle []byte
	env    map[string]string
}

// newMigrateStack starts the node and the source.
func newMigrateStack(t *testing.T) *migrateStack {
	t.Helper()
	stub := source.New(t, source.WithSANs(sourceHost))
	ca := filepath.Join(t.TempDir(), "stub-ca.pem")
	if err := os.WriteFile(ca, stub.CA(), 0o600); err != nil {
		t.Fatal(err)
	}
	bucket := s3test.New(t, "origo")
	env, id := newEnv(t)
	env["ORIGO_S3_ENDPOINT"] = bucket.URL()
	env["ORIGO_S3_REGION"] = s3test.Region
	env["ORIGO_S3_KEY"], env["ORIGO_S3_SECRET"] = s3test.Key, s3test.Secret
	env["ORIGO_S3_PATH_STYLE"] = "1"
	env["ORIGO_EGRESS_ALLOW"] = sourceHost
	env["ORIGO_EGRESS_CA_BUNDLE"] = ca

	// The loopback seam of spec 016, written in a _test.go file and
	// nowhere else: the source serves on 127.0.0.1, which no
	// deployment's dialer admits.
	previous := newHandler
	newHandler = func(o api.Options) *api.Handler {
		o.AllowLoopback = true
		return api.New(o)
	}
	t.Cleanup(func() { newHandler = previous })
	n, stop := startNode(t, env)
	t.Cleanup(func() { _ = stop() })
	public, _, _ := n.addrs()

	// A small repository, bundled once and unpacked under every name
	// the manifest carries.
	src := gittest.NewSource(t)
	src.Commit("a.txt", "one", "first")
	src.Commit("b.txt", "two", "second")
	src.Tag("v1", "the first tag")
	path := filepath.Join(t.TempDir(), "fixture.bundle")
	gittest.Run(t, src.Dir, nil, "bundle", "create", path, "--all")
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return &migrateStack{
		url: "http://" + public, stub: stub, bundle: bundle,
		env: map[string]string{
			"ORIGO_MIGRATE_URL":       "http://" + public,
			"ORIGO_MIGRATE_TOKEN_ENV": "MIGRATE_ADMIN",
			"MIGRATE_ADMIN":           id.token(),
			"MIGRATE_SOURCE":          stub.Token(),
			"ORIGO_MIGRATE_PARALLEL":  "4",
		},
	}
}

// addRepo unpacks the bundle under name and answers the source URL.
func (s *migrateStack) addRepo(t *testing.T, name string) string {
	t.Helper()
	if err := s.stub.AddRepo(name, s.bundle); err != nil {
		t.Fatal(err)
	}
	return s.sourceURL(t, name)
}

func (s *migrateStack) sourceURL(t *testing.T, name string) string {
	t.Helper()
	u, err := url.Parse(s.stub.URL())
	if err != nil {
		t.Fatal(err)
	}
	return "https://" + sourceHost + ":" + u.Port() + "/" + name + ".git"
}

// writeManifest writes the JSON lines file and answers its path.
func writeManifest(t *testing.T, lines []manifestLine) string {
	t.Helper()
	var b bytes.Buffer
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(raw, '\n'))
	}
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readReport parses the report the command wrote.
func readReport(t *testing.T, path string) []reportLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []reportLine
	for raw := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if raw == "" {
			continue
		}
		var l reportLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("report line %q: %v", raw, err)
		}
		out = append(out, l)
	}
	return out
}

// runMigrate runs the subcommand and answers the exit code with what it
// wrote to standard error.
func runMigrate(t *testing.T, env map[string]string, manifest string) (int, []reportLine, string) {
	t.Helper()
	report := filepath.Join(t.TempDir(), "report.jsonl")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"migrate", "-manifest", manifest, "-report", report}, getenv(env), &out, &errOut)
	if _, err := os.Stat(report); err != nil {
		return code, nil, errOut.String()
	}
	return code, readReport(t, report), errOut.String()
}

// migrateID is a UUID naming the nth repository of a batch.
func migrateID(n int) string {
	return fmt.Sprintf("0f5c1d2e-3a4b-4c5d-8e6f-%012d", n)
}

// TestMigrateBatchIsResumableAndReportsFailures is spec 014's batch
// criterion: twenty repositories of a manifest reach mirrored under
// parallelism 4 with one report line each in the documented shape and
// prior_id copied through, a second run reports every one skipped
// without importing again, an unreachable source makes the run exit 1
// and names the failure in error, and a line whose id is not a UUID is
// refused before Origo is called at all.
func TestMigrateBatchIsResumableAndReportsFailures(t *testing.T) {
	s := newMigrateStack(t)
	const repos = 20
	lines := make([]manifestLine, 0, repos)
	for i := range repos {
		name := fmt.Sprintf("repo%02d", i)
		lines = append(lines, manifestLine{
			ID: migrateID(i), PriorID: fmt.Sprintf("ws_%02d", i), Owner: "acme", Slug: name,
			Source: s.addRepo(t, name), TokenEnv: "MIGRATE_SOURCE",
		})
	}
	manifest := writeManifest(t, lines)

	code, report, stderr := runMigrate(t, s.env, manifest)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, report %+v", code, stderr, report)
	}
	if len(report) != repos {
		t.Fatalf("%d report lines, want %d", len(report), repos)
	}
	seen := map[string]reportLine{}
	for _, l := range report {
		if l.State != stateMirrored || l.Error != "" {
			t.Fatalf("%s: %+v", l.ID, l)
		}
		if l.Refs == 0 || l.Objects == 0 || l.Seconds <= 0 {
			t.Fatalf("%s reports no work: %+v", l.ID, l)
		}
		seen[l.ID] = l
	}
	for i, l := range lines {
		got, ok := seen[l.ID]
		if !ok {
			t.Fatalf("%s has no report line", l.ID)
		}
		if got.PriorID != l.PriorID || got.Owner != "acme" || got.Slug != l.Slug {
			t.Fatalf("line %d: %+v", i, got)
		}
	}

	// A second run resumes from Origo's own state: every repository is
	// skipped and the source is not touched.
	s.stub.ClearRequests()
	code, report, stderr = runMigrate(t, s.env, manifest)
	if code != 0 {
		t.Fatalf("second run: exit %d, stderr %q", code, stderr)
	}
	for _, l := range report {
		if l.State != stateSkipped {
			t.Fatalf("second run: %+v", l)
		}
	}
	if reqs := s.stub.Requests(); len(reqs) != 0 {
		t.Fatalf("the second run reached the source: %+v", reqs)
	}

	// One line whose source the stub does not serve: the run exits 1
	// and the report names the failing step and what the source said.
	failing := append([]manifestLine{}, lines[:2]...)
	failing = append(failing, manifestLine{
		ID: migrateID(repos), PriorID: "ws_absent", Owner: "acme", Slug: "absent",
		Source: s.sourceURL(t, "absent"), TokenEnv: "MIGRATE_SOURCE",
	})
	code, report, stderr = runMigrate(t, s.env, writeManifest(t, failing))
	if code != 1 {
		t.Fatalf("an unreachable source: exit %d, stderr %q, report %+v", code, stderr, report)
	}
	var failures int
	for _, l := range report {
		if l.ID != migrateID(repos) {
			continue
		}
		failures++
		if l.State != stateFailed || !strings.HasPrefix(l.Error, "import: ") || !strings.Contains(l.Error, "absent") {
			t.Fatalf("the failing line is %+v", l)
		}
	}
	if failures != 1 {
		t.Fatalf("%d failing lines, want 1", failures)
	}

	// A line whose id is not a UUID is refused before any request: the
	// counter in front of Origo stays at zero and no report is written.
	var calls atomic.Int64
	target, err := url.Parse(s.url)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	env := map[string]string{}
	maps.Copy(env, s.env)
	env["ORIGO_MIGRATE_URL"] = proxy.URL
	bad := writeManifest(t, []manifestLine{{ID: "ws_8f3a", Owner: "acme", Slug: "api", Source: s.sourceURL(t, "repo00")}})
	code, report, stderr = runMigrate(t, env, bad)
	if code != 2 || report != nil {
		t.Fatalf("a manifest id that is not a UUID: exit %d, report %+v", code, report)
	}
	if !strings.Contains(stderr, "line 1") || !strings.Contains(stderr, "UUID") {
		t.Fatalf("stderr %q", stderr)
	}
	if calls.Load() != 0 {
		t.Fatalf("the command called Origo %d times before refusing the manifest", calls.Load())
	}
}

// TestMigrateUsageAndConfiguration covers the command line and the
// three variables of spec 014's table: an unknown subcommand and a
// missing flag are exit 2, a missing or malformed variable is exit 1
// with one line naming every problem, and -version answers under every
// subcommand.
func TestMigrateUsageAndConfiguration(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"sideways"}, getenv(nil), &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), `unknown subcommand "sideways"`) {
		t.Fatalf("unknown subcommand: %d %q", code, errOut.String())
	}
	// check is spec 018's and is not built, so it is unknown like any
	// other word.
	errOut.Reset()
	if code := run(context.Background(), []string{"check"}, getenv(nil), &out, &errOut); code != 2 {
		t.Fatalf("check: %d %q", code, errOut.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"migrate", "-version"}, getenv(nil), &out, &errOut); code != 0 ||
		!strings.HasPrefix(out.String(), "origod ") {
		t.Fatalf("migrate -version: %d %q", code, out.String())
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"migrate", "-report", "r"}, getenv(nil), &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "usage: origod migrate") {
		t.Fatalf("migrate without a manifest: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"migrate", "-badflag"}, getenv(nil), &out, &errOut); code != 2 {
		t.Fatalf("migrate with an unknown flag: %d", code)
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"migrate", "-manifest", "m", "-report", "r"}, getenv(nil), &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "missing ORIGO_MIGRATE_URL") ||
		!strings.Contains(errOut.String(), "missing ORIGO_MIGRATE_TOKEN_ENV") {
		t.Fatalf("migrate without its variables: %d %q", code, errOut.String())
	}

	for name, env := range map[string]map[string]string{
		"a URL that is not absolute": {"ORIGO_MIGRATE_URL": "git.example.com", "ORIGO_MIGRATE_TOKEN_ENV": "T", "T": "t"},
		"a token variable that is empty": {"ORIGO_MIGRATE_URL": "https://git.example.com",
			"ORIGO_MIGRATE_TOKEN_ENV": "T"},
		"a parallelism that is not a number": {"ORIGO_MIGRATE_URL": "https://git.example.com",
			"ORIGO_MIGRATE_TOKEN_ENV": "T", "T": "t", "ORIGO_MIGRATE_PARALLEL": "many"},
		"a parallelism below one": {"ORIGO_MIGRATE_URL": "https://git.example.com",
			"ORIGO_MIGRATE_TOKEN_ENV": "T", "T": "t", "ORIGO_MIGRATE_PARALLEL": "0"},
	} {
		if _, err := loadMigrateOptions(getenv(env)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	o, err := loadMigrateOptions(getenv(map[string]string{
		"ORIGO_MIGRATE_URL": "https://git.example.com/", "ORIGO_MIGRATE_TOKEN_ENV": "T", "T": "secret",
	}))
	if err != nil || o.URL != "https://git.example.com" || o.Token != "secret" || o.Parallel != 4 {
		t.Fatalf("options %+v %v", o, err)
	}
	if o, err := loadMigrateOptions(getenv(map[string]string{
		"ORIGO_MIGRATE_URL": "https://git.example.com", "ORIGO_MIGRATE_TOKEN_ENV": "T", "T": "s", "ORIGO_MIGRATE_PARALLEL": "9",
	})); err != nil || o.Parallel != 9 {
		t.Fatalf("parallelism %+v %v", o, err)
	}
}

// TestManifestIsHeldToTheShape covers every line the manifest refuses
// before Origo is called, and the shape it accepts.
func TestManifestIsHeldToTheShape(t *testing.T) {
	env := getenv(map[string]string{"SOURCE_TOKEN": "t"})
	good := `{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","prior_id":"ws_8f3a","owner":"acme","slug":"api","source":"https://old.example.com/acme/api.git","token_env":"SOURCE_TOKEN"}`
	lines, err := readManifest(writeRaw(t, good+"\n\n"), env)
	if err != nil || len(lines) != 1 {
		t.Fatalf("the documented line: %+v %v", lines, err)
	}
	if l := lines[0]; l.PriorID != "ws_8f3a" || l.Owner != "acme" || l.Slug != "api" || l.token != "t" {
		t.Fatalf("parsed %+v", l)
	}
	for name, raw := range map[string]string{
		"not json":                       `{`,
		"an unknown field":               `{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"a","slug":"b","source":"https://x.example/a.git","extra":1}`,
		"an id that is not a UUID":       `{"id":"ws_8f3a","owner":"a","slug":"b","source":"https://x.example/a.git"}`,
		"an owner that is not a label":   `{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"a/b","slug":"b","source":"https://x.example/a.git"}`,
		"a slug that is not a label":     `{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"a","slug":"b c","source":"https://x.example/a.git"}`,
		"a source that is not https":     `{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"a","slug":"b","source":"http://x.example/a.git"}`,
		"a token variable that is empty": `{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"a","slug":"b","source":"https://x.example/a.git","token_env":"ABSENT"}`,
		"an id named twice":              good + "\n" + good,
		"no repository at all":           "\n\n",
	} {
		if _, err := readManifest(writeRaw(t, raw), env); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := readManifest(filepath.Join(t.TempDir(), "absent.jsonl"), env); err == nil {
		t.Error("a manifest that is not there was accepted")
	}
}

func writeRaw(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMigrateRefusalsAreReported covers the report line of a step Origo
// refused: the code and the developer reason it named.
func TestMigrateRefusalsAreReported(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := &migrateClient{options: migrateOptions{URL: srv.URL, Token: "t", Parallel: 1}, http: srv.Client()}
	l := manifestLine{ID: migrateID(1), Owner: "acme", Slug: "api", Source: "https://x.example/a.git"}

	body = `{"error":{"code":"forbidden","message":"You are not allowed to do that.","details":{"reason":"the authorizer said no"}}}`
	line := c.drive(context.Background(), l)
	if line.State != stateFailed || line.Error != "get: forbidden: the authorizer said no" {
		t.Fatalf("a refused get: %+v", line)
	}
	body = `not an envelope`
	if line := c.drive(context.Background(), l); line.State != stateFailed || line.Error != "get: http 403" {
		t.Fatalf("a refusal without an envelope: %+v", line)
	}
	// A run whose report cannot be written counts as a failure.
	c.options.URL = "http://127.0.0.1:1"
	if failed := c.run(context.Background(), []manifestLine{l}, &reporter{w: failingWriter{}}); failed != 2 {
		t.Fatalf("%d failures, want the drive and the report", failed)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

var _ config.Getenv = getenv(nil)
