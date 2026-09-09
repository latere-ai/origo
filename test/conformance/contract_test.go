// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package conformance_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/conformance"
	"github.com/latere-ai/origo/test/stubs/origo"
	"github.com/latere-ai/origo/test/stubs/source"
)

// The ports table of spec 013 at offset 0, the defaults of the stack
// run, so spec 013's e2e job sets nothing.
const (
	portBalanced   = 30080
	portIssuer     = 30081
	portAuthorizer = 30082
	portSink       = 30083
	portMinIO      = 30900
	// clusterSource is the source stub inside the cluster, the address
	// the nodes reach through ORIGO_EGRESS_ALLOW.
	clusterSource = "https://origo-stubs.origo.svc:8443/fixture.git"
)

// stackURL is ORIGO_TEST_URL, the balanced host port by default.
func stackURL() string {
	if u := os.Getenv("ORIGO_TEST_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return fmt.Sprintf("http://localhost:%d", portBalanced)
}

// answers reports whether something answers /version at the base URL.
func answers(base string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/version", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// mintAt mints a token at the stub issuer's host port for the subject.
func mintAt(t *testing.T, issuer, sub string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", issuer+"/mint", strings.NewReader(`{"sub":"`+sub+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint at %s: %v", issuer, err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("mint at %s: %d %v", issuer, resp.StatusCode, err)
	}
	return out.Token
}

// stackTarget is the stack run's target: ORIGO_TEST_URL and
// ORIGO_TEST_ADMIN_TOKEN with the ports table's defaults, the three
// stub control endpoints at their host ports, the in-cluster source,
// and the Fault over kubectl and the MinIO host port when the job
// exported the bucket family and kubectl is on PATH.
func stackTarget(t *testing.T) conformance.Target {
	t.Helper()
	url := stackURL()
	if !answers(url) {
		t.Skipf("nothing answers at ORIGO_TEST_URL (%s)", url)
	}
	token := os.Getenv("ORIGO_TEST_ADMIN_TOKEN")
	if token == "" {
		token = mintAt(t, fmt.Sprintf("http://localhost:%d", portIssuer), "conformance-"+strings.ToLower(t.Name()))
	}
	target := conformance.Target{
		URL: url, Token: token,
		Issuer:     fmt.Sprintf("http://localhost:%d", portIssuer),
		Authorizer: fmt.Sprintf("http://localhost:%d", portAuthorizer),
		EventsSink: fmt.Sprintf("http://localhost:%d", portSink),
		Source:     clusterSource, SourceToken: source.DefaultToken,
	}
	if f := newStackFault(t, url); f != nil {
		target.Fault = f
	}
	return target
}

// TestContract is spec 021's own test over Run. ORIGO_LIVE_URL set
// selects the live run: the installation it names with ORIGO_LIVE_TOKEN,
// no stub endpoints, no Fault, and exactly the six groups of the skip
// list skipped, each reported by name; otherwise the stack run through
// ORIGO_TEST_URL with the stubs and the Fault wired and nothing
// skipped, or a skip when nothing answers there.
func TestContract(t *testing.T) {
	if live := os.Getenv("ORIGO_LIVE_URL"); live != "" {
		token := os.Getenv("ORIGO_LIVE_TOKEN")
		if token == "" {
			t.Fatal("ORIGO_LIVE_URL is set and ORIGO_LIVE_TOKEN is not")
		}
		report := conformance.Run(t, conformance.Target{URL: live, Token: token})
		if len(report.Failed) != 0 {
			t.Fatalf("failed: %v", report.Failed)
		}
		got := slices.Clone(report.SkippedGroups)
		want := slices.Clone(conformance.Groups)
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Fatalf("the live run skipped %v, want exactly the six groups %v", got, want)
		}
		t.Logf("live run against %s: %d passed, skipped %v, unverified %v", live, len(report.Passed), report.Skipped, report.Unverified)
		return
	}
	target := stackTarget(t)
	report := conformance.Run(t, target)
	if len(report.Failed) != 0 {
		t.Fatalf("failed: %v", report.Failed)
	}
	if target.Fault != nil && len(report.Skipped) != 0 {
		t.Fatalf("the stack run skipped %v, want nothing skipped", report.Skipped)
	}
	if len(report.Unverified) != 0 {
		t.Fatalf("unverified on the stack: %v", report.Unverified)
	}
}

// answer is one step of the consumer-shaped flow: the status, the
// error code, the keys of the body, and the values a consumer relies
// on.
type answer struct {
	Step   string
	Status int
	Code   string
	Keys   []string
	Values map[string]any
}

// consumerFlow runs one consumer-shaped flow against a base URL: create
// a repository, read it, push, list its references, read the commit,
// delete it, and read it again, recording every answer without the
// values that differ by run.
func consumerFlow(t *testing.T, base, token string) []answer {
	t.Helper()
	call := func(step, method, path, body string) answer {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		a := answer{Step: step, Status: resp.StatusCode, Values: map[string]any{}}
		for k, v := range decoded {
			a.Keys = append(a.Keys, k)
			switch k {
			case "owner", "slug", "default_branch", "size_bytes", "head", "frozen_at", "kind":
				a.Values[k] = v
			}
		}
		sort.Strings(a.Keys)
		if e, ok := decoded["error"].(map[string]any); ok {
			a.Code, _ = e["code"].(string)
			a.Values["message"] = e["message"]
		}
		if resp.Header.Get("Origo-Contract") != "1" {
			t.Fatalf("%s: no contract header", step)
		}
		return a
	}
	var out []answer
	id := newID(t)
	slug := conformance.SlugPrefix + "same-" + id[:8]
	out = append(out, call("create", "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, conformance.Owner, slug)))
	out = append(out, call("duplicate", "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, conformance.Owner, slug)))
	out = append(out, call("get", "GET", "/v1/repos/"+id, ""))
	work := filepath.Join(t.TempDir(), "work")
	remote := strings.Replace(base, "://", "://x:"+token+"@", 1) + "/r/" + id + ".git"
	gittest.Run(t, t.TempDir(), nil, "clone", "-q", remote, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, work, nil, "add", "a.txt")
	gittest.Run(t, work, nil, "commit", "-q", "-m", "same")
	gittest.Run(t, work, nil, "push", "-q", "origin", "HEAD:refs/heads/main")
	head := gittest.Run(t, work, nil, "rev-parse", "HEAD")
	out = append(out, call("after-push", "GET", "/v1/repos/"+id, ""))
	out = append(out, call("refs", "GET", "/v1/repos/"+id+"/refs", ""))
	out = append(out, call("commit", "GET", "/v1/repos/"+id+"/commits/"+head, ""))
	out = append(out, call("missing-ref", "GET", "/v1/repos/"+id+"/commits?ref=refs/heads/nope", ""))
	out = append(out, call("token", "POST", "/v1/repos/"+id+"/tokens", `{"scope":"read","ttl":60}`))
	out = append(out, call("delete", "DELETE", "/v1/repos/"+id, ""))
	out = append(out, call("deleted", "GET", "/v1/repos/"+id, ""))
	out = append(out, call("undelete", "POST", "/v1/repos/"+id+"/undelete", ""))
	out = append(out, call("delete-again", "DELETE", "/v1/repos/"+id, ""))
	return out
}

func newID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := fmt.Sprintf("%x", b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// TestSameAnswersOnStubAndStack is spec 003's criterion, owned by spec
// 021: a consumer's integration tests written against the stub pass
// unchanged against a live node. One consumer-shaped flow runs against
// the in-process stub and against the stack at ORIGO_TEST_URL, and the
// answers are compared step by step: the status, the error code and
// sentence, the keys of every body, and the values a consumer relies
// on.
func TestSameAnswersOnStubAndStack(t *testing.T) {
	url := stackURL()
	if !answers(url) {
		t.Skipf("nothing answers at ORIGO_TEST_URL (%s)", url)
	}
	stub := origo.New(t)
	fromStub := consumerFlow(t, stub.URL(), stub.Token("dev", ""))
	token := os.Getenv("ORIGO_TEST_ADMIN_TOKEN")
	if token == "" {
		token = mintAt(t, fmt.Sprintf("http://localhost:%d", portIssuer), "conformance-same-answers")
	}
	fromStack := consumerFlow(t, url, token)
	if len(fromStub) != len(fromStack) {
		t.Fatalf("%d answers from the stub, %d from the stack", len(fromStub), len(fromStack))
	}
	for i := range fromStub {
		a, b := fromStub[i], fromStack[i]
		if a.Status != b.Status || a.Code != b.Code || !slices.Equal(a.Keys, b.Keys) {
			t.Errorf("%s: stub %d %s %v, stack %d %s %v", a.Step, a.Status, a.Code, a.Keys, b.Status, b.Code, b.Keys)
		}
		for k, v := range a.Values {
			if fmt.Sprint(b.Values[k]) != fmt.Sprint(v) {
				t.Errorf("%s: %s is %v on the stub and %v on the stack", a.Step, k, v, b.Values[k])
			}
		}
	}
}
