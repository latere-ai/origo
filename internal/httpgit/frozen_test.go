// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/wal"
)

// TestFrozenRepositoryRefusesAtInfoRefs is spec 019's push criterion: a
// push to a frozen repository is refused at info/refs with the ERR
// pkt-line before any pack is sent, a git-receive-pack sent without the
// advertisement is refused by the hook's verdict with the same
// sentence, the push path reads meta once for the two requests, and a
// clone succeeds throughout.
func TestFrozenRepositoryRefusesAtInfoRefs(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store := wal.NewMemStore()
	var metaReads atomic.Int64
	n := newNode(t, store, withNow(func() time.Time { return now }))
	n.create(repoA, "acme", "app")
	metaKey := n.log.RepoPrefix(repoA) + "meta"
	store.SetFault(func(op, key string) error {
		if op == "Get" && key == metaKey {
			metaReads.Add(1)
		}
		return nil
	})

	work := clone(t, n.url("/r/"+repoA+".git"))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "a.txt")
	mustGit(t, work, "commit", "-q", "-m", "first")

	// One whole push, the advertisement and the upload, reads meta once.
	metaReads.Store(0)
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if got := metaReads.Load(); got != 1 {
		t.Fatalf("%d meta reads for one push, want 1", got)
	}
	base := mustNewest(t, n.log, repoA)

	// The freeze lands after the cached meta of the last push expires.
	now = now.Add(2 * MetaTTL)
	freeze(t, n.log, repoA, now)

	// The push is refused at the advertisement: git prints the remote
	// error and sends no pack, so the log does not move.
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "b.txt")
	mustGit(t, work, "commit", "-q", "-m", "second")
	sentence := contract.CodeRepoFrozen + ": " + contract.Sentence(contract.CodeRepoFrozen)
	out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("the push to a frozen repository succeeded: %s", out)
	}
	if !strings.Contains(out, "remote error: "+sentence) {
		t.Fatalf("push output %q", out)
	}
	if ix := mustNewest(t, n.log, repoA); ix.Seq != base.Seq {
		t.Fatalf("the log moved to %d", ix.Seq)
	}

	// The advertisement itself is the shape of spec 015's refusal: 200,
	// the advertisement content type, and one ERR pkt-line.
	resp, err := n.srv.Client().Get(n.url("/r/" + repoA + ".git/info/refs?service=git-receive-pack"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-git-receive-pack-advertisement" {
		t.Fatalf("advertisement: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), "ERR "+sentence) {
		t.Fatalf("advertisement body %q", body)
	}

	// A clone succeeds throughout: a freeze refuses writes only.
	second := clone(t, n.url("/r/"+repoA+".git"))
	if strings.TrimSpace(mustGit(t, second, "rev-parse", "HEAD")) != base.Refs["refs/heads/main"] {
		t.Fatal("the clone of a frozen repository is not the frozen history")
	}

	// A git-receive-pack sent without the advertisement is refused by
	// the hook's verdict with the same sentence.
	head := strings.TrimSpace(mustGit(t, work, "rev-parse", "HEAD"))
	src := &gittestSource{dir: work, t: t}
	pack := src.thinPack(head, base.Refs["refs/heads/main"])
	raw := receiveBody(t, "report-status side-band-64k", nil, string(pack), base.Refs["refs/heads/main"]+" "+head+" refs/heads/main")
	now = now.Add(2 * MetaTTL)
	status, answer := postReceive(t, n, raw)
	if status != 200 || !strings.Contains(answer, sentence) || !strings.Contains(answer, "pre-receive hook declined") {
		t.Fatalf("receive-pack without the advertisement: %d %q", status, answer)
	}
	if ix := mustNewest(t, n.log, repoA); ix.Seq != base.Seq {
		t.Fatalf("the verdict let the log move to %d", ix.Seq)
	}

	// An import holds the repository the same way, with the 409 its row
	// states rather than the pkt-line, because a consumer driving a
	// migration reads the code.
	now = now.Add(2 * MetaTTL)
	unfreeze(t, n.log, repoA)
	resp, err = n.srv.Client().Get(n.url("/r/" + repoA + ".git/info/refs?service=git-receive-pack"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("advertisement after the unfreeze: %d", resp.StatusCode)
	}
	now = now.Add(2 * MetaTTL)
	importing(t, n.log, repoA, now)
	resp, err = n.srv.Client().Get(n.url("/r/" + repoA + ".git/info/refs?service=git-receive-pack"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 409 || !strings.Contains(string(body), contract.CodeRepoImporting) {
		t.Fatalf("advertisement during an import: %d %q", resp.StatusCode, body)
	}
	now = now.Add(2 * MetaTTL)
	status, answer = postReceive(t, n, raw)
	if status != 200 || !strings.Contains(answer, contract.CodeRepoImporting+": "+contract.Sentence(contract.CodeRepoImporting)) {
		t.Fatalf("receive-pack during an import: %d %q", status, answer)
	}
	// A fetch is served throughout an import too.
	if s, _ := getStatus(t, n, "/r/"+repoA+".git/info/refs?service=git-upload-pack"); s != 200 {
		t.Fatalf("fetch during an import: %d", s)
	}
}

func mustNewest(t *testing.T, l *wal.Log, id string) *wal.Index {
	t.Helper()
	ix, _, err := l.Newest(context.Background(), id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func writeMeta(t *testing.T, l *wal.Log, id string, change func(*wal.Meta)) {
	t.Helper()
	m, err := l.ReadMeta(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	change(m)
	if err := l.WriteMeta(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func freeze(t *testing.T, l *wal.Log, id string, at time.Time) {
	t.Helper()
	writeMeta(t, l, id, func(m *wal.Meta) { m.FrozenAt = &at })
}

func unfreeze(t *testing.T, l *wal.Log, id string) {
	t.Helper()
	writeMeta(t, l, id, func(m *wal.Meta) { m.FrozenAt = nil })
}

func importing(t *testing.T, l *wal.Log, id string, at time.Time) {
	t.Helper()
	writeMeta(t, l, id, func(m *wal.Meta) { m.ImportingSince, m.ImportNode = &at, "n1" })
}

// postReceive sends a raw git-receive-pack body, the request a client
// makes when it skips the advertisement.
func postReceive(t *testing.T, n *node, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", n.url("/r/"+repoA+".git/git-receive-pack"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := n.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(out)
}

func getStatus(t *testing.T, n *node, path string) (int, string) {
	t.Helper()
	resp, err := n.srv.Client().Get(n.url(path))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(out)
}
