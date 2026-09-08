// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	pkgmetrics "latere.ai/x/pkg/metrics"
)

// specPath is the spec whose table this package is. It is resolved from
// this file's own path, never from the working directory, so the suite
// finds it when it runs from an empty temporary directory (the tempdir
// gate of spec 002) and under the hermetic gate.
func specPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test's own source file is unknown")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "specs", "011-observability.md")
}

var backtick = regexp.MustCompile("`([^`]+)`")

// specMetrics reads the Metric table of spec 011: every backticked name
// of the first cell against the type word of the second.
func specMetrics(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(specPath(t))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if cells(line)[0] != "Metric" {
			continue
		}
		for j := i + 2; j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "|"); j++ {
			row := cells(lines[j])
			if len(row) < 2 {
				continue
			}
			kind, _, _ := strings.Cut(row[1], ",")
			for _, m := range backtick.FindAllStringSubmatch(row[0], -1) {
				out[m[1]] = strings.TrimSpace(kind)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no Metric table found", specPath(t))
	}
	return out
}

func cells(line string) []string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") || !strings.HasSuffix(t, "|") {
		return []string{""}
	}
	parts := strings.Split(strings.Trim(t, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// TestRegisterNamesEveryMetric is spec 011's first criterion on this
// package: the table in code is the table in the spec, name for name and
// type for type, and Register puts all of it on the registry.
func TestRegisterNamesEveryMetric(t *testing.T) {
	want := specMetrics(t)
	kinds := map[kind]string{counter: "counter", histogram: "histogram", gauge: "gauge"}

	got := map[string]string{}
	for _, m := range table {
		if _, dup := got[m.Name]; dup {
			t.Errorf("%s is in the table twice", m.Name)
		}
		got[m.Name] = kinds[m.Kind]
	}
	for name, kind := range want {
		switch {
		case got[name] == "":
			t.Errorf("%s: the spec defines it and the table does not", name)
		case got[name] != kind:
			t.Errorf("%s: the table says %s, the spec says %s", name, got[name], kind)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: the table defines it and the spec does not", name)
		}
	}

	reg := pkgmetrics.NewRegistry()
	Register(reg)
	exposed := expose(reg)
	for _, name := range Names() {
		if !strings.Contains(exposed, "# TYPE "+name+" ") {
			t.Errorf("%s is not on the registry after Register", name)
		}
	}
	if len(Names()) != len(table) {
		t.Errorf("Names reports %d of %d rows", len(Names()), len(table))
	}
}

// TestEveryClosedVocabularyReadsZero holds the second half of the
// presence rule: a counter whose labels have a fixed vocabulary carries
// one series per combination at 0 before anything is recorded, and a
// gauge carries one series per value.
func TestEveryClosedVocabularyReadsZero(t *testing.T) {
	reg := pkgmetrics.NewRegistry()
	Register(reg)
	exposed := expose(reg)
	for _, want := range []string{
		"origo_pushes_total 0",
		`origo_evictions_total{reason="idle"} 0`,
		`origo_evictions_total{reason="pressure"} 0`,
		`origo_gossip_packets_total{direction="dropped"} 0`,
		`origo_rate_limited_total{limit="subprocesses"} 0`,
		`origo_compactions_total{result="skipped"} 0`,
		`origo_storage_ops_total{op="create",result="exists"} 0`,
		`origo_storage_ops_total{op="list",result="error"} 0`,
		"origo_requests_in_flight 0",
		"origo_cache_bytes 0",
		"origo_orphan_objects 0",
		`origo_storage_breaker_state{class="read"} 0`,
		`origo_storage_breaker_state{class="write"} 0`,
	} {
		if !strings.Contains(exposed, want+"\n") {
			t.Errorf("%q is not in the first scrape", want)
		}
	}
	// An open vocabulary is seeded with nothing: a route is the mux
	// pattern, so there is no set of values to write at 0.
	if strings.Contains(exposed, "origo_requests_total{") {
		t.Error("origo_requests_total carries a series before a request")
	}
}

func TestGaugesReadTheirBoundSource(t *testing.T) {
	reg := pkgmetrics.NewRegistry()
	set := Register(reg)
	bytes := 0.0
	set.CacheBytes.Bind(func() float64 { return bytes })
	set.StorageBreaker.BindFor("write", func() float64 { return 2 })
	bytes = 4096
	exposed := expose(reg)
	for _, want := range []string{"origo_cache_bytes 4096", `origo_storage_breaker_state{class="write"} 2`, `origo_storage_breaker_state{class="read"} 0`} {
		if !strings.Contains(exposed, want+"\n") {
			t.Errorf("%q is not in the scrape:\n%s", want, exposed)
		}
	}
	bytes = 8192
	if !strings.Contains(expose(reg), "origo_cache_bytes 8192\n") {
		t.Error("the gauge is not read at every scrape")
	}
}

func TestBindingAValueOutsideTheVocabularyPanics(t *testing.T) {
	set := Register(nil)
	for _, bind := range []func(){
		func() { set.StorageBreaker.BindFor("neither", func() float64 { return 0 }) },
		func() { set.CacheBytes.BindFor("read", func() float64 { return 0 }) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("a value outside the vocabulary was accepted")
				}
			}()
			bind()
		}()
	}
}

// TestAFieldWithoutItsRowPanics proves the registrar's guard: a Set
// field whose row is missing or has another type fails at start-up.
func TestAFieldWithoutItsRowPanics(t *testing.T) {
	r := registrar{counters: map[string]*pkgmetrics.Counter{}, histograms: map[string]*pkgmetrics.Histogram{}, gauges: map[string]*Gauge{}}
	for _, get := range []func(){
		func() { r.counter("origo_absent_total") },
		func() { r.histogram("origo_absent_seconds") },
		func() { r.gauge("origo_absent") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("a missing row was accepted")
				}
			}()
			get()
		}()
	}
}

func TestCombinationsAreTheCrossProduct(t *testing.T) {
	if got := combinations(nil); len(got) != 1 || got[0] != nil {
		t.Fatalf("no label: %v", got)
	}
	if got := combinations([]label{{Name: "route"}}); got != nil {
		t.Fatalf("open vocabulary: %v", got)
	}
	got := combinations([]label{{Name: "op", Values: []string{"get", "put"}}, {Name: "result", Values: []string{"ok", "error"}}})
	if len(got) != 4 {
		t.Fatalf("%d combinations, want 4", len(got))
	}
	seen := map[string]bool{}
	for _, m := range got {
		seen[m["op"]+"/"+m["result"]] = true
	}
	for _, want := range []string{"get/ok", "get/error", "put/ok", "put/error"} {
		if !seen[want] {
			t.Errorf("%s is missing from %v", want, got)
		}
	}
}

func expose(reg *pkgmetrics.Registry) string {
	var b strings.Builder
	reg.WritePrometheus(&b)
	return b.String()
}
