/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The burn-rate signal spans two languages that never share a symbol: the SLO
// reconciler publishes an EventBridge event, and terraform-managed rules match
// it and route it to a severity topic. EventBridge matching is exact, so a drift
// on either side of the seam silently stops the page while both sides keep
// looking correct. These tests parse the event patterns and assert they equal
// the Go constants.
//
// They also assert what the shared bus makes load-bearing: the burn-rate rules
// and the budget kill-switch rule must stay disjoint. A burn-rate breach holds a
// tenant's GitOps delivery; the budget breach detaches its IAM. A rename that
// let one match the other's event would suspend a tenant for a latency
// regression, so the disjointness is pinned here rather than left to review.

var (
	tfRuleSourceRE     = regexp.MustCompile(`(?m)^\s*source\s*=\s*\[\s*"([^"]+)"\s*\]`)
	tfRuleDetailTypeRE = regexp.MustCompile(`(?m)^\s*"detail-type"\s*=\s*\[\s*"([^"]+)"\s*\]`)
	tfRuleSeverityRE   = regexp.MustCompile(`(?m)^\s*severity\s*=\s*\[\s*"([^"]+)"\s*\]`)
	tfResourceHeadRE   = regexp.MustCompile(`(?m)^resource\s+"`)
)

// eventPattern is one EventBridge rule's match fields, read out of terraform.
type eventPattern struct {
	source     string
	detailType string
	severity   string
}

// readEventRule extracts a single named aws_cloudwatch_event_rule's block and
// parses its event_pattern.
//
// Scoped to the named resource rather than regexed over the whole file. The
// first-match-wins alternative silently retargets itself the moment a second
// rule lands in the same file — which is exactly what this change adds — so the
// test would keep passing while asserting the wrong rule.
func readEventRule(t *testing.T, tfFile, ruleName string) eventPattern {
	t.Helper()
	// Package dir → repo root is three levels up.
	path := filepath.Join("..", "..", "..", "terraform", "components", "kill-switch", tfFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read terraform kill-switch config at %s: %v", path, err)
	}
	tf := string(raw)

	head := fmt.Sprintf("resource \"aws_cloudwatch_event_rule\" %q {", ruleName)
	start := strings.Index(tf, head)
	if start < 0 {
		t.Fatalf("no aws_cloudwatch_event_rule %q in %s; the rule was renamed or moved", ruleName, path)
	}
	rest := tf[start+len(head):]
	// The block ends where the next top-level resource begins (or at EOF).
	if loc := tfResourceHeadRE.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}

	field := func(re *regexp.Regexp, name string) string {
		m := re.FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("no %s in the event_pattern of rule %q (%s); the contract regex or the terraform layout drifted", name, ruleName, path)
		}
		return m[1]
	}
	return eventPattern{
		source:     field(tfRuleSourceRE, "source"),
		detailType: field(tfRuleDetailTypeRE, "detail-type"),
		severity:   field(tfRuleSeverityRE, "detail.severity"),
	}
}

func TestBurnRateEventContract(t *testing.T) {
	cases := []struct {
		rule         string
		wantSeverity string
	}{
		{"burn_rate_critical", severityCritical},
		{"burn_rate_warning", severityWarning},
	}
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			got := readEventRule(t, "burn-rate.tf", c.rule)
			if got.source != sloEventSource {
				t.Errorf("source: terraform has %q, the Go constant has %q — the EventBridge match is now dead on one side; align both", got.source, sloEventSource)
			}
			if got.detailType != sloEventDetailType {
				t.Errorf("detail-type: terraform has %q, the Go constant has %q", got.detailType, sloEventDetailType)
			}
			if got.severity != c.wantSeverity {
				t.Errorf("detail.severity: terraform has %q, want %q", got.severity, c.wantSeverity)
			}
		})
	}
}

// The two rule sets share one bus. EventBridge ANDs the top-level pattern keys,
// so a single differing key is enough to keep them apart — but only while the
// values actually differ.
func TestBurnRateAndBudgetRulesStayDisjoint(t *testing.T) {
	burn := readEventRule(t, "burn-rate.tf", "burn_rate_critical")
	budget := readEventRule(t, "main.tf", "breach")

	if burn.source == budget.source && burn.detailType == budget.detailType {
		t.Fatalf("the burn-rate and budget rules now match the same events (source %q, detail-type %q) — a burn-rate breach would reach the suspension state machine and detach a tenant's IAM for a latency regression", burn.source, burn.detailType)
	}
	if sloEventSource == budgetEventSource {
		t.Errorf("the Go event sources collapsed to %q; the two control loops must stay distinguishable on the wire", sloEventSource)
	}
	if sloEventDetailType == budgetEventDetailType {
		t.Errorf("the Go detail-types collapsed to %q", sloEventDetailType)
	}
}
