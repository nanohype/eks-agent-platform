/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

func newTenantReconciler() *controller.TenantReconciler {
	return &controller.TenantReconciler{
		Client:          k8sClient,
		Scheme:          scheme,
		Concurrency:     1,
		RequeueInterval: 5 * time.Minute,
	}
}

func reconcileTenant(ctx context.Context, t *testing.T, tn *platformv1alpha1.Tenant) {
	t.Helper()
	r := newTenantReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: tn.Name}}); err != nil {
		t.Fatalf("tenant reconcile: %v", err)
	}
}

func TestTenantReconciler_AggregatesPlatformReadiness(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	tName := uniqueName(t, "tnt")
	tenant := &platformv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tName},
		Spec: platformv1alpha1.TenantSpec{
			DisplayName:    "ACME Corp",
			PrimaryPersona: "ops",
		},
	}
	mustCreate(ctx, t, tenant)

	// Two Platforms own this tenant; one Ready, one Suspended.
	for i, phase := range []string{"Ready", "Suspended"} {
		pName := uniqueName(t, "p") + "-" + []string{"a", "b"}[i]
		p := &platformv1alpha1.Platform{
			ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: testNs},
			Spec: platformv1alpha1.PlatformSpec{
				Persona: "ops",
				Tenant:  tName,
				Budget:  platformv1alpha1.BudgetRef{Name: "x"},
				Identity: platformv1alpha1.IdentitySpec{
					AllowedModelFamilies: []string{"anthropic"},
				},
			},
		}
		mustCreate(ctx, t, p)
		p.Status.Phase = phase
		if err := k8sClient.Status().Update(ctx, p); err != nil {
			t.Fatalf("force platform %s phase=%s: %v", pName, phase, err)
		}
	}

	reconcileTenant(ctx, t, tenant)

	var got platformv1alpha1.Tenant
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: tName}, &got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if got.Status.PlatformCount != 2 {
		t.Errorf("platformCount: got %d want 2", got.Status.PlatformCount)
	}
	if got.Status.ReadyPlatformCount != 1 {
		t.Errorf("readyPlatformCount: got %d want 1", got.Status.ReadyPlatformCount)
	}
	if got.Status.SuspendedPlatformCount != 1 {
		t.Errorf("suspendedPlatformCount: got %d want 1", got.Status.SuspendedPlatformCount)
	}
	if got.Status.Phase != "Suspended" {
		t.Errorf("phase: got %q want Suspended (any suspended → Suspended)", got.Status.Phase)
	}
}

func TestTenantReconciler_PendingWhenNoPlatforms(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	tName := uniqueName(t, "tnt")
	tenant := &platformv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tName},
		Spec:       platformv1alpha1.TenantSpec{PrimaryPersona: "founder"},
	}
	mustCreate(ctx, t, tenant)

	reconcileTenant(ctx, t, tenant)

	var got platformv1alpha1.Tenant
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: tName}, &got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if got.Status.PlatformCount != 0 {
		t.Errorf("platformCount: got %d want 0", got.Status.PlatformCount)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("phase: got %q want Pending (no platforms)", got.Status.Phase)
	}
}
