// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The markers that fence the text Dockerfile and Dockerfile.ci share
// (spec 002's Images section): the runtime base and the runtime stage.
const (
	sharedBaseStart  = "# >>> shared runtime base <<<\n"
	sharedBaseEnd    = "# >>> end shared runtime base <<<\n"
	sharedStageStart = "# >>> shared runtime stage <<<\n"
	sharedStageEnd   = "# >>> end shared runtime stage <<<\n"
)

// runtimeBase is the one base every image runs on (spec 017): trixie-slim,
// pinned by digest, whose git is 2.47.
var runtimeBase = regexp.MustCompile(`^ARG RUNTIME_BASE=docker\.io/library/debian:trixie-slim@sha256:[0-9a-f]{64}$`)

// fenced returns the text between two markers of a Dockerfile.
func fenced(t *testing.T, name, text, start, end string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, start)
	if !ok {
		t.Fatalf("%s has no %q", name, strings.TrimSpace(start))
	}
	body, _, ok := strings.Cut(after, end)
	if !ok {
		t.Fatalf("%s has no %q", name, strings.TrimSpace(end))
	}
	return body
}

// TestDockerfilesShareOneRuntimeStage is spec 017's image criterion, the
// half a test holds: Dockerfile and Dockerfile.ci carry the same runtime
// base and the same runtime stage byte for byte between the markers, the
// base is debian:trixie-slim by one digest, and Dockerfile.stubs runs on
// that base too. The other half, git --version inside the built image,
// is the build job of verify.yml and step 1 of release.yml.
func TestDockerfilesShareOneRuntimeStage(t *testing.T) {
	root := moduleRoot(t)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	dev, ci, stubs := read("Dockerfile"), read("Dockerfile.ci"), read("Dockerfile.stubs")
	base := fenced(t, "Dockerfile", dev, sharedBaseStart, sharedBaseEnd)
	if got := fenced(t, "Dockerfile.ci", ci, sharedBaseStart, sharedBaseEnd); got != base {
		t.Errorf("the runtime base differs:\nDockerfile:\n%sDockerfile.ci:\n%s", base, got)
	}
	if got := fenced(t, "Dockerfile.stubs", stubs, sharedBaseStart, sharedBaseEnd); got != base {
		t.Errorf("the runtime base differs:\nDockerfile:\n%sDockerfile.stubs:\n%s", base, got)
	}
	stage := fenced(t, "Dockerfile", dev, sharedStageStart, sharedStageEnd)
	if got := fenced(t, "Dockerfile.ci", ci, sharedStageStart, sharedStageEnd); got != stage {
		t.Errorf("the runtime stage differs:\nDockerfile:\n%sDockerfile.ci:\n%s", stage, got)
	}
	var pins []string
	for line := range strings.SplitSeq(base, "\n") {
		if strings.HasPrefix(line, "ARG RUNTIME_BASE=") {
			pins = append(pins, line)
		}
	}
	if len(pins) != 1 || !runtimeBase.MatchString(pins[0]) {
		t.Errorf("the runtime base is not debian:trixie-slim pinned by one digest: %q", pins)
	}
	if !strings.Contains(stage, "apt-get install -y --no-install-recommends git ca-certificates") {
		t.Error("the runtime stage does not install git")
	}
}
