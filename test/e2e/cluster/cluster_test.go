// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package cluster_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/test/e2e/cluster"
)

// fakeKubectl puts a kubectl on PATH that records every invocation to a
// log and answers from a case table, so the helper's commands are
// checked without a cluster; TestClusterHelperDrivesKubectl in test/e2e
// runs the same functions against the stack.
func fakeKubectl(t *testing.T, script string) (calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" + script
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(body), 0o755); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		raw, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// failed is a testing.TB that records a Fatalf instead of ending the
// test, so the helper's failure path is observed.
type failed struct {
	testing.TB
	message string
}

func (f *failed) Fatalf(format string, args ...any) {
	f.message = strings.TrimSpace(sprintf(format, args...))
	panic(f)
}

func (f *failed) Helper() {}

func catchFatal(t *testing.T, fn func(tb testing.TB)) string {
	t.Helper()
	f := &failed{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil && r != f {
				panic(r)
			}
		}()
		fn(f)
	}()
	return f.message
}

func TestEveryFunctionRunsItsCommand(t *testing.T) {
	calls := fakeKubectl(t, `case "$*" in
*"get pod origod-1 -o jsonpath"*) printf 'StatefulSet/origod' ;;
*"get hpa origod -o json"*) printf '{"status":{"currentReplicas":3,"desiredReplicas":2}}' ;;
*"get statefulset,deployment -o name"*) printf 'statefulset.apps/origod\ndeployment.apps/minio\n' ;;
*"get statefulset origod -o json"*) printf '{"spec":{"replicas":3}}' ;;
*"kube-system get daemonset cilium -o json"*) printf '{"status":{"numberReady":1}}' ;;
*"apply -f refused.yaml"*) echo 'pods "privileged" is forbidden: violates PodSecurity "restricted:latest"' >&2; exit 1 ;;
esac
exit 0
`)
	cluster.DeletePod(t, "origod-1")
	t.Run("manifest", func(t *testing.T) {
		cluster.ApplyManifest(t, "cut-storage.yaml")
	})
	if got := cluster.ApplyManifestExpectRefusal(t, "refused.yaml"); !strings.Contains(got, "restricted") {
		t.Fatalf("refusal text %q", got)
	}
	if current, desired := cluster.HPAStatus(t, "origod"); current != 3 || desired != 2 {
		t.Fatalf("HPAStatus %d %d", current, desired)
	}
	cluster.Apply(t, "deploy/examples/kind")
	if got := string(cluster.Get(t, "statefulset", "origod")); !strings.Contains(got, `"replicas":3`) {
		t.Fatalf("Get %s", got)
	}
	if got := string(cluster.GetIn(t, "kube-system", "daemonset", "cilium")); !strings.Contains(got, `"numberReady":1`) {
		t.Fatalf("GetIn %s", got)
	}
	want := []string{
		"-n origo get pod origod-1 -o jsonpath={.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}",
		"-n origo delete pod origod-1 --wait=true",
		"-n origo rollout status statefulset/origod --timeout=5m0s",
		"-n origo apply -f cut-storage.yaml",
		"-n origo delete -f cut-storage.yaml --ignore-not-found=true --wait=true",
		"-n origo apply -f refused.yaml",
		"-n origo get hpa origod -o json",
		"-n origo apply -k deploy/examples/kind",
		"-n origo get statefulset,deployment -o name",
		"-n origo rollout status statefulset.apps/origod --timeout=5m0s",
		"-n origo rollout status deployment.apps/minio --timeout=5m0s",
		"-n origo get statefulset origod -o json",
		"-n kube-system get daemonset cilium -o json",
	}
	if got := calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFailuresCarryStderr(t *testing.T) {
	calls := fakeKubectl(t, `case "$*" in
*"get hpa broken"*) printf 'not json' ;;
*"-f accepted.yaml"*) exit 0 ;;
*) echo "Error from server (NotFound): nothing here" >&2; exit 1 ;;
esac
`)
	if msg := catchFatal(t, func(tb testing.TB) { cluster.Get(tb, "pod", "nope") }); !strings.Contains(msg, "NotFound") || !strings.Contains(msg, "kubectl get pod nope") {
		t.Fatalf("Get failure %q", msg)
	}
	if msg := catchFatal(t, func(tb testing.TB) { cluster.HPAStatus(tb, "broken") }); !strings.Contains(msg, "kubectl get hpa broken") {
		t.Fatalf("HPAStatus failure %q", msg)
	}
	if msg := catchFatal(t, func(tb testing.TB) { cluster.ApplyManifestExpectRefusal(tb, "accepted.yaml") }); !strings.Contains(msg, "admission did not refuse") {
		t.Fatalf("ApplyManifestExpectRefusal on success %q", msg)
	}
	// The accepted manifest is deleted again so nothing is left behind.
	if got := calls(); got[len(got)-1] != "-n origo delete -f accepted.yaml --ignore-not-found=true" {
		t.Fatalf("last call %q", got[len(got)-1])
	}
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
