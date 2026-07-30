/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"encoding/json"
	"testing"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

func TestBuildInlineCasesParam_EmitsValidJSON(t *testing.T) {
	cases := []governancev1alpha1.EvalCase{
		{Name: "smoke", Input: "ping", ExpectContains: []string{"pong"}, MaxLatencyMs: 5000, MaxCostUsd: "0.01"},
		// Pathological inputs the old %q-based renderer would emit
		// invalid JSON for: a quote, a backslash, a control byte.
		{Name: `tricky"\name`, Input: "line1\nline2\twith\x07bell", ExpectContains: []string{"a\\b"}, MaxLatencyMs: 1000},
	}
	got, err := buildInlineCasesParam(cases)
	if err != nil {
		t.Fatalf("buildInlineCasesParam: %v", err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, got)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 cases, got %d", len(parsed))
	}
	if parsed[1]["input"] != "line1\nline2\twith\x07bell" {
		t.Errorf("round-trip lost control bytes: got %q", parsed[1]["input"])
	}
}

func TestBuildInlineCasesParam_Empty(t *testing.T) {
	got, err := buildInlineCasesParam(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "[]" {
		t.Errorf("empty: got %q want []", got)
	}
}

// TestModelGatewayEndpoint_IsPerPlatform guards the address eval runs are
// pointed at.
//
// Each Platform runs its own gateway in its own namespace, so a single shared
// URL — which is what the chart used to carry — is correct for at most one
// Platform and silently wrong for every other. Nothing downstream would notice:
// the workflow starts, the parameter is populated, and the run just fails to
// reach a model.
func TestModelGatewayEndpoint_IsPerPlatform(t *testing.T) {
	a := ModelGatewayEndpoint(newPlatform("acme", "team"))
	b := ModelGatewayEndpoint(newPlatform("globex", "team"))

	if a == b {
		t.Fatalf("two Platforms resolved to the same gateway endpoint (%q) — the address must be per-Platform", a)
	}
	const want = "http://acme-gateway.tenants-acme.svc.cluster.local:8080"
	if a != want {
		t.Errorf("endpoint: got %q want %q", a, want)
	}
}
