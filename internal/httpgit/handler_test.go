// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"
	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/stubs/authorizer"
	"github.com/latere-ai/origo/test/stubs/issuer"
	"github.com/latere-ai/origo/test/stubs/sink"
)

const repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

// node is one origod-like server over a shared store: a cache, a
// handler, and an httptest server, so a test runs two nodes against one
// bucket.
type node struct {
	t      *testing.T
	store  wal.Store
	log    *wal.Log
	cache  *repo.Cache
	h      *Handler
	srv    *httptest.Server
	reg    *pkgmetrics.Registry
	logger *slog.Logger
	authz  *authorizer.Server
}

// newGuard builds a guard over a stub authorizer that allows everyone.
func newGuard(t *testing.T, logger *slog.Logger) (*auth.Guard, *authorizer.Server) {
	t.Helper()
	authz := authorizer.New(t)
	client, err := auth.NewClient(auth.ClientOptions{URL: authz.URL(), Token: authz.Token(), HTTP: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewGuard(client, logger), authz
}

func newNode(t *testing.T, store wal.Store) *node {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	reg := pkgmetrics.NewRegistry()
	set := metrics.Register(reg)
	l := wal.New(wal.Options{Store: store, Logger: logger, Metrics: set})
	cache, err := repo.New(repo.Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: logger, Metrics: set})
	if err != nil {
		t.Fatal(err)
	}
	guard, authz := newGuard(t, logger)
	h := New(Options{Cache: cache, Logger: logger, Metrics: set, Timeout: time.Minute, Guard: guard})
	mux := http.NewServeMux()
	h.Register(mux)
	// The verifier is spec 007's own; here the principal is set on the
	// request the way the middleware does.
	srv := httptest.NewServer(contract.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Subject: "alice"})))
	})))
	t.Cleanup(srv.Close)
	return &node{t: t, store: store, log: l, cache: cache, h: h, srv: srv, reg: reg, logger: logger, authz: authz}
}

func (n *node) url(path string) string { return n.srv.URL + path }

func (n *node) create(id, owner, slug string) {
	n.t.Helper()
	if _, err := n.log.CreateRepo(context.Background(), wal.Meta{ID: id, Owner: owner, Slug: slug}, "main"); err != nil {
		n.t.Fatal(err)
	}
}

// git runs the client git against a working directory.
func git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(dir), "GIT_CURL_VERBOSE=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String() + stderr.String(), nil
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func clone(t *testing.T, url string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	mustGit(t, t.TempDir(), "clone", "-q", url, dir)
	return dir
}

func TestCloneFetchPushOverSmartHTTP(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")

	// Clone the empty repository by id, commit, push.
	work := clone(t, n.url("/r/"+repoA+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "first")
	out := mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	c1 := strings.TrimSpace(mustGit(t, work, "rev-parse", "HEAD"))
	_ = out
	ix, _, err := n.log.Newest(context.Background(), repoA, 0, false)
	if err != nil || ix.Seq != 1 || ix.Refs["refs/heads/main"] != c1 || ix.Entries[0].PackSHA256 == "" {
		t.Fatalf("after push: %+v, %v", ix, err)
	}
	if n.h.pushes.Value(nil) != 1 {
		t.Fatal("push not counted")
	}
	// The entry holds the pack the client sent, verified by digest, and
	// a second node builds the same history from it, by the label URL.
	other := newNode(t, store)
	second := clone(t, other.url("/acme/app.git"))
	if gittest.RevList(t, second) != gittest.RevList(t, work) {
		t.Fatal("history differs on the second node")
	}
	// Push from the second clone through the other node: a branch and a
	// tag in one atomic push with a push option, then fetch on the first.
	if err := os.WriteFile(filepath.Join(second, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, second, "add", "b.txt")
	mustGit(t, second, "commit", "-q", "-m", "second")
	mustGit(t, second, "tag", "v1")
	mustGit(t, second, "push", "-q", "--atomic", "-o", "origo.event=off", "origin", "HEAD:refs/heads/dev", "refs/tags/v1")
	c2 := strings.TrimSpace(mustGit(t, second, "rev-parse", "HEAD"))
	mustGit(t, work, "fetch", "-q", "origin")
	if got := strings.TrimSpace(mustGit(t, work, "rev-parse", "origin/dev")); got != c2 {
		t.Fatalf("fetched dev = %s, want %s", got, c2)
	}
	ix, _, _ = n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 2 || ix.Refs["refs/tags/v1"] != c2 || ix.Refs["refs/heads/dev"] != c2 {
		t.Fatalf("after second push: %+v", ix)
	}
	rc, _, _ := store.Get(context.Background(), n.log.RepoPrefix(repoA)+ix.Entry, "")
	hdr, refs, _, err := wal.ReadEntryHead(rc)
	_ = rc.Close()
	if err != nil || hdr.Subject != "alice" || strings.Join(hdr.PushOptions, ",") != "origo.event=off" || len(refs) != 2 {
		t.Fatalf("entry header %+v refs %+v, %v", hdr, refs, err)
	}
	// Protocol v2 and a partial clone work against the same node.
	partial := filepath.Join(t.TempDir(), "partial")
	mustGit(t, t.TempDir(), "-c", "protocol.version=2", "clone", "-q", "--filter=blob:none", n.url("/r/"+repoA+".git"), partial)
	if strings.TrimSpace(mustGit(t, partial, "rev-parse", "HEAD")) != c1 {
		t.Fatal("partial clone HEAD")
	}
	shallow := filepath.Join(t.TempDir(), "shallow")
	mustGit(t, t.TempDir(), "clone", "-q", "--depth", "1", "--branch", "dev", n.url("/r/"+repoA+".git"), shallow)
	if strings.TrimSpace(mustGit(t, shallow, "rev-list", "--count", "HEAD")) != "1" {
		t.Fatal("shallow clone depth")
	}
	// A reachable commit is fetchable by hash.
	byHash := filepath.Join(t.TempDir(), "byhash")
	mustGit(t, t.TempDir(), "init", "-q", byHash)
	mustGit(t, byHash, "fetch", "-q", "--depth", "1", n.url("/r/"+repoA+".git"), c1)
	// Deleting a branch is a push without a pack.
	mustGit(t, second, "push", "-q", "origin", ":refs/heads/dev")
	ix, _, _ = n.log.Newest(context.Background(), repoA, 0, false)
	if _, ok := ix.Refs["refs/heads/dev"]; ok || ix.Seq != 3 {
		t.Fatalf("after delete: %+v", ix)
	}
	// A push of nothing new is a no-op on the log.
	mustGit(t, second, "push", "-q", "origin", "HEAD:refs/heads/main2")
	mustGit(t, second, "push", "-q", "origin", "HEAD:refs/heads/main2")
	ix, _, _ = n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 4 {
		t.Fatalf("repeated push moved the log: %d", ix.Seq)
	}
	// A body of unknown length arrives chunked; the handler streams it
	// to git as it arrives.
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("0000")); _ = pw.Close() }()
	resp, err := n.srv.Client().Post(n.url("/r/"+repoA+".git/git-upload-pack"), "application/x-git-upload-pack-request", pr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || n.h.fetches.Value(nil) == 0 {
		t.Fatalf("chunked fetch: %d, %d fetches counted", resp.StatusCode, n.h.fetches.Value(nil))
	}
}

func TestStalePushIsRefusedAndConcurrentBranchesLand(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	a := clone(t, n.url("/r/"+repoA+".git"))
	_ = os.WriteFile(filepath.Join(a, "a.txt"), []byte("a"), 0o644)
	mustGit(t, a, "add", "a.txt")
	mustGit(t, a, "commit", "-q", "-m", "base")
	mustGit(t, a, "push", "-q", "origin", "HEAD:refs/heads/main")
	b := clone(t, n.url("/r/"+repoA+".git"))

	// Both clones move main; the second push is stale and refused with
	// git's non-fast-forward result, and the log has one entry for it.
	_ = os.WriteFile(filepath.Join(a, "a.txt"), []byte("a2"), 0o644)
	mustGit(t, a, "commit", "-q", "-am", "a2")
	mustGit(t, a, "push", "-q", "origin", "HEAD:refs/heads/main")
	_ = os.WriteFile(filepath.Join(b, "b.txt"), []byte("b"), 0o644)
	mustGit(t, b, "add", "b.txt")
	mustGit(t, b, "commit", "-q", "-m", "b")
	if out, err := git(t, b, "push", "origin", "HEAD:refs/heads/main"); err == nil || !strings.Contains(out, "rejected") {
		t.Fatalf("stale push accepted: %v\n%s", err, out)
	}
	ix, _, _ := n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 2 {
		t.Fatalf("stale push moved the log: %d", ix.Seq)
	}
	// Another node moves main; node n still holds sequence 2 locally.
	// A forced push to n sees the moved reference in the advertisement,
	// because the currency check runs before it, and lands on top.
	mustGit(t, b, "fetch", "-q", "origin")
	other := newNode(t, store)
	c := clone(t, other.url("/r/"+repoA+".git"))
	_ = os.WriteFile(filepath.Join(c, "c.txt"), []byte("c"), 0o644)
	mustGit(t, c, "add", "c.txt")
	mustGit(t, c, "commit", "-q", "-m", "c")
	mustGit(t, c, "push", "-q", "origin", "HEAD:refs/heads/main")
	mustGit(t, b, "push", "-q", "--force", "origin", "HEAD:refs/heads/main")
	ix, _, _ = n.log.Newest(context.Background(), repoA, 0, false)
	bHead := strings.TrimSpace(mustGit(t, b, "rev-parse", "HEAD"))
	if ix.Seq != 4 || ix.Refs["refs/heads/main"] != bHead {
		t.Fatalf("forced push: %+v", ix)
	}
	mustGit(t, c, "fetch", "-q", "origin")
	mustGit(t, c, "reset", "-q", "--hard", "origin/main")

	// Concurrent pushes to different branches from two clones through
	// two nodes both land, one index object per sequence.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, dir := range []string{b, c} {
		branch := []string{"refs/heads/left", "refs/heads/right"}[i]
		wg.Go(func() {
			if out, err := git(t, dir, "push", "-q", "origin", "HEAD:"+branch); err != nil {
				errs <- errors.New(out)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ix, _, _ = n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 6 || ix.Refs["refs/heads/left"] == "" || ix.Refs["refs/heads/right"] == "" {
		t.Fatalf("after concurrent pushes: %+v", ix)
	}
	if n.reg.Counter("origo_wal_commit_retries_total", "").Value(nil)+other.reg.Counter("origo_wal_commit_retries_total", "").Value(nil) == 0 {
		t.Log("the two pushes did not overlap; both landed anyway")
	}
}

func TestRoutesRefuseWhatTheyCannotServe(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	client := n.srv.Client()
	get := func(path string) (int, string) {
		resp, err := client.Get(n.url(path))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(path, body string, header map[string]string) (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", n.url(path), strings.NewReader(body))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code := func(body string) string {
		var env httpjson.ErrorEnvelope
		_ = json.Unmarshal([]byte(body), &env)
		return env.Error.Code
	}
	if s, b := get("/r/" + repoA + ".git/info/refs"); s != 400 || code(b) != contract.CodeInvalid {
		t.Fatalf("dumb protocol: %d %s", s, b)
	}
	if s, b := get("/r/" + repoA + ".git/info/refs?service=git-frobnicate"); s != 400 || code(b) != contract.CodeInvalid {
		t.Fatalf("unknown service: %d %s", s, b)
	}
	if s, b := get("/r/00000000-0000-4000-8000-000000000000.git/info/refs?service=git-upload-pack"); s != 404 || code(b) != contract.CodeRepoNotFound {
		t.Fatalf("unknown id: %d %s", s, b)
	}
	if s, b := get("/nobody/nothing.git/info/refs?service=git-upload-pack"); s != 404 || code(b) != contract.CodeRepoNotFound {
		t.Fatalf("unknown name: %d %s", s, b)
	}
	if s, b := get("/r/" + repoA + ".git/info/refs?service=git-receive-pack"); s != 200 || !strings.Contains(b, "# service=git-receive-pack") || !strings.Contains(b, "atomic") || !strings.Contains(b, "push-options") || !strings.Contains(b, "report-status-v2") {
		t.Fatalf("receive advertisement: %d %q", s, b)
	}
	if s, b := get("/r/" + repoA + ".git/info/refs?service=git-upload-pack"); s != 200 || !strings.Contains(b, "allow-reachable-sha1-in-want") || !strings.Contains(b, "filter") || !strings.Contains(b, "shallow") {
		t.Fatalf("upload advertisement: %d %q", s, b)
	}
	if s, b := post("/r/"+repoA+".git/git-receive-pack", "zz", nil); s != 400 || code(b) != contract.CodeInvalid {
		t.Fatalf("garbage push: %d %s", s, b)
	}
	if s, b := post("/r/"+repoA+".git/git-receive-pack", "zz", map[string]string{"Content-Encoding": "gzip"}); s != 400 {
		t.Fatalf("bad gzip push: %d %s", s, b)
	}
	if s, b := post("/r/"+repoA+".git/git-upload-pack", "zz", map[string]string{"Content-Encoding": "gzip"}); s != 400 {
		t.Fatalf("bad gzip fetch: %d %s", s, b)
	}
	// An empty push (flush only) runs receive-pack without the hook.
	if s, _ := post("/r/"+repoA+".git/git-receive-pack", "0000", nil); s != 200 {
		t.Fatalf("empty push: %d", s)
	}
	// A push whose pack is corrupt is refused by git before the hook,
	// and nothing reaches the log.
	zero := strings.Repeat("0", 40)
	one := strings.Repeat("1", 40)
	body := receiveBody(t, "report-status", nil, "PACK\x00\x00\x00\x02\x00\x00\x00\x01garbagegarbagegarbage", zero+" "+one+" refs/heads/main")
	if s, b := post("/r/"+repoA+".git/git-receive-pack", string(body), nil); s != 200 || !strings.Contains(b, "unpack") {
		t.Fatalf("corrupt pack: %d %q", s, b)
	}
	if ix, _, _ := n.log.Newest(context.Background(), repoA, 0, false); ix.Seq != 0 {
		t.Fatalf("corrupt pack reached the log: %d", ix.Seq)
	}
	// Storage failures answer 503 with the storage code.
	store.SetFault(func(op, key string) error { return errors.New("storage down") })
	if s, b := get("/r/" + repoA + ".git/info/refs?service=git-upload-pack"); s != 503 || code(b) != contract.CodeStorageUnavailable {
		t.Fatalf("storage down: %d %s", s, b)
	}
	if s, b := get("/acme/app.git/info/refs?service=git-upload-pack"); s != 503 || code(b) != contract.CodeStorageUnavailable {
		t.Fatalf("storage down by name: %d %s", s, b)
	}
	store.SetFault(nil)
	// A gzip upload-pack body reaches git.
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("0000"))
	_ = w.Close()
	if s, _ := post("/r/"+repoA+".git/git-upload-pack", gz.String(), map[string]string{"Content-Encoding": "gzip"}); s != 200 {
		t.Fatalf("gzip fetch: %d", s)
	}
}

func TestPushWhenTheLogRefusesTheCommit(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	work := clone(t, n.url("/r/"+repoA+".git"))
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644)
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "a")
	// The entry write fails: the hook declines with the storage code and
	// git reports the rejection; the local copy stays at sequence 0.
	store.SetFault(func(op, key string) error {
		if op == "Put" && strings.Contains(key, "/wal/") {
			return errors.New("bucket full")
		}
		return nil
	})
	out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(out, contract.CodeStorageUnavailable) {
		t.Fatalf("push with a failing log accepted: %v\n%s", err, out)
	}
	store.SetFault(nil)
	r, release, err := n.cache.Acquire(context.Background(), repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if r.Seq != 0 {
		t.Fatalf("local copy moved to %d", r.Seq)
	}
	if _, err := n.cache.Git().Run(context.Background(), r.Dir, nil, "rev-parse", "--verify", "-q", "refs/heads/main"); err == nil {
		t.Fatal("git updated the reference although the log refused")
	}
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	// The pack section cannot be read when the spool vanished.
	if _, err := packBody(filepath.Join(t.TempDir(), "missing"), 0, 10); err == nil {
		t.Fatal("missing spool accepted")
	}
	f := filepath.Join(t.TempDir(), "short")
	_ = os.WriteFile(f, []byte("abc"), 0o644)
	b, err := packBody(f, 1, 2)
	if err != nil || b.Size != 2 || b.SHA256 != wal.BytesBody([]byte("bc")).SHA256 {
		t.Fatalf("section body: %+v, %v", b, err)
	}
	if got, _ := b.ReadAll(); string(got) != "bc" {
		t.Fatalf("section read = %q", got)
	}
	if short("abc") != "abc" || short(strings.Repeat("x", 20)) != strings.Repeat("x", 12) {
		t.Fatal("short")
	}
}

// TestReferenceMovedBetweenAdvertisementAndPush drives the two ways a
// push meets a reference another writer moved. First the winner commits
// before the pack arrives: the currency check applies it and the
// transaction is refused before anything is written. Then the winner
// commits between the currency check and the index create: the create
// answers 412, the catch-up applies the winner into the local copy, and
// the push is replayed and lands on top.
func TestReferenceMovedBetweenAdvertisementAndPush(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	a := clone(t, n.url("/r/"+repoA+".git"))
	_ = os.WriteFile(filepath.Join(a, "a.txt"), []byte("a"), 0o644)
	mustGit(t, a, "add", "a.txt")
	mustGit(t, a, "commit", "-q", "-m", "base")
	mustGit(t, a, "push", "-q", "origin", "HEAD:refs/heads/main")
	base := strings.TrimSpace(mustGit(t, a, "rev-parse", "HEAD"))
	b := clone(t, n.url("/r/"+repoA+".git"))

	// b builds its push against base and the pack for it.
	_ = os.WriteFile(filepath.Join(b, "b.txt"), []byte("b"), 0o644)
	mustGit(t, b, "add", "b.txt")
	mustGit(t, b, "commit", "-q", "-m", "b")
	bHead := strings.TrimSpace(mustGit(t, b, "rev-parse", "HEAD"))
	src := &gittestSource{dir: b, t: t}
	pack := src.thinPack(bHead, base)
	// side-band-64k carries the hook's message to the client.
	body := receiveBody(t, "report-status side-band-64k", nil, string(pack), base+" "+bHead+" refs/heads/main")

	// Meanwhile a moves main through another node.
	other := newNode(t, store)
	_ = os.WriteFile(filepath.Join(a, "a.txt"), []byte("a2"), 0o644)
	mustGit(t, a, "commit", "-q", "-am", "a2")
	mustGit(t, a, "remote", "set-url", "origin", other.url("/r/"+repoA+".git"))
	mustGit(t, a, "push", "-q", "origin", "HEAD:refs/heads/main")
	aHead := strings.TrimSpace(mustGit(t, a, "rev-parse", "HEAD"))

	// b's pack arrives at n with the stale old value.
	post := func() (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", n.url("/r/"+repoA+".git/git-receive-pack"), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		resp, err := n.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(out)
	}
	if status, out := post(); status != 200 || !strings.Contains(out, contract.CodeNonFastForward) || !strings.Contains(out, "pre-receive hook declined") {
		t.Fatalf("stale push: %d %q", status, out)
	}
	ix, _, _ := n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 2 || ix.Refs["refs/heads/main"] != aHead {
		t.Fatalf("log after the refused push: %+v", ix)
	}
	if n.reg.Counter("origo_wal_commit_retries_total", "").Value(nil) != 0 || n.h.rejected.Value(nil) != 1 {
		t.Fatal("the refused push was not caught before the commit")
	}

	// Now the interloper commits exactly when n creates index/3: the
	// store's fault hook runs a's commit of a third branch through the
	// log before n's create is applied.
	_ = os.WriteFile(filepath.Join(a, "c.txt"), []byte("c"), 0o644)
	mustGit(t, a, "add", "c.txt")
	mustGit(t, a, "commit", "-q", "-m", "c")
	cHead := strings.TrimSpace(mustGit(t, a, "rev-parse", "HEAD"))
	interloper := wal.New(wal.Options{Store: store, Logger: n.logger})
	cPack := (&gittestSource{dir: a, t: t}).thinPack(cHead, aHead)
	var once sync.Once
	store.SetFault(func(op, key string) error {
		if op == "Create" && strings.HasSuffix(key, wal.IndexKey(3)) {
			once.Do(func() {
				store.SetFault(nil)
				cur, _, err := interloper.Newest(context.Background(), repoA, 0, false)
				if err != nil {
					t.Error(err)
					return
				}
				e := wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{Ref: "refs/heads/other", Old: wal.ZeroSHA, New: cHead}}, Pack: wal.BytesBody(cPack)}
				if _, err := interloper.Commit(context.Background(), repoA, cur, e, func(context.Context, *wal.Index) error { return nil }); err != nil {
					t.Error(err)
				}
			})
		}
		return nil
	})
	mustGit(t, b, "fetch", "-q", "origin")
	mustGit(t, b, "reset", "-q", "--hard", "origin/main")
	_ = os.WriteFile(filepath.Join(b, "d.txt"), []byte("d"), 0o644)
	mustGit(t, b, "add", "d.txt")
	mustGit(t, b, "commit", "-q", "-m", "d")
	dHead := strings.TrimSpace(mustGit(t, b, "rev-parse", "HEAD"))
	body = receiveBody(t, "report-status side-band-64k", nil, string(src.thinPack(dHead, aHead)), aHead+" "+dHead+" refs/heads/main")
	if status, out := post(); status != 200 || !strings.Contains(out, "ok refs/heads/main") {
		t.Fatalf("replayed push: %d %q", status, out)
	}
	ix, _, _ = n.log.Newest(context.Background(), repoA, 0, false)
	if ix.Seq != 4 || ix.Refs["refs/heads/main"] != dHead || ix.Refs["refs/heads/other"] != cHead {
		t.Fatalf("log after the replay: %+v", ix)
	}
	if n.reg.Counter("origo_wal_commit_retries_total", "").Value(nil) != 1 {
		t.Fatal("the push did not replay one round")
	}
	// The local copy holds the interloper's objects and reference too.
	r, release, err := n.cache.Acquire(context.Background(), repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if r.Seq != 4 {
		t.Fatalf("local seq %d", r.Seq)
	}
	if out, err := n.cache.Git().Run(context.Background(), r.Dir, nil, "rev-parse", "--verify", "refs/heads/other"); err != nil || strings.TrimSpace(string(out)) != cHead {
		t.Fatalf("interloper's branch not applied locally: %q %v", out, err)
	}
	mustGit(t, b, "fetch", "-q", "origin")
	if strings.TrimSpace(mustGit(t, b, "rev-parse", "origin/other")) != cHead {
		t.Fatal("interloper's branch not served")
	}
}

type gittestSource struct {
	dir string
	t   *testing.T
}

func (s *gittestSource) thinPack(want, have string) []byte {
	s.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "pack-objects", "--stdout", "--revs", "--thin", "-q")
	cmd.Dir = s.dir
	cmd.Env = gittest.Env(s.dir)
	cmd.Stdin = strings.NewReader(want + "\n^" + have + "\n")
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

// newNodeAt builds a node over an existing data directory with the git
// binary given, so a test serves a materialized repository with a git
// that fails.
func newNodeAt(t *testing.T, store wal.Store, dataDir, gitBin string) *node {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	reg := pkgmetrics.NewRegistry()
	set := metrics.Register(reg)
	l := wal.New(wal.Options{Store: store, Logger: logger, Metrics: set})
	cache, err := repo.New(repo.Options{Dir: dataDir, Log: l, Logger: logger, Metrics: set, GitBin: gitBin})
	if err != nil {
		t.Fatal(err)
	}
	guard, authz := newGuard(t, logger)
	h := New(Options{Cache: cache, Logger: logger, Metrics: set, Timeout: time.Minute, Guard: guard})
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &node{t: t, store: store, log: l, cache: cache, h: h, srv: srv, reg: reg, logger: logger, authz: authz}
}

func TestGitFailuresAreReported(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	work := clone(t, n.url("/r/"+repoA+".git"))
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644)
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "a")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")

	// A second node over the same materialized copy with a git that
	// always fails: the advertisement and the push answer 503; the fetch
	// has already sent its headers and ends without a body.
	stub := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := newNodeAt(t, store, n.cache.Dir(), stub)
	get := func(path string) int {
		resp, err := broken.srv.Client().Get(broken.url(path))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := get("/r/" + repoA + ".git/info/refs?service=git-upload-pack"); s != 503 {
		t.Fatalf("advertise without git: %d", s)
	}
	zero := strings.Repeat("0", 40)
	one := strings.Repeat("1", 40)
	body := receiveBody(t, "report-status", nil, "", one+" "+zero+" refs/heads/main")
	resp, err := broken.srv.Client().Post(broken.url("/r/"+repoA+".git/git-receive-pack"), "application/x-git-receive-pack-request", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		// receive-pack starts and exits 1 before the hook runs: the
		// handler releases the hook channel and reports nothing to the
		// log.
		t.Fatalf("push without git: %d", resp.StatusCode)
	}
	resp, err = broken.srv.Client().Post(broken.url("/r/"+repoA+".git/git-upload-pack"), "application/x-git-upload-pack-request", strings.NewReader("0000"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(b) != 0 {
		t.Fatalf("fetch without git: %d %q", resp.StatusCode, b)
	}
	// A git that cannot start at all is a 503 on the push.
	missing := newNodeAt(t, store, n.cache.Dir(), stub)
	if err := os.Remove(stub); err != nil {
		t.Fatal(err)
	}
	resp, err = missing.srv.Client().Post(missing.url("/r/"+repoA+".git/git-receive-pack"), "application/x-git-receive-pack-request", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("push with no git binary: %d", resp.StatusCode)
	}

	// A cancelled request kills the process group git runs in.
	r, release, err := n.cache.Acquire(context.Background(), repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cmd, done := n.h.gitCommand(ctx, httptest.NewRequest("GET", "/", nil), r, "cat-file", "--batch")
	defer done()
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("cancelled git exited cleanly")
	}
	_ = stdin.Close()
	// Defaults fill in what the options leave out.
	guard, _ := newGuard(t, nil)
	h := New(Options{Cache: n.cache, Guard: guard})
	if h.timeout != 5*time.Minute || h.logger == nil || h.pushes == nil {
		t.Fatalf("defaults: %+v", h)
	}
}

// TestActClaimIsRecordedOnEntryAndAuthorizer: a service token carrying
// act sets the effective subject; the authorizer request carries both
// and the entry header records subject and actor.
func TestActClaimIsRecordedOnEntryAndAuthorizer(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	// A second server over the same handler with the real verifier and a
	// stub issuer in front.
	iss := issuer.New(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(auth.VerifierOptions{Issuers: []string{iss.URL()}, LocalIssuer: "https://git.example.com", LocalKey: &key.PublicKey, Client: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	n.h.Register(mux)
	srv := httptest.NewServer(contract.Middleware(v.Middleware(mux)))
	t.Cleanup(srv.Close)
	token := iss.Mint(issuer.Claims{Sub: "svc", Act: "alice"})
	u, _ := url.Parse(srv.URL)
	u.User = url.UserPassword("x", token)
	work := clone(t, u.String()+"/r/"+repoA+".git")
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644)
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "a")
	n.authz.ClearRequests()
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	reqs := n.authz.Requests()
	if len(reqs) == 0 {
		t.Fatal("the authorizer was not asked")
	}
	for _, r := range reqs {
		if r.Subject != "alice" || r.Actor != "svc" || r.Repo.ID != repoA || r.Action != "write" {
			t.Fatalf("authorizer request: %+v", r)
		}
	}
	ix, _, _ := n.log.Newest(context.Background(), repoA, 0, false)
	rc, _, _ := store.Get(context.Background(), n.log.RepoPrefix(repoA)+ix.Entry, "")
	hdr, _, _, err := wal.ReadEntryHead(rc)
	_ = rc.Close()
	if err != nil || hdr.Subject != "alice" || hdr.Actor != "svc" {
		t.Fatalf("entry header: %+v, %v", hdr, err)
	}
	// Without a token git is refused with the challenge on info/refs.
	resp, err := srv.Client().Get(srv.URL + "/r/" + repoA + ".git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a handler without a guard")
		}
	}()
	New(Options{Cache: n.cache})
}

// pushHistogram reads origo_push_duration_seconds from the registry:
// the count and the sum per phase.
func pushHistogram(reg *pkgmetrics.Registry) (count map[string]int, sum map[string]float64) {
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	re := regexp.MustCompile(`origo_push_duration_seconds_(sum|count)\{phase="(\w+)"\} (\S+)`)
	count, sum = map[string]int{}, map[string]float64{}
	for _, m := range re.FindAllStringSubmatch(buf.String(), -1) {
		v, _ := strconv.ParseFloat(m[3], 64)
		if m[1] == "count" {
			count[m[2]] = int(v)
		} else {
			sum[m[2]] = v
		}
	}
	return count, sum
}

// TestPushPhasesAreObserved is spec 008's criterion for the metric of
// spec 011: origo_push_duration_seconds{phase} observes the four phases
// of one push, each once, and the sum is within 10% of the push's
// request duration.
func TestPushPhasesAreObserved(t *testing.T) {
	n := newNode(t, wal.NewMemStore())
	n.create(repoA, "acme", "app")
	// A server that times the receive-pack request around the handler.
	var requestTime time.Duration
	mux := http.NewServeMux()
	n.h.Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Subject: "alice"})))
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			requestTime = time.Since(started)
		}
	}))
	t.Cleanup(srv.Close)
	work := clone(t, srv.URL+"/r/"+repoA+".git")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "first")
	if count, _ := pushHistogram(n.reg); len(count) != 0 {
		t.Fatalf("observations before a push: %v", count)
	}
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	count, sum := pushHistogram(n.reg)
	var total float64
	for _, phase := range []string{phaseReceive, phaseEntry, phaseIndex, phaseApply} {
		if count[phase] != 1 || sum[phase] <= 0 {
			t.Errorf("phase %s: %d observations, sum %v", phase, count[phase], sum[phase])
		}
		total += sum[phase]
	}
	if len(count) != 4 {
		t.Errorf("phases %v", count)
	}
	if ratio := total / requestTime.Seconds(); ratio < 0.9 || ratio > 1.1 {
		t.Fatalf("phases sum to %.1f%% of the request's %v", ratio*100, requestTime)
	}
	// A refused push observes receive and nothing else.
	mustGit(t, work, "commit", "-q", "--allow-empty", "-m", "second")
	n.store.(*wal.MemStore).SetFault(func(op, _ string) error {
		if op == "Put" {
			return errors.New("bucket down")
		}
		return nil
	})
	if _, err := git(t, work, "push", "-q", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatal("push landed with the bucket down")
	}
	if count, _ = pushHistogram(n.reg); count[phaseReceive] != 2 || count[phaseEntry] != 1 || count[phaseApply] != 1 {
		t.Fatalf("after a refused push: %v", count)
	}
}

// TestForcedFlagIsComputedBeforeTheVerdict is spec 008's criterion: a
// forced update is reported as such and a fast-forward as not, with the
// flag computed while the objects are still in quarantine, asserted by
// a hook that leaves the quarantine in place: the seam that runs before
// the verdict is written sees the pushed commit only through the
// quarantine.
func TestForcedFlagIsComputedBeforeTheVerdict(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	s := sink.New(t)
	d, err := events.New(events.Options{Log: n.log, Node: "n1", URL: s.URL(), Secret: s.Secret(), Logger: n.logger})
	if err != nil {
		t.Fatal(err)
	}
	if h := New(Options{Cache: n.cache, Guard: n.h.guard, Events: d}); h.events != d {
		t.Fatal("Options.Events not kept")
	}
	n.h.events = d
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = d.Run(ctx) }()

	repoDir := filepath.Join(n.cache.Dir(), "repos", repoA+".git")
	var pushed string
	var seen []map[string]bool
	catFile := func(sha string, env ...string) error {
		cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", sha)
		cmd.Env = append(append(gittest.Env(t.TempDir()), "GIT_DIR="+repoDir), env...)
		return cmd.Run()
	}
	n.h.beforeVerdict = func(quarantine string, forced map[string]bool) {
		seen = append(seen, forced)
		// A push without a pack (a delete) has no quarantine; every
		// other one reports the directory git holds the objects in.
		if pushed == "" {
			return
		}
		if st, err := os.Stat(quarantine); quarantine == "" || err != nil || !st.IsDir() {
			t.Errorf("quarantine %q: %v", quarantine, err)
			return
		}
		// The pushed commit is in the quarantine and not in the
		// repository: the flag was computed before git moved anything.
		if err := catFile(pushed); err == nil {
			t.Errorf("%s already in the repository before the verdict", pushed)
		}
		if err := catFile(pushed, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+quarantine); err != nil {
			t.Errorf("%s not in the quarantine %s: %v", pushed, quarantine, err)
		}
	}

	work := clone(t, n.url("/r/"+repoA+".git"))
	commit := func(name, msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, work, "add", name)
		mustGit(t, work, "commit", "-q", "-m", msg)
		return strings.TrimSpace(mustGit(t, work, "rev-parse", "HEAD"))
	}
	// A create, a fast-forward, a forced update, a create of a branch
	// and a tag, and a delete.
	pushed = commit("a.txt", "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	second := commit("b.txt", "second")
	pushed = second
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	mustGit(t, work, "reset", "-q", "--hard", "HEAD~1")
	rewritten := commit("c.txt", "rewritten")
	pushed = rewritten
	mustGit(t, work, "push", "-q", "--force", "origin", "HEAD:refs/heads/main")
	mustGit(t, work, "tag", "v1")
	pushed = ""
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/dev", "refs/tags/v1")
	mustGit(t, work, "push", "-q", "origin", ":refs/heads/dev")
	got, ok := s.Wait(repoA, events.KindPush, 5, 20*time.Second)
	if !ok {
		t.Fatalf("%d of 5 events delivered", len(got))
	}
	want := []map[string]bool{
		{"refs/heads/main": false}, {"refs/heads/main": false}, {"refs/heads/main": true},
		{"refs/heads/dev": false, "refs/tags/v1": false}, {"refs/heads/dev": false},
	}
	for i, dl := range got {
		var p events.Push
		if err := json.Unmarshal(dl.Body, &p); err != nil {
			t.Fatal(err)
		}
		flags := map[string]bool{}
		for _, u := range p.Updates {
			flags[u.Ref] = u.Forced
		}
		if fmt.Sprint(flags) != fmt.Sprint(want[i]) || p.Seq != uint64(i+1) || p.ID != events.PushID(repoA, p.Seq) || p.Pusher.Sub != "alice" {
			t.Errorf("event %d: %+v, want forced %v", i+1, p, want[i])
		}
		if i == 2 && (p.Updates[0].Before != second || p.Updates[0].After != rewritten) {
			t.Errorf("forced update %+v, want %s to %s", p.Updates[0], second, rewritten)
		}
	}
	if len(seen) != 5 || !seen[2]["refs/heads/main"] || seen[1]["refs/heads/main"] {
		t.Fatalf("flags before the verdict: %v", seen)
	}
	// A git that cannot answer reads as not forced and is logged.
	rp, release, err := n.cache.Acquire(ctx, repoA, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if forced := n.h.forcedUpdates(ctx, rp, []wal.RefUpdate{{Ref: "refs/heads/x", Old: strings.Repeat("1", 40), New: strings.Repeat("2", 40)}}, ""); len(forced) != 0 {
		t.Fatalf("unknown objects read as forced: %v", forced)
	}
}

// seenCompactor records the trigger of spec 006.
type seenCompactor struct {
	mu   sync.Mutex
	seen []uint64
}

func (c *seenCompactor) After(_ string, ix *wal.Index) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, ix.Seq)
}

// TestPushTriggersTheCompactionCheck: every acknowledged push hands the
// index it committed to the compaction trigger of spec 006, and a push
// git refuses hands over nothing.
func TestPushTriggersTheCompactionCheck(t *testing.T) {
	store := wal.NewMemStore()
	n := newNode(t, store)
	n.create(repoA, "acme", "app")
	c := &seenCompactor{}
	if h := New(Options{Cache: n.cache, Guard: n.h.guard, Compaction: c}); h.compact != c {
		t.Fatal("Options.Compaction not kept")
	}
	n.h.compact = c

	work := clone(t, n.url("/r/"+repoA+".git"))
	for i := range 2 {
		if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, work, "add", "a.txt")
		mustGit(t, work, "commit", "-q", "-m", "commit")
		mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	}
	c.mu.Lock()
	seen := append([]uint64(nil), c.seen...)
	c.mu.Unlock()
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("the trigger saw %v, want the sequence of each push", seen)
	}
	// A push the log refuses never reaches the trigger.
	stale := clone(t, n.url("/r/"+repoA+".git"))
	mustGit(t, work, "commit", "-q", "--allow-empty", "-m", "third")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if err := os.WriteFile(filepath.Join(stale, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, stale, "add", "b.txt")
	mustGit(t, stale, "commit", "-q", "-m", "racing")
	if out, err := git(t, stale, "push", "-q", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("a non-fast-forward push was accepted: %s", out)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) != 3 {
		t.Fatalf("the trigger saw %v after a refused push", c.seen)
	}
}
