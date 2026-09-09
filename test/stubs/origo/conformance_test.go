// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origo_test

import (
	"slices"
	"testing"

	"github.com/latere-ai/origo/test/conformance"
	"github.com/latere-ai/origo/test/stubs/origo"
)

// TestStubConforms is spec 021's criterion for the contract stub: the
// suite passes against it with an empty Skip list, the LFS rows
// included, because its bucket is the s3test server the presigned URLs
// resolve against, and the two degraded rows through its Fault. The
// source group alone skips, because a loopback source needs the
// AllowLoopback seam spec 016 restricts to a _test.go file.
func TestStubConforms(t *testing.T) {
	s := origo.New(t)
	report := conformance.Run(t, conformance.Target{
		URL: s.URL(), Token: s.Token("dev", ""),
		Issuer: s.Issuer().URL(), Authorizer: s.Authorizer().URL(), EventsSink: s.Sink().URL(),
		Fault: s.Fault(),
	})
	if len(report.Failed) != 0 {
		t.Fatalf("failed: %v", report.Failed)
	}
	if !slices.Equal(report.SkippedGroups, []string{conformance.GroupSource}) {
		t.Fatalf("skipped groups %v, want the source group alone", report.SkippedGroups)
	}
	if len(report.Unverified) != 0 {
		t.Fatalf("unverified with a sink: %v", report.Unverified)
	}
	if len(report.Passed) < 40 || len(report.Created) < 10 {
		t.Fatalf("%d passed, %d created", len(report.Passed), len(report.Created))
	}
}
