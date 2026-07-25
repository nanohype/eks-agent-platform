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

// TestScaffoldTenant_SecondaryRouteFamily pins the fix for the secondary-route
// family bug: a persona pairing an anthropic primary with an amazon-nova
// secondary (sales-ops, marketing) must render the secondary route's modelFamily
// as amazon-nova — not inherit the primary's anthropic — and the scaffolded
// Platform must grant invoke on every family its routes reference.
func TestScaffoldTenant_SecondaryRouteFamily(t *testing.T) {
	// The CRD enum vocabulary for ModelRouteSpec.modelFamily; every rendered
	// route family must be a member or the scaffold produces an invalid CR.
	validFamilies := map[string]bool{
		"anthropic": true, "meta": true, "mistral": true, "cohere": true,
		"amazon-titan": true, "amazon-nova": true, "stability": true,
	}
	cases := []struct {
		persona          string
		wantSecondaryFam string
		wantAllowed      []string
	}{
		{"sales-ops", "amazon-nova", []string{"anthropic", "amazon-nova"}},
		{"marketing", "amazon-nova", []string{"anthropic", "amazon-nova"}},
		{"support", "anthropic", []string{"anthropic"}},
		{"founder", "anthropic", []string{"anthropic"}},
	}
	for _, c := range cases {
		t.Run(c.persona, func(t *testing.T) {
			res, err := ScaffoldTenant(ScaffoldOptions{TenantName: "t-" + c.persona, Persona: c.persona})
			if err != nil {
				t.Fatalf("ScaffoldTenant: %v", err)
			}
			routes := res.ModelGateway.Spec.Routes
			if len(routes) != 2 {
				t.Fatalf("%s: expected primary+secondary routes, got %d", c.persona, len(routes))
			}
			if got := routes[1].ModelFamily; got != c.wantSecondaryFam {
				t.Errorf("secondary route modelFamily = %q, want %q", got, c.wantSecondaryFam)
			}
			for _, r := range routes {
				if !validFamilies[r.ModelFamily] {
					t.Errorf("route %q renders modelFamily %q outside the CRD enum", r.Name, r.ModelFamily)
				}
			}
			got := res.Platform.Spec.Identity.AllowedModelFamilies
			if len(got) != len(c.wantAllowed) {
				t.Fatalf("allowedModelFamilies = %v, want %v", got, c.wantAllowed)
			}
			for i, f := range c.wantAllowed {
				if got[i] != f {
					t.Errorf("allowedModelFamilies[%d] = %q, want %q (full: %v)", i, got[i], f, got)
				}
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

// The two scaffolders share one parser, so the vocabulary has to reach the typed
// Platform here the same way it reaches the encoder's output in
// platform_new_test.go. This is the surface `agentctl tenant init` uses.
func TestScaffoldTenant_CarriesVocabulary(t *testing.T) {
	vocab, err := ParseVocabulary("demo", VocabularyFlags{
		Datastores: []string{
			"name=tickets,kind=keyValue,partitionKey=ticketId:S",
			"name=work,kind=queue",
		},
		Capabilities:      []string{"eventBridgeScheduler"},
		DirectSecretReads: []string{"zendesk/api-token"},
		Operators:         []string{"operator@example.com"},
	})
	if err != nil {
		t.Fatalf("ParseVocabulary: %v", err)
	}
	res, err := ScaffoldTenant(ScaffoldOptions{TenantName: "demo", Persona: "support", Vocabulary: vocab})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	spec := res.Platform.Spec
	if len(spec.Datastores) != 2 {
		t.Fatalf("want 2 datastores, got %d", len(spec.Datastores))
	}
	if spec.Datastores[0].KeyValue == nil || spec.Datastores[0].KeyValue.PartitionKey.Name != "ticketId" {
		t.Errorf("keyValue partition key did not reach the spec: %+v", spec.Datastores[0].KeyValue)
	}
	if len(spec.Identity.Capabilities) != 1 || spec.Identity.Capabilities[0] != "eventBridgeScheduler" {
		t.Errorf("capabilities: got %v", spec.Identity.Capabilities)
	}
	if len(spec.Identity.DirectSecretReads) != 1 || spec.Identity.DirectSecretReads[0] != "zendesk/api-token" {
		t.Errorf("directSecretReads: got %v", spec.Identity.DirectSecretReads)
	}
	if spec.Attribution == nil || len(spec.Attribution.Operators) != 1 {
		t.Fatalf("attribution did not reach the spec: %+v", spec.Attribution)
	}
	// The model-family choice must survive alongside the new identity fields —
	// they share one IdentitySpec literal.
	if len(spec.Identity.AllowedModelFamilies) == 0 {
		t.Error("allowedModelFamilies was dropped when the new identity fields landed")
	}

	// Declaring nothing must leave the spec free of empty blocks: attribution in
	// particular fails admission if present with no operators.
	bare, err := ScaffoldTenant(ScaffoldOptions{TenantName: "demo", Persona: "support"})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if bare.Platform.Spec.Attribution != nil {
		t.Error("attribution must be nil when no operator is named")
	}
	if bare.Platform.Spec.Datastores != nil {
		t.Error("datastores must be nil when none are declared")
	}
	out, err := bare.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, key := range []string{"attribution:", "datastores:", "capabilities:", "directSecretReads:"} {
		if strings.Contains(string(out), key) {
			t.Errorf("%q should be omitted from a no-vocabulary scaffold", key)
		}
	}
}
