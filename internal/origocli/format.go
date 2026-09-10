// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// The byte rules of spec 025, in one place.
//
//  1. The default answer is the smallest one that answers the question, and
//     the flag that gives more is named where the answer was cut.
//  2. Data goes to stdout and every header, truncation and stale line goes to
//     stderr, so a pipeline filters only payload and a redirect writes only
//     the file.
//  3. The truncation an answer reports is the truncation that happened.
//
// A subject is cut to 72 characters because a history line is read as a
// column, and an object id prints at 7 hexadecimal characters everywhere but
// the head of `origo info`, which is the one place a whole id is worth its
// bytes: it is what -expect takes.
const (
	subjectMax = 72
	shortLen   = 7
)

// short is an object id as a line prints it.
func short(sha string) string {
	if len(sha) <= shortLen {
		return sha
	}
	return sha[:shortLen]
}

// subject is the first line of a commit message, cut to the column.
func subject(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	line = strings.TrimSpace(line)
	if len(line) <= subjectMax {
		return line
	}
	return line[:subjectMax-3] + "..."
}

// day renders a time as a history line prints it.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// stamp renders a time where the whole of it is wanted.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// truncation is rule 1: one bracketed line naming what was cut and the exact
// flag that continues it. Nothing else in an answer may say truncated, and it
// is only ever handed to [out.notice], which writes to stderr alone.
func truncation(what, next string) string {
	return "[truncated: " + what + "; " + next + "]"
}

// wall is the truncation with no continuation: Origo cut the body itself and
// the route has no cursor, so naming a flag would promise a next call that
// does not exist.
func wall(what string) string { return "[truncated: " + what + "]" }

// staleLine is what a response served without a currency check adds (spec
// 015). A caller that reads a head, then writes, gets non_fast_forward and
// should know why.
func staleLine(seconds string) string {
	return "[stale: the installation answered from a copy it last checked " + seconds + " seconds ago]"
}

// fileStat is one file of a comparison: the counts, not the patch.
type fileStat struct {
	Path      string
	Additions int
	Deletions int
	Binary    bool
}

// parseDiff reads a unified diff into per-file counts. The bytes are paid on
// this machine, which is a reason the binary is local: the caller is handed
// one line per file instead of a megabyte of patch.
func parseDiff(body []byte) []fileStat {
	var out []fileStat
	var cur *fileStat
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			out = append(out, fileStat{Path: pathOfHeader(line)})
			cur = &out[len(out)-1]
		case cur == nil:
			// Preamble before the first file header, if any.
		case strings.HasPrefix(line, "+++ "):
			if p, ok := pathOfMarker(line[4:]); ok {
				cur.Path = p
			}
		case strings.HasPrefix(line, "--- "):
			if p, ok := pathOfMarker(line[4:]); ok && cur.Path == "" {
				cur.Path = p
			}
		case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true
		case strings.HasPrefix(line, "+"):
			cur.Additions++
		case strings.HasPrefix(line, "-"):
			cur.Deletions++
		}
	}
	return out
}

// pathOfHeader reads the destination path of a `diff --git a/x b/x` header. It
// is the fallback; the +++ marker is exact and wins.
func pathOfHeader(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if i := strings.Index(rest, " b/"); i >= 0 {
		return strings.TrimPrefix(rest[i+1:], "b/")
	}
	return strings.TrimPrefix(rest, "a/")
}

// pathOfMarker reads a --- or +++ marker, which git writes with an a/ or b/
// prefix and a tab-separated suffix, and /dev/null for an absent side.
func pathOfMarker(rest string) (string, bool) {
	rest, _, _ = strings.Cut(rest, "\t")
	rest = strings.TrimSpace(rest)
	if rest == "/dev/null" || rest == "" {
		return "", false
	}
	for _, p := range []string{"a/", "b/"} {
		if s, ok := strings.CutPrefix(rest, p); ok {
			return s, true
		}
	}
	return rest, true
}

// statLines renders a comparison as the stat-first answer: one line per file
// and a totals line.
func statLines(files []fileStat) []string {
	out := make([]string, 0, len(files)+1)
	adds, dels := 0, 0
	for _, f := range files {
		if f.Binary {
			out = append(out, "binary  "+f.Path)
			continue
		}
		adds, dels = adds+f.Additions, dels+f.Deletions
		out = append(out, fmt.Sprintf("+%d -%d  %s", f.Additions, f.Deletions, f.Path))
	}
	return append(out, fmt.Sprintf("%s  +%d -%d", plural(len(files), "file"), adds, dels))
}

// plural renders a count with its noun.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// cutPatch cuts a patch at a file boundary, the way Origo cuts one, and
// reports whether it cut. A patch with no boundary before the bound is cut at
// the bound and reported, because a whole file past the budget is what the
// budget exists to refuse.
func cutPatch(patch []byte, max int) ([]byte, bool) {
	if len(patch) <= max {
		return patch, false
	}
	head := patch[:max]
	const boundary = "\ndiff --git "
	if i := bytes.LastIndex(head, []byte(boundary)); i > 0 {
		return head[:i+1], true
	}
	return head, true
}

// binaryHead reports whether the first bytes of a blob carry a NUL, which is
// git's own rule and the one this command refuses text on.
func binaryHead(head []byte) bool { return bytes.IndexByte(head, 0) >= 0 }

// lineWindow answers the lines of text from offset, at most count of them and
// at most maxBytes bytes, and the figures a truncation line needs: how many
// lines the text holds and how many bytes the window took.
func lineWindow(text string, offset, count, maxBytes int) (window string, lines, taken, total int) {
	all := strings.Split(text, "\n")
	// A trailing newline is a terminator, not an empty last line.
	if n := len(all); n > 1 && all[n-1] == "" {
		all = all[:n-1]
	}
	total = len(all)
	if offset >= total {
		return "", 0, 0, total
	}
	end := min(offset+count, total)
	var b strings.Builder
	for i := offset; i < end; i++ {
		if b.Len()+len(all[i])+1 > maxBytes && i > offset {
			end = i
			break
		}
		b.WriteString(all[i])
		b.WriteByte('\n')
	}
	return b.String(), end - offset, b.Len(), total
}

// entryLine renders one tree entry: the size, then the path with `ls -F`'s
// markers, a trailing slash for a directory, * for an executable and @ for a
// symbolic link. The path is on the line whole, which is what a grep needs.
func entryLine(path, mode, kind string, size *int64) string {
	sz := "-"
	if size != nil {
		sz = fmt.Sprint(*size)
	}
	switch {
	case kind == "tree":
		path += "/"
	case mode == "100755":
		path += "*"
	case mode == "120000":
		path += "@"
	}
	return fmt.Sprintf("%8s  %s", sz, path)
}

// indent renders a commit message the way `git show` does, four spaces in, so
// a message line can never be read as a stat line.
func indent(message string) []string {
	lines := strings.Split(strings.TrimRight(message, "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, "    "+l)
	}
	return out
}
