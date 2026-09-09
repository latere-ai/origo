// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/test/conformance"
	"github.com/latere-ai/origo/test/stubs/sink"
)

// mutationTargetEnv and mutationReportEnv carry the target and the
// report path from TestMutation to the run of the suite it starts in a
// second process: a failed subtest fails the test that ran it, so the
// suite must run where its failure is the expected outcome and be read
// from outside. Both are private to this test binary.
const (
	mutationTargetEnv = "ORIGO_MUTATION_TARGET"
	mutationReportEnv = "ORIGO_MUTATION_REPORT"
)

// mutations is the set of subtests that assert each dropped
// capability: the capability's row of spec 003's table and the fetch
// or push that uses it. The two sha1-in-want capabilities share one
// configuration key, so dropping either drops both rows.
var mutations = map[string][]string{
	"filter":                       {"003/capability/filter", "003/partial-clone"},
	"allow-tip-sha1-in-want":       {"003/capability/allow-tip-sha1-in-want", "003/capability/allow-reachable-sha1-in-want", "003/fetch-by-hash"},
	"allow-reachable-sha1-in-want": {"003/capability/allow-tip-sha1-in-want", "003/capability/allow-reachable-sha1-in-want", "003/fetch-by-hash"},
	"atomic":                       {"003/capability/atomic", "003/atomic-push"},
	"push-options":                 {"003/capability/push-options", "003/push-options", "008/event-off"},
}

// TestMutation is spec 021's mutation job: one node of its own started
// from the ORIGO_TEST_S3_ENDPOINT family with ORIGO_TEST_DROP_CAPABILITY
// in its environment, the suite run against it with an in-process
// issuer, authorizer, and sink and no Fault, and a pass only when the
// run fails on exactly the subtests that assert the dropped capability.
// The suite runs in a second process of this test binary, TestMutationRun,
// which writes its report where this test reads it. Skips with the
// variable unset.
func TestMutation(t *testing.T) {
	capability := os.Getenv("ORIGO_TEST_DROP_CAPABILITY")
	if capability == "" {
		t.Skip("ORIGO_TEST_DROP_CAPABILITY is not set")
	}
	want, ok := mutations[capability]
	if !ok || !slices.Contains(config.DropCapabilities, capability) {
		t.Fatalf("%s is not in the set", capability)
	}
	s := requireStack(t)
	events := sink.New(t)
	n := startNode(t, s, "", map[string]string{
		"ORIGO_TEST_DROP_CAPABILITY": capability,
		"ORIGO_EVENTS_URL":           events.URL(), "ORIGO_EVENTS_SECRET": events.Secret(),
	})
	target, err := json.Marshal(conformance.Target{
		URL: "http://" + n.public, Token: s.token,
		Issuer: s.issuer.URL(), Authorizer: s.authz.URL(), EventsSink: events.URL(),
	})
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run", "^TestMutationRun$", "-test.count=1", "-test.timeout", "10m")
	cmd.Env = append(os.Environ(), mutationTargetEnv+"="+string(target), mutationReportEnv+"="+reportPath)
	out, runErr := cmd.CombinedOutput()
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("no report from the run: %v\n%s", err, out)
	}
	var report conformance.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if runErr == nil {
		t.Fatalf("the run passed with %s dropped:\n%s", capability, out)
	}
	got := slices.Clone(report.Failed)
	sort.Strings(got)
	want = slices.Clone(want)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("with %s dropped the run failed on %v, want exactly %v\n%s", capability, got, want, out)
	}
	if len(report.Passed) == 0 {
		t.Fatalf("nothing passed:\n%s", out)
	}
	t.Logf("%s dropped: %d passed, failed %v, skipped %v", capability, len(report.Passed), got, report.Skipped)
}

// TestMutationRun is the second process of TestMutation: the suite
// against the target the environment names, its report written where
// the environment says. Skips outside that process.
func TestMutationRun(t *testing.T) {
	raw := os.Getenv(mutationTargetEnv)
	if raw == "" {
		t.Skip("not the second process of TestMutation")
	}
	var target conformance.Target
	if err := json.Unmarshal([]byte(raw), &target); err != nil {
		t.Fatal(err)
	}
	report := conformance.Run(t, target)
	out, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(mutationReportEnv), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMutationsCoverTheSet holds the mutation table to the set the
// node reads: every capability of config.DropCapabilities has its
// subtests here and nothing else does.
func TestMutationsCoverTheSet(t *testing.T) {
	var got []string
	for name := range mutations {
		got = append(got, name)
	}
	sort.Strings(got)
	want := slices.Clone(config.DropCapabilities)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("mutations %v, the set %v", got, want)
	}
}
