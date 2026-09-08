// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"strings"
	"unicode/utf8"
)

// MaxPathBytes bounds a path from a request.
const MaxPathBytes = 4096

// ValidPath is the path rule of spec 009, applied to every path a
// request names (?path= here, the changes and symbolic link targets of
// spec 020): at most 4 096 bytes, no leading slash, no empty component,
// no component . or .., no NUL byte, and no component that names the
// git directory in any of the forms git's core.protectNTFS and
// core.protectHFS refuse. The empty path is accepted and means the
// whole tree; spec 020 refuses it on its own. FuzzValidPath holds the
// function to git update-index: no path it accepts is one git refuses.
func ValidPath(p string) bool {
	if len(p) > MaxPathBytes {
		return false
	}
	if p == "" {
		return true
	}
	if strings.IndexByte(p, 0) >= 0 || p[0] == '/' {
		return false
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." || c == ".." || namesGitDir(c) {
			return false
		}
	}
	return true
}

// namesGitDir reports whether a component would be the git directory
// on a case-insensitive file system with NTFS or HFS+ name rules: .git
// in any case, with trailing dots or spaces (NTFS strips them) or an
// alternate data stream after a colon, the NTFS 8.3 short name git~<n>
// with the same tails, and .git with HFS+ ignorable code points inside.
func namesGitDir(c string) bool {
	// NTFS: what precedes a colon is the file name; trailing dots and
	// spaces are stripped.
	name := c
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	name = strings.ToLower(strings.TrimRight(name, ". "))
	if name == ".git" {
		return true
	}
	if rest, ok := strings.CutPrefix(name, "git~"); ok && rest != "" {
		digits := true
		for i := 0; i < len(rest); i++ {
			if rest[i] < '0' || rest[i] > '9' {
				digits = false
			}
		}
		if digits {
			return true
		}
	}
	// HFS+: the code points it ignores when comparing names are removed
	// and the ASCII case is folded.
	if !strings.ContainsFunc(c, func(r rune) bool { return r >= 0x80 }) {
		return false
	}
	var folded strings.Builder
	for i := 0; i < len(c); {
		r, size := utf8.DecodeRuneInString(c[i:])
		i += size
		if hfsIgnorable(r) {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		folded.WriteRune(r)
	}
	return folded.String() == ".git"
}

// hfsIgnorable is the set of code points HFS+ ignores in a name, git's
// list in utf8.c.
func hfsIgnorable(r rune) bool {
	switch r {
	case 0x200c, 0x200d, 0x200e, 0x200f, // zero width joiners and marks
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, // bidi embeddings and overrides
		0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f, // deprecated format characters
		0xfeff: // zero width no-break space
		return true
	}
	return false
}
