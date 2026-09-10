// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command apidoc renders docs/api.md, the reference a consumer codes
// against: every endpoint, header, and error code the deck defines,
// grouped by the spec that owns it and linked to it, and the one call
// Origo makes rather than serves, the authorization endpoint, carried
// verbatim from the spec that states its contract.
//
// It reads the specs through the package specindex/specs, the parser the
// cross-reference table is built with, so the page carries no name the
// specs do not define and a table shape one tool does not recognize is a
// finding in both. Nothing here decides what an endpoint or a code
// means: each row is the row its spec states.
//
//	go run . -write   # rewrite docs/api.md
//	go run .          # print it
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/latere-ai/origo/tools/specindex/specs"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apidoc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("specs", "../../specs", "the spec directory")
	out := fs.String("out", "../../docs/api.md", "the page to write with -write")
	write := fs.Bool("write", false, "rewrite the page")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	idx, err := specs.Build(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "apidoc:", err)
		return 1
	}
	for _, f := range idx.Findings {
		fmt.Fprintln(stderr, "apidoc:", f)
	}
	if len(idx.Findings) > 0 {
		return 1
	}
	page, err := Page(idx)
	if err != nil {
		fmt.Fprintln(stderr, "apidoc:", err)
		return 1
	}
	if !*write {
		fmt.Fprint(stdout, page)
		return 0
	}
	if err := os.WriteFile(*out, []byte(page), 0o644); err != nil {
		fmt.Fprintln(stderr, "apidoc:", err)
		return 1
	}
	fmt.Fprintf(stdout, "apidoc: wrote %s\n", *out)
	return 0
}
