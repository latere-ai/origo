// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Kind is one class of name the deck defines.
type Kind string

// The kinds, in the order the table lists them.
const (
	KindCode      Kind = "error code"
	KindVariable  Kind = "variable"
	KindMetric    Kind = "metric"
	KindEvent     Kind = "event"
	KindEndpoint  Kind = "endpoint"
	KindHeader    Kind = "header"
	KindFailpoint Kind = "failpoint"
)

var kindOrder = []Kind{KindCode, KindVariable, KindMetric, KindEvent, KindEndpoint, KindHeader, KindFailpoint}

// headerKinds maps the first header cell of a definition table to its kind.
var headerKinds = map[string]Kind{
	"code": KindCode, "variable": KindVariable, "metric": KindMetric,
	"event": KindEvent, "header": KindHeader, "endpoint": KindEndpoint,
	"failpoint": KindFailpoint,
}

// namedKinds are the kinds whose names have no shape of their own: a
// backticked token is one of them only when a spec defines it.
var namedKinds = []Kind{KindCode, KindEvent, KindFailpoint}

// Name is one defined name.
type Name struct {
	Kind  Kind
	Name  string
	Owner string   // the spec number, "004"
	Also  []string // other spec numbers that mention it
}

// Index is the deck's names and the findings against them.
type Index struct {
	Names    []Name
	Findings []string
	specs    []string // numbers in order
	files    map[string]string
}

// Markers delimit the generated table in README.md.
const (
	BeginMarker = "<!-- specindex:begin -->"
	EndMarker   = "<!-- specindex:end -->"
)

var (
	specFile   = regexp.MustCompile(`^(\d{3})-.*\.md$`)
	backtick   = regexp.MustCompile("`([^`]+)`")
	fence      = regexp.MustCompile("(?s)```.*?```")
	varRe      = regexp.MustCompile(`^(ORIGO|OTEL)_[A-Z0-9_]+\*?$`)
	metricRe   = regexp.MustCompile(`^origo_[a-z0-9_]+$`)
	headerRe   = regexp.MustCompile(`^Origo-[A-Za-z-]+$`)
	endpointRe = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD) /`)
)

type mention struct {
	kind Kind
	name string
	spec string
}

type definition struct {
	kind Kind
	name string
	spec string
}

// Build reads every numbered spec under dir and indexes it.
func Build(dir string) (*Index, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	idx := &Index{files: map[string]string{}}
	var defs []definition
	var mentions []mention
	for _, e := range entries {
		m := specFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		num := m[1]
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		idx.specs = append(idx.specs, num)
		idx.files[num] = e.Name()
		d, ms := scan(num, string(data))
		defs = append(defs, d...)
		mentions = append(mentions, ms...)
	}
	sort.Strings(idx.specs)

	owner := map[Kind]map[string]string{}
	for _, d := range defs {
		if owner[d.kind] == nil {
			owner[d.kind] = map[string]string{}
		}
		if prev, ok := owner[d.kind][d.name]; ok && prev != d.spec {
			idx.Findings = append(idx.Findings, fmt.Sprintf("%s %q is defined by %s and by %s", d.kind, d.name, prev, d.spec))
			continue
		}
		owner[d.kind][d.name] = d.spec
	}
	also := map[Kind]map[string]map[string]bool{}
	for _, m := range mentions {
		if m.kind == "" {
			// A backticked token whose kind is known only from the
			// definitions: an error code, an event kind, or a failpoint.
			for _, k := range namedKinds {
				if _, ok := owner[k][m.name]; ok {
					m.kind = k
					break
				}
			}
			if m.kind == "" {
				continue
			}
		}
		o, ok := owner[m.kind][m.name]
		if !ok {
			idx.Findings = append(idx.Findings, fmt.Sprintf("%s: %s %q is named but no spec defines it", idx.files[m.spec], m.kind, m.name))
			continue
		}
		if o == m.spec {
			continue
		}
		if also[m.kind] == nil {
			also[m.kind] = map[string]map[string]bool{}
		}
		if also[m.kind][m.name] == nil {
			also[m.kind][m.name] = map[string]bool{}
		}
		also[m.kind][m.name][m.spec] = true
	}
	for _, k := range kindOrder {
		names := make([]string, 0, len(owner[k]))
		for n := range owner[k] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			var refs []string
			for s := range also[k][n] {
				refs = append(refs, s)
			}
			sort.Strings(refs)
			idx.Names = append(idx.Names, Name{Kind: k, Name: n, Owner: owner[k][n], Also: refs})
		}
	}
	sort.Strings(idx.Findings)
	return idx, nil
}

// scan reads one spec's definitions and mentions.
func scan(num, body string) ([]definition, []mention) {
	var defs []definition
	var mentions []mention
	defined := map[string]bool{} // "kind\x00name" defined in this spec
	body = stripFrontmatter(body)
	seen := map[string]bool{}
	note := func(k Kind, name string) {
		key := string(k) + "\x00" + name
		if defined[key] || seen[key] {
			return
		}
		seen[key] = true
		mentions = append(mentions, mention{kind: k, name: name, spec: num})
	}

	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if !isTableRow(lines[i]) || i+1 >= len(lines) || !isSeparator(lines[i+1]) {
			continue
		}
		header := cells(lines[i])
		kind, byMethod := tableKind(header)
		if kind == "" {
			continue
		}
		for j := i + 2; j < len(lines) && isTableRow(lines[j]); j++ {
			row := cells(lines[j])
			if len(row) == 0 {
				continue
			}
			var names []string
			if byMethod {
				if len(row) < 2 {
					continue
				}
				method := strings.TrimSpace(strings.Trim(row[0], "`"))
				for _, p := range backtick.FindAllStringSubmatch(row[1], -1) {
					names = append(names, method+" "+p[1])
				}
			} else {
				for _, p := range backtick.FindAllStringSubmatch(row[0], -1) {
					names = append(names, p[1])
				}
			}
			for _, n := range names {
				defs = append(defs, definition{kind: kind, name: n, spec: num})
				defined[string(kind)+"\x00"+n] = true
			}
			// The rest of the row is prose and may mention other names.
			rest := row[1:]
			if byMethod && len(row) > 2 {
				rest = row[2:]
			}
			lines[j] = strings.Join(rest, " | ")
		}
	}
	text := fence.ReplaceAllString(strings.Join(lines, "\n"), "")
	for _, p := range backtick.FindAllStringSubmatch(text, -1) {
		for _, tok := range tokens(p[1]) {
			switch {
			case varRe.MatchString(tok):
				note(KindVariable, tok)
			case metricRe.MatchString(tok):
				note(KindMetric, tok)
			case headerRe.MatchString(tok):
				note(KindHeader, tok)
			case endpointRe.MatchString(tok):
				note(KindEndpoint, tok)
			default:
				note("", tok)
			}
		}
	}
	return defs, mentions
}

// tokens reports the names one backticked token may mention. A metric
// named with its labels, origo_x_total{result="error"}, mentions the
// metric. A header with its value, Origo-Event: push, or a variable with
// its value, ORIGO_FAILPOINT=commit.before-index, mentions both sides,
// so the event kind and the failpoint are found where they are used.
func tokens(tok string) []string {
	if i := strings.IndexByte(tok, '{'); i > 0 && strings.HasPrefix(tok, "origo_") && strings.HasSuffix(tok, "}") {
		return []string{tok[:i]}
	}
	out := []string{tok}
	for _, sep := range []string{": ", "="} {
		if name, value, ok := strings.Cut(tok, sep); ok {
			out = append(out, strings.TrimSpace(name), strings.TrimSpace(value))
			break
		}
	}
	return out
}

func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	if end := strings.Index(s[4:], "\n---\n"); end >= 0 {
		return s[4+end+5:]
	}
	return s
}

func isTableRow(l string) bool {
	t := strings.TrimSpace(l)
	return strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|")
}

func isSeparator(l string) bool {
	if !isTableRow(l) {
		return false
	}
	for _, c := range cells(l) {
		if strings.Trim(strings.TrimSpace(c), "-: ") != "" {
			return false
		}
	}
	return true
}

// cells splits a table row on unescaped pipes.
func cells(l string) []string {
	t := strings.TrimSpace(l)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	t = strings.ReplaceAll(t, `\|`, "\x01")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(strings.ReplaceAll(parts[i], "\x01", "|"))
	}
	return parts
}

func tableKind(header []string) (Kind, bool) {
	if len(header) == 0 {
		return "", false
	}
	first := strings.ToLower(strings.TrimSpace(header[0]))
	if first == "method" && len(header) > 1 && strings.ToLower(strings.TrimSpace(header[1])) == "path" {
		return KindEndpoint, true
	}
	return headerKinds[first], false
}

// Table renders the cross-reference table.
func (idx *Index) Table() string {
	var b strings.Builder
	b.WriteString("| Kind | Name | Owner | Also named in |\n|---|---|---|---|\n")
	for _, n := range idx.Names {
		also := "-"
		if len(n.Also) > 0 {
			also = strings.Join(n.Also, ", ")
		}
		fmt.Fprintf(&b, "| %s | `%s` | [%s](%s) | %s |\n", n.Kind, n.Name, n.Owner, idx.files[n.Owner], also)
	}
	return b.String()
}

// Current reads the table between the markers of readme.
func Current(readme string) (string, error) {
	data, err := os.ReadFile(readme)
	if err != nil {
		return "", err
	}
	s := string(data)
	begin := strings.Index(s, BeginMarker)
	end := strings.Index(s, EndMarker)
	if begin < 0 || end < 0 || end < begin {
		return "", errors.New(readme + ": markers " + BeginMarker + " and " + EndMarker + " are missing or out of order")
	}
	return strings.TrimLeft(s[begin+len(BeginMarker):end], "\n"), nil
}

// Splice writes table between the markers of readme.
func Splice(readme, table string) error {
	data, err := os.ReadFile(readme)
	if err != nil {
		return err
	}
	s := string(data)
	begin := strings.Index(s, BeginMarker)
	end := strings.Index(s, EndMarker)
	if begin < 0 || end < 0 || end < begin {
		return errors.New(readme + ": markers " + BeginMarker + " and " + EndMarker + " are missing or out of order")
	}
	out := s[:begin+len(BeginMarker)] + "\n" + table + s[end:]
	return os.WriteFile(readme, []byte(out), 0o644)
}

// Numbers reports the spec numbers the index read, for tests.
func (idx *Index) Numbers() []string { return slices.Clone(idx.specs) }
