// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package conformance_test

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/conformance"
)

// fixtureDir is the harness's own flag, for the conformance job of
// release.yml: the directory TestReleaseFixturePush writes the manifest
// into and TestReleaseFixturePack reads it from and writes the archive
// beside. Both skip when it is empty, so a developer's run of the
// package pushes nothing.
var fixtureDir = flag.String("fixture", "", "the directory the release fixture's manifest and archive are written to")

// stackStore is the stack's bucket through the ORIGO_TEST_S3_ENDPOINT
// family the job exported (spec 013's MinIO row).
func stackStore(t *testing.T) *wal.S3 {
	t.Helper()
	endpoint := os.Getenv("ORIGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("ORIGO_TEST_S3_ENDPOINT is unset; the job exports the stack's bucket family")
	}
	store, err := wal.NewS3(wal.S3Options{
		Endpoint: endpoint, Region: os.Getenv("ORIGO_TEST_S3_REGION"), Bucket: os.Getenv("ORIGO_TEST_S3_BUCKET"),
		Key: os.Getenv("ORIGO_TEST_S3_KEY"), Secret: os.Getenv("ORIGO_TEST_S3_SECRET"), PathStyle: os.Getenv("ORIGO_TEST_S3_PATH_STYLE") == "1",
		Client: &http.Client{Transport: &http.Transport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestReleaseFixturePush is step 2 of spec 017's pipeline, first half:
// the fixture repository pushed through the stack before TestContract
// runs, under a slug outside the conformance- prefix, and kept; the
// manifest goes to <fixture>/fixture.json.
func TestReleaseFixturePush(t *testing.T) {
	if *fixtureDir == "" {
		t.Skip("-fixture unset")
	}
	target := stackTarget(t)
	f := conformance.PushReleaseFixture(t, target)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(*fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*fixtureDir, conformance.ReleaseFixtureManifest), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("pushed the fixture %s (%s/%s) on %s: %d commits", f.ID, f.Owner, f.Slug, f.Version, len(f.RevList))
}

// TestReleaseFixturePack is the second half: after TestContract and
// TestPreviousReleaseFixture ran, every object under the fixture's
// prefix is read from the stack's MinIO and packed as
// <fixture>/fixture.tar.gz, which the job attaches to the release as
// fixture-<version>.tar.gz.
func TestReleaseFixturePack(t *testing.T) {
	if *fixtureDir == "" {
		t.Skip("-fixture unset")
	}
	data, err := os.ReadFile(filepath.Join(*fixtureDir, conformance.ReleaseFixtureManifest))
	if err != nil {
		t.Fatal(err)
	}
	var f conformance.ReleaseFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	// The fixture survived the run: what the stack serves is what was
	// pushed.
	target := stackTarget(t)
	if got := conformance.RevList(t, target.URL, target.Token, f.ID); !slices.Equal(got, f.RevList) {
		t.Fatalf("the fixture after the run serves %v, pushed %v", got, f.RevList)
	}
	out, err := os.Create(filepath.Join(*fixtureDir, "fixture.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := conformance.PackReleaseFixture(context.Background(), stackStore(t), config.Prefix, f, out); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("packed %s", out.Name())
}

// TestPreviousReleaseFixture is spec 017's compatibility criterion: the
// fixture attached to the previous release, downloaded by the job into
// the path ORIGO_PREVIOUS_RELEASE_FIXTURE names, is uploaded under a
// fresh id through the S3 client before anything clones it, the stack
// materializes the repository from the log that release wrote, and
// rev-list --all is what the manifest recorded. Unset, the test skips:
// a fork with no release, or a developer's machine.
func TestPreviousReleaseFixture(t *testing.T) {
	archive := os.Getenv("ORIGO_PREVIOUS_RELEASE_FIXTURE")
	if archive == "" {
		t.Skip("ORIGO_PREVIOUS_RELEASE_FIXTURE unset")
	}
	target := stackTarget(t)
	store := stackStore(t)
	ctx := context.Background()
	id := newID(t)
	f, err := conformance.UploadReleaseFixture(ctx, store, config.Prefix, archive, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conformance.DeletePrefix(ctx, store, conformance.RepoPrefix(config.Prefix, id)); err != nil {
			t.Errorf("removing the copy: %v", err)
		}
	})
	got := conformance.RevList(t, target.URL, target.Token, id)
	if !slices.Equal(got, f.RevList) {
		t.Fatalf("the fixture of %s serves %v on this release, wrote %v", f.Version, got, f.RevList)
	}
	t.Logf("the fixture of %s (%d commits) serves on this release under %s", f.Version, len(f.RevList), id)
}
