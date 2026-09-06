// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package metrics is a small registry of counters and histograms exposed in
// the Prometheus text format on the internal listener's /metrics. Spec 011
// names the series a dashboard reads; this package is what records them
// without a client library in the module.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing value.
type Counter struct {
	name, help string
	v          atomic.Int64
}

// Add increases the counter by n.
func (c *Counter) Add(n int64) { c.v.Add(n) }

// Inc increases the counter by one.
func (c *Counter) Inc() { c.v.Add(1) }

// Value reports the current count.
func (c *Counter) Value() int64 { return c.v.Load() }

// Histogram counts observations into cumulative buckets. Observations are
// float64 seconds by convention.
type Histogram struct {
	name, help string
	bounds     []float64
	counts     []atomic.Int64
	sum        atomic.Uint64 // float64 bits
	count      atomic.Int64
	mu         sync.Mutex
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i].Add(1)
		}
	}
	h.count.Add(1)
	h.mu.Lock()
	h.sum.Store(math.Float64bits(math.Float64frombits(h.sum.Load()) + v))
	h.mu.Unlock()
}

// Count reports the number of observations.
func (h *Histogram) Count() int64 { return h.count.Load() }

// Registry holds the series of one process.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]*Counter
	histograms map[string]*Histogram
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{counters: map[string]*Counter{}, histograms: map[string]*Histogram{}}
}

// Counter returns the counter with the name, creating it on first use.
func (r *Registry) Counter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{name: name, help: help}
	r.counters[name] = c
	return c
}

// Histogram returns the histogram with the name, creating it with the
// bounds on first use. Bounds must be ascending; +Inf is implicit.
func (r *Registry) Histogram(name, help string, bounds []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.histograms[name]; ok {
		return h
	}
	h := &Histogram{name: name, help: help, bounds: bounds, counts: make([]atomic.Int64, len(bounds))}
	r.histograms[name] = h
	return h
}

// Handler serves the registry in the Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.render()))
	})
}

func (r *Registry) render() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, name := range sortedKeys(r.counters) {
		c := r.counters[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, c.help, name, name, c.Value())
	}
	for _, name := range sortedKeys(r.histograms) {
		h := r.histograms[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", name, h.help, name)
		for i, bound := range h.bounds {
			fmt.Fprintf(&b, "%s_bucket{le=%q} %d\n", name, strconv.FormatFloat(bound, 'g', -1, 64), h.counts[i].Load())
		}
		fmt.Fprintf(&b, "%s_bucket{le=\"+Inf\"} %d\n", name, h.count.Load())
		fmt.Fprintf(&b, "%s_sum %s\n", name, strconv.FormatFloat(math.Float64frombits(h.sum.Load()), 'g', -1, 64))
		fmt.Fprintf(&b, "%s_count %d\n", name, h.count.Load())
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
