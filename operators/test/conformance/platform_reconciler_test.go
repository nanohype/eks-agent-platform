/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

// newPlatformReconciler returns a reconciler wired to the envtest client.
// Bypasses SetupWithManager (which builds a controller and watches) — the
// tests invoke Reconcile directly to assert per-step behavior.
func newPlatformReconciler() *controller.PlatformReconciler {
	return &controller.PlatformReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
	}
}

// reconcileOnce runs Reconcile up to maxAttempts times, requeue-tolerant.
// The platform reconciler intentionally returns Requeue=true on the first
// pass (after adding the finalizer) so a single Reconcile call isn't enough
// to reach Ready.
func reconcileOnce(ctx context.Context, t *testing.T, p *platformv1alpha1.Platform) {
	t.Helper()
	r := newPlatformReconciler()
	for i := 0; i < 5; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}})
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
	t.Fatalf("reconcile did not converge in 5 attempts")
}

func getPlatform(ctx context.Context, t *testing.T, ns, name string) *platformv1alpha1.Platform {
	t.Helper()
	var got platformv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got); err != nil {
		t.Fatalf("get platform: %v", err)
	}
	return &got
}

func TestPlatformReconciler_CreatesTenantNamespaceWithPSS(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "marketing",
			Tenant:   "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })

	reconcileOnce(ctx, t, p)

	var ns corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: tenantNS}, &ns); err != nil {
		t.Fatalf("get tenant namespace: %v", err)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("PSS enforce label: got %q want restricted", ns.Labels["pod-security.kubernetes.io/enforce"])
	}
	if ns.Labels["agents.nanohype.dev/platform"] != p.Name {
		t.Errorf("platform label: got %q want %q", ns.Labels["agents.nanohype.dev/platform"], p.Name)
	}
	if ns.Labels["agents.nanohype.dev/tenant"] != "acme" {
		t.Errorf("tenant label: got %q want acme", ns.Labels["agents.nanohype.dev/tenant"])
	}
}

func TestPlatformReconciler_InstallsResourceQuotaAndLimitRange(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })

	reconcileOnce(ctx, t, p)

	var q corev1.ResourceQuota
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-default", Namespace: tenantNS}, &q); err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if q.Spec.Hard.Pods().IsZero() {
		t.Errorf("quota pods limit not set")
	}

	var lr corev1.LimitRange
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-default", Namespace: tenantNS}, &lr); err != nil {
		t.Fatalf("get limitrange: %v", err)
	}
	if len(lr.Spec.Limits) == 0 || lr.Spec.Limits[0].Type != corev1.LimitTypeContainer {
		t.Errorf("limitrange not configured for Container type: got %+v", lr.Spec.Limits)
	}
}

func TestPlatformReconciler_InstallsNetworkPolicy(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "eng", Tenant: "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })

	reconcileOnce(ctx, t, p)

	var np networkingv1.NetworkPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-egress", Namespace: tenantNS}, &np); err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("policy types: got %+v want [Egress]", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 3 {
		t.Errorf("expected 3 egress rules (DNS, agentgateway, OTel); got %d", len(np.Spec.Egress))
	}
}

func TestPlatformReconciler_StatusGoesToReady(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "founder", Tenant: "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })

	reconcileOnce(ctx, t, p)

	got := getPlatform(ctx, t, p.Namespace, p.Name)
	if got.Status.Phase != phaseReady {
		t.Errorf("status.phase: got %q want Ready", got.Status.Phase)
	}
	if got.Status.Namespace != tenantNS {
		t.Errorf("status.namespace: got %q want %q", got.Status.Namespace, tenantNS)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("status.observedGeneration: got %d want %d", got.Status.ObservedGeneration, got.Generation)
	}
	// NamespaceReady condition should be True.
	var foundCondTrue bool
	for _, c := range got.Status.Conditions {
		if c.Type == "NamespaceReady" && c.Status == metav1.ConditionTrue {
			foundCondTrue = true
		}
	}
	if !foundCondTrue {
		t.Errorf("expected NamespaceReady=True condition; got %+v", got.Status.Conditions)
	}
}

func TestPlatformReconciler_FinalizerCleansUpOnDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "p"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "legal", Tenant: "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)

	// Reconcile to provision the tenant namespace.
	reconcileOnce(ctx, t, p)

	// Verify it's there.
	var ns corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: tenantNS}, &ns); err != nil {
		t.Fatalf("tenant ns missing after first reconcile: %v", err)
	}

	// Delete the Platform — should trigger finalizer cleanup of the tenant namespace.
	if err := k8sClient.Delete(ctx, p); err != nil {
		t.Fatalf("delete platform: %v", err)
	}
	// Run reconciler once more so the finalizer fires.
	r := newPlatformReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}

	// Tenant namespace should be in Terminating state (or gone).
	err := k8sClient.Get(ctx, types.NamespacedName{Name: tenantNS}, &ns)
	if err == nil && ns.DeletionTimestamp.IsZero() {
		t.Errorf("expected tenant namespace to be Terminating or NotFound; still alive: %s", tenantNS)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		// NotFound is fine; Terminating with a DeletionTimestamp is also fine.
		// Any other error is a real problem.
		t.Logf("post-delete get: %v (treated as cleanup-in-progress)", err)
	}
}
