// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package contract

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// producers is the spec that produces each code: the spec whose handler
// sends it, which for three codes of spec 003's table is a later spec.
// The call-site rule below runs for the codes whose producer is at
// testing or complete, read from the spec's frontmatter, so a row whose
// handler is not built yet does not fail the table (spec 021).
var producers = map[string]string{
	CodeInvalid:               "003",
	CodeUnauthenticated:       "003",
	CodeForbidden:             "003",
	CodeRepoNotFound:          "003",
	CodeRepoExists:            "003",
	CodeNonFastForward:        "003",
	CodeStorageUnavailable:    "003",
	CodeAuthorizerUnavailable: "007",
	CodeRefNotFound:           "009",
	CodeBlobTooLarge:          "009",
	CodeOperationTimeout:      "009",
	CodeLFSObjectMismatch:     "010",
	CodeLFSObjectNotStored:    "010",
	CodeLFSLocksUnsupported:   "010",
	CodeOverQuota:             "012",
	CodeRateLimited:           "012",
	CodeRepositoryUnavailable: "015",
	CodeGone:                  "019",
	CodeRepoFrozen:            "019",
	CodeRepoImporting:         "019",
	CodeRepoNotEmpty:          "019",
	CodeImportNotFound:        "019",
	CodeMergeConflict:         "020",
	CodeInvalidChange:         "020",
	CodeDirectoryUnsupported:  "026",
}

// moduleRoot is the checkout, resolved from this file with
// runtime.Caller and never from the working directory, so the gates
// that run the suite from an empty directory see the module's files.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// finding is one failure of the walk: the file, the line, and why.
type finding struct {
	file string
	line int
	why  string
}

func (f finding) String() string { return fmt.Sprintf("%s:%d: %s", f.file, f.line, f.why) }

// walker reads Go files with go/ast alone: the selectors on the import
// names httpjson and contract, and nothing that needs a type checker.
type walker struct {
	fset *token.FileSet
	// httpStatus is net/http's Status* constants, parsed from GOROOT.
	httpStatus map[string]int
	// callSites counts the codes a checked call passes.
	callSites map[string]int
	findings  []finding
}

// checkedCalls are the contract functions that take a code, with the
// position of the code argument: the three spec 021 names, and Refuse
// and Line, which this package added beside them and which are the
// other two ways a code leaves the table. A Code* identifier is
// required as the code argument, and Write and Refuse send it under a
// status of its row.
var checkedCalls = map[string]int{"Write": 2, "Error": 0, "Sentence": 0, "Refuse": 1, "Line": 0}

// statusIndex is the position of the status argument of the calls that
// carry one.
var statusIndex = map[string]int{"Write": 1, "Refuse": 0}

// file walks one parsed file. contractPkg says whether the file is in
// internal/contract, where the envelope literal is allowed.
func (w *walker) file(f *ast.File, rel string, contractPkg bool) {
	names := map[string]string{}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		var want string
		switch path {
		case "latere.ai/x/pkg/httpjson":
			want = "httpjson"
		case "github.com/latere-ai/origo/internal/contract":
			want = "contract"
		default:
			continue
		}
		if imp.Name != nil && imp.Name.Name != want {
			w.add(imp.Pos(), rel, "the package "+path+" is imported as "+imp.Name.Name+", which the code-table walk cannot follow")
			continue
		}
		names[want] = want
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if !contractPkg && selectorOf(x.Type, names, "httpjson") == "Error" {
				w.add(x.Pos(), rel, "an httpjson.Error literal outside internal/contract; render it through contract.Write")
			}
		case *ast.CallExpr:
			if !contractPkg && selectorOf(x.Fun, names, "httpjson") == "WriteError" {
				w.add(x.Pos(), rel, "an httpjson.WriteError call outside internal/contract; render it through contract.Write")
			}
			w.contractCall(x, rel, names)
		}
		return true
	})
}

// contractCall checks one call of a contract function: the code
// argument is a Code* identifier, and the status argument of Write and
// Refuse is a status of that code's row.
func (w *walker) contractCall(call *ast.CallExpr, rel string, names map[string]string) {
	fn := selectorOf(call.Fun, names, "contract")
	codeArg, checked := checkedCalls[fn]
	if !checked || len(call.Args) <= codeArg {
		return
	}
	code, ok := codeConstant(call.Args[codeArg], names)
	if !ok {
		w.add(call.Args[codeArg].Pos(), rel, "contract."+fn+" is called with a code that is not a contract.Code* constant")
		return
	}
	value, known := codeValue(code)
	if !known {
		w.add(call.Args[codeArg].Pos(), rel, "contract."+code+" is not a constant of the code table")
		return
	}
	w.callSites[value]++
	idx, hasStatus := statusIndex[fn]
	if !hasStatus {
		return
	}
	status, ok := w.statusOf(call.Args[idx])
	if !ok {
		w.add(call.Args[idx].Pos(), rel, "contract."+fn+" is called with a status that is neither an integer literal nor an http.Status* constant")
		return
	}
	if !slices.Contains(statuses[value], status) {
		w.add(call.Args[idx].Pos(), rel, fmt.Sprintf("contract.%s sends %s under %d, which the code's row does not list (%v)", fn, value, status, statuses[value]))
	}
}

// statusOf resolves a status argument: an integer literal or an
// http.Status* selector.
func (w *walker) statusOf(e ast.Expr) (int, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.INT {
			n, err := strconv.Atoi(x.Value)
			return n, err == nil
		}
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "http" {
			n, ok := w.httpStatus[x.Sel.Name]
			return n, ok
		}
	}
	return 0, false
}

func (w *walker) add(pos token.Pos, rel, why string) {
	w.findings = append(w.findings, finding{file: rel, line: w.fset.Position(pos).Line, why: why})
}

// selectorOf answers Sel of pkg.Sel when pkg is the import name of the
// package, and "" otherwise.
func selectorOf(e ast.Expr, names map[string]string, pkg string) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || names[pkg] == "" || id.Name != names[pkg] {
		return ""
	}
	return sel.Sel.Name
}

// codeConstant answers the name of a contract.Code* identifier.
func codeConstant(e ast.Expr, names map[string]string) (string, bool) {
	name := selectorOf(e, names, "contract")
	if !strings.HasPrefix(name, "Code") {
		return "", false
	}
	return name, true
}

// codeConstants maps the constant names of this package to their
// values, parsed from contract.go, so the walk resolves an identifier
// without a type checker.
var codeConstants = func() map[string]string {
	fset := token.NewFileSet()
	_, file, _, _ := runtime.Caller(0)
	f, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(file), "contract.go"), nil, 0)
	if err != nil {
		panic(err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Code") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					out[name.Name] = v
				}
			}
		}
	}
	return out
}()

func codeValue(name string) (string, bool) {
	v, ok := codeConstants[name]
	return v, ok
}

// httpStatuses parses net/http's Status* integer constants from GOROOT
// with the same parser, so an http.StatusNotFound argument resolves to
// 404 without a type checker.
func httpStatuses(t *testing.T) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(build.Default.GOROOT, "src", "net", "http", "status.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Status") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
					out[name.Name], _ = strconv.Atoi(lit.Value)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Status* constants parsed from net/http")
	}
	return out
}

func newWalker(t *testing.T) *walker {
	t.Helper()
	return &walker{fset: token.NewFileSet(), httpStatus: httpStatuses(t), callSites: map[string]int{}}
}

// parse reads one file by path and feeds it to the walk under the name
// rel; contractPkg says whether it sits in internal/contract.
func (w *walker) parse(t *testing.T, path, rel string, contractPkg bool) {
	t.Helper()
	f, err := parser.ParseFile(w.fset, path, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	w.file(f, rel, contractPkg)
}

// walkModule reads every Go file of the module, skipping tools/, every
// testdata directory, and the hidden directories of the checkout.
func (w *walker) walkModule(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel == "tools" || rel == "out" || d.Name() == "testdata" || (strings.HasPrefix(d.Name(), ".") && rel != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		w.parse(t, path, filepath.ToSlash(rel), strings.HasPrefix(filepath.ToSlash(rel), "internal/contract/"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// specStatus reads the status: line of the frontmatter of
// specs/<nnn>-*.md, or of specs/.archive/<nnn>-*.md where a terminal
// spec sits.
func specStatus(t *testing.T, root, number string) string {
	t.Helper()
	var matches []string
	for _, dir := range []string{"specs", filepath.Join("specs", ".archive")} {
		found, _ := filepath.Glob(filepath.Join(root, dir, number+"-*.md"))
		matches = append(matches, found...)
	}
	if len(matches) != 1 {
		t.Fatalf("spec %s: %d files match under specs/ and specs/.archive/", number, len(matches))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^status:\s*(\S+)\s*$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("spec %s has no status: line", number)
	}
	return m[1]
}

// TestEveryCodeHasOneSentence is spec 021's table check. The table
// itself: every code constant has one sentence and at least one status,
// Codes lists every constant, Status answers the first status, and the
// producers table names every code and nothing else. The module: no
// httpjson.Error literal or httpjson.WriteError call outside this
// package, every contract.Write, Error, Sentence, Refuse, and Line call
// passes a Code* constant, every Write and Refuse sends it under a status of
// its row, and every code whose producing spec is at testing or later
// has a call site.
func TestEveryCodeHasOneSentence(t *testing.T) {
	root := moduleRoot(t)
	codes := Codes()
	if len(codes) != len(codeConstants) {
		t.Fatalf("Codes lists %d codes, the package declares %d constants", len(codes), len(codeConstants))
	}
	for name, value := range codeConstants {
		if _, ok := sentences[value]; !ok {
			t.Errorf("%s has no sentence", name)
		}
		if len(statuses[value]) == 0 {
			t.Errorf("%s has no status", name)
		} else if Status(value) != statuses[value][0] {
			t.Errorf("Status(%s) is not the first status of the row", value)
		}
		if _, ok := producers[value]; !ok {
			t.Errorf("%s is not in the producers table", name)
		}
	}
	for code := range producers {
		if _, ok := sentences[code]; !ok {
			t.Errorf("producers names %s, which Codes does not list", code)
		}
	}
	if len(codes) != 25 {
		t.Errorf("the table holds %d codes, spec 021 counts 25", len(codes))
	}
	if got := Statuses(CodeRepoFrozen); !slices.Equal(got, []int{403, 409}) {
		t.Errorf("repo_frozen statuses %v", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("Status of an unknown code did not panic")
		}
	}()

	w := newWalker(t)
	w.walkModule(t, root)
	for _, f := range w.findings {
		t.Error(f)
	}
	built := map[string]bool{}
	for code, number := range producers {
		status := specStatus(t, root, number)
		built[code] = status == "testing" || status == "complete"
	}
	for _, code := range codes {
		if built[code] && w.callSites[code] == 0 {
			t.Errorf("%s (spec %s at testing or later) has no call site of contract.Write, Error, Sentence, Refuse, or Line", code, producers[code])
		}
	}
	Status("nope")
}

// TestTableWalkFailsOnTheNegativeFixture proves the walk can fail: the
// fixture test/conformance/testdata/negative/bad.go.txt, outside this
// package and outside the module walk, yields exactly two findings, the
// httpjson.Error literal at line 16 and the string code at line 17, and
// no status finding, because the status rule runs only on a Code*
// identifier.
func TestTableWalkFailsOnTheNegativeFixture(t *testing.T) {
	root := moduleRoot(t)
	rel := "test/conformance/testdata/negative/bad.go.txt"
	w := newWalker(t)
	w.parse(t, filepath.Join(root, filepath.FromSlash(rel)), rel, false)
	var got []string
	for _, f := range w.findings {
		got = append(got, fmt.Sprintf("%s:%d", f.file, f.line))
	}
	sort.Strings(got)
	want := []string{rel + ":16", rel + ":17"}
	if !slices.Equal(got, want) {
		t.Fatalf("findings %v, want %v:\n%v", got, want, w.findings)
	}
}

// TestTableWalkReportsEveryRule covers the rules the tree never
// trips: an import under another name, an unknown constant, a status
// the row lacks, a variable status, and a WriteError call.
func TestTableWalkReportsEveryRule(t *testing.T) {
	src := `package x

import (
	"net/http"

	hj "latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/contract"
)

var status = 200

func bad(w http.ResponseWriter) {
	hj.WriteError(w, 500, hj.Error{})
	contract.Write(w, http.StatusTeapot, contract.CodeRepoNotFound, nil)
	contract.Write(w, status, contract.CodeRepoNotFound, nil)
	contract.Write(w, 404, contract.CodeNope, nil)
	_ = contract.Sentence(contract.CodeGone)
	_ = contract.Refuse(http.StatusGone, contract.CodeGone, nil)
	_ = contract.Refuse(409, contract.CodeRepoFrozen, nil)
	_ = contract.Line("gone")
}
`
	path := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	w := newWalker(t)
	w.parse(t, path, "x.go", false)
	var lines []int
	for _, f := range w.findings {
		lines = append(lines, f.line)
	}
	sort.Ints(lines)
	if !slices.Equal(lines, []int{6, 15, 16, 17, 21}) {
		t.Fatalf("findings at %v:\n%v", lines, w.findings)
	}
	if w.callSites[CodeGone] != 2 || w.callSites[CodeRepoFrozen] != 1 || w.callSites[CodeRepoNotFound] != 2 {
		t.Fatalf("call sites %v", w.callSites)
	}
}
