// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command origod is the Origo server: git hosting with a write-ahead log in
// object storage as the source of truth. The listeners, the log client, and
// the placement logic land with their specs; today the binary reports its
// version and exits, so the release pipeline and the quality gate run
// against a real program from the first commit.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/latere-ai/origo/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the command line and dispatches. It returns the process exit
// code so tests can drive it without a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("origod", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "origod: the server is not implemented yet; see specs/README.md")
	return 1
}
