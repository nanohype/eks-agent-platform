/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

func newSandboxPoolReconciler() *controller.SandboxPoolReconciler {
	return &controller.SandboxPoolReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
		ShimImage:   "ghcr.io/nanohype/eks-agent-platform/operator:test",
	}
}

func reconcileSandboxPool(ctx context.Context, t *testing.T, pool *agentsv1alpha1.SandboxPool) {
	t.Helper()
	r := newSandboxPoolReconciler()
	// First call adds the finalizer + 100ms requeue; subsequent calls
	// drive the real reconcile.
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}})
		if err != nil {
			t.Fatalf("sandboxpool reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func sandboxEnvKeyRef() corev1.SecretKeySelector {
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "sandbox-env"},
		Key:                  "environment-key",
	}
}

// readySandboxPlatform creates a Platform, forces it Ready, and stubs the
// tenant namespace the PlatformReconciler would normally create — the
// shared fixture for SandboxPool tests that need a Ready Platform.
func readySandboxPlatform(ctx context.Context, t *testing.T) *agentsv1alpha1.Platform {
	t.Helper()
	p := &agentsv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "platfo"), Namespace: testNs},
		Spec: agentsv1alpha1.PlatformSpec{
			Persona: "ops", Tenant: "acme",
			Budget:   agentsv1alpha1.BudgetRef{Name: "x"},
			Identity: agentsv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	p.Status.Phase = phaseReady
	p.Status.Namespace = controller.PlatformNamespace(p)
	if err := k8sClient.Status().Update(ctx, p); err != nil {
		t.Fatalf("force platform Ready: %v", err)
	}
	tenantNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Status.Namespace}}
	if err := k8sClient.Create(ctx, tenantNs); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create tenant ns: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, tenantNs) })
	return p
}

func TestSandboxPoolReconciler_PendingWhenPlatformMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	pool := &agentsv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "pool"), Namespace: testNs},
		Spec: agentsv1alpha1.SandboxPoolSpec{
			PlatformRef:          agentsv1alpha1.LocalRef{Name: "no-such-platform"},
			EnvironmentID:        "env_test",
			EnvironmentKeySecret: sandboxEnvKeyRef(),
		},
	}
	mustCreate(ctx, t, pool)
	reconcileSandboxPool(ctx, t, pool)

	var got agentsv1alpha1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (PlatformRef dangles)", got.Status.Phase)
	}
}

func TestSandboxPoolReconciler_ReadyWhenPlatformReady(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	p := readySandboxPlatform(ctx, t)

	pool := &agentsv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "pool"), Namespace: testNs},
		Spec: agentsv1alpha1.SandboxPoolSpec{
			PlatformRef:          agentsv1alpha1.LocalRef{Name: p.Name},
			EnvironmentID:        "env_test",
			EnvironmentKeySecret: sandboxEnvKeyRef(),
		},
	}
	mustCreate(ctx, t, pool)
	reconcileSandboxPool(ctx, t, pool)

	var got agentsv1alpha1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phaseReady {
		t.Errorf("status.phase: got %q want phaseReady", got.Status.Phase)
	}
	// The worker Deployment must have landed in the tenant namespace.
	var dep appsv1.Deployment
	depKey := types.NamespacedName{Namespace: p.Status.Namespace, Name: "sandbox-" + pool.Name}
	if err := k8sClient.Get(ctx, depKey, &dep); err != nil {
		t.Fatalf("worker Deployment not created: %v", err)
	}
}

func TestSandboxPoolReconciler_Autoscaling(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	p := readySandboxPlatform(ctx, t)

	pool := &agentsv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "pool"), Namespace: testNs},
		Spec: agentsv1alpha1.SandboxPoolSpec{
			PlatformRef:          agentsv1alpha1.LocalRef{Name: p.Name},
			EnvironmentID:        "env_test",
			EnvironmentKeySecret: sandboxEnvKeyRef(),
			APIKeySecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "sandbox-api"},
				Key:                  "api-key",
			},
		},
	}
	mustCreate(ctx, t, pool)
	reconcileSandboxPool(ctx, t, pool)

	var got agentsv1alpha1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// KEDA CRDs are absent in envtest; the ScaledObject step is tolerated
	// non-fatally, so the pool still settles Ready.
	if got.Status.Phase != phaseReady {
		t.Errorf("status.phase: got %q want phaseReady", got.Status.Phase)
	}
	// The metrics bridge Deployment must have landed in the tenant namespace.
	var bridge appsv1.Deployment
	bridgeKey := types.NamespacedName{Namespace: p.Status.Namespace, Name: "sandbox-" + pool.Name + "-metrics"}
	if err := k8sClient.Get(ctx, bridgeKey, &bridge); err != nil {
		t.Fatalf("metrics bridge Deployment not created: %v", err)
	}
}
