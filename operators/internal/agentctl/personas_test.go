/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"strings"
	"testing"
)

// TestModelTiers asserts the SHAPE of the tier table, not its values.
//
// The exact ids belong to the org LLM policy, and scripts/check-model-tiers.py
// compares the two by reading the published standard. Restating them here made
// this test a second copy of the same values — which is the defect that let the
// table drift a full generation while the file's own comment claimed it mirrored
// the standard, and left a reader two places to look and no way to tell which
// was right. A test that pins values it also has to be updated with proves the
// updater was consistent, not that the values are correct.
//
// What is worth pinning here is what no external document can say: that all
// three tiers exist, that none is empty, and that each is a cross-region
// inference-profile id. The last is a runtime requirement rather than a style
// preference — llm-policy's inference-profile-required rule exists because
// Bedrock refuses a bare foundation-model id for the current Claude family with
// a ValidationException on the first call.
func TestModelTiers(t *testing.T) {
	tiers := ModelTiers()
	for _, tier := range []string{"default", "light", "escalation"} {
		id, ok := tiers[tier]
		if !ok {
			t.Errorf("tier %q is absent from the tier table", tier)
			continue
		}
		if strings.TrimSpace(id) == "" {
			t.Errorf("tier %q is empty", tier)
			continue
		}
		if !strings.HasPrefix(id, "us.") {
			t.Errorf("tier %q = %q, which is not a us. cross-region inference-profile id; "+
				"Bedrock refuses a bare foundation-model id for the current Claude family", tier, id)
		}
	}
}

// TestPersonaModelsStampedFromSSOT asserts every persona picked up a non-empty
// current-generation model from the embedded SSOT — no persona is left on a
// zero value or a retired claude-3-5 default.
func TestPersonaModelsStampedFromSSOT(t *testing.T) {
	for _, p := range ListPersonas() {
		if p.PrimaryModelFamily == "" || p.PrimaryModelID == "" {
			t.Errorf("persona %q has empty primary model (family=%q id=%q)", p.Name, p.PrimaryModelFamily, p.PrimaryModelID)
		}
		if strings.Contains(p.PrimaryModelID, "claude-3-5") {
			t.Errorf("persona %q primary model still on a claude-3-5 default: %q", p.Name, p.PrimaryModelID)
		}
		if strings.Contains(p.SecondaryModelID, "claude-3-5") {
			t.Errorf("persona %q secondary model still on a claude-3-5 default: %q", p.Name, p.SecondaryModelID)
		}
		// Every persona with a secondary route has a secondary model.
		if p.SecondaryRouteName != "" && p.SecondaryModelID == "" {
			t.Errorf("persona %q declares secondary route %q but no secondary model", p.Name, p.SecondaryRouteName)
		}
	}
}

// TestDefaultPersonaModel confirms the anthropic default resolves to the
// current sonnet, matching the llm-policy default tier.
func TestDefaultPersonaModel(t *testing.T) {
	p, err := PersonaByName("generic")
	if err != nil {
		t.Fatalf("PersonaByName(generic): %v", err)
	}
	if p.PrimaryModelID != ModelTiers()["default"] {
		t.Errorf("generic persona primary %q != default tier %q", p.PrimaryModelID, ModelTiers()["default"])
	}
}
