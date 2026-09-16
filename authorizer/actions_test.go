// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"github.com/latere-ai/origo/authorizer"
	"github.com/latere-ai/origo/internal/contract"
)

// wire is spec 028's table written out: the four action strings and the
// resource kind exactly as they travel. It is the one deliberate second
// copy of them, here so a rename of the constants cannot rename the
// wire quietly; every other copy in the module is a compile error by
// TestTheActionStringsHaveOneHomeInTheModule below.
var wire = []authz.Action{
	{Name: "repo.read", Kind: "Repository"},
	{Name: "repo.write", Kind: "Repository"},
	{Name: "repo.admin", Kind: "Repository"},
	{Name: "repo.list", Kind: "Repository"},
}

// TestTheVocabularyIsSpec028sTable: the vocabulary is the four rows in
// the spec's order, and Actions, Kind, and Known read that same value.
func TestTheVocabularyIsSpec028sTable(t *testing.T) {
	v := authorizer.Vocabulary()
	if v.Core != "origo" {
		t.Errorf("the vocabulary belongs to %q", v.Core)
	}
	if !slices.Equal(v.Actions, wire) {
		t.Fatalf("the table is %v, want %v", v.Actions, wire)
	}
	if got, want := authorizer.Actions(), []string{"repo.read", "repo.write", "repo.admin", "repo.list"}; !slices.Equal(got, want) {
		t.Errorf("Actions() = %v, want %v", got, want)
	}
	for _, a := range wire {
		if !authorizer.Known(a.Name) {
			t.Errorf("Known(%s) is false", a.Name)
		}
		if k := authorizer.Kind(a.Name); k != a.Kind {
			t.Errorf("Kind(%s) = %q, want %q", a.Name, k, a.Kind)
		}
	}
	for _, a := range []string{"", "repo", "read", "repo.rename", "Repo.Read"} {
		if authorizer.Known(a) || authorizer.Kind(a) != "" {
			t.Errorf("%q is outside the table and reads as %q", a, authorizer.Kind(a))
		}
	}
}

// TestThePageActionIsTheDirectoryAndNothingElse: repo.list is the one
// action whose answer is a page of Origo's own shape (spec 026), and the
// shared vocabulary carries no flag for it, so the package names it for
// authz/server's Options.PageActions. The three that answer a decision
// are not named.
func TestThePageActionIsTheDirectoryAndNothingElse(t *testing.T) {
	pages := authorizer.PageActions()
	if !slices.Equal(pages, []string{authorizer.ActionList}) {
		t.Fatalf("PageActions() = %v, want [%s]", pages, authorizer.ActionList)
	}
	for _, a := range pages {
		if !authorizer.Known(a) {
			t.Errorf("PageActions names %q, which is outside the vocabulary", a)
		}
	}
	if !authz.IsList(authorizer.ActionList) {
		t.Errorf("%s does not read as a list action", authorizer.ActionList)
	}
}

// TestEveryReaderGetsItsOwnCopy: Vocabulary, Actions, and PageActions
// answer a fresh value every call, so a consumer that sorts or trims
// what it gets does not revoke the table for the next one.
func TestEveryReaderGetsItsOwnCopy(t *testing.T) {
	v := authorizer.Vocabulary()
	v.Actions[0] = authz.Action{Name: "repo.rename", Kind: "Nothing"}
	if authorizer.Vocabulary().Actions[0].Name != authorizer.ActionRead {
		t.Fatal("a caller's edit reached the package's table")
	}
	a := authorizer.Actions()
	a[0] = "repo.rename"
	if authorizer.Actions()[0] != authorizer.ActionRead {
		t.Fatal("Actions hands out the same slice twice")
	}
	p := authorizer.PageActions()
	p[0] = "repo.read"
	if authorizer.PageActions()[0] != authorizer.ActionList {
		t.Fatal("PageActions hands out the same slice twice")
	}
}

// TestTheNodeAndTheClientReadTheSameStrings: internal/contract is what
// the node writes into details.action and what the agent client
// branches on, and it reads the four strings from this package rather
// than declaring them.
func TestTheNodeAndTheClientReadTheSameStrings(t *testing.T) {
	for _, pair := range [][2]string{
		{contract.ActionRead, authorizer.ActionRead},
		{contract.ActionWrite, authorizer.ActionWrite},
		{contract.ActionAdmin, authorizer.ActionAdmin},
		{contract.ActionList, authorizer.ActionList},
	} {
		if pair[0] != pair[1] {
			t.Errorf("internal/contract says %q where the vocabulary says %q", pair[0], pair[1])
		}
		if !authorizer.Known(pair[0]) {
			t.Errorf("internal/contract names %q, which is outside the vocabulary", pair[0])
		}
	}
}

// TestTheActionStringsHaveOneHomeInTheModule is the check the equality
// above cannot make: constants that alias each other are equal by
// construction, and the question is whether a second declaration exists
// at all. Every string literal of the table in the module's shipped Go
// source is in this package's actions.go, so a rename is one edit and
// no second home can drift from it.
//
// Two places are outside the walk and write the strings out on purpose.
// A test pins the wire: an observable value checked against the constant
// that produced it agrees with any bug by construction. test/ is the
// same argument one level up — the stub of spec 013 answers the contract
// as an operator's endpoint would, and the conformance suite of spec 021
// drives an installation it did not build, so both speak the wire rather
// than the node's constants.
func TestTheActionStringsHaveOneHomeInTheModule(t *testing.T) {
	table := []string{"repo.read", "repo.write", "repo.admin", "repo.list", "Repository"}
	home := filepath.Join("authorizer", "actions.go")
	fset := token.NewFileSet()
	err := filepath.WalkDir(moduleRoot(t), func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			switch name := d.Name(); {
			case name == "testdata", name == "out", name == "web",
				strings.HasPrefix(name, ".") && name != ".",
				path == filepath.Join(moduleRoot(t), "test"):
				return fs.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		rel, err := filepath.Rel(moduleRoot(t), path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !slices.Contains(table, s) || rel == home {
				return true
			}
			t.Errorf("%s:%d: the literal %q; read it from github.com/latere-ai/origo/authorizer",
				rel, fset.Position(lit.Pos()).Line, s)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// moduleRoot is the checkout, resolved from this file with
// runtime.Caller and never from the working directory, so the gates that
// run the suite from an empty directory see the module's files.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(file))
}
