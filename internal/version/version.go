// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package version reports the build identity of the running binary. The
// values are set at link time by the release pipeline and default to
// development markers so a local build is never mistaken for a release.
package version

import "fmt"

// Set by -ldflags "-X github.com/latere-ai/origo/internal/version.Version=..." at
// build time.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders the identity in the form printed by `origod -version` and
// reported by the /version endpoint.
func String() string {
	return fmt.Sprintf("origod %s (%s, %s)", Version, Commit, Date)
}
