// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package cluster is the helper a cluster test of the e2e tier changes
// the kind stack with (spec 013). It shells out to kubectl from PATH
// under the kubeconfig kind wrote for the job (KUBECONFIG, or the
// default path), in the namespace origo, so no test runs kubectl on its
// own and no test imports a Kubernetes client, which keeps spec 001's
// dependency rule. Every function takes the testing.TB, fails the test
// on a non-zero exit with kubectl's stderr, and is named by the
// criterion that uses it.
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Namespace is the stack's namespace, the -n of every command.
const Namespace = "origo"

// rolloutTimeout bounds one wait for a rollout.
const rolloutTimeout = 5 * time.Minute

// run runs kubectl in the namespace and returns stdout, stderr, and the
// exit error.
func run(t testing.TB, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rolloutTimeout+time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"-n", Namespace}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// must runs kubectl and fails the test on a non-zero exit with stderr.
func must(t testing.TB, args ...string) string {
	t.Helper()
	out, stderr, err := run(t, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(stderr))
	}
	return out
}

// DeletePod deletes the pod and waits until the workload that owns it
// has its replacement ready, for spec 005's node removal.
func DeletePod(t testing.TB, name string) {
	t.Helper()
	owner := strings.TrimSpace(must(t, "get", "pod", name, "-o", "jsonpath={.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}"))
	must(t, "delete", "pod", name, "--wait=true")
	must(t, "rollout", "status", strings.ToLower(owner), "--timeout="+rolloutTimeout.String())
}

// ApplyManifest applies a manifest under test/e2e/testdata/ and
// registers its deletion to run when the test ends, so a fault lasts
// exactly one test.
func ApplyManifest(t testing.TB, path string) {
	t.Helper()
	must(t, "apply", "-f", path)
	t.Cleanup(func() { must(t, "delete", "-f", path, "--ignore-not-found=true", "--wait=true") })
}

// ApplyManifestExpectRefusal applies a manifest admission must refuse:
// it fails the test when the apply succeeds and returns kubectl's
// stderr, the refusal text, for the test to assert on. Nothing was
// created, so no cleanup is registered.
func ApplyManifestExpectRefusal(t testing.TB, path string) string {
	t.Helper()
	out, stderr, err := run(t, "apply", "-f", path)
	if err == nil {
		must(t, "delete", "-f", path, "--ignore-not-found=true")
		t.Fatalf("kubectl apply -f %s succeeded, admission did not refuse it\n%s", path, out)
	}
	return stderr
}

// HPAStatus returns the current and desired replica counts of the
// HorizontalPodAutoscaler.
func HPAStatus(t testing.TB, name string) (current, desired int) {
	t.Helper()
	var hpa struct {
		Status struct {
			Current int `json:"currentReplicas"`
			Desired int `json:"desiredReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(must(t, "get", "hpa", name, "-o", "json")), &hpa); err != nil {
		t.Fatalf("kubectl get hpa %s: %v", name, err)
	}
	return hpa.Status.Current, hpa.Status.Desired
}

// Apply applies the overlay with kubectl apply -k and waits for every
// StatefulSet and Deployment of the namespace to roll out, which
// restores the stack after a test changed it.
func Apply(t testing.TB, overlay string) {
	t.Helper()
	must(t, "apply", "-k", overlay)
	for workload := range strings.FieldsSeq(must(t, "get", "statefulset,deployment", "-o", "name")) {
		must(t, "rollout", "status", workload, "--timeout="+rolloutTimeout.String())
	}
}

// Get returns the object as JSON, for a test that asserts on what the
// overlay applied rather than on the stack's behaviour.
func Get(t testing.TB, kind, name string) []byte {
	t.Helper()
	return []byte(must(t, "get", kind, name, "-o", "json"))
}
