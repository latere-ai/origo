// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"strings"
	"testing"
)

// TestTheReleaseCarriesBothBinaries holds the one coupling spec 025 warns a
// builder about, in the place the coupling lives rather than in prose.
//
// release-archives writes one archive per binary per platform. The release
// workflow's archives command copies every archive it wrote into dist/,
// and the shared publish stage attaches and sums every file there, so a
// second binary reaches the release and checksums.txt with no line of its
// own. The shared verify stage downloads every asset before it checks the
// sums, so the explicit download patterns that once had to name each
// binary are gone. If the copy ever becomes an explicit list, this test is
// where a builder should be told.
func TestTheReleaseCarriesBothBinaries(t *testing.T) {
	makefile := read(t, "../../Makefile")
	workflow := read(t, "../../.github/workflows/release.yml")

	if !strings.Contains(makefile, "RELEASE_BINARIES := $(SERVICE) origo") {
		t.Fatal("the Makefile does not build origo into the release archives")
	}
	if !strings.Contains(makefile, "$(RELEASE_DIR)/$${b}_$(VERSION)_$${os}_$${arch}.tar.gz") {
		t.Fatal("the archive name is not built from the binary name, so a second binary would overwrite the first")
	}
	if !strings.Contains(workflow, "make release-archives") {
		t.Fatal("the release workflow no longer builds the archives with make release-archives")
	}
	if !strings.Contains(workflow, "cp out/release/*.tar.gz dist/") {
		t.Fatal("the copy into dist/ is no longer glob-driven; a second binary now needs a line there too")
	}
	// No second image: the command runs beside an agent, not in a
	// cluster. The workflow hands the build stage its images by name in
	// image_1 and image_2, so the name to look for is the JSON name, and
	// the jobs after the build read the references from its outputs.
	if strings.Contains(workflow, `"name": "origo"`) || strings.Contains(workflow, "/origo:") || strings.Contains(workflow, "ORIGO_IMAGE:") {
		t.Fatal("an image for origo was added; spec 025 says the command takes an archive and no image")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
