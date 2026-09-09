// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/latere-ai/origo/test/e2e/cluster"
)

// TestClusterPodSecurityContext is spec 016's stack criterion: a node
// of the kind stack runs with the documented security context in the
// namespace labelled restricted, a pod that asks for privilege
// escalation is refused by Pod Security admission with the label named
// in the refusal and never created, and the NetworkPolicy origod-gossip
// exists with one ingress rule admitting UDP 7946 from the origod pods
// and nothing else.
func TestClusterPodSecurityContext(t *testing.T) {
	requireCluster(t)
	testdata := filepath.Join(repoRoot(t), "test", "e2e", "testdata")

	var ns struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(cluster.Get(t, "namespace", cluster.Namespace), &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("namespace labels %v", ns.Metadata.Labels)
	}

	var pod struct {
		Spec struct {
			AutomountServiceAccountToken *bool `json:"automountServiceAccountToken"`
			SecurityContext              struct {
				RunAsNonRoot   *bool `json:"runAsNonRoot"`
				RunAsUser      int64 `json:"runAsUser"`
				RunAsGroup     int64 `json:"runAsGroup"`
				SeccompProfile struct {
					Type string `json:"type"`
				} `json:"seccompProfile"`
			} `json:"securityContext"`
			Containers []struct {
				Name            string `json:"name"`
				SecurityContext struct {
					AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
					ReadOnlyRootFilesystem   *bool `json:"readOnlyRootFilesystem"`
					Capabilities             struct {
						Drop []string `json:"drop"`
					} `json:"capabilities"`
				} `json:"securityContext"`
				VolumeMounts []struct {
					MountPath string `json:"mountPath"`
					ReadOnly  bool   `json:"readOnly"`
				} `json:"volumeMounts"`
				Resources struct {
					Requests map[string]string `json:"requests"`
					Limits   map[string]string `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cluster.Get(t, "pod", "origod-0"), &pod); err != nil {
		t.Fatal(err)
	}
	sc := pod.Spec.SecurityContext
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser != 65532 || sc.RunAsGroup != 65532 || sc.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("pod security context %+v, automount %v", sc, pod.Spec.AutomountServiceAccountToken)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "origod" {
		t.Fatalf("containers %+v", pod.Spec.Containers)
	}
	c := pod.Spec.Containers[0]
	csc := c.SecurityContext
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation || csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem || !slices.Equal(csc.Capabilities.Drop, []string{"ALL"}) {
		t.Fatalf("container security context %+v", csc)
	}
	// The cache volume and /tmp are the only writable mounts; the CA
	// bundle of the test-source component is read only.
	var writable []string
	for _, m := range c.VolumeMounts {
		if !m.ReadOnly {
			writable = append(writable, m.MountPath)
		}
	}
	slices.Sort(writable)
	if !slices.Equal(writable, []string{"/tmp", "/var/lib/origo"}) {
		t.Fatalf("writable mounts %v", writable)
	}
	// The CPU request is the overlay's 50m on the kind stack and the
	// base's 250m on a stack the overlay does not patch; no other
	// figure is one spec 016 states.
	if c.Resources.Requests["memory"] != "256Mi" || c.Resources.Limits["memory"] != "2Gi" ||
		!slices.Contains([]string{"50m", "250m"}, c.Resources.Requests["cpu"]) {
		t.Fatalf("resources %+v", c.Resources)
	}

	// Pod Security admission refuses a pod asking for more.
	if text := cluster.ApplyManifestExpectRefusal(t, filepath.Join(testdata, "privileged-pod.yaml")); !strings.Contains(text, "restricted") {
		t.Fatalf("refusal text %q", text)
	}
	var pods struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(cluster.Get(t, "pods", "--field-selector=metadata.name=privileged"), &pods); err != nil || len(pods.Items) != 0 {
		t.Fatalf("the refused pod exists: %d %v", len(pods.Items), err)
	}

	// The gossip NetworkPolicy: one ingress rule, UDP 7946, from pods
	// carrying the origod label, no other port, protocol, or peer.
	var policy struct {
		Spec struct {
			PodSelector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"podSelector"`
			PolicyTypes []string `json:"policyTypes"`
			Ingress     []struct {
				From []struct {
					PodSelector *struct {
						MatchLabels      map[string]string `json:"matchLabels"`
						MatchExpressions []any             `json:"matchExpressions"`
					} `json:"podSelector"`
					NamespaceSelector *json.RawMessage `json:"namespaceSelector"`
					IPBlock           *json.RawMessage `json:"ipBlock"`
				} `json:"from"`
				Ports []struct {
					Port     json.RawMessage `json:"port"`
					Protocol string          `json:"protocol"`
					EndPort  *int            `json:"endPort"`
				} `json:"ports"`
			} `json:"ingress"`
			Egress []any `json:"egress"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cluster.Get(t, "networkpolicy", "origod-gossip"), &policy); err != nil {
		t.Fatal(err)
	}
	spec := policy.Spec
	if spec.PodSelector.MatchLabels["app.kubernetes.io/name"] != "origod" || !slices.Equal(spec.PolicyTypes, []string{"Ingress"}) || len(spec.Egress) != 0 {
		t.Fatalf("policy selects %v with types %v and egress %v", spec.PodSelector, spec.PolicyTypes, spec.Egress)
	}
	if len(spec.Ingress) != 1 {
		t.Fatalf("%d ingress rules, want 1", len(spec.Ingress))
	}
	rule := spec.Ingress[0]
	if len(rule.Ports) != 1 || string(rule.Ports[0].Port) != "7946" || rule.Ports[0].Protocol != "UDP" || rule.Ports[0].EndPort != nil {
		t.Fatalf("ports %+v", rule.Ports)
	}
	if len(rule.From) != 1 || rule.From[0].NamespaceSelector != nil || rule.From[0].IPBlock != nil || rule.From[0].PodSelector == nil {
		t.Fatalf("peers %+v", rule.From)
	}
	peer := rule.From[0].PodSelector
	if len(peer.MatchExpressions) != 0 || len(peer.MatchLabels) != 1 || peer.MatchLabels["app.kubernetes.io/name"] != "origod" {
		t.Fatalf("peer selector %+v", peer)
	}
}
