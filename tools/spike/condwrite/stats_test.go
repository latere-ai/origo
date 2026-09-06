// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	s := make([]time.Duration, 0, 200)
	for i := 1; i <= 200; i++ {
		s = append(s, time.Duration(i)*time.Millisecond)
	}
	for _, tc := range []struct {
		p    int
		want time.Duration
	}{
		{50, 100 * time.Millisecond},
		{95, 190 * time.Millisecond},
		{99, 198 * time.Millisecond},
		{100, 200 * time.Millisecond},
		{0, 1 * time.Millisecond},
	} {
		if got := percentile(s, tc.p); got != tc.want {
			t.Errorf("p%d = %v, want %v", tc.p, got, tc.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}

func TestSummarize(t *testing.T) {
	l := summarize("x", []time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond}, 1)
	if l.N != 3 || l.Fail != 1 || l.Min != 1 || l.Max != 3 || l.P50 != 2 || l.Mean != 2 {
		t.Errorf("unexpected summary %+v", l)
	}
	if e := summarize("x", nil, 0); e.N != 0 || e.Min != 0 {
		t.Errorf("empty summary %+v", e)
	}
}

func TestParseDefaults(t *testing.T) {
	o, err := parse([]string{"-endpoint", "http://x", "-bucket", "b", "-key", "k", "-secret", "s"})
	if err != nil {
		t.Fatal(err)
	}
	if o.region != "us-east-1" || o.samples != 200 || len(o.prefix) != len("origo-spike/0123456789abcdef/") {
		t.Errorf("unexpected defaults %+v", o)
	}
	if _, err := parse([]string{"-endpoint", "http://x"}); err == nil {
		t.Error("missing credentials accepted")
	}
}
