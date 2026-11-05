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

// jobText is the lines of one job of a workflow, from its id to the next
// key at the same indent, and empty when the workflow has no such job.
func jobText(text, name string) string {
	var b strings.Builder
	in := false
	for line := range strings.SplitSeq(text, "\n") {
		if m := jobID.FindStringSubmatch(line); m != nil {
			in = m[1] == name
		}
		if in {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
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

// The shared image release stages of latere-ai/ci the workflow calls, as
// the start of a `uses:` reference; the pin after the @ is the
// workflow's own.
const (
	stageBuild   = "uses: latere-ai/ci/.github/workflows/image-build.yml@"
	stageDeploy  = "uses: latere-ai/ci/.github/workflows/image-deploy.yml@"
	stagePublish = "uses: latere-ai/ci/.github/workflows/image-publish.yml@"
	stageVerify  = "uses: latere-ai/ci/.github/workflows/release-verify.yml@"
)

// stageJobs answers the jobs of a workflow that call the stage, sorted.
func stageJobs(text string, jobs map[string]workflowJob, stage string) []string {
	var out []string
	for name := range jobs {
		if strings.Contains(jobText(text, name), stage) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// TestChecksumsCoverEveryArchiveTheReleasePublishes holds spec 017's rule
// that checksums.txt sums every archive an operator downloads, the deploy
// archive and the fixture with the binaries, and that the published release
// is checked against it. The publish stage sums and signs every file the
// build stage left in dist/ and every run artifact its `artifacts` input
// names, so the rule holds when build writes the binary archives and the
// deploy archive into dist/, publish names the fixture conformance uploads
// and needs both jobs, and a job after publish checks that the sums name
// the deploy archive and the fixture. When build signed sums over the
// binaries alone, an operator who ran `sha256sum -c --ignore-missing` over a
// downloaded deploy archive, as docs/upgrades/README.md says to, checked
// nothing.
func TestChecksumsCoverEveryArchiveTheReleasePublishes(t *testing.T) {
	text := readWorkflow(t, "release.yml")
	jobs := parseJobs(text)
	for stage, want := range map[string]string{stageBuild: "build", stagePublish: "publish", stageVerify: "release-verify"} {
		if got := stageJobs(text, jobs, stage); !slices.Equal(got, []string{want}) {
			t.Fatalf("jobs with %q = %v, want [%s]", stage, got, want)
		}
	}
	build := jobText(text, "build")
	for _, want := range []string{"cp out/release/*.tar.gz dist/", `"dist/deploy-${TAG}.tar.gz"`} {
		if !strings.Contains(build, want) {
			t.Errorf("build does not leave an archive in dist/ for the sums: %s is missing", want)
		}
	}
	// conformance uploads the fixture with if-no-files-found: error, and
	// publish runs only when conformance succeeded, so a release that
	// reaches publish has the fixture for the stage to download and sum.
	if !strings.Contains(jobText(text, "conformance"), "name: release-fixture") {
		t.Error("conformance does not upload the fixture as release-fixture")
	}
	if !strings.Contains(jobText(text, "publish"), "artifacts: release-fixture") {
		t.Error("publish does not attach release-fixture, so checksums.txt leaves the fixture out")
	}
	for _, packer := range []string{"build", "conformance"} {
		if !dependsOn(jobs, "publish", packer) {
			t.Errorf("publish does not need %s, which packs an archive the sums cover", packer)
		}
	}
	// release-verify checks every attached file against the sums; that the
	// deploy archive and the fixture are among them is Origo's own claim.
	install := jobText(text, "install-release")
	for _, archive := range []string{"deploy-${TAG}.tar.gz", "fixture-${TAG}.tar.gz"} {
		if !strings.Contains(install, `grep -q " `+archive+`$" checksums.txt`) {
			t.Errorf("install-release does not check that checksums.txt names %s", archive)
		}
	}
	for _, checker := range []string{"install-release", "release-verify"} {
		if !dependsOn(jobs, checker, "publish") {
			t.Errorf("%s does not need publish, so it can check a release before it exists", checker)
		}
	}
}

// TestReleaseSignsOnlyThroughTheStages holds the shared stages' rule that
// the signatures, the bills of materials, the attestations and the image
// tags of a release are made by latere-ai/ci's stages alone, so every
// Latere image is verified with one command shape and the tools' pins live
// in ci. A step here that signed or attested an image itself would sign as
// this repository's workflow, which docs/upgrades/README.md does not tell an
// operator to accept for a current release.
func TestReleaseSignsOnlyThroughTheStages(t *testing.T) {
	for i, line := range strings.Split(readWorkflow(t, "release.yml"), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") {
			continue
		}
		for _, tool := range []string{"cosign", "sigstore/", "anchore/sbom-action", "actions/attest-", "docker buildx imagetools"} {
			if strings.Contains(code, tool) {
				t.Errorf("release.yml:%d calls %s directly: %s", i+1, tool, code)
			}
		}
	}
}

// pipeIntoGrepQ finds every line of a workflow that ends a pipeline in
// `grep -q`. Under `set -o pipefail`, which every run block here sets,
// grep closes the pipe on its first match and the producer dies with
// EPIPE, so the step fails or passes on whether the producer's output
// happened to fit in the 64 KiB pipe buffer before grep exited. Run
// 34546335576 of v0.1.3 failed the release verification that way, with
// `tar: stdout: write error`, after one file was added to the archive
// being listed. The producer's output belongs in a file that grep then
// reads.
func pipeIntoGrepQ(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") || !strings.Contains(t, "|") {
			continue
		}
		last := strings.TrimSpace(t[strings.LastIndex(t, "|")+1:])
		if strings.HasPrefix(last, "grep -q") {
			out = append(out, t)
		}
	}
	return out
}

// TestNoStepPipesIntoGrepQ keeps the release workflow free of the
// broken-pipe race above: a check that reads a listing must not decide
// its own outcome on the size of what it listed.
func TestNoStepPipesIntoGrepQ(t *testing.T) {
	for _, name := range []string{"release.yml", "verify.yml"} {
		if got := pipeIntoGrepQ(readWorkflow(t, name)); len(got) != 0 {
			for _, line := range got {
				t.Errorf("%s pipes into grep -q under pipefail: %s", name, line)
			}
		}
	}
}

// TestPipeIntoGrepQIsFound proves the check on the line that failed.
func TestPipeIntoGrepQIsFound(t *testing.T) {
	const before = "          tar -tzf \"deploy-${TAG}.tar.gz\" | grep -q '^deploy/base/deployment.yaml$'"
	if got := pipeIntoGrepQ(before); len(got) != 1 {
		t.Fatalf("the failing line was not found: %v", got)
	}
	const after = "          tar -tzf \"deploy-${TAG}.tar.gz\" > listing.txt"
	if got := pipeIntoGrepQ(after); len(got) != 0 {
		t.Errorf("the fixed line was flagged: %v", got)
	}
}

// TestReleasePublishesUnderTheRepositoryOwnersNamespace is spec 017's
// fork row as far as a test can hold it: the workflow fixes no image
// namespace of its own. The build stage derives it from the repository
// variable RELEASE_IMAGE_NAMESPACE or, unset, from the owner of the
// repository the tag was pushed to, lowered, so no stage is handed one
// here. Every job after the build reads both image names from the build
// stage's outputs, and the deploy archive takes its namespace from the
// reference the build pushed, so a fork's tag pushes, signs, attests,
// deploys, and verifies its own packages. The default namespace may
// still be written where the workflow rewrites it into the manifests,
// which is a source of a substitution and not a name anything is
// published under; every other mention is a name a fork cannot write.
func TestReleasePublishesUnderTheRepositoryOwnersNamespace(t *testing.T) {
	text := readWorkflow(t, "release.yml")
	for _, want := range []string{
		"ORIGOD_IMAGE: ${{ needs.build.outputs.ref_1 }}",
		"STUBS_IMAGE: ${{ needs.build.outputs.ref_2 }}",
		`ORIGO_IMAGE_NAMESPACE="${IMAGE_1%/*}"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("release.yml does not take the image names from the build stage: %s is missing", want)
		}
	}
	for i, line := range strings.Split(text, "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "namespace:") {
			t.Errorf("release.yml:%d hands a stage a namespace of its own: %s", i+1, code)
		}
		if !strings.Contains(line, "ghcr.io/latere-ai") {
			continue
		}
		// A rewrite names the namespace it replaces and the one it
		// writes; anything else fixes a name a fork cannot publish to.
		if strings.Contains(line, "ORIGOD_IMAGE") || strings.Contains(line, "IMAGE_NAMESPACE") {
			continue
		}
		// The deploy stage is given the overlay's name for the image next
		// to the ref the build pushed, which is the same substitution.
		if strings.Contains(line, "=${{ needs.build.outputs.ref_1 }}") {
			continue
		}
		t.Errorf("release.yml:%d fixes an image namespace a fork cannot write: %s", i+1, code)
	}
}

// TestTheDeployJobCallsTheDeployStageWithOnlyTheDeployedImage holds the
// deploy job to the shared stage: it is gated by the repository variable,
// hands the stage the one image deploy/prod runs under the overlay's own
// name for it, by the digest the build pushed, and maps its kubeconfig
// secret onto the stage's. The build also pushes origo-stubs, which the
// overlay does not name, and the stage fails on an image it cannot place, so
// that one is never passed. live and publish still wait for the job.
func TestTheDeployJobCallsTheDeployStageWithOnlyTheDeployedImage(t *testing.T) {
	text := readWorkflow(t, "release.yml")
	jobs := parseJobs(text)
	if got := stageJobs(text, jobs, stageDeploy); !slices.Equal(got, []string{"deploy"}) {
		t.Fatalf("jobs with %q = %v, want [deploy]", stageDeploy, got)
	}
	deploy := jobText(text, "deploy")
	for _, want := range []string{
		"\n    if: ${{ vars.ORIGO_RELEASE_DEPLOY != '' }}",
		"\n      images: ghcr.io/latere-ai/origod=${{ needs.build.outputs.ref_1 }}@${{ needs.build.outputs.digest_1 }}\n",
		"\n      deployments: origod\n",
		"\n      url: https://code.latere.ai\n",
		"\n      DEPLOY_KUBECONFIG: ${{ secrets.ORIGO_KUBECONFIG }}",
		"\n      contents: read",
	} {
		if !strings.Contains(deploy, want) {
			t.Errorf("the deploy job does not carry %q", strings.TrimSpace(want))
		}
	}
	for _, not := range []string{"needs.build.outputs.images", "ref_2", "digest_2", "kubectl", "sed -i", "LANDING", "tools/smoke/release.sh"} {
		if strings.Contains(deploy, not) {
			t.Errorf("the deploy job names %q; it passes only origod, and the stage runs the rest", not)
		}
	}
	for _, waiter := range []string{"live", "publish"} {
		if !dependsOn(jobs, waiter, "deploy") {
			t.Errorf("%s does not need deploy", waiter)
		}
	}
}
