/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package agentctl implements the agentctl CLI subcommands. Split from
// cmd/agentctl/main.go so the persona-defaults logic is testable.
package agentctl

import (
	"fmt"
	"sort"
	"strings"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

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

	// PrimaryModelFamily defaults — typically 'anthropic' but personas
	// with image work (marketing) get 'amazon-nova' on the side.
	PrimaryModelFamily string
	PrimaryModelID     string

	// SecondaryRouteName + model — empty when the persona only needs
	// one route by default.
	SecondaryRouteName string
	SecondaryModelID   string
	SecondaryRateLimit int32

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
// here automatically surface in `agentctl persona list`.
var personaCatalog = map[string]PersonaDefaults{
	"sales-ops": {
		Name: "sales-ops", DisplayLabel: "Sales Operations",
		PrimaryRouteName: "research", PrimaryModelFamily: "anthropic",
		PrimaryModelID:     "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		PrimaryRateLimit:   60,
		SecondaryRouteName: "enrichment",
		SecondaryModelID:   "us.amazon.nova-lite-v1:0",
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
		PrimaryRouteName: "triage", PrimaryModelFamily: "anthropic",
		PrimaryModelID:     "us.anthropic.claude-3-5-haiku-20241022-v1:0",
		PrimaryRateLimit:   120,
		SecondaryRouteName: "escalation",
		SecondaryModelID:   "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
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
		PrimaryRouteName: "analysis", PrimaryModelFamily: "anthropic",
		PrimaryModelID:   "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
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
		PrimaryRouteName: "incident", PrimaryModelFamily: "anthropic",
		PrimaryModelID:   "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
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
		PrimaryRouteName: "deep", PrimaryModelFamily: "anthropic",
		PrimaryModelID:     "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		PrimaryRateLimit:   30,
		SecondaryRouteName: "fast",
		SecondaryModelID:   "us.anthropic.claude-3-5-haiku-20241022-v1:0",
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
		PrimaryRouteName: "primary", PrimaryModelFamily: "anthropic",
		PrimaryModelID:   "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
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
		PrimaryRouteName: "copy", PrimaryModelFamily: "anthropic",
		PrimaryModelID:     "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		PrimaryRateLimit:   60,
		SecondaryRouteName: "image",
		SecondaryModelID:   "us.amazon.nova-pro-v1:0",
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
		PrimaryRouteName: "review", PrimaryModelFamily: "anthropic",
		PrimaryModelID:   "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
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
		PrimaryRouteName: "primary", PrimaryModelFamily: "anthropic",
		PrimaryModelID:   "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
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
