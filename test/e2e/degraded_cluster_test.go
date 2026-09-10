// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/e2e/cluster"
	"github.com/latere-ai/origo/test/stubs/slowproxy"
)

// freshClient opens a new connection per request, so every request
// of the degraded scenario is routed the way a client's would be: a
// pooled connection survives a pod leaving the endpoint list, a fresh
// one does not.
var freshClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// gitGet fetches a smart HTTP path of a node with the bearer over a
// fresh connection and returns the status, the headers, and the body,
// 0 when nothing answered inside the timeout.
func gitGet(t *testing.T, base, token, path string, timeout time.Duration) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := freshClient.Do(req)
	if err != nil {
		return 0, nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// nodeMetric reads one series from a node's internal host port over a
// fresh connection. Every fault this file injects takes the node out of
// the endpoint list for a few seconds and puts it back, so nothing
// answering right now is a reason to wait, the same reason
// tryNodeMetric exists; a node that never returns fails the wait.
func nodeMetric(t *testing.T, internalPort int, name, labels string) float64 {
	t.Helper()
	var v float64
	waitUntil(t, fmt.Sprintf("node %d back on its internal host port", internalPort), time.Minute, func() bool {
		var ok bool
		v, ok = tryNodeMetric(t, internalPort, name, labels)
		return ok
	})
	return v
}

// tryNodeMetric is nodeMetric for a wait: a node out of rotation
// answers on neither host port, which is a reason to wait, not to
// fail.
func tryNodeMetric(t *testing.T, internalPort int, name, labels string) (float64, bool) {
	t.Helper()
	status, body := httpGet(freshClient, fmt.Sprintf("http://localhost:%d/metrics", internalPort))
	if status != 200 {
		return 0, false
	}
	return parseMetric(t, string(body), name, labels), true
}

// podConditions reports a pod's status conditions, for the log line
// of a failure that looks like a pod leaving the endpoint list.
func podConditions(t *testing.T, name string) string {
	t.Helper()
	var out []string
	for _, c := range conditions(t, name) {
		out = append(out, fmt.Sprintf("%s=%s %s", c.Type, c.Status, c.Reason))
	}
	return strings.Join(out, ", ")
}

// podCondition is one entry of a pod's status conditions.
type podCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func conditions(t *testing.T, name string) []podCondition {
	t.Helper()
	var pod struct {
		Status struct {
			Conditions []podCondition `json:"conditions"`
		} `json:"status"`
	}
	_ = json.Unmarshal(cluster.Get(t, "pod", name), &pod)
	return pod.Status.Conditions
}

// podReady reports whether a pod's Ready condition is true, which is
// what puts it back in every Service's endpoint list.
func podReady(t *testing.T, name string) bool {
	t.Helper()
	for _, c := range conditions(t, name) {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// inRotation reports whether a node is back in the rotation of both of
// its Services: its pod reports Ready and both host ports answer. One
// answered request is not that proof. kube-proxy withdraws a pod's
// endpoints after the kubelet flips the Ready condition, not with it,
// so a request served during that lag comes from a pod that is already
// leaving, and the next request on either port is refused. The unreachable
// scenario runs inside exactly that lag: the failing listings take node 1
// out of rotation a moment before the breaker opens and puts it back.
func inRotation(t *testing.T, pod string, public, internal int) bool {
	t.Helper()
	if !podReady(t, pod) {
		return false
	}
	if status, _ := httpGet(freshClient, fmt.Sprintf("http://localhost:%d/readyz", internal)); status != 200 {
		return false
	}
	status, _ := httpGet(freshClient, fmt.Sprintf("http://localhost:%d/version", public))
	return status == 200
}

// TestClusterDegradedStorage is spec 015's cluster criterion, through
// node 1 of spec 013's ports table with its counters read from that
// node's internal host port: the bucket unreachable under
// cut-storage.yaml, slow under the slow proxy's delay, and partial
// after a deletion through the MinIO host port. ORIGO_STALE_MAX is 30s
// and ORIGO_STORAGE_TIMEOUT 5s on the stack's nodes.
func TestClusterDegradedStorage(t *testing.T) {
	requireCluster(t)
	s := requireStack(t)
	ctx := context.Background()
	token := adminToken(t)
	testdata := filepath.Join(repoRoot(t), "test", "e2e", "testdata")
	node1 := fmt.Sprintf("http://localhost:%d", portNode1)
	node3 := fmt.Sprintf("http://localhost:%d", portNode1+2)
	node1Int, node3Int := portNode1Int, portNode1Int+2
	proxy := fmt.Sprintf("http://localhost:%d", portSlowProxy)
	if err := slowproxy.Set(ctx, proxy, 0); err != nil {
		t.Fatalf("the slow proxy's control endpoint: %v", err)
	}
	t.Cleanup(func() { _ = slowproxy.Set(context.Background(), proxy, 0) })
	nodeURL := func(base, id string) string {
		return strings.Replace(base, "http://", "http://x:"+token+"@", 1) + "/r/" + id + ".git"
	}
	create := func(base, id, slug string) {
		t.Helper()
		if status, body := stackAPI(t, base, token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"degraded","slug":%q}`, id, slug)); status != 201 {
			t.Fatalf("create %s: %d %v", slug, status, body)
		}
		s.cleanup(t, id)
	}

	// Every node loses the bucket under the cut and every node's
	// readiness listings count toward its breaker, so after a fault the
	// scenario waits until every node's read breaker is closed and its
	// readiness listing answers, and leaves the stack that way for the
	// tests after it; a closed breaker and a ready replica together
	// mean the bucket answered the listing. Nothing here materializes
	// a repository, so node 3 stays cold for the partial case.
	warm, cold := newID(t), newID(t)
	waitStackHealthy := func(t *testing.T) {
		t.Helper()
		waitUntil(t, "every node's read breaker closed and the bucket answering", 3*time.Minute, func() bool {
			for i := range 3 {
				if v, ok := tryNodeMetric(t, portNode1Int+i, "origo_storage_breaker_state", `class="read"`); !ok || v != 0 {
					return false
				}
				if status, _ := httpGet(freshClient, fmt.Sprintf("http://localhost:%d/readyz", portNode1Int+i)); status != 200 {
					return false
				}
			}
			return true
		})
	}
	t.Cleanup(func() {
		if !t.Failed() {
			waitStackHealthy(t)
		}
	})

	// Warm on node 1: a push and a clone through it, consistent.
	create(node1, warm, "warm-"+warm[:8])
	create(node1, cold, "cold-"+cold[:8])
	work := clone(t, nodeURL(node1, warm))
	c1 := commitFile(t, work, "a.txt", "one", "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if status, header, _ := gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second); status != 200 || header.Get("Origo-Stale") != "" {
		t.Fatalf("consistent advertisement: %d Origo-Stale %q", status, header.Get("Origo-Stale"))
	}
	commitFile(t, work, "b.txt", "two", "second")

	t.Run("unreachable", func(t *testing.T) {
		cluster.ApplyManifest(t, filepath.Join(testdata, "cut-storage.yaml"))
		// Bursts of six concurrent checks, each failing at the storage
		// deadline once the policy is enforced, open node 1's read
		// breaker; a burst before enforcement answers and counts
		// nothing, so the bursts repeat until the gauge reads open.
		waitUntil(t, "node 1's read breaker open", 2*time.Minute, func() bool {
			var wg sync.WaitGroup
			for range 6 {
				wg.Go(func() { gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", time.Minute) })
			}
			wg.Wait()
			v, ok := tryNodeMetric(t, node1Int, "origo_storage_breaker_state", `class="read"`)
			return ok && v == 1
		})
		t.Logf("breaker open; origod-0: %s", podConditions(t, "origod-0"))
		// Node 1 left the rotation between its second failed readiness
		// listing and the breaker opening; the next probe the breaker
		// refuses brings it back. The wait is for the return itself,
		// not for one answered request: inRotation reads the pod's Ready
		// condition and both host ports, because a request answered
		// inside kube-proxy's withdrawal lag proves neither, and every
		// assertion below reads node 1's counters on the internal port.
		// The return takes a kubelet probe period and the endpoint
		// programming after it, so the wait is a minute and not the 30
		// seconds one request took.
		var status int
		var header http.Header
		waitUntil(t, "node 1 back in rotation with the stale advertisement", time.Minute, func() bool {
			if !inRotation(t, "origod-0", portNode1, node1Int) {
				return false
			}
			status, header, _ = gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second)
			return status == 200
		})
		age, err := strconv.Atoi(header.Get("Origo-Stale"))
		if err != nil || age < 0 || age > 30 {
			t.Fatalf("stale advertisement: %d Origo-Stale %q; origod-0: %s", status, header.Get("Origo-Stale"), podConditions(t, "origod-0"))
		}
		stale := filepath.Join(t.TempDir(), "stale")
		if out, err := git(t, t.TempDir(), "clone", "-q", nodeURL(node1, warm), stale); err != nil {
			t.Fatalf("stale clone: %v\n%s\norigod-0: %s", err, out, podConditions(t, "origod-0"))
		}
		if mustGit(t, stale, "rev-parse", "HEAD") != c1 {
			t.Fatal("the stale clone is not the copy")
		}
		// A push is refused at info/refs with the ERR pkt-line, no pack
		// uploaded, and a cold repository answers 503 at once.
		var body []byte
		status, header, body = gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-receive-pack", 30*time.Second)
		if status != 200 || header.Get("Retry-After") == "" || !strings.Contains(string(body), "ERR storage_unavailable: ") {
			t.Fatalf("push advertisement: %d Retry-After %q %q", status, header.Get("Retry-After"), body)
		}
		out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
		if err == nil || !strings.Contains(out, "remote error: storage_unavailable: ") {
			t.Fatalf("push under the cut: %v\n%s", err, out)
		}
		started := time.Now()
		status, _, body = gitGet(t, node1, token, "/r/"+cold+".git/info/refs?service=git-upload-pack", 30*time.Second)
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &env)
		if took := time.Since(started); status != 503 || env.Error.Code != "storage_unavailable" || env.Error.Details["error"] != "breaker open" || took > 3*time.Second {
			t.Fatalf("cold repository: %d %s after %s", status, body, took)
		}
		if nodeMetric(t, node1Int, "origo_stale_responses_total", "") < 1 {
			t.Fatal("no stale response counted on node 1")
		}
		// Past ORIGO_STALE_MAX the warm copy is refused too.
		waitUntil(t, "503 for the warm repository after the stale bound", time.Minute, func() bool {
			status, _, _ := gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second)
			return status == 503
		})
	})
	// The policy is gone: the next probe after the window closes the
	// breaker and the first consistent response carries no header.
	waitUntil(t, "node 1 consistent again", 3*time.Minute, func() bool {
		status, header, _ := gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second)
		return status == 200 && header.Get("Origo-Stale") == ""
	})
	waitStackHealthy(t)
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")

	t.Run("slow", func(t *testing.T) {
		// Every connection's first bytes are held past the storage
		// deadline: the check fails after the deadline, one call and one
		// failure, the breaker stays closed below its threshold.
		before := nodeMetric(t, node1Int, "origo_storage_ops_total", `op="head",result="error"`)
		if err := slowproxy.Set(ctx, proxy, 8*time.Second); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = slowproxy.Set(context.Background(), proxy, 0) })
		started := time.Now()
		status, _, body := gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second)
		took := time.Since(started)
		if status != 503 || !strings.Contains(string(body), `"storage_unavailable"`) || took < 5*time.Second || took > 25*time.Second {
			t.Fatalf("slow bucket: %d %s after %s", status, body, took)
		}
		if after := nodeMetric(t, node1Int, "origo_storage_ops_total", `op="head",result="error"`); after != before+1 {
			t.Fatalf("head errors went from %v to %v for one slow check", before, after)
		}
		if nodeMetric(t, node1Int, "origo_storage_breaker_state", `class="read"`) != 0 {
			t.Fatal("one slow check opened the breaker")
		}
		if err := slowproxy.Set(ctx, proxy, 0); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "node 1 answering after the delay is cleared", time.Minute, func() bool {
			status, header, _ := gitGet(t, node1, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second)
			return status == 200 && header.Get("Origo-Stale") == ""
		})
		waitStackHealthy(t)
	})

	t.Run("partial", func(t *testing.T) {
		// The newest entry deleted through the MinIO host port: node 3,
		// cold for the repository, refuses it with the key and counts
		// one integrity error; another repository is served; the entry
		// restored, the next request materializes again.
		ix, _, err := s.log.Newest(ctx, warm, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		key := s.log.RepoPrefix(warm) + ix.Entry
		rc, _, err := s.store.Get(ctx, key, "")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		if err := s.store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		before := nodeMetric(t, node3Int, "origo_log_integrity_errors_total", "")
		status, _, body := gitGet(t, node3, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second)
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &env)
		if status != 503 || env.Error.Code != "repository_unavailable" || env.Error.Details["key"] != key {
			t.Fatalf("missing entry on node 3: %d %s", status, body)
		}
		if after := nodeMetric(t, node3Int, "origo_log_integrity_errors_total", ""); after != before+1 {
			t.Fatalf("integrity errors went from %v to %v", before, after)
		}
		other := newID(t)
		create(node3, other, "other-"+other[:8])
		if status, _, _ := gitGet(t, node3, token, "/r/"+other+".git/info/refs?service=git-upload-pack", 30*time.Second); status != 200 {
			t.Fatalf("the other repository on node 3: %d", status)
		}
		if _, err := s.store.Put(ctx, key, wal.BytesBody(data)); err != nil {
			t.Fatal(err)
		}
		if status, header, _ := gitGet(t, node3, token, "/r/"+warm+".git/info/refs?service=git-upload-pack", 30*time.Second); status != 200 || header.Get("Origo-Stale") != "" {
			t.Fatalf("after the restore on node 3: %d", status)
		}
		restored := clone(t, nodeURL(node3, warm))
		if mustGit(t, restored, "rev-list", "--count", "HEAD") != "2" {
			t.Fatal("the restored repository lacks the second push")
		}
	})
}
