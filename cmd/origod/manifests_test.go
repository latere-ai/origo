// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The two manifests this file holds to spec 018's rule: the base's
// Ingress, which names no controller, and the kind example's patch,
// which is where a controller's settings go. The paths are resolved from
// this file's own location, so the test reads the tree and not a copy.
const (
	baseIngress = "deploy/base/ingress.yaml"
	kindIngress = "deploy/examples/kind/patches/ingress-nginx.yaml"
	baseDeploy  = "deploy/base/deployment.yaml"
)

func manifest(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(moduleRoot(t), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestBaseIngressIsControllerNeutral is spec 018's manifest criterion:
// nothing in the base names one ingress controller or one certificate
// issuer, and the settings a git remote needs from ingress-nginx are in
// the overlay, where an operator finds them and copies the shape.
func TestBaseIngressIsControllerNeutral(t *testing.T) {
	base := manifest(t, baseIngress)
	for line := range strings.SplitSeq(base, "\n") {
		field := strings.TrimSpace(line)
		if strings.HasPrefix(field, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(field, "nginx.ingress.kubernetes.io/"),
			strings.HasPrefix(field, "cert-manager.io/"):
			t.Errorf("%s carries the controller-specific annotation %q", baseIngress, field)
		case strings.HasPrefix(field, "ingressClassName:"):
			t.Errorf("%s names an ingress class: %q", baseIngress, field)
		case strings.HasPrefix(field, "host:"), strings.HasPrefix(field, "secretName:"):
			t.Errorf("%s carries an operator's own value: %q", baseIngress, field)
		}
	}
	if !strings.Contains(base, "kind: Ingress") {
		t.Fatalf("%s is not an Ingress", baseIngress)
	}

	overlay := manifest(t, kindIngress)
	for _, want := range []string{
		`nginx.ingress.kubernetes.io/proxy-body-size: "0"`,
		`nginx.ingress.kubernetes.io/proxy-read-timeout: "600"`,
	} {
		if !strings.Contains(overlay, want) {
			t.Errorf("%s does not carry %s", kindIngress, want)
		}
	}
	if !strings.Contains(manifest(t, "deploy/examples/kind/kustomization.yaml"), "patches/ingress-nginx.yaml") {
		t.Error("the kind overlay does not apply the ingress patch")
	}
}

var (
	// A container is a list entry at the pod spec's indent; env entries,
	// ports, and volume mounts sit deeper, and the pod's volumes sit at
	// the same depth after the last container, which ends its block.
	containerStart = regexp.MustCompile(`(?m)^        - name: (\S+)$`)
	envName        = regexp.MustCompile(`(?m)^\s+- name: (ORIGO_\S+)$`)
	secretRef      = regexp.MustCompile(`(?m)^\s+name: (origod-\S+)$`)
)

// TestCheckInitContainerSharesTheNodesEnvironment holds the init
// container to the one property that makes it worth having: it runs the
// check against the environment the node will run under. A check that
// read another bucket, another issuer, or another authorizer would pass
// while the node fails, and no other test would notice.
func TestCheckInitContainerSharesTheNodesEnvironment(t *testing.T) {
	body := manifest(t, baseDeploy)
	blocks := map[string]string{}
	starts := containerStart.FindAllStringSubmatchIndex(body, -1)
	for i, m := range starts {
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		if name := body[m[2]:m[3]]; name == "check" || name == "origod" {
			blocks[name] = body[m[0]:end]
		}
	}
	if len(blocks) != 2 {
		t.Fatalf("%s: found %d of the containers check and origod", baseDeploy, len(blocks))
	}
	if !strings.Contains(blocks["check"], "args: [check]") {
		t.Errorf("the init container does not run the check: %q", blocks["check"])
	}
	names := func(re *regexp.Regexp, block string) []string {
		var out []string
		for _, m := range re.FindAllStringSubmatch(block, -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return out
	}
	if got, want := names(envName, blocks["check"]), names(envName, blocks["origod"]); !slices.Equal(got, want) {
		t.Errorf("the init container reads %v and the node %v", got, want)
	}
	if got, want := names(secretRef, blocks["check"]), names(secretRef, blocks["origod"]); !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("the init container reads the secrets %v and the node %v", got, want)
	}
	// The check must run the release it is checking, never another.
	image := regexp.MustCompile(`image: (\S+)`)
	if got, want := image.FindStringSubmatch(blocks["check"]), image.FindStringSubmatch(blocks["origod"]); len(got) != 2 || len(want) != 2 || got[1] != want[1] {
		t.Errorf("the init container runs %v and the node %v", got, want)
	}
}
