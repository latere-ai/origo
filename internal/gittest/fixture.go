// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package gittest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// Fixture is the history the read API of spec 009 is tested against:
// merges, a rename, a binary file, a commit with trailers, an annotated
// and a lightweight tag, more than 100 commits on main with a merge
// whose parents interleave in rev-list order, a directory of 5 001
// entries, a 5 MiB change on the branch large, and a 60 MiB blob on the
// branch big. Every commit id is fixed by the Source's clock, so a
// golden expectation holds across runs.
type Fixture struct {
	*Source
	// Refs is the reference map of the history, HEAD included.
	Refs map[string]string

	// Commits on main, oldest first: the root, the rename, the binary
	// file, the trailers, the merge of feature, the merge of side, and
	// the tip that adds the many directory.
	Root, Rename, Binary, Trailers, Merge, SideMerge, Tip string
	// Large is the tip of the branch large, a 5 MiB text change over Tip.
	Large string
	// Big is the tip of the branch big, which adds the 60 MiB blob
	// BigBlob whose content has the digest BigSHA256.
	Big, BigBlob, BigSHA256 string
	// TagV1 is the annotated tag object v1.0 on Trailers.
	TagV1 string
}

// Sizes of the fixture's large objects.
const (
	// BigBlobSize is the size of the blob on the branch big.
	BigBlobSize = 60 << 20
	// ManyEntries is the number of files under the many directory.
	ManyEntries = 5001
	// LargeFiles and LargeFileSize shape the change on the branch large:
	// LargeFiles text files of about LargeFileSize bytes each, so the
	// diff exceeds 5 MiB and a 1 MiB cut falls on a file boundary.
	LargeFiles     = 18
	LargeFileSize  = 300 << 10
	sideCommits    = 50
	bigBlobSeed    = 0x9e3779b97f4a7c15
	binaryFileSeed = 0xdeadbeef
)

// NewFixture builds the fixture in dir, or in a directory of the test
// when dir is empty.
func NewFixture(t testing.TB, dir string) *Fixture {
	t.Helper()
	var src *Source
	if dir == "" {
		src = NewSource(t)
	} else {
		src = NewSourceAt(t, dir)
	}
	f := &Fixture{Source: src}
	f.Root = src.Commit("README.md", "# Fixture\n", "Initial commit")
	src.Commit("src/a.txt", "alpha\nbeta\n", "Add src/a.txt")
	src.Rename("src/a.txt", "src/b.txt")
	f.Rename = src.CommitAll("Rename a to b")
	src.Write("assets/logo.bin", Bytes(1024, binaryFileSeed))
	f.Binary = src.CommitAll("Add a binary file")
	src.Write("README.md", []byte("# Fixture\n\nSigned.\n"))
	f.Trailers = src.CommitAll("Sign the work\n\nThe body of the message.\n\nSigned-off-by: Alice <alice@example.com>\nSigned-off-by: Bob <bob@example.com>\nCo-authored-by: Carol <carol@example.com>\n")
	f.TagV1 = src.Tag("v1.0", "Release 1.0")

	// A feature branch merged with a merge commit.
	src.Branch("feature")
	src.Commit("feat/one.txt", "one\n", "Feature one")
	src.Checkout("main")
	src.Commit("trunk/six.txt", "six\n", "Main six")
	src.Checkout("feature")
	src.Commit("feat/two.txt", "two\n", "Feature two")
	src.Checkout("main")
	f.Merge = src.Merge("feature", "Merge feature")

	// Fifty commits on side interleaved in time with fifty on main, so
	// rev-list alternates between the two parents of the merge.
	src.Branch("side")
	src.Checkout("main")
	for i := range sideCommits {
		src.Checkout("side")
		src.Commit(fmt.Sprintf("side/%02d.txt", i), fmt.Sprintf("side %d\n", i), fmt.Sprintf("Side %d", i))
		src.Checkout("main")
		src.Commit(fmt.Sprintf("trunk/%02d.txt", i), fmt.Sprintf("main %d\n", i), fmt.Sprintf("Main %d", i))
	}
	f.SideMerge = src.Merge("side", "Merge side")

	for i := range ManyEntries {
		src.Write(fmt.Sprintf("many/f%05d", i), fmt.Appendf(nil, "%d\n", i))
	}
	f.Tip = src.CommitAll("Add many")
	Run(t, src.Dir, nil, "tag", "latest")

	src.Branch("large")
	for i := range LargeFiles {
		var b strings.Builder
		for line := 0; b.Len() < LargeFileSize; line++ {
			fmt.Fprintf(&b, "file %02d line %06d of the large change\n", i, line)
		}
		src.Write(fmt.Sprintf("large/%02d.txt", i), []byte(b.String()))
	}
	f.Large = src.CommitAll("A large change")

	src.Checkout("main")
	src.Branch("big")
	big := Bytes(BigBlobSize, bigBlobSeed)
	sum := sha256.Sum256(big)
	f.BigSHA256 = hex.EncodeToString(sum[:])
	src.Write("blobs/big.bin", big)
	f.Big = src.CommitAll("Add the big blob")
	f.BigBlob = src.Rev("HEAD:blobs/big.bin")
	src.Checkout("main")
	f.Refs = src.Refs()
	return f
}
