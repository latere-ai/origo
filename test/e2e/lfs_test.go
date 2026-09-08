// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
)

// lfsObjectBytes is the file the round trip carries: large enough that
// it could not pass through the node unnoticed.
const lfsObjectBytes = 500 << 20

// nodeRequestBudget is what the node may receive for the whole round
// trip. The pack carries a pointer file of about 130 bytes, so
// everything the node sees is protocol.
const nodeRequestBudget = 1 << 20

// countingProxy is the reverse proxy the client uses as its remote: it
// forwards to ORIGO_TEST_URL and counts every request byte the node
// receives, so a transfer that went through the node is visible in the
// count. The presigned transfers go to the bucket directly and are not
// counted, which is the point of the measurement.
type countingProxy struct {
	srv *httptest.Server

	mu       sync.Mutex
	bytes    int64
	requests int
}

func newCountingProxy(t *testing.T, target string) *countingProxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := &countingProxy{}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			// The node builds the verify action's href from the Host it
			// was asked on, so keeping the client's Host is what keeps
			// the verify request inside the count.
			pr.Out.Host = pr.In.Host
		},
		FlushInterval: -1,
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counted := &countingReader{r: r.Body}
		r.Body = counted
		head := requestHeadBytes(r)
		rp.ServeHTTP(w, r)
		p.mu.Lock()
		p.bytes += head + counted.n
		p.requests++
		p.mu.Unlock()
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *countingProxy) URL() string { return p.srv.URL }

func (p *countingProxy) seen() (int64, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes, p.requests
}

// requestHeadBytes is the request line and the headers as they arrived
// on the wire, the Host header included: net/http parses it out of the
// map, so it is added back.
func requestHeadBytes(r *http.Request) int64 {
	n := len(r.Method) + 1 + len(r.RequestURI) + 1 + len(r.Proto) + 2
	n += len("Host: ") + len(r.Host) + 2
	for name, values := range r.Header {
		for _, v := range values {
			n += len(name) + 2 + len(v) + 2
		}
	}
	return int64(n + 2)
}

type countingReader struct {
	r io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

// requireLFSStack skips unless the stack answers and git-lfs is on PATH.
func requireLFSStack(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not on PATH")
	}
	if status, _ := httpGet(http.DefaultClient, stackURL()+"/version"); status != 200 {
		t.Skipf("nothing answers at %s", stackURL())
	}
}

// lfsGit runs the client git with extra environment on top of the
// suite's, so a test turns the smudge filter off for one command.
func lfsGit(t *testing.T, dir string, extra []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(dir), extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeLargeFile writes n bytes of a repeatable pattern and returns the
// content's SHA-256, which is also the LFS object id.
func writeLargeFile(t *testing.T, path string, n int64) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i * 31)
	}
	sum := sha256.New()
	w := io.MultiWriter(f, sum)
	for written := int64(0); written < n; {
		chunk := block
		if rest := n - written; rest < int64(len(chunk)) {
			chunk = chunk[:rest]
		}
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
		written += int64(len(chunk))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// TestSlowLFSRoundTripBypassesTheNode is spec 010's first criterion: a
// 500 MiB file goes to the stack's MinIO and comes back through
// git-lfs, while the node receives under 1 MiB of request bytes. Every
// request the client makes to Origo passes through a counting reverse
// proxy in front of ORIGO_TEST_URL, which is the client's remote, so
// the count is everything the node received; the presigned transfers
// address MinIO's host port directly and are not counted.
func TestSlowLFSRoundTripBypassesTheNode(t *testing.T) {
	requireLFSStack(t)
	proxy := newCountingProxy(t, stackURL())
	token := adminToken(t)
	id := newID(t)
	if status, body := stackAPI(t, proxy.URL(), token, "POST", "/v1/repos",
		fmt.Sprintf(`{"id":%q,"owner":"lfs","slug":"repo-%s"}`, id, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	t.Cleanup(func() {
		if status, body := stackAPI(t, proxy.URL(), token, "DELETE", "/v1/repos/"+id, ""); status != 202 {
			t.Logf("delete: %d %v", status, body)
		}
	})
	remote := strings.Replace(proxy.URL(), "http://", "http://x:"+token+"@", 1) + "/r/" + id + ".git"

	// The working copy: one tracked file of 500 MiB and its pointer.
	work := t.TempDir()
	lfsGit(t, work, nil, "init", "-q", "-b", "main")
	lfsGit(t, work, nil, "lfs", "install", "--local")
	lfsGit(t, work, nil, "lfs", "track", "*.bin")
	want := writeLargeFile(t, filepath.Join(work, "large.bin"), lfsObjectBytes)
	lfsGit(t, work, nil, "add", "--", ".gitattributes", "large.bin")
	lfsGit(t, work, nil, "commit", "-q", "-m", "the large file")
	lfsGit(t, work, nil, "remote", "add", "origin", remote)

	// The push carries the pointer; git lfs push sends the bytes to the
	// bucket over the presigned URL and completes them at verify.
	lfsGit(t, work, nil, "lfs", "push", "origin", "main")
	lfsGit(t, work, nil, "push", "-q", "origin", "main")
	afterPush, requests := proxy.seen()
	if afterPush >= nodeRequestBudget {
		t.Fatalf("the node received %d bytes over %d requests on the push, budget %d", afterPush, requests, nodeRequestBudget)
	}

	// The clone takes the pointer only; git lfs fetch brings the bytes
	// back from the bucket and checkout writes the file.
	dst := filepath.Join(t.TempDir(), "clone")
	lfsGit(t, t.TempDir(), []string{"GIT_LFS_SKIP_SMUDGE=1"}, "clone", "-q", remote, dst)
	lfsGit(t, dst, nil, "lfs", "install", "--local")
	lfsGit(t, dst, nil, "lfs", "fetch", "--all", "origin")
	lfsGit(t, dst, nil, "lfs", "checkout")

	got := filepath.Join(dst, "large.bin")
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != lfsObjectBytes {
		t.Fatalf("the fetched file is %d bytes, want %d", info.Size(), lfsObjectBytes)
	}
	if sum := sha256OfFile(t, got); sum != want {
		t.Fatalf("the fetched file hashes %s, want %s", sum, want)
	}

	total, requests := proxy.seen()
	if total >= nodeRequestBudget {
		t.Fatalf("the node received %d bytes over %d requests, budget %d", total, requests, nodeRequestBudget)
	}
	t.Logf("the node received %d bytes over %d requests for a %d byte object", total, requests, int64(lfsObjectBytes))
}
