// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

const repoC = "2b3c4d5e-6f70-4182-9394-a5b6c7d8e9f0"

// logRecords keeps a node's log records so a test reads the figures a
// refused push writes; the sideband carries the code and the sentence
// alone (spec 021), so this line is where bytes and max are.
type logRecords struct {
	mu      sync.Mutex
	records []map[string]any
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecords) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, fields)
	return nil
}

func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecords) WithGroup(string) slog.Handler      { return l }

// find answers the last record with the message, or nil.
func (l *logRecords) find(msg string) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, v := range slices.Backward(l.records) {
		if v["msg"] == msg {
			return v
		}
	}
	return nil
}

// seedLFS writes one object under the repository's lfs/ prefix, the
// bytes the quota rule adds to what the log holds.
func seedLFS(t *testing.T, l *wal.Log, id string, size int) {
	t.Helper()
	key := l.RepoPrefix(id) + "lfs/" + strings.Repeat("a", 64)
	if _, err := l.Store().Put(context.Background(), key, wal.BytesBody(make([]byte, size))); err != nil {
		t.Fatal(err)
	}
}

// pushTo commits a file and pushes it to the remote, answering git's
// output and whether the push succeeded.
func pushTo(t *testing.T, work, remote, branch string) (string, error) {
	t.Helper()
	mustGit(t, work, "remote", "set-url", "origin", remote)
	return git(t, work, "push", "origin", "HEAD:"+branch)
}

// TestPushOverQuotaWritesNothing is spec 012's repository size rule on
// a push: what the log holds plus the bytes under lfs/ plus this pack,
// against the authorizer's quota_bytes. The refusal reaches the client
// in the sideband as the code and its sentence, the figures are on the
// handler's info line, and no entry is written. The lfs/ object is what
// makes the difference: the same push to a repository without one is
// taken.
func TestPushOverQuotaWritesNothing(t *testing.T) {
	store := wal.NewMemStore()
	records := &logRecords{}
	n := newNode(t, store, withLog(records))
	// One mebibyte under lfs/ on repository A and nothing on C, with a
	// quota of one mebibyte: A is over it by the pack alone.
	const quota = 1 << 20
	n.authz.SetRules(authorizer.Rule{Allow: true, QuotaBytes: quota})
	n.create(repoA, "acme", "app")
	n.create(repoC, "acme", "lean")
	seedLFS(t, n.log, repoA, quota)

	work := clone(t, n.url("/r/"+repoC+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "first")

	out, err := pushTo(t, work, n.url("/r/"+repoA+".git"), "refs/heads/main")
	if err == nil {
		t.Fatalf("the push over the quota was taken:\n%s", out)
	}
	want := contract.CodeOverQuota + ": " + contract.Sentence(contract.CodeOverQuota)
	if !strings.Contains(out, want) {
		t.Errorf("the sideband does not carry %q:\n%s", want, out)
	}
	if ix, _, err := n.log.Newest(context.Background(), repoA, 0, false); err != nil || ix.Seq != 0 {
		t.Fatalf("the refused push reached the log: seq %d, %v", ix.Seq, err)
	}
	line := records.find("push refused")
	if line == nil {
		t.Fatalf("no refusal line: %v", records.records)
	}
	if line["limit"] != limits.LimitRepository {
		t.Errorf("limit: %v", line["limit"])
	}
	bytes, _ := line["bytes"].(int64)
	max, _ := line["max"].(int64)
	if max != quota || bytes <= quota {
		t.Errorf("bytes %d, max %d, want more than %d against %d", bytes, max, quota, quota)
	}
	// The same push to a repository with no bytes under lfs/ fits.
	if out, err := pushTo(t, work, n.url("/r/"+repoC+".git"), "refs/heads/main"); err != nil {
		t.Fatalf("the same push without the lfs/ object was refused:\n%s", out)
	}
	if ix, _, _ := n.log.Newest(context.Background(), repoC, 0, false); ix.Seq != 1 {
		t.Fatalf("the accepted push is not in the log: seq %d", ix.Seq)
	}
}

// TestPushOverTwoGiBIsRefused is the single-push bound: a body past it
// is 413 over_quota with details.limit "push", whether the client
// declares its length or streams it, and the spool stops at the limit
// rather than writing the whole body to the disk first.
func TestPushOverTwoGiBIsRefused(t *testing.T) {
	if limits.MaxPushBytes != 2<<30 {
		t.Fatalf("the spec bounds a push at 2 GiB; the code bounds it at %d", limits.MaxPushBytes)
	}
	const max = 1 << 12
	store := wal.NewMemStore()
	records := &logRecords{}
	n := newNode(t, store, withLimits(limits.Options{MaxPushBytes: max}), withLog(records))
	n.create(repoA, "acme", "app")
	body := strings.Repeat("x", max+1)

	// A declared Content-Length past the bound is refused before a byte
	// is read.
	code, env := n.push(t, strings.NewReader(body), int64(len(body)))
	if code != http.StatusRequestEntityTooLarge || env.Code != contract.CodeOverQuota {
		t.Fatalf("declared length: %d %+v", code, env)
	}
	if env.Details["limit"] != limits.LimitPush || env.Details["max"] != float64(max) {
		t.Errorf("details: %+v", env.Details)
	}
	if line := records.find("push refused"); line == nil || line["limit"] != limits.LimitPush {
		t.Errorf("refusal line: %v", line)
	}
	// A body with no declared length is stopped while it is spooled.
	code, env = n.push(t, strings.NewReader(body), -1)
	if code != http.StatusRequestEntityTooLarge || env.Details["limit"] != limits.LimitPush {
		t.Fatalf("streamed body: %d %+v", code, env)
	}
	// Nothing is left in the spool directory.
	entries, err := os.ReadDir(n.cache.SpoolDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("%d files left in the spool: %v", len(entries), err)
	}
	// The copy itself stops one byte past the limit rather than reading
	// the whole body.
	counted := &countingReader{r: strings.NewReader(strings.Repeat("y", 8*max))}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "/", io.NopCloser(counted))
	if _, err := spoolBody(req, t.TempDir(), max); !errors.Is(err, errTooLarge) {
		t.Fatalf("spoolBody: %v", err)
	}
	if counted.n > max+1 {
		t.Errorf("the spool read %d bytes for a bound of %d", counted.n, max)
	}
}

// countingReader counts what the spool read off a body.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// push sends one receive-pack body with the declared length, -1 for a
// body the client streams, and answers the status and the envelope.
func (n *node) push(t *testing.T, body io.Reader, length int64) (int, httpjson.Error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "POST", n.url("/r/"+repoA+".git/git-receive-pack"), body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = length
	resp, err := n.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var envelope httpjson.ErrorEnvelope
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &envelope)
	return resp.StatusCode, envelope.Error
}

// TestPushOverTheReferenceCapIsRefused is the reference row: more
// commands than the cap is over_quota with details.limit "refs", where
// phase 1 answered 400 invalid_request.
func TestPushOverTheReferenceCapIsRefused(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	zero, one := strings.Repeat("0", 40), strings.Repeat("1", 40)
	refs := make([]string, limits.MaxRefs+1)
	for i := range refs {
		refs[i] = zero + " " + one + " refs/heads/b" + itoa(i)
	}
	body := receiveBody(t, "", nil, "", refs...)
	code, env := n.push(t, strings.NewReader(string(body)), int64(len(body)))
	if code != http.StatusRequestEntityTooLarge || env.Code != contract.CodeOverQuota {
		t.Fatalf("%d %+v", code, env)
	}
	if env.Details["limit"] != limits.LimitRefs || env.Details["max"] != float64(limits.MaxRefs) {
		t.Errorf("details: %+v", env.Details)
	}
	if ix, _, _ := n.log.Newest(context.Background(), repoA, 0, false); ix.Seq != 0 {
		t.Fatalf("the refused push reached the log: %d", ix.Seq)
	}
}

// TestReferencesAfterAPushAreCounted is the second half of the
// reference row: the index the push would leave must hold no more than
// the cap, counted from the index and the commands.
func TestReferencesAfterAPushAreCounted(t *testing.T) {
	full := map[string]string{}
	for i := range limits.MaxRefs {
		full["refs/heads/b"+itoa(i)] = strings.Repeat("1", 40)
	}
	ix := &wal.Index{Refs: full}
	create := wal.RefUpdate{Ref: "refs/heads/new", Old: wal.ZeroSHA, New: strings.Repeat("2", 40)}
	update := wal.RefUpdate{Ref: "refs/heads/b0", Old: strings.Repeat("1", 40), New: strings.Repeat("2", 40)}
	remove := wal.RefUpdate{Ref: "refs/heads/b1", Old: strings.Repeat("1", 40), New: wal.ZeroSHA}
	if n := refsAfter(ix, []wal.RefUpdate{update}); n != limits.MaxRefs {
		t.Errorf("an update changes the count: %d", n)
	}
	if n := refsAfter(ix, []wal.RefUpdate{create}); n != limits.MaxRefs+1 {
		t.Errorf("a create: %d", n)
	}
	if n := refsAfter(ix, []wal.RefUpdate{create, remove}); n != limits.MaxRefs {
		t.Errorf("a create beside a delete: %d", n)
	}
	if n := refsAfter(nil, []wal.RefUpdate{create}); n != 1 {
		t.Errorf("no index yet: %d", n)
	}

	// A push that would cross the cap is refused before git runs, so
	// the index the test built is what the handler measures.
	store := wal.NewMemStore()
	records := &logRecords{}
	n := newNode(t, store, withLog(records))
	n.create(repoA, "acme", "app")
	rp, release, err := n.cache.Acquire(context.Background(), repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rp.Index.Refs = full
	refusal, err := n.h.overQuota(context.Background(), repoA, rp, &receiveRequest{Commands: []wal.RefUpdate{create}}, 1<<40)
	if err != nil || refusal == nil || refusal.limit != limits.LimitRefs {
		t.Fatalf("%+v %v", refusal, err)
	}
	if refusal.bytes != limits.MaxRefs+1 || refusal.max != limits.MaxRefs {
		t.Errorf("figures: %+v", refusal)
	}
}

// TestQuotaFailsClosedWhenTheListingFails: a push whose lfs/ sum cannot
// be read is 503, never taken on an unmeasured repository.
func TestQuotaFailsClosedWhenTheListingFails(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	work := clone(t, n.url("/r/"+repoA+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "first")
	store.SetFault(func(op, key string) error {
		if op == "List" && strings.Contains(key, "/lfs/") {
			return errors.New("listing down")
		}
		return nil
	})
	if out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("a push was taken with no measurement:\n%s", out)
	}
	if ix, _, _ := n.log.Newest(context.Background(), repoA, 0, false); ix.Seq != 0 {
		t.Fatalf("the push reached the log: %d", ix.Seq)
	}
}

// TestSubprocessSlotsAdmitAndRefuse is the semaphore on the smart HTTP
// routes: a request that waits out the slot wait is 429 rate_limited
// with details.limit "subprocesses", and a freed slot serves again.
func TestSubprocessSlotsAdmitAndRefuse(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store, withLimits(limits.Options{MaxGitProcs: 1, SlotWait: 20 * time.Millisecond}))
	n.create(repoA, "acme", "app")
	held, ok := n.h.limits.Slots().Acquire(context.Background(), time.Second)
	if !ok {
		t.Fatal("the test could not take the one slot")
	}
	for _, path := range []string{
		"/r/" + repoA + ".git/info/refs?service=git-upload-pack",
		"/r/" + repoA + ".git/git-upload-pack",
		"/r/" + repoA + ".git/git-receive-pack",
	} {
		code, env, header := n.request(t, path)
		if code != http.StatusTooManyRequests || env.Code != contract.CodeRateLimited {
			t.Fatalf("%s: %d %+v", path, code, env)
		}
		if env.Details["limit"] != limits.LimitSubprocesses || header.Get("Retry-After") == "" {
			t.Errorf("%s: %+v %v", path, env.Details, header.Get("Retry-After"))
		}
	}
	held()
	if code, env, _ := n.request(t, "/r/"+repoA+".git/info/refs?service=git-upload-pack"); code != http.StatusOK {
		t.Fatalf("after the slot was freed: %d %+v", code, env)
	}
}

// request sends one smart HTTP request, POST for the two service paths
// and GET for the advertisement, and answers the status, the envelope,
// and the headers.
func (n *node) request(t *testing.T, path string) (int, httpjson.Error, http.Header) {
	t.Helper()
	method, body := "GET", io.Reader(nil)
	if strings.Contains(path, "git-upload-pack") && !strings.Contains(path, "info/refs") {
		method, body = "POST", strings.NewReader("0000")
	}
	if strings.Contains(path, "git-receive-pack") && !strings.Contains(path, "info/refs") {
		method, body = "POST", strings.NewReader("0000")
	}
	req, err := http.NewRequestWithContext(context.Background(), method, n.url(path), body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := n.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var envelope httpjson.ErrorEnvelope
	_ = json.Unmarshal(raw, &envelope)
	return resp.StatusCode, envelope.Error, resp.Header
}

// TestConcurrentPushesShareTheSubprocessCap holds the rule that a slot
// is admission control for the request's own subprocess and not for the
// helpers inside it: with two slots, two concurrent pushes both land.
// A helper that took a slot of its own would wedge them against each
// other for the whole wait.
func TestConcurrentPushesShareTheSubprocessCap(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store, withLimits(limits.Options{MaxGitProcs: 2, SlotWait: 5 * time.Second}))
	n.create(repoA, "acme", "app")
	work := clone(t, n.url("/r/"+repoA+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "base")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	// Two branches off the base, so both pushes carry objects and
	// neither is a fast-forward of the other.
	dirs := []string{}
	for _, name := range []string{"left", "right"} {
		dir := filepath.Join(t.TempDir(), name)
		mustGit(t, t.TempDir(), "clone", "-q", n.url("/r/"+repoA+".git"), dir)
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, dir, "add", ".")
		mustGit(t, dir, "commit", "-q", "-m", name)
		dirs = append(dirs, dir)
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, dir := range dirs {
		branch := []string{"refs/heads/left", "refs/heads/right"}[i]
		wg.Go(func() {
			if out, err := git(t, dir, "push", "origin", "HEAD:"+branch); err != nil {
				errs <- errors.New(out)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ix, _, _ := n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Refs["refs/heads/left"] == "" || ix.Refs["refs/heads/right"] == "" {
		t.Fatalf("both pushes did not land: %+v", ix.Refs)
	}
}
