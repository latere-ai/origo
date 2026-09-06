// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

// Package e2e drives a built origod binary against MinIO with the real
// git client. `make test-integration` runs it; it skips without
// ORIGO_TEST_S3_ENDPOINT.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/wal"
)

const token = "e2e-token"

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

// origod builds the binary once per test process.
func origod(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "origo-e2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(dir, "origod")
		cmd := exec.CommandContext(context.Background(), "go", "build", "-o", binary, "../../cmd/origod")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binary != "" {
		_ = os.RemoveAll(filepath.Dir(binary))
	}
	os.Exit(code)
}

// stack is the bucket every node of a test shares.
type stack struct {
	endpoint, region, bucket, key, secret string
	pathStyle                             bool
	store                                 *wal.S3
	log                                   *wal.Log
}

func requireStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{
		endpoint: os.Getenv("ORIGO_TEST_S3_ENDPOINT"), region: os.Getenv("ORIGO_TEST_S3_REGION"),
		bucket: os.Getenv("ORIGO_TEST_S3_BUCKET"), key: os.Getenv("ORIGO_TEST_S3_KEY"),
		secret: os.Getenv("ORIGO_TEST_S3_SECRET"), pathStyle: os.Getenv("ORIGO_TEST_S3_PATH_STYLE") == "1",
	}
	if s.endpoint == "" {
		t.Skip("ORIGO_TEST_S3_ENDPOINT is not set")
	}
	store, err := wal.NewS3(wal.S3Options{
		Endpoint: s.endpoint, Region: s.region, Bucket: s.bucket, Key: s.key, Secret: s.secret, PathStyle: s.pathStyle,
		Client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	s.log = wal.New(wal.Options{Store: store})
	return s
}

// keys lists every key under a repository's prefix with the suffix.
func (s *stack) keys(t *testing.T, id, sub string) []string {
	t.Helper()
	var out []string
	after := ""
	for {
		res, err := s.store.List(context.Background(), wal.ListOptions{Prefix: s.log.RepoPrefix(id) + sub, StartAfter: after, Max: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range res.Objects {
			out = append(out, strings.TrimPrefix(o.Key, s.log.RepoPrefix(id)))
			after = o.Key
		}
		if !res.Truncated || len(res.Objects) == 0 {
			return out
		}
	}
}

// cleanup removes a repository's objects and name after the test.
func (s *stack) cleanup(t *testing.T, id string) {
	t.Cleanup(func() {
		ctx := context.Background()
		if m, err := s.log.ReadMeta(ctx, id); err == nil {
			_ = s.store.Delete(ctx, "origo/names/"+m.Owner+"/"+m.Slug)
		}
		for _, k := range s.keys(t, id, "") {
			_ = s.store.Delete(ctx, s.log.RepoPrefix(id)+k)
		}
	})
}

func newID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// node is one origod process.
type node struct {
	t        *testing.T
	s        *stack
	cmd      *exec.Cmd
	dataDir  string
	public   string
	internal string
	extra    map[string]string
	logs     *bytes.Buffer
	done     chan error
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startNode starts a process with its own data directory unless one is
// given, and waits for /readyz.
func startNode(t *testing.T, s *stack, dataDir string, extra map[string]string) *node {
	t.Helper()
	if dataDir == "" {
		dataDir = filepath.Join(t.TempDir(), "data")
	}
	n := &node{t: t, s: s, dataDir: dataDir, extra: extra, public: freePort(t), internal: freePort(t), logs: &bytes.Buffer{}}
	n.start()
	return n
}

func (n *node) start() {
	n.t.Helper()
	env := map[string]string{
		"ORIGO_S3_ENDPOINT": n.s.endpoint, "ORIGO_S3_REGION": n.s.region, "ORIGO_S3_BUCKET": n.s.bucket,
		"ORIGO_S3_KEY": n.s.key, "ORIGO_S3_SECRET": n.s.secret, "ORIGO_DATA_DIR": n.dataDir,
		"ORIGO_PUBLIC_URL": "http://" + n.public, "ORIGO_PUBLIC_ADDR": n.public,
		"ORIGO_INTERNAL_ADDR": n.internal, "ORIGO_GOSSIP_ADDR": "127.0.0.1:0", "ORIGO_DEV_TOKEN": token,
		"PATH": os.Getenv("PATH"),
	}
	if n.s.pathStyle {
		env["ORIGO_S3_PATH_STYLE"] = "1"
	}
	for k, v := range n.extra {
		env[k] = v
	}
	cmd := exec.CommandContext(context.Background(), origod(n.t))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = n.logs, n.logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		n.t.Fatal(err)
	}
	n.cmd = cmd
	n.done = make(chan error, 1)
	go func() { n.done <- cmd.Wait() }()
	n.t.Cleanup(func() { n.stop() })
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-n.done:
			n.t.Fatalf("origod exited: %v\n%s", err, n.logs.String())
		default:
		}
		if code, _ := n.get(n.internal, "/readyz"); code == 200 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	n.t.Fatalf("origod not ready\n%s", n.logs.String())
}

func (n *node) get(addr, path string) (int, []byte) {
	resp, err := http.Get("http://" + addr + path) //nolint:noctx // test helper
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// stop ends the process gracefully; kill ends it at once. Both are
// idempotent.
func (n *node) stop() {
	if n.cmd == nil || n.cmd.Process == nil {
		return
	}
	_ = n.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-n.done:
	case <-time.After(30 * time.Second):
		_ = n.cmd.Process.Kill()
		<-n.done
	}
	n.cmd = nil
}

func (n *node) kill() {
	if n.cmd == nil || n.cmd.Process == nil {
		return
	}
	_ = n.cmd.Process.Kill()
	<-n.done
	n.cmd = nil
}

// wait blocks until the process exits on its own.
func (n *node) wait() error {
	err := <-n.done
	n.cmd = nil
	return err
}

func (n *node) url(id string) string {
	return "http://x:" + token + "@" + n.public + "/r/" + id + ".git"
}

// api calls the repository API and returns the status and body.
func (n *node) api(method, path, body string) (int, map[string]any) {
	n.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, "http://"+n.public+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (n *node) createRepo(id, owner, slug string) {
	n.t.Helper()
	if status, out := n.api("POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, owner, slug)); status != 201 {
		n.t.Fatalf("create: %d %v", status, out)
	}
	n.s.cleanup(n.t, id)
}

// git runs the client git in dir.
func git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = gittest.Env(dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func clone(t *testing.T, url string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	mustGit(t, t.TempDir(), "clone", "-q", url, dir)
	return dir
}

func commitFile(t *testing.T, dir, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "--", name)
	mustGit(t, dir, "commit", "-q", "-m", message)
	return mustGit(t, dir, "rev-parse", "HEAD")
}
