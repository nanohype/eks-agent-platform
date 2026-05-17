/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/stxkxs/eks-agent-platform/operators/api/v1alpha1"
	"github.com/stxkxs/eks-agent-platform/operators/internal/controller"
)

func newAgentFleetReconciler() *controller.AgentFleetReconciler {
	return &controller.AgentFleetReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
	}
}

func reconcileFleet(ctx context.Context, t *testing.T, fleet *agentsv1alpha1.AgentFleet) {
	t.Helper()
	r := newAgentFleetReconciler()
	// Same shape as the gateway reconciler driver: first call adds the
	// finalizer + 100ms requeue; second drives the real reconcile.
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: fleet.Name, Namespace: fleet.Namespace}})
		if err != nil {
			t.Fatalf("agentfleet reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func TestAgentFleetReconciler_PendingWhenPlatformMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	fleet := &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "fleet"), Namespace: testNs},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: "no-such-platform"},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "primary", SystemPrompt: "be brief", ModelRoute: "primary"},
			},
		},
	}
	mustCreate(ctx, t, fleet)
	reconcileFleet(ctx, t, fleet)

	var got agentsv1alpha1.AgentFleet
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: fleet.Name, Namespace: fleet.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (PlatformRef dangles)", got.Status.Phase)
	}
}

func TestAgentFleetReconciler_PendingWhenKagentMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	pName := uniqueName(t, "platfo")
	p := &agentsv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: testNs},
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
	// Real flow: PlatformReconciler creates the tenant namespace. Here
	// we stub it directly so the fleet reconciler's tenant-side step
	// (ensureTenantServiceAccount) has somewhere to write.
	tenantNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Status.Namespace}}
	if err := k8sClient.Create(ctx, tenantNs); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create tenant ns: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, tenantNs) })

	fleet := &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "fleet"), Namespace: testNs},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: pName},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "primary", SystemPrompt: "be brief", ModelRoute: "primary"},
			},
		},
	}
	mustCreate(ctx, t, fleet)
	reconcileFleet(ctx, t, fleet)

	var got agentsv1alpha1.AgentFleet
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: fleet.Name, Namespace: fleet.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// envtest has no kagent.dev CRDs installed → reconciler tolerates
	// NoKindMatch on Agent/ModelConfig and surfaces Pending. This proves
	// the platform-ready gate works AND that missing kagent is non-fatal.
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (kagent CRDs not installed in envtest)", got.Status.Phase)
	}
}
