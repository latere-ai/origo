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
// release-archives writes one archive per binary per platform and sums them
// all into checksums.txt. release-verify downloads by an explicit pattern and
// then runs `sha256sum -c checksums.txt` over what it fetched, so a sum whose
// file was never fetched fails the tag. Adding a binary to the loop without
// adding its pattern is therefore a failure that first appears on a release,
// which is the worst place to find it.
func TestTheReleaseCarriesBothBinaries(t *testing.T) {
	makefile := read(t, "../../Makefile")
	workflow := read(t, "../../.github/workflows/release.yml")

	if !strings.Contains(makefile, "RELEASE_BINARIES := $(SERVICE) origo") {
		t.Fatal("the Makefile does not build origo into the release archives")
	}
	if !strings.Contains(makefile, "$(RELEASE_DIR)/$${b}_$(VERSION)_$${os}_$${arch}.tar.gz") {
		t.Fatal("the archive name is not built from the binary name, so a second binary would overwrite the first")
	}
	if !strings.Contains(makefile, "$(SHA256) *.tar.gz > checksums.txt") {
		t.Fatal("checksums.txt is not built over every archive")
	}
	// Two patterns, because neither glob matches the other's name: origod_
	// does not match origo_v1.0.0_linux_amd64.tar.gz and origo_ does not
	// match origod_v1.0.0_linux_amd64.tar.gz.
	for _, pattern := range []string{"--pattern 'origod_*.tar.gz'", "--pattern 'origo_*.tar.gz'"} {
		if !strings.Contains(workflow, pattern) {
			t.Fatalf("release-verify does not download %s, so sha256sum -c would fail on its sums", pattern)
		}
	}
	if !strings.Contains(workflow, "sha256sum -c checksums.txt") {
		t.Fatal("release-verify no longer checks the sums, so this coupling is gone and so is this test's reason")
	}
	// The upload is glob-driven, so the archives reach the release with no
	// change of their own. If that ever becomes an explicit list, this test
	// is where a builder should be told.
	if !strings.Contains(workflow, "out/release/*.tar.gz") {
		t.Fatal("the upload is no longer glob-driven; a second binary now needs a line there too")
	}
	// No second image: the command runs beside an agent, not in a
	// cluster. The workflow names its images through ORIGOD_IMAGE and
	// STUBS_IMAGE, whose namespace is the repository owner's, so the
	// name to look for is the suffix and not a fixed namespace.
	if strings.Contains(workflow, "/origo:") || strings.Contains(workflow, "ORIGO_IMAGE:") {
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
