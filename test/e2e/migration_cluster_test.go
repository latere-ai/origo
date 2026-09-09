// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/source"
)

// The migration of spec 014 against the stack of spec 013: the nodes
// fetch from the in-cluster source stub through the pinned entry of
// ORIGO_EGRESS_ALLOW, and the test drives the stub through its host
// port of the ports table.

// clusterSourceBase is the stub the nodes reach; clusterSourceURL is
// the fixture repository under it.
const clusterSourceBase = "https://origo-stubs.origo.svc:8443"

// stubPost calls one control endpoint of the source stub through its
// host port.
func stubPost(t *testing.T, client *http.Client, path, body string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	url := fmt.Sprintf("https://localhost:%d%s", portSource, path)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("POST %s: %d %s", path, resp.StatusCode, raw)
	}
	return raw
}

// smallSourceRepo puts a two-commit repository on the stub and answers
// the URL the nodes reach it under. The fixture the stub embeds is
// 5 000 commits, which spec 019's import criterion already carries;
// this one keeps a migration scenario inside a few seconds.
func smallSourceRepo(t *testing.T, client *http.Client, name string) string {
	t.Helper()
	src := gittest.NewSource(t)
	src.Commit("a.txt", "one", "first")
	src.Commit("b.txt", "two", "second")
	path := filepath.Join(t.TempDir(), "fixture.bundle")
	mustGit(t, src.Dir, "bundle", "create", path, "--all")
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"name": name, "bundle": base64.StdEncoding.EncodeToString(bundle)})
	if err != nil {
		t.Fatal(err)
	}
	stubPost(t, client, "/repos", string(body))
	return clusterSourceBase + "/" + name + ".git"
}

// importOnStack registers a repository, imports the source into it, and
// waits for the import to leave running.
func importOnStack(t *testing.T, token, slug, src string) string {
	t.Helper()
	id := newID(t)
	if status, body := stackAPI(t, stackURL(), token, "POST", "/v1/repos",
		fmt.Sprintf(`{"id":%q,"owner":"migration","slug":"%s-%s"}`, id, slug, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	requireStack(t).cleanup(t, id)
	body := fmt.Sprintf(`{"source":%q,"token":%q}`, src, source.DefaultToken)
	if status, out := stackAPI(t, stackURL(), token, "POST", "/v1/repos/"+id+"/import", body); status != 202 {
		t.Fatalf("import: %d %v", status, out)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st := importState(t, token, id)
		if st.State == api.ImportDone {
			return id
		}
		if st.State != api.ImportRunning {
			t.Fatalf("import of %s: %+v", id, st)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the import of %s did not finish within 5 minutes", id)
		}
		time.Sleep(2 * time.Second)
	}
}

// verifyOnStack calls verify through the balanced port.
func verifyOnStack(t *testing.T, token, id, src string) api.VerifyResult {
	t.Helper()
	body := fmt.Sprintf(`{"source":%q,"token":%q}`, src, source.DefaultToken)
	status, out := stackAPI(t, stackURL(), token, "POST", "/v1/repos/"+id+"/verify", body)
	if status != 200 {
		t.Fatalf("verify: %d %v", status, out)
	}
	raw, _ := json.Marshal(out)
	var res api.VerifyResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// TestClusterMigrationCatchesALateWrite is spec 014's stack criterion:
// a commit made on the source between the import and the verification
// is what verification is for, and after a fresh id and a second import
// the repository reaches mirrored.
func TestClusterMigrationCatchesALateWrite(t *testing.T) {
	requireNodes(t)
	client := stubClient(t)
	token := adminToken(t)
	name := "late-" + newID(t)[:8]
	src := smallSourceRepo(t, client, name)

	first := importOnStack(t, token, "late", src)
	if res := verifyOnStack(t, token, first, src); !res.Equal {
		t.Fatalf("the import did not mirror the source: %+v", res)
	}

	// The late write, through the stub's host port of the ports table.
	var commit struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(stubPost(t, client, "/commit", fmt.Sprintf(`{"repo":%q,"branch":"main"}`, name)), &commit); err != nil || commit.Commit == "" {
		t.Fatalf("the late write: %v", err)
	}

	res := verifyOnStack(t, token, first, src)
	if res.Equal || len(res.Refs.Differing) != 1 {
		t.Fatalf("the late write was not caught: %+v", res)
	}
	if d := res.Refs.Differing[0]; d.Name != "refs/heads/main" || d.Source != commit.Commit || d.Origo == "" || d.Origo == commit.Commit {
		t.Fatalf("differing: %+v, the source committed %s", d, commit.Commit)
	}

	// The one documented path after a difference: a fresh id, because
	// an import refuses a repository that already holds history.
	if status, out := stackAPI(t, stackURL(), token, "POST", "/v1/repos/"+first+"/import",
		fmt.Sprintf(`{"source":%q,"token":%q}`, src, source.DefaultToken)); status != 409 || errorCode(out) != "repo_not_empty" {
		t.Fatalf("a second import into the same id: %d %v", status, out)
	}
	second := importOnStack(t, token, "late-again", src)
	if res := verifyOnStack(t, token, second, src); !res.Equal || res.Objects.Origo == 0 {
		t.Fatalf("the second import is not mirrored: %+v", res)
	}
}

// TestClusterMigrationDocCommandsRun is spec 014's document criterion:
// the shell blocks of docs/migration.md, one repository and then one
// batch, run unchanged against the stack and end with every repository
// mirrored.
func TestClusterMigrationDocCommandsRun(t *testing.T) {
	requireNodes(t)
	// The stack answers, so the document is expected to run: a missing
	// tool is a failure and not a skip, because a skip here would leave
	// the criterion green and unproven.
	for _, bin := range []string{"jq", "curl", "uuidgen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("the document needs %s on PATH: %v", bin, err)
		}
	}
	client := stubClient(t)
	root := repoRoot(t)
	src := smallSourceRepo(t, client, "docs-"+newID(t)[:8])

	// The document runs origod migrate by name, so the binary this
	// checkout builds goes in front of PATH.
	binDir := filepath.Dir(origod(t))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, "tools", "docs", "run-blocks.sh"), "docs/migration.md")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ORIGO_TEST_URL="+stackURL(),
		"ORIGO_TEST_ADMIN_TOKEN="+adminToken(t),
		"ORIGO_TEST_SOURCE_URL="+src,
		"ORIGO_TEST_SOURCE_TOKEN="+source.DefaultToken,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run-blocks.sh docs/migration.md: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "the batch is mirrored") {
		t.Fatalf("the document did not reach the batch:\n%s", out.String())
	}
}
