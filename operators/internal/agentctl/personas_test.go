/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"strings"
	"testing"
)

// TestModelTiers pins the org LLM-policy model tiers. These are the values the
// scaffolder and downstream defaults must track (nanohype llm-policy standard).
func TestModelTiers(t *testing.T) {
	tiers := ModelTiers()
	want := map[string]string{
		"default":    "us.anthropic.claude-sonnet-4-6",
		"light":      "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		"escalation": "us.anthropic.claude-opus-4-8",
	}
	for k, v := range want {
		if tiers[k] != v {
			t.Errorf("tier %q: got %q want %q", k, tiers[k], v)
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
