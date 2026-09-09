// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package conformance_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/e2e/cluster"
)

// stackFault is spec 021's Fault on the kind stack: the bucket cut by
// the NetworkPolicy of spec 015's cluster scenario, applied with
// cluster.ApplyManifest and removed by its cleanup, and an object
// deleted through the MinIO host port with the ORIGO_TEST_S3_ENDPOINT
// family the job exported. It is the one file of the package that
// imports test/e2e/cluster, under the e2e tag.
type stackFault struct {
	url   string
	store *wal.S3
	root  string
}

// newStackFault builds the fault when the bucket family is exported
// and kubectl is on PATH, and answers nil otherwise, so a run without
// them skips the two rows and reports them.
func newStackFault(t *testing.T, url string) *stackFault {
	t.Helper()
	endpoint := os.Getenv("ORIGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return nil
	}
	store, err := wal.NewS3(wal.S3Options{
		Endpoint: endpoint, Region: os.Getenv("ORIGO_TEST_S3_REGION"), Bucket: os.Getenv("ORIGO_TEST_S3_BUCKET"),
		Key: os.Getenv("ORIGO_TEST_S3_KEY"), Secret: os.Getenv("ORIGO_TEST_S3_SECRET"), PathStyle: os.Getenv("ORIGO_TEST_S3_PATH_STYLE") == "1",
		Client: &http.Client{Transport: &http.Transport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return &stackFault{url: url, store: store, root: filepath.Dir(filepath.Dir(filepath.Dir(file)))}
}

// CutStorage applies cut-storage.yaml for the test and, once the
// policy is removed with the test, waits until every node's breakers
// are closed and its readiness answers, so the cases after it meet a
// healthy stack.
func (f *stackFault) CutStorage(t testing.TB) {
	t.Helper()
	t.Cleanup(func() { f.waitHealthy(t) })
	cluster.ApplyManifest(t, filepath.Join(f.root, "test", "e2e", "testdata", "cut-storage.yaml"))
}

// waitHealthy polls the three nodes' internal host ports of spec 013's
// ports table until both breakers read closed and readiness answers.
func (f *stackFault) waitHealthy(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		healthy := true
		for i := range 3 {
			base := fmt.Sprintf("http://localhost:%d", 30190+i)
			status, body := fetch(base + "/metrics")
			ready, _ := fetch(base + "/readyz")
			if status != 200 || ready != 200 || !strings.Contains(body, `origo_storage_breaker_state{class="read"} 0`) || !strings.Contains(body, `origo_storage_breaker_state{class="write"} 0`) {
				healthy = false
			}
		}
		if healthy {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("the stack did not recover from the cut within 3 minutes")
}

// fetch reads a URL over a fresh connection.
func fetch(url string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

// DeleteObject deletes the one object under the prefix through the
// MinIO host port and reports its key.
func (f *stackFault) DeleteObject(t testing.TB, prefix string) string {
	t.Helper()
	res, err := f.store.List(context.Background(), wal.ListOptions{Prefix: prefix, Max: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("%d objects under %s, want one", len(res.Objects), prefix)
	}
	key := res.Objects[0].Key
	if err := f.store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return key
}
