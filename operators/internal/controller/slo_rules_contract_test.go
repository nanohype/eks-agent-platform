/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The operator's SLO alerting lives in a chart file (charts/operator/files/slo/
// prometheusrule.yaml) that no Go code imports, so nothing else keeps it honest.
// This test pins the observability-slo contract: the SLI recording-rule naming
// convention (<metric>:sli_error:ratio_rate<window>) for every burn-rate window,
// and the four multi-window multi-burn-rate alert pairs that reference them
// instead of recomputing an instantaneous ratio. If someone renames a recording
// rule, drops a window, or reintroduces an instantaneous-ratio page, the build
// fails here.

type sloRuleFile struct {
	Spec struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Record string            `json:"record"`
				Alert  string            `json:"alert"`
				Expr   string            `json:"expr"`
				For    string            `json:"for"`
				Labels map[string]string `json:"labels"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"spec"`
}

func loadSLORules(t *testing.T) sloRuleFile {
	t.Helper()
	// Package dir → repo root is three levels up.
	path := filepath.Join("..", "..", "..", "charts", "operator", "files", "slo", "prometheusrule.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read SLO PrometheusRule at %s: %v", path, err)
	}
	var f sloRuleFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse SLO PrometheusRule: %v", err)
	}
	return f
}

func TestSLORecordingRuleConvention(t *testing.T) {
	f := loadSLORules(t)
	records := map[string]string{} // record name -> expr
	for _, g := range f.Spec.Groups {
		for _, r := range g.Rules {
			if r.Record != "" {
				records[r.Record] = r.Expr
			}
		}
	}

	// One SLI error-ratio recording rule per burn-rate window, named per the
	// observability-slo convention.
	windows := []string{"5m", "30m", "1h", "2h", "6h", "1d", "3d"}
	for _, w := range windows {
		name := "eks_agent_platform:sli_error:ratio_rate" + w
		expr, ok := records[name]
		if !ok {
			t.Errorf("missing SLI recording rule %q", name)
			continue
		}
		// The SLI must be a good/valid error ratio over the matching window.
		if !strings.Contains(expr, "controller_runtime_reconcile_errors_total") ||
			!strings.Contains(expr, "controller_runtime_reconcile_total") {
			t.Errorf("recording rule %q is not an errors/total ratio: %s", name, expr)
		}
		if !strings.Contains(expr, "["+w+"]") {
			t.Errorf("recording rule %q does not range over its own window [%s]: %s", name, w, expr)
		}
	}
}

func TestSLOBurnRateAlerts(t *testing.T) {
	f := loadSLORules(t)
	alerts := map[string]struct {
		expr   string
		labels map[string]string
	}{}
	for _, g := range f.Spec.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				alerts[r.Alert] = struct {
					expr   string
					labels map[string]string
				}{r.Expr, r.Labels}
			}
		}
	}

	// The canonical four multi-window multi-burn-rate pairs: each references a
	// long AND a short SLI recording rule (multi-window), and the page/ticket
	// severity follows the Google SRE table.
	cases := []struct {
		alert       string
		long, short string
		severity    string
	}{
		{"ReconcileErrorBudgetFastBurn", "ratio_rate1h", "ratio_rate5m", "critical"},
		{"ReconcileErrorBudgetBurn", "ratio_rate6h", "ratio_rate30m", "critical"},
		{"ReconcileErrorBudgetSlowBurn", "ratio_rate1d", "ratio_rate2h", "warning"},
		{"ReconcileErrorBudgetLowBurn", "ratio_rate3d", "ratio_rate6h", "warning"},
	}
	for _, c := range cases {
		a, ok := alerts[c.alert]
		if !ok {
			t.Errorf("missing burn-rate alert %q", c.alert)
			continue
		}
		if !strings.Contains(a.expr, c.long) || !strings.Contains(a.expr, c.short) {
			t.Errorf("alert %q must reference both %q and %q; expr: %s", c.alert, c.long, c.short, a.expr)
		}
		if !strings.Contains(a.expr, " and ") && !strings.Contains(a.expr, "\nand\n") {
			t.Errorf("alert %q is not multi-window (no `and` joining long+short): %s", c.alert, a.expr)
		}
		// It must reference the pre-recorded SLI series, not recompute the ratio
		// inline (observability-slo: alert on burn rate, not instantaneous ratio).
		if strings.Contains(a.expr, "rate(controller_runtime_reconcile_errors_total") {
			t.Errorf("alert %q recomputes the error ratio inline; it must reference the SLI recording rules", c.alert)
		}
		if a.labels["severity"] != c.severity {
			t.Errorf("alert %q severity = %q; want %q", c.alert, a.labels["severity"], c.severity)
		}
		if a.labels["slo"] != "reconcile-availability" {
			t.Errorf("alert %q missing slo=reconcile-availability label", c.alert)
		}
	}

	// The instantaneous-error-ratio page must be gone — it is replaced by the
	// burn-rate pairs (observability-slo do_not: alert on instantaneous ratio).
	if _, ok := alerts["ReconcileErrorRateHigh"]; ok {
		t.Error("instantaneous ReconcileErrorRateHigh alert still present; it must be replaced by the burn-rate pairs")
	}
}
