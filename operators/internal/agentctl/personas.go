/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package agentctl implements the agentctl CLI subcommands. Split from
// cmd/agentctl/main.go so the persona-defaults logic is testable.
package agentctl

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// model_defaults.json is the source of truth for which model ids the persona
// routes default to. The agentctl CLI embeds it here and reads it from both
// scaffolders (`tenant init` and `platform new`). The model ids in
// personaCatalog are stamped from it at init, so there is exactly one place to
// bump a default model.
//
//go:embed model_defaults.json
var modelDefaultsJSON []byte

type modelDefaults struct {
	// Tiers names the org LLM-policy models (default / light / escalation).
	Tiers map[string]string `json:"tiers"`
	// Personas maps a persona key to the model ids its routes default to.
	Personas map[string]personaModelSpec `json:"personas"`
}

type personaModelSpec struct {
	Family           string `json:"family"`
	PrimaryModelID   string `json:"primaryModelId"`
	SecondaryModelID string `json:"secondaryModelId"`
	// SecondaryFamily is the Bedrock family of the secondary route. It can
	// differ from Family — several personas pair an anthropic primary with an
	// amazon-nova secondary — so the scaffolder renders the secondary route's
	// modelFamily from this rather than reusing the primary's.
	SecondaryFamily string `json:"secondaryFamily"`
}

var parsedModelDefaults modelDefaults

func init() {
	if err := json.Unmarshal(modelDefaultsJSON, &parsedModelDefaults); err != nil {
		panic(fmt.Sprintf("agentctl: parse model_defaults.json: %v", err))
	}
	for name, p := range personaCatalog {
		spec, ok := parsedModelDefaults.Personas[name]
		if !ok {
			panic(fmt.Sprintf("agentctl: persona %q missing from model_defaults.json", name))
		}
		if spec.Family == "" || spec.PrimaryModelID == "" {
			panic(fmt.Sprintf("agentctl: persona %q has an empty model default", name))
		}
		if spec.SecondaryModelID != "" && spec.SecondaryFamily == "" {
			panic(fmt.Sprintf("agentctl: persona %q sets secondaryModelId without secondaryFamily", name))
		}
		p.PrimaryModelFamily = spec.Family
		p.PrimaryModelID = spec.PrimaryModelID
		p.SecondaryModelID = spec.SecondaryModelID
		p.SecondaryModelFamily = spec.SecondaryFamily
		personaCatalog[name] = p
	}
}

// ModelTiers returns the org LLM-policy model tiers (default / light /
// escalation) from the embedded SSOT.
func ModelTiers() map[string]string {
	out := make(map[string]string, len(parsedModelDefaults.Tiers))
	for k, v := range parsedModelDefaults.Tiers {
		out[k] = v
	}
	return out
}

// PersonaDefaults describes the persona-flexed defaults the
// 'agentctl tenant init' command emits when scaffolding a Platform.
//
// Each persona has different vocabulary and integration expectations.
// Sales-ops thinks in "prospects" + "outreach"; support in "tickets" +
// "escalations"; finance in "month-end close" + "audit". The CLI
// surface uses the persona's own language; the underlying CRDs are
// uniform.
type PersonaDefaults struct {
	// Name is the persona key (matches PlatformSpec.Persona enum).
	Name string

	// DisplayLabel is the human-facing name used in CLI output.
	DisplayLabel string

	// PrimaryRouteName is the default 'primary' route name; persona-
	// specific (e.g. 'triage' for support, 'research' for sales-ops).
	PrimaryRouteName string

	// PrimaryModelFamily / PrimaryModelID are stamped at init from the
	// model-default SSOT (model_defaults.json) — do not set them inline.
	PrimaryModelFamily string
	PrimaryModelID     string

	// SecondaryRouteName + model — empty when the persona only needs
	// one route by default. SecondaryModelID and SecondaryModelFamily are
	// stamped from the SSOT; the family can differ from the primary's
	// (e.g. an amazon-nova secondary under an anthropic primary).
	SecondaryRouteName   string
	SecondaryModelID     string
	SecondaryModelFamily string
	SecondaryRateLimit   int32

	// DefaultAgents — one or two starter agents with system prompts
	// pre-flexed for the persona. Users edit before applying.
	DefaultAgents []agentsv1alpha1.AgentSpec

	// MonthlyBudgetUsd default. Conservative; users tune up.
	MonthlyBudgetUsd string

	// ComplianceDefaults — finance, legal, support default soc2=true;
	// founder and eng default soc2=false unless explicitly set.
	Compliance platformv1alpha1.ComplianceSpec

	// PrimaryRateLimit (rpm) on the primary route.
	PrimaryRateLimit int32

	// FleetMin/Max — KEDA scaling bounds.
	FleetMin int32
	FleetMax int32
}

// personaCatalog is the source of truth for persona-flexed defaults.
// Keys must match the PlatformSpec.Persona enum. New personas added
// here automatically surface in `agentctl persona list`. Model ids are
// stamped from model_defaults.json at init (see above).
var personaCatalog = map[string]PersonaDefaults{
	"sales-ops": {
		Name: "sales-ops", DisplayLabel: "Sales Operations",
		PrimaryRouteName:   "research",
		PrimaryRateLimit:   60,
		SecondaryRouteName: "enrichment",
		SecondaryRateLimit: 120,
		MonthlyBudgetUsd:   "2500",
		FleetMin:           1, FleetMax: 6,
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "prospector",
				ModelRoute:   "research",
				SystemPrompt: "You research B2B prospects. Return personas, triggers, and outreach channels. Cite sources.",
			},
		},
	},
	"support": {
		Name: "support", DisplayLabel: "Customer Support",
		PrimaryRouteName:   "triage",
		PrimaryRateLimit:   120,
		SecondaryRouteName: "escalation",
		SecondaryRateLimit: 30,
		MonthlyBudgetUsd:   "1500",
		FleetMin:           1, FleetMax: 5,
		Compliance: platformv1alpha1.ComplianceSpec{SOC2: true},
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "triager",
				ModelRoute:   "triage",
				SystemPrompt: "You triage support tickets. Classify urgency (P0..P3), category, and whether human escalation is needed. JSON output.",
			},
		},
	},
	"finance": {
		Name: "finance", DisplayLabel: "Finance",
		PrimaryRouteName: "analysis",
		PrimaryRateLimit: 20,
		MonthlyBudgetUsd: "1000",
		FleetMin:         1, FleetMax: 2,
		Compliance: platformv1alpha1.ComplianceSpec{SOC2: true},
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "month-end-helper",
				ModelRoute:   "analysis",
				SystemPrompt: "You support month-end close. Reconcile transaction batches, flag anomalies > 3 stddev from monthly mean, and produce audit-ready commentary.",
			},
		},
	},
	"ops": {
		Name: "ops", DisplayLabel: "Operations / Platform",
		PrimaryRouteName: "incident",
		PrimaryRateLimit: 30,
		MonthlyBudgetUsd: "1500",
		FleetMin:         1, FleetMax: 3,
		Compliance: platformv1alpha1.ComplianceSpec{SOC2: true},
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "incident-summarizer",
				ModelRoute:   "incident",
				SystemPrompt: "Given alarms + slack + runbook references, produce a single-paragraph status update. Lead with impact, then what's being done, then ETA.",
			},
		},
	},
	"founder": {
		Name: "founder", DisplayLabel: "Founder / Exec",
		PrimaryRouteName:   "deep",
		PrimaryRateLimit:   30,
		SecondaryRouteName: "fast",
		SecondaryRateLimit: 60,
		MonthlyBudgetUsd:   "500",
		FleetMin:           0, FleetMax: 1,
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "sparring-partner",
				ModelRoute:   "deep",
				SystemPrompt: "You spar on strategy + product framing. Challenge the premise before validating. Don't sycophant.",
			},
		},
	},
	"eng": {
		Name: "eng", DisplayLabel: "Engineering",
		PrimaryRouteName: "primary",
		PrimaryRateLimit: 60,
		MonthlyBudgetUsd: "2000",
		FleetMin:         1, FleetMax: 4,
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "code-reviewer",
				ModelRoute:   "primary",
				SystemPrompt: "Review the diff for correctness, security, and style. Flag only things that materially matter. No nitpicks.",
			},
		},
	},
	"marketing": {
		Name: "marketing", DisplayLabel: "Marketing",
		PrimaryRouteName:   "copy",
		PrimaryRateLimit:   60,
		SecondaryRouteName: "image",
		SecondaryRateLimit: 10,
		MonthlyBudgetUsd:   "1500",
		FleetMin:           1, FleetMax: 4,
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "copy-writer",
				ModelRoute:   "copy",
				SystemPrompt: "Write marketing copy that respects the brand voice. Show 3 variants with the trade-off for each.",
			},
		},
	},
	"legal": {
		Name: "legal", DisplayLabel: "Legal",
		PrimaryRouteName: "review",
		PrimaryRateLimit: 15,
		MonthlyBudgetUsd: "800",
		FleetMin:         0, FleetMax: 2,
		Compliance: platformv1alpha1.ComplianceSpec{SOC2: true, HIPAA: false},
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{
				Name:         "contract-reviewer",
				ModelRoute:   "review",
				SystemPrompt: "Review the contract. Flag non-standard clauses, missing liability caps, and any obligations whose duration isn't bounded. Cite section numbers.",
			},
		},
	},
	"generic": {
		Name: "generic", DisplayLabel: "Generic",
		PrimaryRouteName: "primary",
		PrimaryRateLimit: 30,
		MonthlyBudgetUsd: "1000",
		FleetMin:         1, FleetMax: 3,
		DefaultAgents: []agentsv1alpha1.AgentSpec{
			{Name: "assistant", ModelRoute: "primary", SystemPrompt: "You are a helpful assistant."},
		},
	},
}

// PersonaByName returns the catalog entry for a persona key. Unknown
// persona names return an error listing the supported set.
func PersonaByName(name string) (PersonaDefaults, error) {
	if p, ok := personaCatalog[name]; ok {
		return p, nil
	}
	keys := make([]string, 0, len(personaCatalog))
	for k := range personaCatalog {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return PersonaDefaults{}, fmt.Errorf("unknown persona %q; supported: %s", name, strings.Join(keys, ", "))
}

// ListPersonas returns every persona key in display order.
func ListPersonas() []PersonaDefaults {
	keys := make([]string, 0, len(personaCatalog))
	for k := range personaCatalog {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]PersonaDefaults, 0, len(keys))
	for _, k := range keys {
		out = append(out, personaCatalog[k])
	}
	return out
}
