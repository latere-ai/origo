// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command configdoc writes docs/configuration.md from internal/config,
// where the reference lives beside the code that reads it: a default on
// the page is the constant the binary uses, and a variable the deck
// defines with no row in that table fails the test in that package.
//
//	go run ./tools/configdoc -write   # rewrite the page
//	go run ./tools/configdoc          # print it
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/latere-ai/origo/internal/config"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("configdoc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "docs/configuration.md", "the page to write with -write")
	write := fs.Bool("write", false, "rewrite the page")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	page := config.Document()
	if !*write {
		_, _ = fmt.Fprint(stdout, page)
		return 0
	}
	if err := os.WriteFile(*out, []byte(page), 0o644); err != nil {
		_, _ = fmt.Fprintln(stderr, "configdoc:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "configdoc: wrote %s\n", *out)
	return 0
}
