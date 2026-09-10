// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package release

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// workflowJob is the part of a job release.yml's shape depends on: what
// it needs and the condition it runs under.
type workflowJob struct {
	needs []string
	cond  string
}

var (
	jobID     = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
	jobNeeds  = regexp.MustCompile(`^    needs:\s*(.*?)\s*$`)
	jobIf     = regexp.MustCompile(`^    if:\s*(.*?)\s*$`)
	blockItem = regexp.MustCompile(`^      -\s*(\S+)\s*$`)
)

// parseJobs reads the `jobs:` mapping of a workflow. The file's shape is
// fixed: a job id at indent 2, its keys at indent 4, `needs` a scalar, an
// inline list, or a block list, and `if` one line. Anything this cannot
// read leaves the job's fields empty, which the callers check for.
func parseJobs(text string) map[string]workflowJob {
	jobs := map[string]workflowJob{}
	var inJobs bool
	var current string
	var collecting bool
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case line == "jobs:":
			inJobs, current, collecting = true, "", false
			continue
		case !inJobs:
			continue
		case line != "" && !strings.HasPrefix(line, " "):
			inJobs, current, collecting = false, "", false
			continue
		}
		if m := jobID.FindStringSubmatch(line); m != nil {
			current, collecting = m[1], false
			jobs[current] = workflowJob{}
			continue
		}
		if current == "" {
			continue
		}
		if collecting {
			if m := blockItem.FindStringSubmatch(line); m != nil {
				j := jobs[current]
				j.needs = append(j.needs, strings.Trim(m[1], `"',`))
				jobs[current] = j
				continue
			}
			collecting = false
		}
		if m := jobNeeds.FindStringSubmatch(line); m != nil {
			j := jobs[current]
			value := m[1]
			if value == "" {
				collecting = true
			} else {
				value = strings.Trim(value, "[]")
				for n := range strings.SplitSeq(value, ",") {
					if n = strings.Trim(strings.TrimSpace(n), `"'`); n != "" {
						j.needs = append(j.needs, n)
					}
				}
			}
			jobs[current] = j
			continue
		}
		if m := jobIf.FindStringSubmatch(line); m != nil {
			j := jobs[current]
			j.cond = m[1]
			jobs[current] = j
		}
	}
	return jobs
}

// silentlySkipped answers the jobs GitHub skips when one job is skipped.
// The implicit `success()` gate of a job with no `if` is evaluated over
// the whole ancestor closure, not over its direct `needs` alone, so the
// skip travels through an intermediate job that runs under `always()`.
// Only a descendant whose own condition says `always()` survives it.
func silentlySkipped(jobs map[string]workflowJob, skipped string) []string {
	var out []string
	for name, j := range jobs {
		if name == skipped || !dependsOn(jobs, name, skipped) {
			continue
		}
		if !strings.Contains(j.cond, "always()") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func dependsOn(jobs map[string]workflowJob, name, ancestor string) bool {
	for _, n := range jobs[name].needs {
		if n == ancestor || dependsOn(jobs, n, ancestor) {
			return true
		}
	}
	return false
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestReleaseSurvivesASkippedDeploy holds spec 017's rule that a tag on a
// repository with ORIGO_RELEASE_DEPLOY unset publishes every artifact and
// verifies what it published: `deploy` is skipped there, and a job that
// leans on the implicit success() gate anywhere below it is skipped with
// it, however far down the graph. Run 34450584556 of v0.1.0 skipped
// `install-release` and `release-verify` that way, so the release was
// published and nothing verified it.
func TestReleaseSurvivesASkippedDeploy(t *testing.T) {
	jobs := parseJobs(readWorkflow(t, "release.yml"))
	for _, name := range []string{"build", "conformance", "deploy", "live", "publish", "install-release", "release-verify"} {
		if _, ok := jobs[name]; !ok {
			t.Fatalf("release.yml has no job %q; the parser or the workflow changed shape", name)
		}
	}
	if got := silentlySkipped(jobs, "deploy"); len(got) != 0 {
		t.Errorf("a skipped deploy silently skips %v; each needs always() in its if", got)
	}
}

// TestSilentSkipIsFound proves the check on the shape release.yml had
// when the v0.1.0 tag published an unverified release: publish runs under
// always(), and the two jobs below it carry no condition at all.
func TestSilentSkipIsFound(t *testing.T) {
	const before = `name: Release
jobs:
  build:
    runs-on: ubuntu-latest
  deploy:
    needs: build
    if: ${{ vars.ORIGO_RELEASE_DEPLOY != '' }}
  publish:
    needs: [build, deploy]
    if: ${{ always() && needs.build.result == 'success' }}
  install-release:
    needs: publish
  release-verify:
    needs:
      - publish
`
	jobs := parseJobs(before)
	if len(jobs) != 5 {
		t.Fatalf("parsed %d jobs, want 5: %v", len(jobs), jobs)
	}
	if got, want := jobs["release-verify"].needs, []string{"publish"}; !slices.Equal(got, want) {
		t.Errorf("block list needs = %v, want %v", got, want)
	}
	got := silentlySkipped(jobs, "deploy")
	if want := []string{"install-release", "release-verify"}; !slices.Equal(got, want) {
		t.Errorf("silentlySkipped = %v, want %v", got, want)
	}
}
