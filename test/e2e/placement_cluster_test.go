// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/e2e/cluster"
)

// nodePorts maps a node name of the stack to its public and internal
// host ports of spec 013's ports table: origod-0 is node 1.
func nodePorts(t *testing.T, name string) (public, internal int) {
	t.Helper()
	var i int
	if _, err := fmt.Sscanf(name, "origod-%d", &i); err != nil || i < 0 || i > 2 {
		t.Fatalf("node %q is not one of the ports table", name)
	}
	return portNode1 + i, portNode1Int + i
}

// stackMetric reads one series from a node's internal host port.
func stackMetric(t *testing.T, internalPort int, name, labels string) float64 {
	t.Helper()
	status, body := httpGet(http.DefaultClient, fmt.Sprintf("http://localhost:%d/metrics", internalPort))
	if status != 200 {
		t.Fatalf("metrics on %d: %d", internalPort, status)
	}
	return parseMetric(t, string(body), name, labels)
}

// stackURL of a repository through one port, with the token as git's
// basic auth password.
func repoURL(port int, token, id string) string {
	return fmt.Sprintf("http://x:%s@localhost:%d/r/%s.git", token, port, id)
}

// createOnStack creates a repository through the balanced port and
// returns its id.
func createOnStack(t *testing.T, token, slug string) string {
	t.Helper()
	id := newID(t)
	if status, body := stackAPI(t, stackURL(), token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"placement","slug":"%s-%s"}`, id, slug, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	return id
}

// preferHeader reads Origo-Prefer from GET /v1/repos/{id} through one
// public port.
func preferHeader(t *testing.T, port int, token, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d/v1/repos/%s", port, id), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /v1/repos/%s on %d: %d", id, port, resp.StatusCode)
	}
	return resp.Header.Get("Origo-Prefer")
}

// TestClusterPreferredNodeIsWarm is spec 005's placement on the stack:
// with 3 nodes and replicas = 1, a clone through each of nodes 1, 2,
// and 3 answers Origo-Prefer with the same first name, and a second
// clone sent to that node's own public host port leaves its
// origo_repo_materialized_total unchanged.
func TestClusterPreferredNodeIsWarm(t *testing.T) {
	requireCluster(t)
	token := adminToken(t)
	id := createOnStack(t, token, "warm")
	work := clone(t, repoURL(portBalanced, token, id))
	commitFile(t, work, "a.txt", "one", "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")

	var first string
	for i := range 3 {
		port := portNode1 + i
		clone(t, repoURL(port, token, id))
		prefer := preferHeader(t, port, token, id)
		name := strings.Split(prefer, ",")[0]
		t.Logf("node %d answers Origo-Prefer: %s", i+1, prefer)
		if i == 0 {
			first = name
		} else if name != first {
			t.Fatalf("node %d prefers %s, node 1 prefers %s", i+1, name, first)
		}
		if strings.Count(prefer, ",") != 0 {
			t.Fatalf("replicas = 1 but the header names %q", prefer)
		}
	}
	public, internal := nodePorts(t, first)
	materialized := stackMetric(t, internal, "origo_repo_materialized_total", "")
	clone(t, repoURL(public, token, id))
	if got := stackMetric(t, internal, "origo_repo_materialized_total", ""); got != materialized {
		t.Fatalf("the preferred node %s materialized again: %v -> %v", first, materialized, got)
	}
}

// cloneLoad runs clones through the balanced port at up to perSecond
// per second, with at most inFlight at once, until stop is closed, and
// returns the counts of clones and failures with the first failure's
// output.
func cloneLoad(t *testing.T, url string, perSecond, inFlight int, stop <-chan struct{}) (clones, failures int64, firstFailure string) {
	t.Helper()
	var (
		wg      sync.WaitGroup
		ok, bad atomic.Int64
		mu      sync.Mutex
		first   string
		slots   = make(chan struct{}, inFlight)
	)
	root := t.TempDir()
	ticker := time.NewTicker(time.Second / time.Duration(perSecond))
	defer ticker.Stop()
	var n int
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-ticker.C:
		}
		select {
		case slots <- struct{}{}:
		default:
			continue // every slot busy: the runner is the bound, not the design
		}
		n++
		dir := filepath.Join(root, fmt.Sprintf("c%d", n))
		wg.Go(func() {
			defer func() { <-slots }()
			out, err := git(t, root, "clone", "-q", "--bare", url, dir)
			_ = os.RemoveAll(dir)
			if err != nil {
				bad.Add(1)
				mu.Lock()
				if first == "" {
					first = out
				}
				mu.Unlock()
				return
			}
			ok.Add(1)
		})
	}
	wg.Wait()
	return ok.Load(), bad.Load(), first
}

// TestClusterNodeRemovalUnderReadLoad is spec 005's node removal:
// deleting node 2 of the ports table with cluster.DeletePod during a
// load of 50 clones per second through the balanced port causes no
// failed request, and the replacement pod is ready before the test
// ends.
func TestClusterNodeRemovalUnderReadLoad(t *testing.T) {
	requireCluster(t)
	token := adminToken(t)
	id := createOnStack(t, token, "removal")
	work := clone(t, repoURL(portBalanced, token, id))
	commitFile(t, work, "a.txt", "one", "first")
	mustGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	// Every node holds the copy before the load starts.
	for i := range 3 {
		clone(t, repoURL(portNode1+i, token, id))
	}
	stop := make(chan struct{})
	var clones, failures int64
	var first string
	var load sync.WaitGroup
	started := time.Now()
	load.Go(func() { clones, failures, first = cloneLoad(t, repoURL(portBalanced, token, id), 50, 16, stop) })
	time.Sleep(5 * time.Second)
	cluster.DeletePod(t, "origod-1")
	time.Sleep(5 * time.Second)
	close(stop)
	load.Wait()
	elapsed := time.Since(started)
	t.Logf("MEASURE node removal: %d clones in %s (%.1f/s), %d failed", clones, elapsed.Round(time.Second), float64(clones)/elapsed.Seconds(), failures)
	if failures != 0 {
		t.Fatalf("%d clones failed during the removal; the first:\n%s", failures, first)
	}
	if _, ready := statefulSetReplicas(t, "origod"); ready != 3 {
		t.Fatalf("%d pods ready after the replacement", ready)
	}
}

// tenMiBRepo creates a repository of about 10 MiB through the balanced
// port and returns its id.
func tenMiBRepo(t *testing.T, token string) string {
	t.Helper()
	id := createOnStack(t, token, "load")
	src := gittest.NewSource(t)
	for i := range 10 {
		src.Write(fmt.Sprintf("blob-%d.bin", i), gittest.Bytes(1<<20, uint64(i+7)))
	}
	src.CommitAll("ten mebibytes")
	mustGit(t, src.Dir, "push", "-q", repoURL(portBalanced, token, id), "HEAD:refs/heads/main")
	return id
}

// setReplicas applies the hpa-<n>.yaml fixture and waits until the
// autoscaler and the StatefulSet report n ready replicas.
func setReplicas(t *testing.T, n int) {
	t.Helper()
	cluster.ApplyManifest(t, filepath.Join(repoRoot(t), "test", "e2e", "testdata", fmt.Sprintf("hpa-%d.yaml", n)))
	waitUntil(t, fmt.Sprintf("%d replicas", n), 5*time.Minute, func() bool {
		current, desired := cluster.HPAStatus(t, "origod")
		spec, ready := statefulSetReplicas(t, "origod")
		return current == n && desired == n && spec == n && ready == n
	})
}

// readLoadAt runs the synthetic read load of spec 005 at the replica
// count: 200 clones of the 10 MiB repository through the balanced port
// with a push every second beside them. It fails on any failed clone
// or push and returns the clones per second.
func readLoadAt(t *testing.T, token, id string, replicas int) float64 {
	t.Helper()
	setReplicas(t, replicas)
	pusher := clone(t, repoURL(portBalanced, token, id))
	stop := make(chan struct{})
	var pushes, failedPushes int
	var pushing sync.WaitGroup
	pushing.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			commitFile(t, pusher, "beat.txt", fmt.Sprintf("%d", i), fmt.Sprintf("beat %d", i))
			if out, err := git(t, pusher, "push", "-q", "origin", "HEAD:refs/heads/main"); err != nil {
				failedPushes++
				t.Errorf("push %d at %d replicas: %v\n%s", i, replicas, err, out)
			}
			pushes++
		}
	})
	const clones = 200
	root := t.TempDir()
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	var failed atomic.Int64
	started := time.Now()
	for i := range clones {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			dir := filepath.Join(root, fmt.Sprintf("c%d", i))
			if out, err := git(t, root, "clone", "-q", "--bare", repoURL(portBalanced, token, id), dir); err != nil {
				failed.Add(1)
				t.Errorf("clone %d at %d replicas: %v\n%s", i, replicas, err, out)
			}
			_ = os.RemoveAll(dir)
		})
	}
	wg.Wait()
	elapsed := time.Since(started)
	close(stop)
	pushing.Wait()
	rate := clones / elapsed.Seconds()
	t.Logf("MEASURE %d replicas: %d clones of 10 MiB in %s = %.2f clones/s, %d pushes beside them, %d failed", replicas, clones, elapsed.Round(time.Millisecond), rate, pushes, failed.Load()+int64(failedPushes))
	return rate
}

// TestSlowReplicasScaleReads is spec 005's read scaling: 200 clones of
// a 10 MiB repository with a push every second beside them, at 2, 4,
// and 8 replicas set with hpa-<n>.yaml; no push and no clone fails,
// the three clones-per-second figures are recorded, and nothing about
// them is asserted, because the runner's CPU bounds them. The overlay's
// autoscaler is restored at the end.
func TestSlowReplicasScaleReads(t *testing.T) {
	requireCluster(t)
	overlay := renderedOverlay(t)
	t.Cleanup(func() {
		cluster.Apply(t, overlay)
		waitUntil(t, "three replicas restored", 5*time.Minute, func() bool {
			spec, ready := statefulSetReplicas(t, "origod")
			return spec == 3 && ready == 3
		})
	})
	token := adminToken(t)
	id := tenMiBRepo(t, token)
	for _, n := range []int{2, 4, 8} {
		readLoadAt(t, token, id, n)
	}
}

// hpaUtilization reads the autoscaler's current CPU utilization, -1
// when it reports none yet.
func hpaUtilization(t *testing.T) int {
	t.Helper()
	var hpa struct {
		Status struct {
			CurrentMetrics []struct {
				Resource struct {
					Current struct {
						AverageUtilization *int `json:"averageUtilization"`
					} `json:"current"`
				} `json:"resource"`
			} `json:"currentMetrics"`
		} `json:"status"`
	}
	if err := json.Unmarshal(cluster.Get(t, "hpa", "origod"), &hpa); err != nil {
		t.Fatal(err)
	}
	for _, m := range hpa.Status.CurrentMetrics {
		if m.Resource.Current.AverageUtilization != nil {
			return *m.Resource.Current.AverageUtilization
		}
	}
	return -1
}

// TestSlowAutoscalerScalesUp is spec 005's autoscaler: with the
// autoscaler free to scale on CPU, a clone load drives the utilization
// past the 70% target and cluster.HPAStatus reports 4 replicas within
// 60 seconds of the crossing. Scale-down is not asserted; the overlay's
// autoscaler, held at three, is restored at the end.
func TestSlowAutoscalerScalesUp(t *testing.T) {
	requireCluster(t)
	overlay := renderedOverlay(t)
	cluster.Apply(t, overlay)
	t.Cleanup(func() {
		cluster.Apply(t, overlay)
		waitUntil(t, "three replicas restored", 5*time.Minute, func() bool {
			spec, ready := statefulSetReplicas(t, "origod")
			return spec == 3 && ready == 3
		})
	})
	token := adminToken(t)
	id := tenMiBRepo(t, token)
	cluster.ApplyManifest(t, filepath.Join(repoRoot(t), "test", "e2e", "testdata", "hpa-scale.yaml"))
	stop := make(chan struct{})
	var load sync.WaitGroup
	var clones, failures int64
	load.Go(func() { clones, failures, _ = cloneLoad(t, repoURL(portBalanced, token, id), 50, 24, stop) })
	var crossed time.Time
	started := time.Now()
	for {
		utilization := hpaUtilization(t)
		current, desired := cluster.HPAStatus(t, "origod")
		if crossed.IsZero() && utilization > 70 {
			crossed = time.Now()
			t.Logf("utilization %d%% crossed the target %s after the load started", utilization, crossed.Sub(started).Round(time.Second))
		}
		if desired >= 4 && current >= 4 {
			t.Logf("MEASURE autoscaler: %d replicas %s after the crossing, %s after the load started", current, time.Since(crossed).Round(time.Second), time.Since(started).Round(time.Second))
			break
		}
		if !crossed.IsZero() && time.Since(crossed) > 60*time.Second {
			close(stop)
			load.Wait()
			t.Fatalf("utilization crossed the target at %s but the autoscaler reports %d/%d replicas 60 seconds later", crossed.Format(time.TimeOnly), current, desired)
		}
		if time.Since(started) > 6*time.Minute {
			close(stop)
			load.Wait()
			t.Fatalf("utilization %d%% never crossed the target within 6 minutes (%d clones, %d failed)", utilization, clones, failures)
		}
		time.Sleep(2 * time.Second)
	}
	close(stop)
	load.Wait()
	t.Logf("%d clones drove the load, %d failed", clones, failures)
}
