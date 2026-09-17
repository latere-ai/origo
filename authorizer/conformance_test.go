// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer_test

import (
	"testing"

	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/stub"

	"github.com/latere-ai/origo/authorizer"
)

// TestConformanceAgainstTheStub holds the published table to the suite
// every authorizer of the shared contract passes, which is what handing
// Vocabulary to latere.ai/x/pkg/authz/conformance is for: a case per row
// of the declaration rather than a list written out here. The endpoint
// under test is the shared stub told that same table, so a row spec 028
// gains is a case the run gains on both sides at once, and neither side
// carries a copy of the strings.
//
// The run covers the probe id denied for every subject and every action,
// the anonymous subject included; a wrong bearer and no bearer refused;
// an action outside the table answered with a 400 rather than a deny;
// and one well-formed request per subject and action answered with a
// body of the contract's shape. PageActions names repo.list, so that
// answer is read as a page of Origo's own shape (spec 026) and the other
// three as decisions.
func TestConformanceAgainstTheStub(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	conformance.Run(t, s.URL(), s.Token(),
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithPageActions(authorizer.PageActions()...),
	)
}
