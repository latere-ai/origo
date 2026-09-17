// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file was TestTheSharedVerifierCannotCarrySpec007, the waiver of
// .lateregate.yaml written as an assertion: it held the rows spec 007
// asked and latere.ai/x/pkg/authkit/jwt could not answer, and it reddened
// when they closed. pkg v0.74.0 closed the last three. `jwt.Config.Now` is
// the one clock the exp, nbf and iat windows, the key-set cache and the
// refresh back-off are all read against; discovery runs OpenID Connect
// Discovery 4.3 on the `Issuers` path; and a key set that cannot be read
// is `ErrIssuerUnavailable`, reason `issuer_unavailable`, where a cached
// set, however stale, still answers.
//
// So the waiver is retired and the test is inverted. What it holds now is
// the move itself: the shared verifier is in the build list, and the token
// path takes no token apart. A signature verified here again, or a segment
// decoded here again, is the duplication spec 028 removed coming back.
//
// The token path is token.go and verifier.go. mint.go signs the
// repository-bound tokens of spec 007 and keys.go reads the two documents
// `origod check` proves an issuer with, so both hold a key and base64 by
// the work they do; neither reads a token presented to the node.
const tokenPath = "token.go, verifier.go"

// TestTheTokenPathTakesNoTokenApart is the inverse of the waiver.
func TestTheTokenPathTakesNoTokenApart(t *testing.T) {
	t.Run("the shared verifier is in the build list", theSharedVerifierIsInTheBuildList)
	t.Run("the token path parses no JWT of its own", theTokenPathParsesNoJWTOfItsOwn)
}

// theSharedVerifierIsInTheBuildList is the move read off the build rather
// than off an import line: internal/auth reaches authkit/jwt, whatever
// file names it.
func theSharedVerifierIsInTheBuildList(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	if !slices.Contains(strings.Fields(string(out)), "latere.ai/x/pkg/authkit/jwt") {
		t.Error("internal/auth does not reach latere.ai/x/pkg/authkit/jwt: the verification is its own again")
	}
}

// theTokenPathParsesNoJWTOfItsOwn reads the two files a token is decided
// in and refuses a signature or a segment decoded there: the shared
// verifier holds the parser, the key selection and the signature, and what
// is left is the rows of spec 007's table it does not carry.
func theTokenPathParsesNoJWTOfItsOwn(t *testing.T) {
	// The packages a token is taken apart with. A call into any of them
	// from the token path is the duplication coming back; crypto/ecdsa is
	// named by VerifierOptions.LocalKey as a type, which is the node's own
	// key and not a token, so the ban is on calls and not on the import.
	banned := map[string]bool{"base64": true, "rsa": true, "ecdsa": true, "ed25519": true}
	for file := range strings.SplitSeq(tokenPath, ", ") {
		f, err := parser.ParseFile(token.NewFileSet(), strings.TrimSpace(file), nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, spec := range f.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if path == "encoding/base64" || path == "crypto/rsa" {
				t.Errorf("%s imports %s: the token path decodes a segment or checks a signature again", file, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && banned[pkg.Name] {
				t.Errorf("%s calls %s.%s: the token path takes a token apart again", file, pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
}
