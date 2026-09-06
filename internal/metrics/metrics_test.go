// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCountersAndHistogramsRenderInExpositionFormat(t *testing.T) {
	r := New()
	c := r.Counter("origo_pushes_total", "pushes")
	c.Inc()
	c.Add(2)
	if r.Counter("origo_pushes_total", "again") != c {
		t.Fatal("a second lookup made a second counter")
	}
	h := r.Histogram("origo_head_seconds", "head", []float64{0.01, 0.1})
	h.Observe(0.005)
	h.Observe(0.05)
	h.Observe(1)
	if r.Histogram("origo_head_seconds", "again", nil) != h {
		t.Fatal("a second lookup made a second histogram")
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE origo_pushes_total counter\norigo_pushes_total 3\n",
		"origo_head_seconds_bucket{le=\"0.01\"} 1\n",
		"origo_head_seconds_bucket{le=\"0.1\"} 2\n",
		"origo_head_seconds_bucket{le=\"+Inf\"} 3\n",
		"origo_head_seconds_sum 1.055\n",
		"origo_head_seconds_count 3\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if c.Value() != 3 || h.Count() != 3 {
		t.Fatalf("Value = %d, Count = %d", c.Value(), h.Count())
	}
}

func TestObserveIsSafeUnderConcurrency(t *testing.T) {
	h := New().Histogram("h", "", []float64{1})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				h.Observe(0.5)
			}
		})
	}
	wg.Wait()
	if h.Count() != 800 {
		t.Fatalf("Count = %d", h.Count())
	}
}
