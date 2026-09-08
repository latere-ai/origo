// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/e2e/cluster"
)

// The ports table of spec 013 at offset 0.
const (
	portBalanced   = 30080
	portNode1      = 30180
	portNode1Int   = 30190
	portIssuer     = 30081
	portAuthorizer = 30082
	portSink       = 30083
	portSource     = 30084
	portSlowProxy  = 30085
	portMinIO      = 30900
)

// stackURL is ORIGO_TEST_URL, the balanced host port by default.
func stackURL() string {
	if u := os.Getenv("ORIGO_TEST_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return fmt.Sprintf("http://localhost:%d", portBalanced)
}

// httpGet fetches a URL with a short timeout and returns the status and
// body, 0 when nothing answers.
func httpGet(client *http.Client, url string) (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// requireCluster skips when nothing answers at ORIGO_TEST_URL or kubectl
// is not on PATH, so the tier runs on a developer's machine without the
// stack.
func requireCluster(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not on PATH")
	}
	if status, _ := httpGet(http.DefaultClient, stackURL()+"/version"); status != 200 {
		t.Skipf("nothing answers at %s", stackURL())
	}
}

// adminToken is ORIGO_TEST_ADMIN_TOKEN, or a token minted for the dev
// subject at the issuer's host port.
func adminToken(t *testing.T) string {
	t.Helper()
	if tok := os.Getenv("ORIGO_TEST_ADMIN_TOKEN"); tok != "" {
		return tok
	}
	return mintAt(t, fmt.Sprintf("http://localhost:%d", portIssuer), "dev")
}

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

// stackAPI calls the repository API of the stack through one node.
func stackAPI(t *testing.T, base, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// renderedOverlay is the kustomization up.sh wrote for the current
// cluster, out/kind/<name>/, the name from out/kind/current.
func renderedOverlay(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "out", "kind", "current"))
	if err != nil {
		t.Skipf("no cluster of up.sh is current: %v", err)
	}
	return filepath.Join(root, "out", "kind", strings.TrimSpace(string(raw)))
}

// statefulSetReplicas reads spec.replicas and status.readyReplicas of a
// StatefulSet through cluster.Get.
func statefulSetReplicas(t *testing.T, name string) (spec, ready int) {
	t.Helper()
	var sts struct {
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(cluster.Get(t, "statefulset", name), &sts); err != nil {
		t.Fatal(err)
	}
	return sts.Spec.Replicas, sts.Status.ReadyReplicas
}

// waitUntil polls until the condition holds or the timeout passes.
func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestClusterHelperDrivesKubectl is spec 013's criterion for
// test/e2e/cluster: each of its functions against the stack.
func TestClusterHelperDrivesKubectl(t *testing.T) {
	requireCluster(t)
	overlay := renderedOverlay(t)
	testdata := filepath.Join(repoRoot(t), "test", "e2e", "testdata")
	token := adminToken(t)
	node1 := fmt.Sprintf("http://localhost:%d", portNode1)

	// Get of the StatefulSet names three replicas.
	if spec, ready := statefulSetReplicas(t, "origod"); spec != 3 || ready != 3 {
		t.Fatalf("StatefulSet origod: %d replicas, %d ready", spec, ready)
	}
	// HPAStatus reports the counts kubectl shows.
	var hpa struct {
		Status struct {
			Current int `json:"currentReplicas"`
			Desired int `json:"desiredReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(cluster.Get(t, "hpa", "origod"), &hpa); err != nil {
		t.Fatal(err)
	}
	if current, desired := cluster.HPAStatus(t, "origod"); current != hpa.Status.Current || desired != hpa.Status.Desired {
		t.Fatalf("HPAStatus %d/%d, kubectl %d/%d", current, desired, hpa.Status.Current, hpa.Status.Desired)
	}
	// DeletePod of node 2 returns once the replacement pod is ready.
	cluster.DeletePod(t, "origod-1")
	if _, ready := statefulSetReplicas(t, "origod"); ready != 3 {
		t.Fatalf("after DeletePod: %d ready", ready)
	}
	// The replacement is ready; its NodePort follows once the endpoint
	// list carries the new pod.
	waitUntil(t, "node 2 answering after its replacement", time.Minute, func() bool {
		status, _ := httpGet(http.DefaultClient, fmt.Sprintf("http://localhost:%d/version", portNode1+1))
		return status == 200
	})
	// ApplyManifestExpectRefusal returns the refusal text and leaves no
	// pod behind.
	if text := cluster.ApplyManifestExpectRefusal(t, filepath.Join(testdata, "privileged-pod.yaml")); !strings.Contains(text, "restricted") {
		t.Fatalf("refusal text %q", text)
	}
	var pods struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(cluster.Get(t, "pods", "--field-selector=metadata.name=privileged"), &pods); err != nil || len(pods.Items) != 0 {
		t.Fatalf("the refused pod exists: %d %v", len(pods.Items), err)
	}
	// ApplyManifest of cut-storage.yaml makes a node answer 503
	// storage_unavailable and its cleanup restores the answer.
	id := newID(t)
	if status, body := stackAPI(t, node1, token, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":"helper","slug":"repo-%s"}`, id, id[:8])); status != 201 {
		t.Fatalf("create: %d %v", status, body)
	}
	t.Run("cut-storage", func(t *testing.T) {
		cluster.ApplyManifest(t, filepath.Join(testdata, "cut-storage.yaml"))
		waitUntil(t, "503 storage_unavailable from node 1", 4*time.Minute, func() bool {
			status, body := stackAPI(t, node1, token, "GET", "/v1/repos/"+id, "")
			return status == 503 && errorCode(body) == "storage_unavailable"
		})
	})
	waitUntil(t, "node 1 answering again", 2*time.Minute, func() bool {
		status, _ := stackAPI(t, node1, token, "GET", "/v1/repos/"+id, "")
		return status == 200
	})
	// Apply of the overlay restores the replica count after an hpa-2.yaml.
	t.Run("hpa-2", func(t *testing.T) {
		cluster.ApplyManifest(t, filepath.Join(testdata, "hpa-2.yaml"))
		waitUntil(t, "two replicas", 3*time.Minute, func() bool {
			spec, _ := statefulSetReplicas(t, "origod")
			return spec == 2
		})
		if _, desired := cluster.HPAStatus(t, "origod"); desired != 2 {
			t.Fatalf("HPAStatus desired %d after hpa-2.yaml", desired)
		}
	})
	cluster.Apply(t, overlay)
	waitUntil(t, "three replicas restored", 3*time.Minute, func() bool {
		spec, ready := statefulSetReplicas(t, "origod")
		return spec == 3 && ready == 3
	})
}

// versionsEnv reads deploy/examples/kind/versions.env.
func versionsEnv(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "examples", "kind", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// TestClusterUpScript is spec 013's criterion for up.sh and down.sh: a
// cluster of its own with every host port offset by 1000, checked
// against the list of the criterion, then removed whatever happened. It
// runs in the up-script job from the two tarballs of the
// candidate-images artifact at out/images/, and skips without kind or
// the tarballs.
func TestClusterUpScript(t *testing.T) {
	root := repoRoot(t)
	for _, tool := range []string{"kind", "kubectl", "helm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
	origodTar := filepath.Join(root, "out", "images", "origod.tar")
	stubsTar := filepath.Join(root, "out", "images", "origo-stubs.tar")
	for _, tar := range []string{origodTar, stubsTar} {
		if _, err := os.Stat(tar); err != nil {
			t.Skipf("no candidate image at %s", tar)
		}
	}
	const offset = 1000
	port := func(p int) int { return p + offset }
	scripts := filepath.Join(root, "deploy", "examples", "kind")
	run := func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(scripts, name), args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() {
		if out, err := run("down.sh", "-name", "up-test"); err != nil {
			t.Errorf("down.sh: %v\n%s", err, out)
		}
		clusters, _ := exec.CommandContext(context.Background(), "kind", "get", "clusters").Output()
		if slices.Contains(strings.Fields(string(clusters)), "origo-up-test") {
			t.Error("down.sh left the cluster origo-up-test")
		}
	})
	started := time.Now()
	out, err := run("up.sh", "-name", "up-test", "-port-offset", strconv.Itoa(offset), origodTar, stubsTar)
	if err != nil {
		t.Fatalf("up.sh: %v\n%s", err, out)
	}
	if took := time.Since(started); took > 5*time.Minute {
		t.Fatalf("up.sh took %s", took)
	}
	t.Logf("up.sh in %s", time.Since(started).Round(time.Second))

	// The three pods, the stubs, metrics-server, and Cilium are ready.
	if spec, ready := statefulSetReplicas(t, "origod"); spec != 3 || ready != 3 {
		t.Fatalf("StatefulSet origod: %d replicas, %d ready", spec, ready)
	}
	for _, name := range []string{"minio", "origo-stubs"} {
		var d struct {
			Status struct {
				Ready int `json:"readyReplicas"`
			} `json:"status"`
		}
		if err := json.Unmarshal(cluster.Get(t, "deployment", name), &d); err != nil || d.Status.Ready != 1 {
			t.Fatalf("deployment %s: %d ready %v", name, d.Status.Ready, err)
		}
	}
	versions := versionsEnv(t)
	var ds struct {
		Status struct {
			Ready int `json:"numberReady"`
		} `json:"status"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cluster.GetIn(t, "kube-system", "daemonset", "cilium"), &ds); err != nil || ds.Status.Ready < 1 {
		t.Fatalf("cilium: %d ready %v", ds.Status.Ready, err)
	}
	if image := ds.Spec.Template.Spec.Containers[0].Image; !strings.Contains(image, "cilium:v"+versions["CILIUM_CHART_VERSION"]+"@sha256:") {
		t.Fatalf("cilium image %s is not chart %s by digest", image, versions["CILIUM_CHART_VERSION"])
	}
	var ms struct {
		Status struct {
			Ready int `json:"readyReplicas"`
		} `json:"status"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string   `json:"image"`
						Args  []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cluster.GetIn(t, "kube-system", "deployment", "metrics-server"), &ms); err != nil || ms.Status.Ready != 1 {
		t.Fatalf("metrics-server: %d ready %v", ms.Status.Ready, err)
	}
	if c := ms.Spec.Template.Spec.Containers[0]; c.Image != versions["METRICS_SERVER_IMAGE"] || !slices.Contains(c.Args, "--kubelet-insecure-tls") {
		t.Fatalf("metrics-server runs %s with %v", c.Image, c.Args)
	}
	// The namespace carries the restricted label.
	var ns struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(cluster.Get(t, "namespace", "origo"), &ns); err != nil || ns.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("namespace labels %v", ns.Metadata.Labels)
	}
	// Every host port of the ports table answers, offset by 1000.
	for p, path := range map[int]string{
		portBalanced: "/version", portNode1: "/version", portNode1 + 1: "/version", portNode1 + 2: "/version",
		portNode1Int: "/metrics", portNode1Int + 1: "/metrics", portNode1Int + 2: "/metrics",
		portIssuer: "/jwks", portAuthorizer: "/requests", portSink: "/deliveries", portSlowProxy: "/", portMinIO: "/minio/health/live",
	} {
		if status, _ := httpGet(http.DefaultClient, fmt.Sprintf("http://localhost:%d%s", port(p), path)); status != 200 {
			t.Errorf("port %d%s: %d", port(p), path, status)
		}
	}
	// The source's host port is trusted through the CA file up.sh wrote.
	caFile := filepath.Join(root, "test", "e2e", "testdata", "stub-ca.pem")
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("stub-ca.pem: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	if status, served := httpGet(tlsClient, fmt.Sprintf("https://localhost:%d/ca.pem", port(portSource))); status != 200 || string(served) != string(caPEM) {
		t.Errorf("port %d/ca.pem: %d, or the served CA differs from stub-ca.pem", port(portSource), status)
	}
	// The Secret origo-stubs-ca holds a certificate with both SANs, the
	// one stub-ca.pem carries.
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(cluster.Get(t, "secret", "origo-stubs-ca"), &secret); err != nil {
		t.Fatal(err)
	}
	certPEM, err := base64.StdEncoding.DecodeString(secret.Data["ca.crt"])
	if err != nil || string(certPEM) != string(caPEM) {
		t.Fatalf("the Secret's ca.crt differs from stub-ca.pem: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("ca.crt is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, san := range []string{"origo-stubs.origo.svc", "localhost"} {
		if !slices.Contains(cert.DNSNames, san) {
			t.Errorf("the CA lacks the SAN %s: %v", san, cert.DNSNames)
		}
	}
	// The nodes carry the variables of the overlay table.
	var sts struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cluster.Get(t, "statefulset", "origod"), &sts); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"ORIGO_STALE_MAX": "30s", "ORIGO_STORAGE_TIMEOUT": "5s", "ORIGO_PUBLIC_URL": "http://localhost:30080", "ORIGO_S3_PUBLIC_ENDPOINT": "http://localhost:30900",
		"ORIGO_S3_ENDPOINT":  "http://slowproxy.origo.svc:8086",
		"ORIGO_OIDC_ISSUERS": "http://origo-stubs.origo.svc:8081", "ORIGO_OIDC_INSECURE_ISSUERS": "http://origo-stubs.origo.svc:8081",
		"ORIGO_AUTHORIZER_URL": "http://origo-stubs.origo.svc:8082", "ORIGO_AUTHORIZER_TOKEN": "stub-authorizer-token",
		"ORIGO_EVENTS_URL": "http://origo-stubs.origo.svc:8083", "ORIGO_EVENTS_SECRET": "stub-sink-secret",
		"ORIGO_EGRESS_ALLOW": "origo-stubs.origo.svc=10.96.0.42", "ORIGO_EGRESS_CA_BUNDLE": "/etc/origo/stub-ca.pem",
		"ORIGO_S3_BUCKET": "origo-test", "ORIGO_GOSSIP_PEERS": "origod-gossip",
	} {
		if env[name] != want {
			t.Errorf("%s=%q on the nodes, want %q", name, env[name], want)
		}
	}
	if env["ORIGO_GOSSIP_SECRET"] == "" || env["ORIGO_CLUSTER_CIDRS"] == "" {
		t.Error("ORIGO_GOSSIP_SECRET or ORIGO_CLUSTER_CIDRS is unset on the nodes")
	}
	// A token minted at the offset issuer port is accepted by the offset
	// stack, and the source stub answers the nodes' clone through the
	// egress the component set: the stack is whole.
	tok := mintAt(t, fmt.Sprintf("http://localhost:%d", port(portIssuer)), "dev")
	if status, body := stackAPI(t, fmt.Sprintf("http://localhost:%d", port(portBalanced)), tok, "POST", "/v1/repos", `{"id":"`+newID(t)+`","owner":"up","slug":"test"}`); status != 201 {
		t.Fatalf("create through the offset stack: %d %v", status, body)
	}
}
