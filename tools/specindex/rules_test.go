// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rulePath = "../../deploy/base/prometheusrule.yaml"

// TestAlertRulesNameDefinedMetrics is spec 011's alert criterion in this
// module: every Origo metric an alert names is a metric the deck defines,
// and the document handed to promtool is the rules file inside the
// object.
func TestAlertRulesNameDefinedMetrics(t *testing.T) {
	document, rules, err := Rules(rulePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 10 {
		t.Errorf("%d alerts, spec 011's table has 10", len(rules))
	}
	idx, err := Build("../../specs")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range idx.CheckRules(rules) {
		t.Error(f)
	}
	if !strings.HasPrefix(document, "groups:\n") {
		t.Errorf("the document promtool reads starts %q", document[:min(20, len(document))])
	}
	if strings.Contains(document, "apiVersion") || strings.Contains(document, "kind: PrometheusRule") {
		t.Error("the Kubernetes object leaked into the rules document")
	}
	// The one metric no spec owns is another exporter's, which the check
	// leaves alone because it does not start with origo_.
	var pinned bool
	for _, r := range rules {
		if r.Alert == "OrigoReplicasPinned" {
			pinned = strings.Contains(r.Expr, "kube_horizontalpodautoscaler_")
		}
	}
	if !pinned {
		t.Error("OrigoReplicasPinned does not read the autoscaler's replica counts")
	}
}

func TestRulesReportsAnUndefinedMetricAndAMalformedFile(t *testing.T) {
	idx, err := Build("../../specs")
	if err != nil {
		t.Fatal(err)
	}
	found := idx.CheckRules([]Rule{
		{Alert: "A", Expr: `rate(origo_nowhere_total[5m]) > 0`},
		{Alert: "B", Expr: `histogram_quantile(0.99, rate(origo_wal_head_check_seconds_bucket[5m])) > 0.05`},
		{Alert: "C", Expr: `kube_horizontalpodautoscaler_status_current_replicas > 0`},
	})
	if len(found) != 1 || !strings.Contains(found[0], "origo_nowhere_total") {
		t.Fatalf("findings %v", found)
	}

	dir := t.TempDir()
	for name, body := range map[string]string{
		"no-spec.yaml":    "kind: PrometheusRule\n",
		"unindented.yaml": "spec:\ngroups: []\n",
		"no-alert.yaml":   "spec:\n  groups: []\n",
		"no-expr.yaml":    "spec:\n  groups:\n    - rules:\n        - alert: A\n",
		"early-expr.yaml": "spec:\n  groups:\n    - rules:\n        expr: up\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Rules(path); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, _, err := Rules(filepath.Join(dir, "absent.yaml")); err == nil {
		t.Error("a missing file was accepted")
	}
}
