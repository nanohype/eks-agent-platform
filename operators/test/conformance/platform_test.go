/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
)

const testNs = "conformance"

// ensureNs creates the test namespace once per package; idempotent across tests.
func ensureNs(ctx context.Context, t *testing.T) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNs}}
	_ = k8sClient.Create(ctx, ns) // ignore AlreadyExists
}

func TestPlatform_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &agentsv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: agentsv1alpha1.PlatformSpec{
			Persona: "generic",
			Tenant:  "conformance",
			Budget:  agentsv1alpha1.BudgetRef{Name: "conformance-budget"},
			Identity: agentsv1alpha1.IdentitySpec{
				AllowedModelFamilies: []string{"anthropic"},
			},
		},
	}

	mustCreate(ctx, t, p)

	var got agentsv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Spec.Persona != "generic" {
		t.Errorf("spec.persona round-trip: got %q want %q", got.Spec.Persona, "generic")
	}
	if got.Spec.Tenant != "conformance" {
		t.Errorf("spec.tenant round-trip: got %q want %q", got.Spec.Tenant, "conformance")
	}
	if got.Spec.Isolation != "namespace" {
		t.Errorf("spec.isolation default: got %q want %q (defaulted by CRD)", got.Spec.Isolation, "namespace")
	}
}

func TestPlatform_StatusSubresource(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &agentsv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: agentsv1alpha1.PlatformSpec{
			Persona:  "eng",
			Tenant:   "conformance",
			Budget:   agentsv1alpha1.BudgetRef{Name: "x"},
			Identity: agentsv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}

	mustCreate(ctx, t, p)

	p.Status.Phase = "Provisioning"
	p.Status.Namespace = "tenants-conformance-status"
	p.Status.ObservedGeneration = p.Generation
	if err := k8sClient.Status().Update(ctx, p); err != nil {
		t.Fatalf("status update: %v", err)
	}

	var got agentsv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get after status update: %v", err)
	}
	if got.Status.Phase != "Provisioning" {
		t.Errorf("status.phase: got %q want %q", got.Status.Phase, "Provisioning")
	}
	if got.Status.Namespace != "tenants-conformance-status" {
		t.Errorf("status.namespace: got %q", got.Status.Namespace)
	}
}

func TestPlatform_InvalidPersona(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &agentsv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: agentsv1alpha1.PlatformSpec{
			Persona:  "not-a-real-persona",
			Tenant:   "x",
			Budget:   agentsv1alpha1.BudgetRef{Name: "x"},
			Identity: agentsv1alpha1.IdentitySpec{},
		},
	}

	err := k8sClient.Create(ctx, p)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, p) })
		t.Fatalf("expected validation error for invalid persona, got nil")
	}
	// Any non-nil error counts — the CRD validation rejected the value.
}
