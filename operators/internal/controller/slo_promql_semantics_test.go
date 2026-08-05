/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

// The other query tests assert the STRING buildErrorRatioQuery produces. That
// catches a builder that stops emitting what it used to, and cannot catch a
// query that is well-formed and means the wrong thing.
//
// These execute it. The query the operator actually builds is run by a real
// PromQL engine against synthetic series, and the assertion is the number that
// comes out the far end.
//
// It earns its keep on the zero-default guards, which is where every subtlety
// in this builder lives. "Absent numerator" has to mean healthy-zero in one
// situation and no-data in another, and which is which is decided by operator
// precedence between `or`, `and`, `unless` and `on()` — the kind of thing that
// reads correct and evaluates backwards.

// promSeries is one input series and its sample progression.
type promSeries struct {
	series string
	values string
}

// promCase is one executed scenario: series in, expected ratio out.
type promCase struct {
	name string
	sli  governancev1alpha1.SLI
	// input series, in promtool's `series`/`values` form.
	input []promSeries
	// want is the expected sample value; wantNoData asserts the query returns
	// nothing at all, which for this builder means "unknown, do not act".
	want       float64
	wantNoData bool
	why        string
}

func promCases() []promCase {
	availability := func(errSel map[string]string) governancev1alpha1.SLI {
		return governancev1alpha1.SLI{
			Type:          "availability",
			Metric:        "app",
			Selector:      map[string]string{"route": "/pay"},
			ErrorSelector: errSel,
		}
	}
	return []promCase{
		{
			name: "same-series/healthy-zero-when-the-error-label-is-present",
			sli:  availability(map[string]string{"status": "error"}),
			input: []promSeries{
				{`app_requests_total{route="/pay",status="ok"}`, "0+10x20"},
			},
			want: 0,
			why: "the error label key is on the series and nothing matched its value, " +
				"which is a service that is genuinely not erroring",
		},
		{
			name: "same-series/no-data-when-the-error-label-key-is-absent",
			sli:  availability(map[string]string{"status": "error"}),
			input: []promSeries{
				{`app_requests_total{route="/pay"}`, "0+10x20"},
			},
			wantNoData: true,
			why: "nothing on the series carries the key, so the selector is wrong. " +
				"This is the case the guard exists for: without it a typo would read a " +
				"permanent, perfectly healthy zero",
		},
		{
			name: "same-series/real-error-ratio",
			sli:  availability(map[string]string{"status": "error"}),
			input: []promSeries{
				{`app_requests_total{route="/pay",status="ok"}`, "0+90x20"},
				{`app_requests_total{route="/pay",status="error"}`, "0+10x20"},
			},
			want: 0.1,
			why:  "10 of every 100 requests carry the error label",
		},
		{
			name: "same-series/partially-present-key-set-still-refuses",
			sli: governancev1alpha1.SLI{
				Type:          "availability",
				Metric:        "app",
				Selector:      map[string]string{"route": "/pay"},
				ErrorSelector: map[string]string{"status": "error", "tier": "page"},
			},
			input: []promSeries{
				{`app_requests_total{route="/pay",tier="page"}`, "0+10x20"},
			},
			wantNoData: true,
			why:        "one of the two named keys is missing, so the selector is still wrong",
		},
		{
			name: "two-counter/healthy-zero-when-the-errors-family-does-not-exist",
			sli:  availability(nil),
			input: []promSeries{
				{`app_requests_total{route="/pay"}`, "0+10x20"},
			},
			want: 0,
			why:  "a service that has never returned an error publishes no _errors_total at all",
		},
		{
			name: "two-counter/no-data-when-the-family-exists-but-the-selector-misses",
			sli:  availability(nil),
			input: []promSeries{
				{`app_requests_total{route="/pay"}`, "0+10x20"},
				{`app_errors_total{route="/refund"}`, "0+1x20"},
			},
			wantNoData: true,
			why: "the family exists, so absent() does not fire, and a selector matching no " +
				"errors series must read unknown rather than healthy",
		},
		{
			name: "two-counter/real-error-ratio",
			sli:  availability(nil),
			input: []promSeries{
				{`app_requests_total{route="/pay"}`, "0+100x20"},
				{`app_errors_total{route="/pay"}`, "0+5x20"},
			},
			want: 0.05,
			why:  "5 errors per 100 requests",
		},
		{
			name: "latency/absent-bucket-is-unknown-not-a-total-failure",
			sli: governancev1alpha1.SLI{
				Type:             "latency",
				Metric:           "app",
				ThresholdSeconds: "0.5",
			},
			input: []promSeries{
				{`app_request_duration_seconds_count`, "0+10x20"},
				{`app_request_duration_seconds_bucket{le="1"}`, "0+10x20"},
			},
			wantNoData: true,
			why: "no bucket is published at the threshold. Defaulting that to zero would " +
				"read as 'no request was fast' and fabricate a 100% error ratio — an " +
				"instant page from a threshold that merely names the wrong boundary",
		},
		{
			name: "latency/real-ratio-over-the-named-bucket",
			sli: governancev1alpha1.SLI{
				Type:             "latency",
				Metric:           "app",
				ThresholdSeconds: "0.5",
			},
			input: []promSeries{
				{`app_request_duration_seconds_count`, "0+100x20"},
				{`app_request_duration_seconds_bucket{le="0.5"}`, "0+97x20"},
			},
			want: 0.03,
			why:  "97 of every 100 requests landed under the threshold",
		},
	}
}

func TestBuildErrorRatioQuery_PromQLSemantics(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		// Never a silent skip in CI. Locally, `brew install prometheus` (or any
		// promtool on PATH) turns this on; CI sets REQUIRE_PROMTOOL so a runner
		// that lost the binary fails loudly instead of reporting a green suite
		// that executed none of these.
		if os.Getenv("REQUIRE_PROMTOOL") != "" {
			t.Fatal("REQUIRE_PROMTOOL is set but promtool is not on PATH; these assertions did not run")
		}
		t.Skip("promtool not on PATH; install prometheus to execute the generated queries")
	}

	for _, c := range promCases() {
		t.Run(c.name, func(t *testing.T) {
			// One window is enough: the window is interpolated into rate() and
			// the guards are what is under test, not the range selector.
			expr, err := buildErrorRatioQuery(c.sli, "5m")
			if err != nil {
				t.Fatalf("build query: %v", err)
			}

			var b strings.Builder
			b.WriteString("evaluation_interval: 1m\ntests:\n  - interval: 1m\n    input_series:\n")
			for _, s := range c.input {
				fmt.Fprintf(&b, "      - series: '%s'\n        values: '%s'\n", s.series, s.values)
			}
			// round() only for the comparison. promtool matches sample values
			// exactly, and a ratio built from two rate()s lands a few ulp off
			// its decimal form — 5 errors per 100 requests evaluates to
			// 4.9999999999999996e-02. Rounding is applied outside the generated
			// query, so what executes is still exactly what the operator sends,
			// and it does not soften the guard cases: round(0) is 0 and
			// round(<empty>) is empty.
			b.WriteString("    promql_expr_test:\n")
			fmt.Fprintf(&b, "      - expr: %q\n        eval_time: 10m\n        exp_samples:\n",
				fmt.Sprintf("round(%s, 0.0001)", expr))
			if c.wantNoData {
				b.WriteString("          []\n")
			} else {
				fmt.Fprintf(&b, "          - labels: '{}'\n            value: %v\n", c.want)
			}

			path := filepath.Join(t.TempDir(), "case.yaml")
			if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			out, err := exec.Command(promtool, "test", "rules", path).CombinedOutput()
			if err != nil {
				t.Errorf("query does not mean what it should.\nwhy this case matters: %s\nquery: %s\npromtool: %s",
					c.why, expr, out)
			}
		})
	}
}
