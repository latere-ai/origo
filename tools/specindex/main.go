// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command specindex builds the cross-reference table at the end of
// specs/README.md: every error code, configuration variable, metric,
// event kind, failpoint, endpoint, and header the deck defines, which
// spec owns it, and which other specs name it. It is its own module,
// like tools/spike, so it stays out of the service's build.
//
// The parser is the package specs beside this file, exported so
// tools/apidoc renders docs/api.md from the same reading of the same
// tables. A name is defined by a table whose first header cell is Code,
// Variable, Metric, Event, Failpoint, or Header, or whose first two are
// Method and Path; the backticked tokens of the first cell (or METHOD
// plus the path) are the names. Every other backticked token that looks
// like a name is a mention, and a token that carries a header or a
// variable with its value (Origo-Event: push,
// ORIGO_FAILPOINT=commit.before-index) mentions both sides. One spec
// owns each name; a second definition, or a mention of a name no spec
// defines, is a finding, and the test in specs/ fails on findings and on
// a README table that differs from the specs.
//
// The -rules mode reads the PrometheusRule of spec 011: it fails when an
// alert names a metric no spec defines, and prints the plain Prometheus
// rules document inside the object, which is what `promtool check rules`
// parses.
//
//	go run . -write          # rewrite the table in specs/README.md
//	go run . -check          # exit 1 on findings or drift
//	go run . -rules F        # check F's alerts and print them for promtool
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/latere-ai/origo/tools/specindex/specs"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is the command, with its streams and arguments passed in so a test
// drives every mode without a subprocess. It returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("specindex", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("specs", "../../specs", "the spec directory")
	write := fs.Bool("write", false, "rewrite the table in README.md")
	check := fs.Bool("check", false, "exit non-zero on findings or when README.md differs")
	rules := fs.String("rules", "", "the PrometheusRule to check against the deck and print for promtool")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	idx, err := specs.Build(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "specindex:", err)
		return 1
	}
	for _, f := range idx.Findings {
		fmt.Fprintln(stderr, "specindex:", f)
	}
	if *rules != "" {
		document, alerts, err := specs.Rules(*rules)
		if err != nil {
			fmt.Fprintln(stderr, "specindex:", err)
			return 1
		}
		found := idx.CheckRules(alerts)
		for _, f := range found {
			fmt.Fprintln(stderr, "specindex:", f)
		}
		if len(found) > 0 || len(idx.Findings) > 0 {
			return 1
		}
		fmt.Fprint(stdout, document)
		return 0
	}
	readme := filepath.Join(*dir, "README.md")
	table := idx.Table()
	switch {
	case *write:
		if err := specs.Splice(readme, table); err != nil {
			fmt.Fprintln(stderr, "specindex:", err)
			return 1
		}
		fmt.Fprintf(stdout, "specindex: %d names written to %s\n", len(idx.Names), readme)
	case *check:
		current, err := specs.Current(readme)
		if err != nil {
			fmt.Fprintln(stderr, "specindex:", err)
			return 1
		}
		if current != table {
			fmt.Fprintln(stderr, "specindex: README.md table differs from the specs; run go run ./tools/specindex -write")
			return 1
		}
		fmt.Fprintf(stdout, "specindex: %d names, README.md current\n", len(idx.Names))
	default:
		fmt.Fprint(stdout, table)
	}
	if len(idx.Findings) > 0 && (*check || *write) {
		return 1
	}
	return 0
}
