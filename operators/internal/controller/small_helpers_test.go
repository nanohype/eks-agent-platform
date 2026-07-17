/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

func TestParseDecimal(t *testing.T) {
	if _, ok := parseDecimal(""); ok {
		t.Error("empty string must not parse")
	}
	if _, ok := parseDecimal("not-a-number"); ok {
		t.Error("garbage must not parse")
	}
	v, ok := parseDecimal("1500.25")
	if !ok || v == nil {
		t.Fatal("a valid decimal must parse")
	}
	if f, _ := v.Float64(); f != 1500.25 {
		t.Errorf("parsed value: got %v want 1500.25", f)
	}
}

func TestConditionTenantOverBudget(t *testing.T) {
	c := conditionTenantOverBudget(tenantReading{aggregateSpend: "2600", pct: 130})
	if c.Type != "TenantBudgetExceeded" || c.Status != metav1.ConditionTrue {
		t.Errorf("over-budget condition: %+v", c)
	}
}

func TestWorkerImage(t *testing.T) {
	if got := workerImage(&agentsv1alpha1.SandboxPool{}); got != defaultSandboxWorkerImage {
		t.Errorf("default worker image: got %q", got)
	}
	pool := &agentsv1alpha1.SandboxPool{Spec: agentsv1alpha1.SandboxPoolSpec{Image: "ghcr.io/x/worker:1.2.3"}}
	if got := workerImage(pool); got != "ghcr.io/x/worker:1.2.3" {
		t.Errorf("override worker image: got %q", got)
	}
}

func TestSandboxCleanupClient_NamespaceTierUsesHost(t *testing.T) {
	s := fleetScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentSandboxReconciler{Client: cl, Scheme: s}
	if got := r.sandboxCleanupClient(context.Background(), newPlatform("acme", "team")); got != cl {
		t.Error("namespace tier sandbox cleanup must use the host client")
	}
}
