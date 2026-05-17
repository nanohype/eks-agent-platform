/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"bytes"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/stxkxs/eks-agent-platform/operators/api/v1alpha1"
)

// ScaffoldOptions captures the inputs to ScaffoldTenant. All required;
// the CLI's tenant init subcommand validates before calling.
type ScaffoldOptions struct {
	TenantName   string
	DisplayName  string
	Persona      string
	Namespace    string // namespace where the Platform/Budget/Gateway/Fleet/Eval CRs land
	Schedule     string // EvalSuite cron schedule; empty disables eval scheduling
	SlackChannel string
}

// ScaffoldedResources is the set of CRs emitted by ScaffoldTenant.
// Returned as separate fields rather than a slice of unstructured so
// callers (tests, CLI) can mutate before serializing.
type ScaffoldedResources struct {
	Tenant       *agentsv1alpha1.Tenant
	Platform     *agentsv1alpha1.Platform
	Budget       *agentsv1alpha1.BudgetPolicy
	ModelGateway *agentsv1alpha1.ModelGateway
	AgentFleet   *agentsv1alpha1.AgentFleet
	EvalSuite    *agentsv1alpha1.EvalSuite
}

// ScaffoldTenant produces a complete CR set for a new tenant onboarding,
// using persona-flexed defaults. Caller serializes to YAML and either
// pipes to kubectl or saves for review.
func ScaffoldTenant(opts ScaffoldOptions) (*ScaffoldedResources, error) {
	if opts.TenantName == "" {
		return nil, fmt.Errorf("tenant name required")
	}
	if opts.Namespace == "" {
		opts.Namespace = "eks-agent-platform"
	}
	if opts.DisplayName == "" {
		opts.DisplayName = opts.TenantName
	}
	pdefs, err := PersonaByName(opts.Persona)
	if err != nil {
		return nil, err
	}

	platformName := opts.TenantName
	budgetName := opts.TenantName + "-budget"
	gatewayName := opts.TenantName + "-gateway"
	fleetName := opts.TenantName + "-fleet"
	evalName := opts.TenantName + "-eval"

	out := &ScaffoldedResources{
		Tenant: &agentsv1alpha1.Tenant{
			TypeMeta:   metav1.TypeMeta{Kind: "Tenant", APIVersion: agentsv1alpha1.GroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: opts.TenantName},
			Spec: agentsv1alpha1.TenantSpec{
				DisplayName:               opts.DisplayName,
				PrimaryPersona:            pdefs.Name,
				Contact:                   agentsv1alpha1.ContactSpec{SlackChannel: opts.SlackChannel},
				Compliance:                pdefs.Compliance,
				AggregateMonthlyBudgetUsd: pdefs.MonthlyBudgetUsd,
			},
		},
		Platform: &agentsv1alpha1.Platform{
			TypeMeta: metav1.TypeMeta{Kind: "Platform", APIVersion: agentsv1alpha1.GroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{
				Name: platformName, Namespace: opts.Namespace,
				Labels: map[string]string{
					"eks-agent-platform/persona": pdefs.Name,
					"eks-agent-platform/tenant":  opts.TenantName,
				},
			},
			Spec: agentsv1alpha1.PlatformSpec{
				DisplayName: opts.DisplayName,
				Persona:     pdefs.Name,
				Tenant:      opts.TenantName,
				Isolation:   "namespace",
				Budget:      agentsv1alpha1.BudgetRef{Name: budgetName},
				Identity:    agentsv1alpha1.IdentitySpec{AllowedModelFamilies: []string{pdefs.PrimaryModelFamily}},
				Compliance:  pdefs.Compliance,
			},
		},
		Budget: &agentsv1alpha1.BudgetPolicy{
			TypeMeta:   metav1.TypeMeta{Kind: "BudgetPolicy", APIVersion: agentsv1alpha1.GroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: budgetName, Namespace: opts.Namespace},
			Spec: agentsv1alpha1.BudgetPolicySpec{
				PlatformRef:            agentsv1alpha1.LocalRef{Name: platformName},
				MonthlyUsd:             pdefs.MonthlyBudgetUsd,
				AlertThresholdsPercent: []int32{50, 80, 100},
				KillSwitchEnabled:      true,
			},
		},
		ModelGateway: &agentsv1alpha1.ModelGateway{
			TypeMeta:   metav1.TypeMeta{Kind: "ModelGateway", APIVersion: agentsv1alpha1.GroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: opts.Namespace},
			Spec: agentsv1alpha1.ModelGatewaySpec{
				PlatformRef: agentsv1alpha1.LocalRef{Name: platformName},
				Routes:      routesFor(pdefs),
			},
		},
		AgentFleet: &agentsv1alpha1.AgentFleet{
			TypeMeta:   metav1.TypeMeta{Kind: "AgentFleet", APIVersion: agentsv1alpha1.GroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: fleetName, Namespace: opts.Namespace},
			Spec: agentsv1alpha1.AgentFleetSpec{
				PlatformRef: agentsv1alpha1.LocalRef{Name: platformName},
				Scaling: agentsv1alpha1.ScalingSpec{
					Enabled:           true,
					Min:               int32Ptr(pdefs.FleetMin),
					Max:               int32Ptr(pdefs.FleetMax),
					QueueDepthTrigger: 10,
				},
				Agents: pdefs.DefaultAgents,
			},
		},
		EvalSuite: &agentsv1alpha1.EvalSuite{
			TypeMeta:   metav1.TypeMeta{Kind: "EvalSuite", APIVersion: agentsv1alpha1.GroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: evalName, Namespace: opts.Namespace},
			Spec: agentsv1alpha1.EvalSuiteSpec{
				PlatformRef:   agentsv1alpha1.LocalRef{Name: platformName},
				AgentFleetRef: agentsv1alpha1.LocalRef{Name: fleetName},
				Schedule:      opts.Schedule,
				PassThreshold: "0.85",
				Cases: []agentsv1alpha1.EvalCase{{
					Name:           "smoke",
					Input:          "ping",
					ExpectContains: []string{"pong"},
					MaxLatencyMs:   5000,
				}},
			},
		},
	}
	return out, nil
}

// routesFor builds the persona's ModelGateway routes — primary always,
// secondary when defined.
func routesFor(p PersonaDefaults) []agentsv1alpha1.ModelRouteSpec {
	routes := []agentsv1alpha1.ModelRouteSpec{{
		Name: p.PrimaryRouteName, ModelFamily: p.PrimaryModelFamily,
		ModelID: p.PrimaryModelID, RateLimit: p.PrimaryRateLimit,
	}}
	if p.SecondaryRouteName != "" {
		routes = append(routes, agentsv1alpha1.ModelRouteSpec{
			Name: p.SecondaryRouteName, ModelFamily: p.PrimaryModelFamily,
			ModelID: p.SecondaryModelID, RateLimit: p.SecondaryRateLimit,
		})
	}
	return routes
}

func int32Ptr(v int32) *int32 { return &v }

// Render returns the multi-document YAML serialization (one document per
// resource, separated by '---'). Order matches the apply order: Tenant
// first, then everything else.
func (s *ScaffoldedResources) Render() ([]byte, error) {
	var buf bytes.Buffer
	for i, obj := range []any{s.Tenant, s.Platform, s.Budget, s.ModelGateway, s.AgentFleet, s.EvalSuite} {
		b, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(b)
	}
	return buf.Bytes(), nil
}
