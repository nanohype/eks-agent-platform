/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"strings"
	"testing"
)

func TestScaffoldTenant_PersonaDefaults(t *testing.T) {
	cases := []struct {
		persona        string
		wantBudget     string
		wantPrimaryRtN string
		wantSecondary  bool
	}{
		{"sales-ops", "2500", "research", true},
		{"support", "1500", "triage", true},
		{"finance", "1000", "analysis", false},
		{"founder", "500", "deep", true},
		{"legal", "800", "review", false},
		{"generic", "1000", "primary", false},
	}
	for _, c := range cases {
		t.Run(c.persona, func(t *testing.T) {
			res, err := ScaffoldTenant(ScaffoldOptions{
				TenantName: "test-" + c.persona, Persona: c.persona,
			})
			if err != nil {
				t.Fatalf("ScaffoldTenant: %v", err)
			}
			if res.Budget.Spec.MonthlyUsd != c.wantBudget {
				t.Errorf("budget: got %q want %q", res.Budget.Spec.MonthlyUsd, c.wantBudget)
			}
			if len(res.ModelGateway.Spec.Routes) == 0 || res.ModelGateway.Spec.Routes[0].Name != c.wantPrimaryRtN {
				t.Errorf("primary route: got %v want %q", res.ModelGateway.Spec.Routes, c.wantPrimaryRtN)
			}
			hasSecondary := len(res.ModelGateway.Spec.Routes) > 1
			if hasSecondary != c.wantSecondary {
				t.Errorf("secondary present: got %v want %v", hasSecondary, c.wantSecondary)
			}
			// every persona produces at least one default agent
			if len(res.AgentFleet.Spec.Agents) == 0 {
				t.Errorf("no default agent for persona %q", c.persona)
			}
		})
	}
}

func TestScaffoldTenant_RequiresName(t *testing.T) {
	_, err := ScaffoldTenant(ScaffoldOptions{Persona: "generic"})
	if err == nil {
		t.Fatal("expected error for missing tenant name")
	}
}

func TestScaffoldTenant_UnknownPersona(t *testing.T) {
	_, err := ScaffoldTenant(ScaffoldOptions{TenantName: "x", Persona: "doesnotexist"})
	if err == nil || !strings.Contains(err.Error(), "unknown persona") {
		t.Errorf("expected unknown-persona error, got %v", err)
	}
}

func TestScaffoldTenant_RenderEmitsMultiDoc(t *testing.T) {
	res, err := ScaffoldTenant(ScaffoldOptions{TenantName: "demo", Persona: "ops"})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	out, err := res.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)
	for _, kind := range []string{"kind: Tenant", "kind: Platform", "kind: BudgetPolicy", "kind: ModelGateway", "kind: AgentFleet", "kind: EvalSuite"} {
		if !strings.Contains(got, kind) {
			t.Errorf("missing %s in rendered output", kind)
		}
	}
	// 6 documents → 5 separators (no leading '---').
	if strings.Count(got, "---\n") != 5 {
		t.Errorf("expected 5 document separators, got %d", strings.Count(got, "---\n"))
	}
}
