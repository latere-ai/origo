// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/latere-ai/origo/tools/specindex/specs"
)

// section is one kind of name the page carries, in the order a reader
// meets it: the calls, then what every response carries, then what a
// refusal says.
type section struct {
	kind    specs.Kind
	heading string
	intro   string
}

var sections = []section{
	{specs.KindEndpoint, "Endpoints", "Every path below is under the base URL of the installation. `{repo}` is either `r/{id}.git` or `{owner}/{slug}.git`; both address the same repository, and the id form never changes."},
	{specs.KindHeader, "Headers", "Headers Origo sets on its responses. A consumer reads them; none is sent by a client."},
	{specs.KindCode, "Error codes", "Every refusal is one JSON body, `{\"error\": {\"code\", \"message\", \"details\"}}`. `code` is the stable name to branch on, `message` is one sentence to show a person, and `details` carries the developer fields listed here."},
}

// specLink is the label and target of one spec: the number and the file
// name's words, linked from docs/ into specs/.
func specLink(idx *specs.Index, num string) string {
	file := idx.File(num)
	label := strings.TrimSuffix(file, ".md")
	if _, rest, ok := strings.Cut(label, "-"); ok {
		label = num + " " + strings.ReplaceAll(rest, "-", " ")
	}
	return fmt.Sprintf("[%s](../specs/%s)", label, file)
}

// group is one table of one spec: the rows of one definition table, in
// the order the spec writes them.
type group struct {
	owner  string
	header []string
	rows   []row
	line   int // the first row's line, which orders the groups of a spec
}

// row is one definition row and the line it sits on, which is the order
// the spec writes it in and the order the page keeps.
type row struct {
	line  int
	cells []string
}

// groups collects the definitions of one kind into the tables they came
// from. Two tables of one spec with different columns stay two tables,
// and a row that defines several names is rendered once.
func groups(idx *specs.Index, kind specs.Kind) []group {
	byKey := map[string]*group{}
	seen := map[string]bool{}
	for _, n := range idx.Names {
		if n.Kind != kind || len(n.Row) == 0 {
			continue
		}
		key := n.Owner + "\x00" + strings.Join(n.Header, "\x00")
		g := byKey[key]
		if g == nil {
			g = &group{owner: n.Owner, header: n.Header, line: n.Line}
			byKey[key] = g
		}
		if rowKey := key + "\x00" + fmt.Sprint(n.Line); !seen[rowKey] {
			seen[rowKey] = true
			g.rows = append(g.rows, row{line: n.Line, cells: n.Row})
		}
		g.line = min(g.line, n.Line)
	}
	out := make([]group, 0, len(byKey))
	for _, g := range byKey {
		slices.SortFunc(g.rows, func(a, b row) int { return a.line - b.line })
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b group) int {
		if a.owner != b.owner {
			return strings.Compare(a.owner, b.owner)
		}
		return a.line - b.line
	})
	return out
}

// table renders a header and its rows, escaping a pipe inside a cell the
// way the spec wrote it.
func table(header []string, rows []row) string {
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for _, c := range cells {
			b.WriteString(" " + strings.ReplaceAll(c, "|", `\|`) + " |")
		}
		b.WriteString("\n")
	}
	line(header)
	b.WriteString("|" + strings.Repeat("---|", len(header)) + "\n")
	for _, r := range rows {
		// A short row is padded so the table stays rectangular.
		cells := slices.Clone(r.cells)
		for len(cells) < len(header) {
			cells = append(cells, "")
		}
		line(cells)
	}
	return b.String()
}

var contractValue = regexp.MustCompile("`([0-9]+)`")

// contract is the contract version spec 003 states in its Origo-Contract
// row: the number a consumer's client pins. It is read from the spec
// rather than written here, so the page states nothing a spec does not.
func contract(idx *specs.Index) (string, error) {
	for _, n := range idx.Names {
		if n.Kind != specs.KindHeader || n.Name != "Origo-Contract" || len(n.Row) < 2 {
			continue
		}
		if m := contractValue.FindStringSubmatch(n.Row[1]); m != nil {
			return m[1], nil
		}
	}
	return "", errors.New("no spec states a contract version in the Origo-Contract row")
}

// Page renders docs/api.md from the index.
func Page(idx *specs.Index) (string, error) {
	version, err := contract(idx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# The Origo API

For whoever writes against an Origo installation. Everything here is
generated from the design specs by %s, so a name on this page is a name
the implementation is held to; run it again after a spec changes.

This is contract version %s. Every response carries `+"`Origo-Contract: %s`"+`.
A later version keeps every field and every code of this one and may add
more, so a client reads the fields it knows and ignores the rest. A
breaking change raises the number, and the upgrade notes say what moved.

Every route is authenticated: send `+"`Authorization: Bearer <token>`"+` on
every request, including the git routes. How to get a token, and what a
token may do, is your installation's own configuration; see
[`+"`install.md`"+`](install.md) for the operator's side and
[`+"`configuration.md`"+`](configuration.md) for the variables behind it.
`, "`make docs`", version, version)

	for _, s := range sections {
		gs := groups(idx, s.kind)
		if len(gs) == 0 {
			return "", errors.New("no " + string(s.kind) + " is defined by any spec")
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", s.heading, s.intro)
		for _, g := range gs {
			fmt.Fprintf(&b, "\nDefined by %s.\n\n%s", specLink(idx, g.owner), table(g.header, g.rows))
		}
	}
	return b.String(), nil
}
