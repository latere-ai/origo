// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/origo/internal/config"
)

// repoRoot is the checkout, resolved from this file with runtime.Caller
// and never from the working directory, so the gates that run the suite
// from an empty directory see the files (spec 011's rule for a test that
// reads outside its package).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// job is one job of tiers.yml as far as this test reads it: its text
// from its name line to the next job's.
type job struct {
	name, text string
}

// jobs splits the workflow into its jobs by the two-space indented keys
// under jobs:.
func jobs(t *testing.T, workflow string) map[string]job {
	t.Helper()
	_, body, ok := strings.Cut(workflow, "\njobs:\n")
	if !ok {
		t.Fatal("tiers.yml has no jobs section")
	}
	out := map[string]job{}
	heading := regexp.MustCompile(`(?m)^  ([a-z0-9-]+):\n`)
	locs := heading.FindAllStringSubmatchIndex(body, -1)
	for i, loc := range locs {
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		name := body[loc[2]:loc[3]]
		out[name] = job{name: name, text: body[loc[0]:end]}
	}
	return out
}

// TestE2EJobsSelectByPrefix is spec 013's criterion for the CI jobs: the
// jobs of the table exist with its budgets, each job that runs the e2e
// tier selects its own tests and nothing else, the two cluster jobs
// export the bucket values of the overlay table, e2e-slow installs
// git-lfs, build is the only job with a docker build step and uploads
// candidate-images, which every job that runs against a cluster needs
// and downloads, and no job outside the table runs the e2e tier. The
// jobs are those of tiers.yml, which a tag, a dispatch and the two
// schedules start. The two suites a push runs, the one-node tier against
// MinIO and spec 018's install walk, are declared in .lateregate.yaml and
// checked by TestE2EPushSuitesAreDeclared.
func TestE2EJobsSelectByPrefix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "tiers.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)
	all := jobs(t, workflow)

	budgets := map[string]string{"build": "15", "e2e": "30", "e2e-slow": "30", "up-script": "15", "mutation": "20", "fuzz": "60"}
	for name, minutes := range budgets {
		j, ok := all[name]
		if !ok {
			t.Errorf("job %s is missing", name)
			continue
		}
		if !strings.Contains(j.text, "timeout-minutes: "+minutes+"\n") {
			t.Errorf("job %s: budget is not %s minutes", name, minutes)
		}
	}
	// Each test job's go test lines carry its selections and nothing
	// else: one prefix per job, and for the e2e job the conformance
	// suite of spec 021 beside its prefix, in a line of its own that
	// selects the package's two stack tests and spec 017's previous
	// release fixture test by name.
	selections := map[string][]string{
		"e2e":       {"-run 'TestCluster' -skip 'TestClusterUpScript'", "./test/conformance/... -run 'TestContract|TestSameAnswersOnStubAndStack|TestPreviousReleaseFixture'"},
		"e2e-slow":  {"-run 'TestSlow'"},
		"up-script": {"-run 'TestClusterUpScript'"},
		"mutation":  {"-run 'TestMutation'"},
	}
	goTest := regexp.MustCompile(`go test [^\n]*-tags=e2e[^\n]*`)
	for name, wants := range selections {
		j := all[name]
		for _, want := range wants {
			if !strings.Contains(j.text, want) {
				t.Errorf("job %s does not run %q", name, want)
			}
		}
		for _, line := range goTest.FindAllString(j.text, -1) {
			if !slices.ContainsFunc(wants, func(want string) bool { return strings.Contains(line, want) }) {
				t.Errorf("job %s runs the e2e tier without its selection: %s", name, line)
			}
		}
	}
	// The mutation job runs once per capability of the set, and the
	// set is the node's.
	if !strings.Contains(all["mutation"].text, `MUTATION_CAPABILITIES: "`+strings.Join(config.DropCapabilities, " ")+`"`) {
		t.Errorf("mutation does not loop over %v", config.DropCapabilities)
	}
	// The e2e tier is run by those four jobs, and no other job of this
	// workflow runs it or the Makefile's tier target.
	for name, j := range all {
		if _, ok := selections[name]; ok {
			continue
		}
		if goTest.MatchString(j.text) || strings.Contains(j.text, "make test-tiers") {
			t.Errorf("job %s runs the e2e tier", name)
		}
	}
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`-tags=e2e [^\n]*-run 'TestE2E'`).Match(makefile) {
		t.Error("make test-tiers does not select the TestE2E prefix")
	}
	// The two cluster jobs export the six bucket values of the overlay.
	family := map[string]string{
		"ORIGO_TEST_S3_ENDPOINT": "http://localhost:30900", "ORIGO_TEST_S3_REGION": "us-east-1", "ORIGO_TEST_S3_BUCKET": "origo-test",
		"ORIGO_TEST_S3_KEY": "minioadmin", "ORIGO_TEST_S3_SECRET": "minioadmin", "ORIGO_TEST_S3_PATH_STYLE": `"1"`,
	}
	for _, name := range []string{"e2e", "e2e-slow"} {
		for key, value := range family {
			if !strings.Contains(all[name].text, key+": "+value+"\n") {
				t.Errorf("job %s does not export %s=%s", name, key, value)
			}
		}
	}
	if !strings.Contains(all["e2e-slow"].text, "git-lfs") {
		t.Error("e2e-slow does not install git-lfs")
	}
	// One image build, shared as candidate-images.
	for name, j := range all {
		if strings.Contains(j.text, "docker build") && name != "build" {
			t.Errorf("job %s has a docker build step", name)
		}
	}
	if b := all["build"].text; !strings.Contains(b, "docker build") || !strings.Contains(b, "upload-artifact") || !strings.Contains(b, "name: candidate-images") {
		t.Error("build does not build the images and upload candidate-images")
	}
	for _, name := range []string{"e2e", "e2e-slow", "up-script"} {
		j := all[name].text
		if !strings.Contains(j, "needs: build") || !strings.Contains(j, "download-artifact") || !strings.Contains(j, "name: candidate-images") {
			t.Errorf("job %s does not need build and download candidate-images", name)
		}
	}
	// The fuzz job runs on the schedule and nowhere else.
	if !strings.Contains(workflow, "schedule:") || !strings.Contains(all["fuzz"].text, "github.event_name == 'schedule'") || !strings.Contains(all["fuzz"].text, "make fuzz") {
		t.Error("fuzz is not the weekly make fuzz")
	}
}

// TestE2EPushSuitesAreDeclared holds the two e2e suites a push runs, which
// used to be the integration and install jobs of verify.yml and are now
// entries of .lateregate.yaml: the one-node tier selects the TestE2E
// prefix and the wal integration tests against MinIO with the race
// detector, both export the six bucket values the tests read, and the
// install walk runs the script that builds the candidate images and walks
// docs/install.md, which ends in the conformance contract against the
// installation and in no other e2e line.
func TestE2EPushSuitesAreDeclared(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".lateregate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	declared := string(raw)
	for _, want := range []string{
		"test: {tags: [integration], packages: [./internal/wal/...], race: true}",
		"test: {tags: [e2e], packages: [./test/e2e/...], run: TestE2E, race: true}",
		"command: tools/docs/install-walk.sh",
	} {
		if !strings.Contains(declared, want) {
			t.Errorf(".lateregate.yaml does not declare %q", want)
		}
	}
	for _, key := range []string{
		"ORIGO_TEST_S3_ENDPOINT", "ORIGO_TEST_S3_REGION", "ORIGO_TEST_S3_BUCKET",
		"ORIGO_TEST_S3_KEY", "ORIGO_TEST_S3_SECRET", "ORIGO_TEST_S3_PATH_STYLE",
	} {
		if got := strings.Count(declared, key+":"); got != 2 {
			t.Errorf(".lateregate.yaml sets %s %d times, want once for each of the two MinIO suites", key, got)
		}
	}
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "docs", "install-walk.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(script)
	for _, want := range []string{
		"tools/docs/run-blocks.sh docs/install.md",
		"./test/conformance/... -run 'TestContract'",
		"--entrypoint git",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("install-walk.sh does not run %q", want)
		}
	}
	if got := regexp.MustCompile(`go test [^\n]*-tags=e2e`).FindAllString(body, -1); len(got) != 1 {
		t.Errorf("install-walk.sh has %d e2e go test lines, want the one contract run", len(got))
	}
}
