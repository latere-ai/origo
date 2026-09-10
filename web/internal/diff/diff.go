// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package diff reads the unified diff text Origo's compare endpoint returns
// so a screen can show it as a table.
//
// This is presentation and not computation. It produces no fact the server
// did not send: every path, every line, and every count below is read out of
// the bytes git wrote. Nothing here runs git, opens a repository, or decides
// what changed.
package diff

import (
	"strconv"
	"strings"
)

// Kind is what one row of a file's body is.
type Kind uint8

const (
	// Context is a line both sides share.
	Context Kind = iota
	// Add is a line the head has and the base does not.
	Add
	// Del is a line the base has and the head does not.
	Del
	// Hunk is the @@ line that introduces a run of rows.
	Hunk
	// Note is git's own remark inside a body, such as the missing final
	// newline.
	Note
)

// Row is one line of a file's body.
type Row struct {
	Kind Kind
	// Old and New are the line numbers on each side, zero where the side
	// has no line.
	Old, New int
	// Text is the line without its leading +, - or space. The sign is
	// rendered as its own column, so colour is never the only signal.
	Text string
	// Sign is the literal +, - or space git wrote.
	Sign string
}

// File is one file's part of a diff.
type File struct {
	// Path is the file's name on the head side, or on the base side for a
	// deletion.
	Path string
	// OldPath is set only when the file moved.
	OldPath string
	// Status is one of "added", "deleted", "renamed" or "modified".
	Status string
	// Binary reports that git wrote "Binary files differ" instead of a body.
	Binary bool
	// Additions and Deletions count the file's changed lines.
	Additions, Deletions int
	// Rows is the file's body, empty when the file is binary or when the
	// body was over the render budget.
	Rows []Row
	// Collapsed asks the page to render the file shut. The body is still
	// there: a reader opens it without another request.
	Collapsed bool
	// OverBudget reports that the body was dropped because it was larger
	// than the render budget. The page then shows the counts and the ways
	// to see it anyway.
	OverBudget bool
}

// Changed is the file's changed lines.
func (f File) Changed() int { return f.Additions + f.Deletions }

// Patch is a whole diff.
type Patch struct {
	Files []File
	// Additions and Deletions are the totals over every file received.
	Additions, Deletions int
	// Truncated reports that Origo cut the diff at its own cap, so files
	// past the cut are not in Files at all.
	Truncated bool
	// OverBudget counts the files whose bodies were dropped.
	OverBudget int
}

// Budget bounds what one page renders.
type Budget struct {
	// Collapse is the changed-line count past which a file renders shut.
	Collapse int
	// PerFile is the changed-line count past which a file's body is not
	// rendered at all.
	PerFile int
	// PerPatch is the changed-line count past which every file renders
	// shut, so a four file commit and a four hundred file commit are the
	// same document at the same cost.
	PerPatch int
	// Force is the one path to render whatever its size, which is what the
	// "render it anyway" link asks for.
	Force string
}

// DefaultBudget is what every screen uses.
var DefaultBudget = Budget{Collapse: 500, PerFile: 5000, PerPatch: 20000}

// Parse reads unified diff text into a patch, applying the budget.
func Parse(text []byte, b Budget) Patch {
	if b.Collapse == 0 && b.PerFile == 0 && b.PerPatch == 0 {
		b = DefaultBudget
	}
	p := Patch{Files: parseFiles(string(text))}
	for i := range p.Files {
		p.Additions += p.Files[i].Additions
		p.Deletions += p.Files[i].Deletions
	}
	applyBudget(&p, b)
	return p
}

func applyBudget(p *Patch, b Budget) {
	total := p.Additions + p.Deletions
	shutAll := b.PerPatch > 0 && total > b.PerPatch
	for i := range p.Files {
		f := &p.Files[i]
		if f.Binary || len(f.Rows) == 0 {
			continue
		}
		forced := b.Force != "" && b.Force == f.Path
		switch {
		case !forced && b.PerFile > 0 && f.Changed() > b.PerFile:
			f.Rows = nil
			f.OverBudget = true
			p.OverBudget++
		case forced:
			f.Collapsed = false
		case shutAll:
			f.Collapsed = true
		default:
			f.Collapsed = b.Collapse > 0 && f.Changed() > b.Collapse
		}
	}
}

// parseFiles splits the text at each "diff --git" header and reads each part.
func parseFiles(text string) []File {
	var files []File
	var cur *File
	var oldLine, newLine int

	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if cur != nil {
				files = append(files, *cur)
			}
			old, new_ := splitGitHeader(strings.TrimPrefix(line, "diff --git "))
			cur = &File{Path: new_, OldPath: old, Status: "modified"}
			oldLine, newLine = 0, 0
		case cur == nil:
			// Text before the first header: git writes none, so there is
			// nothing to keep.
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "added"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "deleted"
		case strings.HasPrefix(line, "rename from "):
			cur.OldPath, cur.Status = strings.TrimPrefix(line, "rename from "), "renamed"
		case strings.HasPrefix(line, "rename to "):
			cur.Path, cur.Status = strings.TrimPrefix(line, "rename to "), "renamed"
		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true
		case strings.HasPrefix(line, "--- "):
			if p, ok := stripPrefix(strings.TrimPrefix(line, "--- ")); ok {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p, ok := stripPrefix(strings.TrimPrefix(line, "+++ ")); ok {
				cur.Path = p
			}
		case strings.HasPrefix(line, "@@"):
			oldLine, newLine = hunkStarts(line)
			cur.Rows = append(cur.Rows, Row{Kind: Hunk, Text: line})
		case len(cur.Rows) == 0:
			// index, mode and similarity lines, before any hunk.
		case strings.HasPrefix(line, "+"):
			cur.Additions++
			cur.Rows = append(cur.Rows, Row{Kind: Add, New: newLine, Text: line[1:], Sign: "+"})
			newLine++
		case strings.HasPrefix(line, "-"):
			cur.Deletions++
			cur.Rows = append(cur.Rows, Row{Kind: Del, Old: oldLine, Text: line[1:], Sign: "-"})
			oldLine++
		case strings.HasPrefix(line, `\`):
			cur.Rows = append(cur.Rows, Row{Kind: Note, Text: strings.TrimSpace(line)})
		case line == "" || strings.HasPrefix(line, " "):
			cur.Rows = append(cur.Rows, Row{Kind: Context, Old: oldLine, New: newLine, Text: strings.TrimPrefix(line, " "), Sign: " "})
			oldLine++
			newLine++
		}
	}
	if cur != nil {
		files = append(files, *cur)
	}
	// A trailing empty line of the text becomes a context row of the last
	// file; git's diff always ends with a newline, so it is an artefact.
	if n := len(files); n > 0 {
		f := &files[n-1]
		if r := len(f.Rows); r > 0 && f.Rows[r-1].Kind == Context && f.Rows[r-1].Text == "" {
			f.Rows = f.Rows[:r-1]
		}
	}
	return files
}

// splitGitHeader reads "a/old b/new" from a diff --git line. A path with a
// space in it is ambiguous in that form, which is why the ---/+++ lines,
// which are unambiguous, overwrite whatever this returns.
func splitGitHeader(s string) (old, new_ string) {
	i := strings.Index(s, " b/")
	if i < 0 {
		return "", strings.TrimSpace(s)
	}
	old, _ = stripPrefix(s[:i])
	new_, _ = stripPrefix(s[i+1:])
	return old, new_
}

// stripPrefix removes git's a/ or b/ prefix, and reports false for /dev/null,
// which names no file.
func stripPrefix(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return "", false
	}
	if p, ok := strings.CutPrefix(s, "a/"); ok {
		return p, true
	}
	if p, ok := strings.CutPrefix(s, "b/"); ok {
		return p, true
	}
	// A tab separates the path from a timestamp git does not write here,
	// but another producer might.
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	return s, s != ""
}

// hunkStarts reads the first line number of each side out of an @@ header.
func hunkStarts(line string) (old, new_ int) {
	body := line
	if i := strings.Index(body, "@@"); i >= 0 {
		body = body[i+2:]
	}
	if i := strings.Index(body, "@@"); i >= 0 {
		body = body[:i]
	}
	for f := range strings.FieldsSeq(body) {
		n := number(strings.TrimLeft(f, "+-"))
		switch {
		case strings.HasPrefix(f, "-"):
			old = n
		case strings.HasPrefix(f, "+"):
			new_ = n
		}
	}
	return old, new_
}

func number(s string) int {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// The five predicates a template asks a row about. A template cannot compare
// a typed constant, so the question is a method rather than an equality.

// IsContext reports a line both sides share.
func (r Row) IsContext() bool { return r.Kind == Context }

// IsAdd reports a line the head has and the base does not.
func (r Row) IsAdd() bool { return r.Kind == Add }

// IsDel reports a line the base has and the head does not.
func (r Row) IsDel() bool { return r.Kind == Del }

// IsHunk reports the @@ line that introduces a run of rows.
func (r Row) IsHunk() bool { return r.Kind == Hunk }

// IsNote reports git's own remark inside a body.
func (r Row) IsNote() bool { return r.Kind == Note }
