/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

// budget99 is the error budget for the standard's default availability
// objective: 1 - 0.999.
const budget99 = 0.001

func TestQueryWindowsAreDistinctAndCoverEveryPair(t *testing.T) {
	got := queryWindows()
	seen := map[string]int{}
	for _, w := range got {
		seen[w]++
	}
	for w, n := range seen {
		if n > 1 {
			t.Errorf("window %q queried %d times; the tick must de-duplicate overlapping pairs", w, n)
		}
	}
	// 6h is the long window of the 6x page pair AND the short window of the 1x
	// ticket pair. If de-duplication regresses, this is the window that doubles.
	if seen["6h"] != 1 {
		t.Errorf("6h appears %d times; it is shared between a page and a ticket pair and must be queried once", seen["6h"])
	}
	for _, w := range burnWindows {
		if seen[w.long] == 0 {
			t.Errorf("long window %q of the %s pair is never queried", w.long, w.severity)
		}
		if seen[w.short] == 0 {
			t.Errorf("short window %q of the %s pair is never queried", w.short, w.severity)
		}
	}
	if seen[sloWindow] == 0 {
		t.Errorf("the SLO window %q is never queried, so error budget remaining cannot be computed", sloWindow)
	}
	if len(got) != 8 {
		t.Errorf("want 8 distinct windows (7 burn + the 30d SLO window), got %d: %v", len(got), got)
	}
}

// The burn-rate table is transcribed from the observability-slo standard. This
// pins the transcription so a stray edit fails here rather than silently
// changing when the platform holds a tenant's rollout.
func TestBurnWindowsMatchTheStandard(t *testing.T) {
	want := []burnWindow{
		{severity: severityCritical, long: "1h", short: "5m", factor: 14.4},
		{severity: severityCritical, long: "6h", short: "30m", factor: 6},
		{severity: severityWarning, long: "1d", short: "2h", factor: 3},
		{severity: severityWarning, long: "3d", short: "6h", factor: 1},
	}
	if len(burnWindows) != len(want) {
		t.Fatalf("burnWindows has %d entries, the standard defines %d", len(burnWindows), len(want))
	}
	for i, w := range want {
		if burnWindows[i] != w {
			t.Errorf("burnWindows[%d] = %+v, standard says %+v", i, burnWindows[i], w)
		}
	}
}

func TestEvaluateBurn(t *testing.T) {
	// At objective 0.999 the budget is 0.001, so an error ratio of 0.0144 is
	// exactly 14.4x — the fast-burn factor.
	const atFastFactor = 14.4 * budget99
	const overFastFactor = 0.02
	const overSlowTicket = 0.0031 // 3.1x, over the 3x ticket factor

	cases := []struct {
		name       string
		ratios     map[string]float64
		wantSev    string
		wantWindow string
		wantData   bool
	}{
		{
			name:     "healthy",
			ratios:   map[string]float64{"1h": 0, "5m": 0, "6h": 0, "30m": 0, "1d": 0, "2h": 0, "3d": 0},
			wantSev:  "",
			wantData: true,
		},
		{
			name:       "fast page burn on both windows",
			ratios:     map[string]float64{"1h": overFastFactor, "5m": overFastFactor, "6h": 0, "30m": 0, "1d": 0, "2h": 0, "3d": 0},
			wantSev:    severityCritical,
			wantWindow: "1h",
			wantData:   true,
		},
		{
			// The long window is burning but the short one has recovered. This
			// is the flap the dual-window method exists to suppress.
			name:     "long window only does not breach",
			ratios:   map[string]float64{"1h": overFastFactor, "5m": 0, "6h": 0, "30m": 0, "1d": 0, "2h": 0, "3d": 0},
			wantSev:  "",
			wantData: true,
		},
		{
			// Strictly greater-than, matching the operator's own PrometheusRule
			// (`ratio > factor * budget`). Exactly at the factor is not a breach
			// on either evaluator.
			name:     "exactly at the factor does not breach",
			ratios:   map[string]float64{"1h": atFastFactor, "5m": atFastFactor, "6h": 0, "30m": 0, "1d": 0, "2h": 0, "3d": 0},
			wantSev:  "",
			wantData: true,
		},
		{
			name:       "ticket tier alone",
			ratios:     map[string]float64{"1h": 0, "5m": 0, "6h": 0, "30m": 0, "1d": overSlowTicket, "2h": overSlowTicket, "3d": 0},
			wantSev:    severityWarning,
			wantWindow: "1d",
			wantData:   true,
		},
		{
			// A page-tier burn is also a ticket-tier burn; the higher tier is
			// the one that must route.
			name: "critical outranks warning",
			ratios: map[string]float64{
				"1h": overFastFactor, "5m": overFastFactor, "6h": 0, "30m": 0,
				"1d": overSlowTicket, "2h": overSlowTicket, "3d": overSlowTicket,
			},
			wantSev:    severityCritical,
			wantWindow: "1h",
			wantData:   true,
		},
		{
			// Missing the short window means the pair cannot be confirmed, so
			// it is not evaluated at all — not evaluated as healthy.
			name:     "pair with a missing short window is skipped",
			ratios:   map[string]float64{"1h": overFastFactor},
			wantSev:  "",
			wantData: false,
		},
		{
			name:     "no data at all",
			ratios:   map[string]float64{},
			wantSev:  "",
			wantData: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evaluateBurn(c.ratios, budget99)
			if got.severity != c.wantSev {
				t.Errorf("severity = %q, want %q (page %.4f, ticket %.4f)", got.severity, c.wantSev, got.pageRatio, got.ticketRatio)
			}
			if c.wantWindow != "" && got.breachedWindow != c.wantWindow {
				t.Errorf("breachedWindow = %q, want %q", got.breachedWindow, c.wantWindow)
			}
			if got.anyData != c.wantData {
				t.Errorf("anyData = %v, want %v", got.anyData, c.wantData)
			}
		})
	}
}

// A zero budget (objective 1.0) would divide by zero and make every comparison
// meaningless. The CRD pattern forbids it; the evaluator refuses it anyway.
func TestEvaluateBurnRejectsZeroBudget(t *testing.T) {
	got := evaluateBurn(map[string]float64{"1h": 1, "5m": 1}, 0)
	if got.severity != "" || got.anyData {
		t.Errorf("a zero error budget must yield no verdict, got %+v", got)
	}
}

func TestErrorBudgetRemaining(t *testing.T) {
	cases := []struct {
		name      string
		ratios    map[string]float64
		want      float64
		wantHave  bool
		tolerance float64
	}{
		{name: "untouched budget", ratios: map[string]float64{sloWindow: 0}, want: 1, wantHave: true},
		{name: "half spent", ratios: map[string]float64{sloWindow: 0.0005}, want: 0.5, wantHave: true, tolerance: 1e-9},
		{name: "overspent clamps to zero", ratios: map[string]float64{sloWindow: 0.05}, want: 0, wantHave: true},
		{name: "no reading", ratios: map[string]float64{"1h": 0}, wantHave: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, have := errorBudgetRemaining(c.ratios, budget99)
			if have != c.wantHave {
				t.Fatalf("have = %v, want %v", have, c.wantHave)
			}
			if have && got-c.want > c.tolerance && c.want-got > c.tolerance {
				t.Errorf("remaining = %v, want %v", got, c.want)
			}
		})
	}
}

func TestBuildErrorRatioQuery(t *testing.T) {
	availability := governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: "checkout_api"}
	latency := governancev1alpha1.SLI{Type: sliTypeLatency, Metric: "checkout_api", ThresholdSeconds: "0.5"}

	t.Run("availability defaults an absent errors counter to zero", func(t *testing.T) {
		q, err := buildErrorRatioQuery(availability, "1h")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Without the OR, a service that has never errored has no _errors_total
		// series, the whole ratio evaluates empty, and a healthy service reports
		// NoData forever. The OR is gated on absent() so it covers only that
		// case — see TestAvailabilityZeroDefaultIsScopedToAnAbsentFamily.
		if !strings.Contains(q, "or (vector(0) and absent(") {
			t.Errorf("availability query must default an ABSENT errors family to zero: %s", q)
		}
		if !strings.Contains(q, "checkout_api_errors_total") || !strings.Contains(q, "checkout_api_requests_total") {
			t.Errorf("availability query must divide errors by requests: %s", q)
		}
		if !strings.Contains(q, "[1h]") {
			t.Errorf("query must range over its own window: %s", q)
		}
	})

	t.Run("latency does not default an absent bucket", func(t *testing.T) {
		q, err := buildErrorRatioQuery(latency, "5m")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// The opposite of availability, and deliberately: an absent
		// under-threshold bucket would read as "no request was fast" and
		// fabricate a 100% error ratio — an instant page from a threshold that
		// merely names a bucket the histogram does not publish.
		if strings.Contains(q, "vector(0)") {
			t.Errorf("latency query must NOT default a missing bucket to zero, or an unpublished le fabricates a total breach: %s", q)
		}
		if !strings.Contains(q, `le="0.5"`) {
			t.Errorf("latency query must select the threshold bucket: %s", q)
		}
	})

	t.Run("denominator is floored and the result clamped", func(t *testing.T) {
		q, err := buildErrorRatioQuery(availability, "1h")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(q, "clamp_min(sum(rate(checkout_api_requests_total[1h])), 0.001)") {
			t.Errorf("denominator must be floored so an idle service reads 0, not NaN: %s", q)
		}
		if !strings.HasPrefix(q, "clamp_max(clamp_min(") {
			t.Errorf("ratio must be clamped into [0,1] so a mid-scrape histogram cannot manufacture a burn: %s", q)
		}
	})

	t.Run("selector renders sorted exact matchers", func(t *testing.T) {
		sli := availability
		sli.Selector = map[string]string{"route": "/pay", "namespace": "tenants-acme"}
		q, err := buildErrorRatioQuery(sli, "1h")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Sorted, so the generated query is stable across reconciles.
		if !strings.Contains(q, `{namespace="tenants-acme",route="/pay"}`) {
			t.Errorf("selector must render as sorted exact matchers: %s", q)
		}
	})

	t.Run("rejects the injection surface", func(t *testing.T) {
		bad := []struct {
			name string
			sli  governancev1alpha1.SLI
			win  string
		}{
			{"metric with a brace", governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: `x} or up{`}, "1h"},
			{"reserved le label", governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: "x", Selector: map[string]string{"le": "1"}}, "1h"},
			{"reserved __name__ label", governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: "x", Selector: map[string]string{"__name__": "up"}}, "1h"},
			{"label name with a quote", governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: "x", Selector: map[string]string{`a"b`: "1"}}, "1h"},
			{"unknown sli type", governancev1alpha1.SLI{Type: "throughput", Metric: "x"}, "1h"},
			{"latency without a threshold", governancev1alpha1.SLI{Type: sliTypeLatency, Metric: "x"}, "1h"},
			{"non-duration window", governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: "x"}, "1 hour"},
		}
		for _, c := range bad {
			if _, err := buildErrorRatioQuery(c.sli, c.win); err == nil {
				t.Errorf("%s must be rejected before a query is signed and sent", c.name)
			}
		}
	})

	t.Run("selector values are escaped, not rejected", func(t *testing.T) {
		sli := availability
		sli.Selector = map[string]string{"route": `a"b\c`}
		q, err := buildErrorRatioQuery(sli, "1h")
		if err != nil {
			t.Fatalf("a quote in a label VALUE is legal once escaped: %v", err)
		}
		if !strings.Contains(q, `route="a\"b\\c"`) {
			t.Errorf("label value must be escaped so it cannot terminate the matcher: %s", q)
		}
	})
}

func TestEscapePromLabelValue(t *testing.T) {
	cases := map[string]string{
		"plain":     "plain",
		`say "hi"`:  `say \"hi\"`,
		`back\\ard`: `back\\\\ard`,
		"two\nline": `two\nline`,
	}
	for in, want := range cases {
		if got := escapePromLabelValue(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHoldGraceWindowDefaults(t *testing.T) {
	cases := []struct {
		name     string
		r        SLOReconciler
		expected time.Duration
	}{
		{"explicit", SLOReconciler{RequeueInterval: time.Minute, HoldGraceIntervals: 4}, 4 * time.Minute},
		{"default intervals", SLOReconciler{RequeueInterval: time.Minute}, 2 * time.Minute},
		{"default interval too", SLOReconciler{}, 10 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.r.holdGraceWindow(); got != c.expected {
				t.Errorf("holdGraceWindow = %s, want %s", got, c.expected)
			}
		})
	}
}

func TestHoldUnobserved(t *testing.T) {
	r := &SLOReconciler{RequeueInterval: time.Minute, HoldGraceIntervals: 2} // 2m grace
	now := time.Now()
	engagedAt := func(d time.Duration) *governancev1alpha1.SLOPolicy {
		ts := metav1.NewTime(now.Add(d))
		return &governancev1alpha1.SLOPolicy{Status: governancev1alpha1.SLOPolicyStatus{HoldEngagedAt: &ts}}
	}

	cases := []struct {
		name string
		sp   *governancev1alpha1.SLOPolicy
		obs  holdObservation
		want bool
	}{
		{"never engaged", &governancev1alpha1.SLOPolicy{}, holdObservation{}, false},
		{"engaged and observed", engagedAt(-time.Hour), holdObservation{present: true}, false},
		{"engaged, inside grace", engagedAt(-30 * time.Second), holdObservation{}, false},
		{"engaged, past grace, absent", engagedAt(-10 * time.Minute), holdObservation{}, true},
		// No AppProject to inspect is not a broken hold — ArgoCD is not on every
		// cluster, and reporting that as a fault would page for a design choice.
		{"engaged, past grace, unverifiable", engagedAt(-10 * time.Minute), holdObservation{unverifiable: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.holdUnobserved(c.sp, c.obs, now); got != c.want {
				t.Errorf("holdUnobserved = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsSLOHoldWindow(t *testing.T) {
	if !isSLOHoldWindow(sloDenyWindow()) {
		t.Fatal("the reconciler must recognize the window it renders")
	}
	// A subset match, so an extra field defaulted by ArgoCD does not stop the
	// reconciler recognizing its own write and lighting the hold-broken signal.
	withExtra := sloDenyWindow()
	withExtra["namespaces"] = []interface{}{"*"}
	if !isSLOHoldWindow(withExtra) {
		t.Error("an extra field on the window must not break recognition")
	}
	foreign := []map[string]interface{}{
		{"kind": "allow", "schedule": sloHoldSchedule, "duration": sloHoldDuration, "applications": []interface{}{"*"}},
		{"kind": "deny", "schedule": "0 9 * * 1-5", "duration": sloHoldDuration, "applications": []interface{}{"*"}},
		{"kind": "deny", "schedule": sloHoldSchedule, "duration": "1h", "applications": []interface{}{"*"}},
		{"kind": "deny", "schedule": sloHoldSchedule, "duration": sloHoldDuration, "applications": []interface{}{"one-app"}},
		{"kind": "deny", "schedule": sloHoldSchedule, "duration": sloHoldDuration},
	}
	for i, w := range foreign {
		if isSLOHoldWindow(w) {
			t.Errorf("foreign window %d must not be claimed by the reconciler: %v", i, w)
		}
	}
}

// The hold leaves manual sync open on purpose: it stops automation from
// advancing a burning rollout, it does not lock an on-call engineer out of
// shipping the fix.
func TestSLODenyWindowKeepsManualSyncOpen(t *testing.T) {
	w := sloDenyWindow()
	if w["manualSync"] != true {
		t.Error("the deny window must leave manualSync open so a human can still push a fix")
	}
	if w["kind"] != "deny" {
		t.Errorf("kind = %v, want deny", w["kind"])
	}
}

func TestFormatRatio(t *testing.T) {
	if got := formatRatio(1.0 / 3.0); got != "0.333333" {
		t.Errorf("formatRatio = %q, want six fractional digits", got)
	}
}

func TestTierRatioPicksTheBreachingTier(t *testing.T) {
	eval := burnEvaluation{severity: severityCritical, pageRatio: 2, ticketRatio: 9}
	if got := tierRatio(eval); got != 2 {
		t.Errorf("critical breach must report the page ratio, got %v", got)
	}
	eval.severity = severityWarning
	if got := tierRatio(eval); got != 9 {
		t.Errorf("warning breach must report the ticket ratio, got %v", got)
	}
}

// The zero-default exists for a service that has never errored. It must NOT
// cover a selector that matches no errors series — that is a misconfiguration,
// and defaulting it to zero reports a perfectly healthy objective that is
// measuring nothing at all.
func TestAvailabilityZeroDefaultIsScopedToAnAbsentFamily(t *testing.T) {
	q, err := buildErrorRatioQuery(governancev1alpha1.SLI{
		Type: sliTypeAvailability, Metric: "acme_api",
		Selector: map[string]string{"route": "/checkout"},
	}, "1h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(q, "absent(acme_api_errors_total)") {
		t.Errorf("the zero-default must be gated on the errors family being absent entirely, or a selector that matches nothing reads as healthy: %s", q)
	}
	// The guard has to be on the unselected family name: absent() of the
	// selected series would be true whenever the selector misses, which is the
	// very case that must NOT default to zero.
	if strings.Contains(q, `absent(acme_api_errors_total{route="/checkout"})`) {
		t.Errorf("absent() must test the metric family, not the selected series — otherwise it re-creates the bug it fixes: %s", q)
	}
}

// A tier whose windows did not resolve must not be reported at all. Publishing
// 0 for it is a fabricated healthy reading for a tier nobody measured, and a
// single missed scrape empties the 5m window while the long ones still resolve.
func TestEvaluateBurnMarksAnUnresolvedTier(t *testing.T) {
	// Every ticket window present, both page short-windows missing.
	ratios := map[string]float64{"1h": 0.02, "6h": 0.02, "1d": 0, "2h": 0, "3d": 0}
	got := evaluateBurn(ratios, budget99)

	if got.pageHaveData {
		t.Error("a page tier whose short windows are absent must not report data — the dual-window check cannot be evaluated")
	}
	if !got.ticketHaveData {
		t.Error("the ticket tier resolved and should still be evaluated")
	}
	if !got.anyData {
		t.Error("anyData must stay true — one tier resolved, so this is not a total signal loss")
	}
	if got.severity != "" {
		t.Errorf("severity = %q; an unevaluable page tier must not produce a verdict", got.severity)
	}
}
