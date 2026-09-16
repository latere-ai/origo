// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer

import (
	"testing"

	"latere.ai/x/pkg/authz"
)

// TestTheTableConstructs: the declared table is one NewVocabulary
// accepts, so importing the package cannot panic; must refuses one it
// does not, which is the programming error it stands for. The test is
// in the package because must is, and because the rest of the suite
// imports internal/contract, which imports this package back.
func TestTheTableConstructs(t *testing.T) {
	if _, err := authz.NewVocabulary(vocabulary.Core, vocabulary.Actions...); err != nil {
		t.Fatalf("the declared table does not construct: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("must accepted a table NewVocabulary refused")
		}
	}()
	must(authz.NewVocabulary("origo",
		authz.Action{Name: ActionRead, Kind: KindRepository},
		authz.Action{Name: ActionRead, Kind: KindRepository},
	))
}
