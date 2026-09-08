// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Rule is one alert of the PrometheusRule.
type Rule struct {
	Alert string
	Expr  string
}

var (
	alertLine  = regexp.MustCompile(`^\s*- alert:\s*(\S+)\s*$`)
	exprLine   = regexp.MustCompile(`^\s*expr:\s*(.+?)\s*$`)
	metricTok  = regexp.MustCompile(`origo_[a-z0-9_]+`)
	suffixes   = []string{"_bucket", "_sum", "_count"}
	specKey    = "spec:"
	ruleIndent = "  "
)

// Rules reads a PrometheusRule and returns two things: the plain
// Prometheus rules document inside its spec, which is what `promtool
// check rules` parses (promtool reads a rules file, not a Kubernetes
// object), and the alerts the file declares.
//
// The extraction is the file's own shape: spec is the last key at column
// zero and every line under it is indented by two spaces, so removing
// that indent leaves the document promtool expects.
func Rules(path string) (string, []Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimRight(l, " ") == specKey {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", nil, errors.New(path + ": no spec: key at column zero")
	}
	var document []string
	var rules []Rule
	for _, l := range lines[start:] {
		switch {
		case strings.TrimSpace(l) == "":
			document = append(document, "")
			continue
		case !strings.HasPrefix(l, ruleIndent):
			return "", nil, fmt.Errorf("%s: %q under spec: is not indented", path, l)
		}
		document = append(document, strings.TrimPrefix(l, ruleIndent))
		if m := alertLine.FindStringSubmatch(l); m != nil {
			rules = append(rules, Rule{Alert: m[1]})
			continue
		}
		if m := exprLine.FindStringSubmatch(l); m != nil {
			if len(rules) == 0 {
				return "", nil, fmt.Errorf("%s: an expr before any alert: %q", path, l)
			}
			rules[len(rules)-1].Expr = m[1]
		}
	}
	if len(rules) == 0 {
		return "", nil, errors.New(path + ": no alert declared")
	}
	for _, r := range rules {
		if r.Expr == "" {
			return "", nil, errors.New(path + ": " + r.Alert + " has no expr")
		}
	}
	return strings.Join(document, "\n"), rules, nil
}

// CheckRules reports every Origo metric an alert names that no spec
// defines. A series selector carries the metric with a histogram's
// _bucket, _sum, or _count suffix, which is not a name of its own; a
// metric another exporter publishes does not start with origo_ and is
// not this file's to check.
func (idx *Index) CheckRules(rules []Rule) []string {
	defined := map[string]bool{}
	for _, n := range idx.Names {
		if n.Kind == KindMetric {
			defined[n.Name] = true
		}
	}
	var findings []string
	for _, r := range rules {
		for _, tok := range metricTok.FindAllString(r.Expr, -1) {
			name := tok
			for _, s := range suffixes {
				if base := strings.TrimSuffix(name, s); base != name {
					name = base
					break
				}
			}
			if !defined[name] {
				findings = append(findings, fmt.Sprintf("alert %s names the metric %q, which no spec defines", r.Alert, name))
			}
		}
	}
	sort.Strings(findings)
	return findings
}
