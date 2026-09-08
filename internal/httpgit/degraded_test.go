// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

const repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

// fakeClock moves only when advanced.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// degradedNode is a node over the breaker store of spec 015 with a fake
// clock, a breaker threshold of one so one failure opens a class, and
// a count of the receive-pack POSTs that reached the handler.
type degradedNode struct {
	*node
	mem      *wal.MemStore
	bs       *wal.BreakerStore
	clock    *fakeClock
	receives atomic.Int64
}

func newDegradedNode(t *testing.T, staleMax time.Duration) *degradedNode {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	reg := pkgmetrics.NewRegistry()
	set := metrics.Register(reg)
	mem := wal.NewMemStore()
	clock := &fakeClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	bs := wal.NewBreakerStore(wal.BreakerOptions{Store: mem, Clock: clock, Metrics: set, Threshold: 1})
	l := wal.New(wal.Options{Store: bs, Logger: logger, Metrics: set})
	cache, err := repo.New(repo.Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: logger, Metrics: set, Now: clock.Now, StaleMax: staleMax})
	if err != nil {
		t.Fatal(err)
	}
	guard, authz := newGuard(t, logger)
	h := New(Options{Cache: cache, Logger: logger, Metrics: set, Timeout: time.Minute, Guard: guard, BreakerPoll: 5 * time.Millisecond})
	mux := http.NewServeMux()
	h.Register(mux)
	d := &degradedNode{mem: mem, bs: bs, clock: clock}
	srv := httptest.NewServer(contract.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			d.receives.Add(1)
		}
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Subject: "alice"})))
	})))
	t.Cleanup(srv.Close)
	d.node = &node{t: t, store: bs, log: l, cache: cache, h: h, srv: srv, reg: reg, logger: logger, authz: authz}
	return d
}

// cutReads makes every read of the store fail: the bucket unreachable
// for reads, writes untouched.
func (d *degradedNode) cutReads() {
	d.mem.SetFault(func(op, _ string) error {
		if op == "Head" || op == "Get" || op == "List" {
			return errors.New("unreachable")
		}
		return nil
	})
}

// openWrites fails one write through the wrapper, which opens the
// write breaker at threshold one, and keeps every later write failing
// until the fault is cleared.
func (d *degradedNode) openWrites() {
	d.t.Helper()
	d.mem.SetFault(func(op, _ string) error {
		if op == "Put" || op == "Create" || op == "Delete" {
			return errors.New("write refused")
		}
		return nil
	})
	if _, err := d.bs.Put(context.Background(), "origo/probe", wal.BytesBody(nil)); err == nil {
		d.t.Fatal("the opening write answered")
	}
	if d.bs.Admits(wal.ClassWrite) {
		d.t.Fatal("the write breaker is not open")
	}
}

// warm pushes one commit and returns the working copy.
func (d *degradedNode) warm() string {
	d.t.Helper()
	d.create(repoA, "acme", "app")
	work := clone(d.t, d.url("/r/"+repoA+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		d.t.Fatal(err)
	}
	mustGit(d.t, work, "add", "a.txt")
	mustGit(d.t, work, "commit", "-q", "-m", "first")
	mustGit(d.t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return work
}

// get fetches a path of the node and returns the response, read whole.
func (d *degradedNode) get(path string) (*http.Response, []byte) {
	d.t.Helper()
	resp, err := d.srv.Client().Get(d.url(path))
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func envelope(t *testing.T, body []byte) httpjson.Error {
	t.Helper()
	var env httpjson.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an envelope: %s", body)
	}
	return env.Error
}

// pktLines splits a pkt-line body into its payloads, "" for a flush.
func pktLines(t *testing.T, body []byte) []string {
	t.Helper()
	br := bufio.NewReader(bytes.NewReader(body))
	var out []string
	for {
		payload, kind, err := readPkt(br)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if kind == pktFlush {
			out = append(out, "")
			continue
		}
		out = append(out, string(payload))
	}
}

// TestReadBreakerServesStaleThenRefuses is spec 015's first criterion:
// with the bucket unreachable, a warm repository clones with
// Origo-Stale for StaleMax and answers 503 storage_unavailable after, a
// cold one answers 503 at once, a push is refused at info/refs with the
// ERR pkt-line before any pack is uploaded, and an Acquire for writing
// answers the breaker's refusal at once while the clone is still served
// stale.
func TestReadBreakerServesStaleThenRefuses(t *testing.T) {
	d := newDegradedNode(t, 5*time.Minute)
	ctx := context.Background()
	work := d.warm()
	d.create(repoB, "acme", "cold")
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "b.txt")
	mustGit(t, work, "commit", "-q", "-m", "second")
	c1 := strings.TrimSpace(mustGit(t, work, "rev-parse", "HEAD~1"))
	receives, puts := d.receives.Load(), d.mem.Calls["Put"]

	// The bucket goes away. The check that runs into it fails on its own
	// error and opens the read breaker: 503 without Origo-Stale.
	d.cutReads()
	d.clock.Advance(time.Minute)
	resp, body := d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if e := envelope(t, body); resp.StatusCode != 503 || e.Code != contract.CodeStorageUnavailable || resp.Header.Get(contract.HeaderStale) != "" || e.Details["op"] != "head" {
		t.Fatalf("the failing check: %d %s %+v", resp.StatusCode, resp.Header.Get(contract.HeaderStale), e)
	}
	// Open: the warm repository is served stale, aged from the last
	// check that answered, and clones.
	resp, _ = d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if resp.StatusCode != 200 || resp.Header.Get(contract.HeaderStale) != "60" {
		t.Fatalf("stale advertisement: %d Origo-Stale %q", resp.StatusCode, resp.Header.Get(contract.HeaderStale))
	}
	stale := clone(t, d.url("/r/"+repoA+".git"))
	if strings.TrimSpace(mustGit(t, stale, "rev-parse", "HEAD")) != c1 {
		t.Fatal("the stale clone is not the copy")
	}
	// A cold repository answers 503 at once with the breaker's details
	// and a Retry-After.
	resp, body = d.get("/r/" + repoB + ".git/info/refs?service=git-upload-pack")
	if e := envelope(t, body); resp.StatusCode != 503 || e.Code != contract.CodeStorageUnavailable || e.Details["error"] != "breaker open" || e.Details["op"] != "list" || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("cold repository: %d %+v Retry-After %q", resp.StatusCode, e, resp.Header.Get("Retry-After"))
	}
	// A push is refused at info/refs with the ERR pkt-line, before any
	// pack is uploaded: no POST reaches the handler and no entry is
	// written.
	resp, body = d.get("/r/" + repoA + ".git/info/refs?service=git-receive-pack")
	lines := pktLines(t, body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-git-receive-pack-advertisement" || resp.Header.Get("Retry-After") != "30" ||
		len(lines) != 3 || lines[0] != "# service=git-receive-pack\n" || lines[1] != "" || lines[2] != "ERR storage_unavailable: "+contract.Sentence(contract.CodeStorageUnavailable)+"\n" {
		t.Fatalf("push advertisement: %d %s Retry-After %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Retry-After"), lines)
	}
	out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(out, "remote error: storage_unavailable: "+contract.Sentence(contract.CodeStorageUnavailable)) {
		t.Fatalf("push under the open read breaker: %v\n%s", err, out)
	}
	if d.receives.Load() != receives || d.mem.Calls["Put"] != puts {
		t.Fatalf("%d receive-pack POSTs, %d puts: a pack was uploaded", d.receives.Load()-receives, d.mem.Calls["Put"]-puts)
	}
	// An Acquire for writing is refused at once while the clone is still
	// served stale.
	if _, _, err := d.cache.Acquire(ctx, repoA, true); !errors.Is(err, wal.ErrStorageOpen) {
		t.Fatalf("acquire for writing: %v", err)
	}
	resp, _ = d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if resp.StatusCode != 200 || resp.Header.Get(contract.HeaderStale) != "60" {
		t.Fatalf("stale after the refused write: %d %q", resp.StatusCode, resp.Header.Get(contract.HeaderStale))
	}
	// Five minutes after the last check that answered: the probe of
	// each window fails on the store, and past the bound the copy is
	// refused.
	d.clock.Advance(4 * time.Minute)
	resp, _ = d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if resp.StatusCode != 503 || resp.Header.Get(contract.HeaderStale) != "" {
		t.Fatalf("the probe: %d", resp.StatusCode)
	}
	resp, _ = d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if resp.StatusCode != 200 || resp.Header.Get(contract.HeaderStale) != "300" {
		t.Fatalf("at the bound: %d %q", resp.StatusCode, resp.Header.Get(contract.HeaderStale))
	}
	d.clock.Advance(time.Second)
	resp, body = d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if e := envelope(t, body); resp.StatusCode != 503 || e.Code != contract.CodeStorageUnavailable || e.Details["error"] != "breaker open" {
		t.Fatalf("past the bound: %d %+v", resp.StatusCode, e)
	}
	if out, err := git(t, t.TempDir(), "clone", "-q", d.url("/r/"+repoA+".git"), filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatalf("clone past the bound succeeded\n%s", out)
	}
	if d.h.rejected.Value(nil) != 0 {
		t.Fatal("a refused advertisement counted as a rejected push")
	}
	// The bucket returns: the next request after the window is the
	// probe, answers consistent, and Origo-Stale is gone.
	d.mem.SetFault(nil)
	d.clock.Advance(30 * time.Second)
	resp, _ = d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if resp.StatusCode != 200 || resp.Header.Get(contract.HeaderStale) != "" {
		t.Fatalf("after recovery: %d %q", resp.StatusCode, resp.Header.Get(contract.HeaderStale))
	}
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if d.receives.Load() != receives+1 {
		t.Fatalf("%d receive-pack POSTs after recovery", d.receives.Load()-receives)
	}
}

// TestWriteBreakerRefusesBeforeUpload is spec 015's third criterion's
// first half: with the write breaker open, info/refs?service=git-receive-pack
// answers 200 with the ERR pkt-line and Retry-After, git push exits with
// the sentence in remote error having sent no pack, and reads are
// served consistent because the read breaker is closed.
func TestWriteBreakerRefusesBeforeUpload(t *testing.T) {
	d := newDegradedNode(t, 5*time.Minute)
	work := d.warm()
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "b.txt")
	mustGit(t, work, "commit", "-q", "-m", "second")
	receives := d.receives.Load()
	d.openWrites()
	d.clock.Advance(7 * time.Second)

	resp, body := d.get("/r/" + repoA + ".git/info/refs?service=git-receive-pack")
	lines := pktLines(t, body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-git-receive-pack-advertisement" || resp.Header.Get("Retry-After") != "23" ||
		len(lines) != 3 || lines[2] != "ERR storage_unavailable: "+contract.Sentence(contract.CodeStorageUnavailable)+"\n" {
		t.Fatalf("push advertisement: %d Retry-After %q %q", resp.StatusCode, resp.Header.Get("Retry-After"), lines)
	}
	for _, version := range []string{"0", "2"} {
		out, err := git(t, work, "-c", "protocol.version="+version, "push", "origin", "HEAD:refs/heads/main")
		if err == nil || !strings.Contains(out, "remote error: storage_unavailable: "+contract.Sentence(contract.CodeStorageUnavailable)) {
			t.Fatalf("push under the open write breaker, protocol %s: %v\n%s", version, err, out)
		}
	}
	if d.receives.Load() != receives {
		t.Fatalf("%d receive-pack POSTs reached the handler", d.receives.Load()-receives)
	}
	// Reads are consistent: the read breaker is closed.
	resp, _ = d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if resp.StatusCode != 200 || resp.Header.Get(contract.HeaderStale) != "" {
		t.Fatalf("read under an open write breaker: %d %q", resp.StatusCode, resp.Header.Get(contract.HeaderStale))
	}
	clone(t, d.url("/r/"+repoA+".git"))
	// A node with no breaker store refuses nothing here.
	plain := newNode(t, wal.NewMemStore())
	plain.create(repoA, "acme", "app")
	resp, err := plain.srv.Client().Get(plain.url("/r/" + repoA + ".git/info/refs?service=git-receive-pack"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Retry-After") != "" {
		t.Fatalf("plain node: %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// TestSpooledPushWaitsForTheBreaker is the second half of the third
// criterion: a push whose pack was spooled before the write breaker
// opened is committed when the breaker admits a probe within the wait
// of fake time, refused in the sideband when the wait passes with no
// admission, and refused when the probe fails.
func TestSpooledPushWaitsForTheBreaker(t *testing.T) {
	d := newDegradedNode(t, 5*time.Minute)
	work := d.warm()
	commit := func(name string) {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, work, "add", name)
		mustGit(t, work, "commit", "-q", "-m", name)
	}
	// The breaker opens once the pack is spooled and the hook has handed
	// over; each case then drives the clock from a goroutine after a
	// moment of polling.
	later := func(f func()) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			f()
		}()
	}
	// Admitted within the minute: the store recovers, the window passes,
	// the commit is the probe and lands.
	commit("b.txt")
	d.h.beforeCommit = func() {
		d.openWrites()
		later(func() {
			d.mem.SetFault(nil)
			d.clock.Advance(30 * time.Second)
		})
	}
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if ix, _, err := d.log.Newest(context.Background(), repoA, 0, false); err != nil || ix.Seq != 2 {
		t.Fatalf("after the admitted push: %+v %v", ix, err)
	}
	if !d.bs.Admits(wal.ClassWrite) {
		t.Fatal("the successful probe did not close the breaker")
	}
	// The minute passes with no admission: another caller's probe holds
	// the breaker half-open the whole time, so the push is refused in
	// the sideband with the sentence and nothing is written.
	commit("c.txt")
	hold := make(chan struct{})
	probeDone := make(chan struct{})
	d.h.beforeCommit = func() {
		d.openWrites()
		d.mem.SetFault(func(op, _ string) error {
			if op == "Put" {
				<-hold
				return errors.New("write refused")
			}
			return nil
		})
		d.clock.Advance(30 * time.Second)
		go func() {
			_, _ = d.bs.Put(context.Background(), "origo/probe", wal.BytesBody(nil))
			close(probeDone)
		}()
		for d.bs.Admits(wal.ClassWrite) {
			time.Sleep(time.Millisecond)
		}
		later(func() { d.clock.Advance(BreakerWait + time.Second) })
	}
	out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(out, "remote: storage_unavailable: "+contract.Sentence(contract.CodeStorageUnavailable)) {
		t.Fatalf("push past the wait: %v\n%s", err, out)
	}
	close(hold)
	<-probeDone
	if ix, _, _ := d.log.Newest(context.Background(), repoA, 0, false); ix.Seq != 2 {
		t.Fatalf("a refused push moved the log to %d", ix.Seq)
	}
	// The probe fails: the same line, and the breaker is open again.
	d.mem.SetFault(nil)
	d.clock.Advance(30 * time.Second)
	if _, err := d.bs.Put(context.Background(), "origo/probe", wal.BytesBody(nil)); err != nil {
		t.Fatalf("closing the breaker: %v", err)
	}
	d.h.beforeCommit = func() {
		d.openWrites()
		later(func() { d.clock.Advance(30 * time.Second) })
	}
	out, err = git(t, work, "push", "origin", "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(out, "remote: storage_unavailable: "+contract.Sentence(contract.CodeStorageUnavailable)) {
		t.Fatalf("push with a failed probe: %v\n%s", err, out)
	}
	if d.bs.Admits(wal.ClassWrite) {
		t.Fatal("the failed probe did not reopen the breaker")
	}
	if d.h.rejected.Value(nil) != 2 {
		t.Fatalf("%d rejected pushes", d.h.rejected.Value(nil))
	}
}

// TestIntegrityErrorIsRepositoryUnavailable: a repository whose entry
// is gone from the log answers 503 repository_unavailable with the key,
// counts one integrity error, and leaves another repository served.
func TestIntegrityErrorIsRepositoryUnavailable(t *testing.T) {
	d := newDegradedNode(t, 5*time.Minute)
	ctx := context.Background()
	d.warm()
	d.create(repoB, "acme", "other")
	ix, _, err := d.log.Newest(ctx, repoA, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	key := d.log.RepoPrefix(repoA) + ix.Entry
	if err := d.mem.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	d.cache.Evict(repoA)
	resp, body := d.get("/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if e := envelope(t, body); resp.StatusCode != 503 || e.Code != contract.CodeRepositoryUnavailable || e.Details["key"] != key || e.Details["error"] == "" || e.Message != contract.Sentence(contract.CodeRepositoryUnavailable) {
		t.Fatalf("missing entry: %d %+v", resp.StatusCode, e)
	}
	if resp, _ := d.get("/r/" + repoB + ".git/info/refs?service=git-upload-pack"); resp.StatusCode != 200 {
		t.Fatalf("the other repository: %d", resp.StatusCode)
	}
	var text bytes.Buffer
	d.reg.WritePrometheus(&text)
	if !strings.Contains(text.String(), "origo_log_integrity_errors_total 1") {
		t.Fatalf("metrics:\n%s", text.String())
	}
}
