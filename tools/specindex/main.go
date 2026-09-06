// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command specindex builds the cross-reference table at the end of
// specs/README.md: every error code, configuration variable, metric,
// event kind, failpoint, endpoint, and header the deck defines, which
// spec owns it, and which other specs name it. It is its own module,
// like tools/spike, so it stays out of the service's build.
//
// A name is defined by a table whose first header cell is Code, Variable,
// Metric, Event, Failpoint, or Header, or whose first two are Method and
// Path; the backticked tokens of the first cell (or METHOD plus the path)
// are the names. Every other backticked token that looks like a name is
// a mention, and a token that carries a header or a variable with its
// value (Origo-Event: push, ORIGO_FAILPOINT=commit.before-index) mentions
// both sides. One spec owns each name; a second definition, or a mention
// of a name no spec defines, is a finding, and the test in this directory
// fails on findings and on a README table that differs from the specs.
//
//	go run . -write          # rewrite the table in specs/README.md
//	go run . -check          # exit 1 on findings or drift
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	specs := flag.String("specs", "../../specs", "the spec directory")
	write := flag.Bool("write", false, "rewrite the table in README.md")
	check := flag.Bool("check", false, "exit non-zero on findings or when README.md differs")
	flag.Parse()

	idx, err := Build(*specs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "specindex:", err)
		os.Exit(1)
	}
	for _, f := range idx.Findings {
		fmt.Fprintln(os.Stderr, "specindex:", f)
	}
	readme := filepath.Join(*specs, "README.md")
	table := idx.Table()
	switch {
	case *write:
		if err := Splice(readme, table); err != nil {
			fmt.Fprintln(os.Stderr, "specindex:", err)
			os.Exit(1)
		}
		fmt.Printf("specindex: %d names written to %s\n", len(idx.Names), readme)
	case *check:
		current, err := Current(readme)
		if err != nil {
			fmt.Fprintln(os.Stderr, "specindex:", err)
			os.Exit(1)
		}
		if current != table {
			fmt.Fprintln(os.Stderr, "specindex: README.md table differs from the specs; run go run ./tools/specindex -write")
			os.Exit(1)
		}
		fmt.Printf("specindex: %d names, README.md current\n", len(idx.Names))
	default:
		fmt.Print(table)
	}
	if len(idx.Findings) > 0 && (*check || *write) {
		os.Exit(1)
	}
}
