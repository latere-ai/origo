// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/compact"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/test/stubs/source"
)

// sourceURL is the in-cluster address of the source stub, which the
// nodes reach through the pinned entry of ORIGO_EGRESS_ALLOW and trust
// through ORIGO_EGRESS_CA_BUNDLE (specs 013, 016).
const clusterSourceURL = "https://origo-stubs.origo.svc:8443/fixture.git"

// stubClient is an HTTP client that trusts the source stub's CA through
// the file up.sh wrote for the runner.
func stubClient(t *testing.T) *http.Client {
	t.Helper()
	caPEM, err := os.ReadFile(filepath.Join(repoRoot(t), "test", "e2e", "testdata", "stub-ca.pem"))
	if err != nil {
		t.Skipf("no stub-ca.pem for this cluster: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("stub-ca.pem holds no certificate")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

// importState reads GET /v1/repos/{id}/import off the stack.
func importState(t *testing.T, token, id string) api.ImportState {
	t.Helper()
	status, body := stackAPI(t, stackURL(), token, "GET", "/v1/repos/"+id+"/import", "")
	if status != 200 {
		t.Fatalf("import state: %d %v", status, body)
	}
	raw, _ := json.Marshal(body)
	var st api.ImportState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// repoStats reads GET /v1/repos/{id}/stats off the stack.
func repoStats(t *testing.T, token, id string) api.Stats {
	t.Helper()
	status, body := stackAPI(t, stackURL(), token, "GET", "/v1/repos/"+id+"/stats", "")
	if status != 200 {
		t.Fatalf("stats: %d %v", status, body)
	}
	raw, _ := json.Marshal(body)
	var st api.Stats
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestClusterImportFixture is spec 019's import criterion: the
// 5 000-commit fixture the source stub of spec 013 embeds, fetched by
// the stack from the in-cluster source over TLS, arrives as one compact
// entry inside the budget, import reports done with the source's
// reference count and the uploaded packs' bytes, every request the stub
// saw carried the bearer, and a clone from Origo has the same
// rev-list --all as a clone of the source.
func TestClusterImportFixture(t *testing.T) {
	requireNodes(t)
	client := stubClient(t)
	token := adminToken(t)
	id := newID(t)
	if status, body := stackAPI(t, stackURL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"migration","slug":"import-%s"}`, id, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	requireStack(t).cleanup(t, id)

	// The stub's request list is read afterwards, so it starts empty.
	clearStubRequests(t, client)

	body := fmt.Sprintf(`{"source":%q,"token":%q}`, clusterSourceURL, source.DefaultToken)
	status, out := stackAPI(t, stackURL(), token, "POST", "/v1/repos/"+id+"/import", body)
	if status != 202 || out["state"] != api.ImportRunning {
		t.Fatalf("import: %d %v", status, out)
	}
	deadline := time.Now().Add(10 * time.Minute)
	var st api.ImportState
	for {
		st = importState(t, token, id)
		if st.State != api.ImportRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the import did not finish within 10 minutes")
		}
		time.Sleep(5 * time.Second)
	}
	if st.State != api.ImportDone {
		t.Fatalf("import state: %+v", st)
	}

	// One compact entry with the packs, and size_bytes the entry's own
	// pack bytes, which is what import reports.
	ix := newestIndex(t, requireStack(t), id)
	if len(ix.Entries) != 1 || string(ix.Entries[0].Kind) != "compact" || len(ix.Packs) == 0 {
		t.Fatalf("the import is not one compact entry: %+v", ix.Entries)
	}
	if ix.SizeBytes != st.Bytes || st.Bytes == 0 {
		t.Fatalf("import reported %d bytes, the index holds %d", st.Bytes, ix.SizeBytes)
	}

	// A clone of the source through its host port is the reference.
	stubClone := filepath.Join(t.TempDir(), "source.git")
	mustGit(t, t.TempDir(), "-c", "http.sslCAInfo="+filepath.Join(repoRoot(t), "test", "e2e", "testdata", "stub-ca.pem"),
		"-c", "http.extraHeader=Authorization: Bearer "+source.DefaultToken,
		"clone", "-q", "--mirror", fmt.Sprintf("https://localhost:%d/fixture.git", portSource), stubClone)
	refs := strings.Count(strings.TrimSpace(mustGit(t, stubClone, "show-ref")), "\n") + 1
	if st.Refs != refs {
		t.Fatalf("import reported %d refs, the source has %d", st.Refs, refs)
	}

	origoClone := filepath.Join(t.TempDir(), "origo.git")
	mustGit(t, t.TempDir(), "clone", "-q", "--mirror", repoURL(portBalanced, token, id), origoClone)
	if got, want := gittest.RevList(t, origoClone), gittest.RevList(t, stubClone); got != want {
		t.Fatal("the imported history differs from the source's")
	}

	// Every request the stub saw carried the bearer, and none of them
	// names it.
	for _, r := range stubRequests(t, client) {
		if !r.Bearer {
			t.Fatalf("the stub saw a request without the bearer: %+v", r)
		}
	}
}

// clearStubRequests empties the source stub's request list through its
// host port of spec 013's ports table.
func clearStubRequests(t *testing.T, client *http.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("https://localhost:%d/requests", portSource), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("clearing the stub's request list: %v", err)
	}
	_ = resp.Body.Close()
}

// stubRequests reads the source stub's request list.
func stubRequests(t *testing.T, client *http.Client) []source.Request {
	t.Helper()
	status, body := httpGet(client, fmt.Sprintf("https://localhost:%d/requests", portSource))
	if status != 200 {
		t.Fatalf("the stub's request list: %d", status)
	}
	var out []source.Request
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the stub's request list: %v\n%s", err, body)
	}
	if len(out) == 0 {
		t.Fatal("the stub saw no request of the import")
	}
	return out
}

// TestClusterGcBoundsStorage is spec 019's storage criterion: after 500
// pushes a gc is refused with the repository limit, because the
// threshold compactions the pushes triggered count as compactions
// within the hour; once the run the last crossing scheduled finishes,
// size_bytes is within 10% of the pack size of a fresh mirror clone,
// and one run of the weekly sweep reports no orphan.
func TestClusterGcBoundsStorage(t *testing.T) {
	requireNodes(t)
	s := requireStack(t)
	token := adminToken(t)
	id := idPreferring(t, stackNodes[0])
	if status, body := stackAPI(t, stackURL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"administration","slug":"gc-%s"}`, id, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	s.cleanup(t, id)
	waitUntil(t, "node 1 named as the preferred node", placementWindow, func() bool {
		return preferHeader(t, portNode1, token, id) == stackNodes[0]
	})

	work := clone(t, repoURL(portNode1, token, id))
	pushCommits(t, work, portNode1, token, id, 500, 1<<10)

	// A gc within an hour of any compaction is refused with the
	// repository limit and a Retry-After.
	status, out := stackAPI(t, stackURL(), token, "POST", "/v1/repos/"+id+"/gc", "")
	if status != 429 || errorCode(out) != "rate_limited" {
		t.Fatalf("gc after 500 pushes: %d %v", status, out)
	}
	if d, _ := out["error"].(map[string]any)["details"].(map[string]any); d == nil || d["limit"] != limits.LimitRepository || d["retry_after"] == nil {
		t.Fatalf("gc refusal details: %v", out)
	}

	// The run the last threshold crossing scheduled bounds the log.
	ix := waitForCompaction(t, s, work, portNode1, token, id)
	st := repoStats(t, token, id)
	if st.EntriesSinceCompaction > compact.MaxEntries || st.CompactedAt == nil {
		t.Fatalf("stats after the compaction: %+v", st)
	}
	if st.SizeBytes != ix.SizeBytes || st.Packs != len(ix.Packs) {
		t.Fatalf("stats %+v does not match the index %d bytes, %d packs", st, ix.SizeBytes, len(ix.Packs))
	}

	// size_bytes is within 10% of the packs a fresh mirror clone holds.
	bare := filepath.Join(t.TempDir(), "fresh.git")
	mustGit(t, t.TempDir(), "clone", "-q", "--mirror", repoURL(portNode1, token, id), bare)
	mustGit(t, bare, "repack", "-a", "-d")
	fresh := packBytes(t, bare)
	if fresh == 0 {
		t.Fatal("the fresh clone holds no pack")
	}
	if diff := float64(st.SizeBytes-fresh) / float64(fresh); diff > 0.10 || diff < -0.10 {
		t.Fatalf("size_bytes %d is %.1f%% off the fresh clone's %d", st.SizeBytes, diff*100, fresh)
	}

	// One run of the weekly sweep over the stack's own bucket finds no
	// orphan: every object under the prefix is one an index, a marker,
	// or metadata names.
	sweeper := api.NewSweeper(api.SweeperOptions{Log: s.log, Node: "runner", Logger: slog.New(slog.DiscardHandler)})
	rep, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 0 {
		t.Fatalf("the sweep found %d orphans: %v", rep.Orphans, rep.Keys)
	}
	if rep.StorageBytes <= 0 {
		t.Fatalf("the sweep reports %d bytes under the prefix", rep.StorageBytes)
	}
}

// packBytes is the bytes of the .pack files under a bare repository.
func packBytes(t *testing.T, dir string) int64 {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}
