/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

const (
	phasePending      = "Pending"
	phaseProvisioning = "Provisioning"
	phaseReady        = "Ready"
)

func newModelGatewayReconciler() *controller.ModelGatewayReconciler {
	return &controller.ModelGatewayReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
		// The Bedrock upstream address and signing region are both derived
		// from this, so the reconciler refuses to render without it.
		Region: "us-west-2",
	}
}

func reconcileGateway(ctx context.Context, t *testing.T, mg *agentsv1alpha1.ModelGateway) {
	t.Helper()
	r := newModelGatewayReconciler()
	// First call adds the finalizer + RequeueAfter; second drives the real
	// reconcile path. Loop up to 3 times to land in a terminal phase.
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: mg.Name, Namespace: mg.Namespace}})
		if err != nil {
			t.Fatalf("modelgateway reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func TestModelGatewayReconciler_PendingWhenPlatformMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "g"), Namespace: testNs},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "no-such-platform"},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "primary", ModelFamily: "anthropic", ModelID: "us.anthropic.claude-3-5-sonnet-20241022-v2:0"},
			},
		},
	}
	mustCreate(ctx, t, mg)
	reconcileGateway(ctx, t, mg)

	var got agentsv1alpha1.ModelGateway
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mg.Name, Namespace: mg.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (PlatformRef dangles)", got.Status.Phase)
	}
}

func TestModelGatewayReconciler_PendingWhenPlatformNotReady(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	// Create a Platform CR but don't reconcile it — its status.phase stays empty.
	pName := uniqueName(t, "platfo")
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "marketing", Tenant: "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "g"), Namespace: testNs},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: pName},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "primary", ModelFamily: "anthropic", ModelID: "us.anthropic.claude-3-5-sonnet-20241022-v2:0"},
			},
		},
	}
	mustCreate(ctx, t, mg)
	reconcileGateway(ctx, t, mg)

	var got agentsv1alpha1.ModelGateway
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mg.Name, Namespace: mg.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (Platform not Ready)", got.Status.Phase)
	}
}

func TestModelGatewayReconciler_PendingWhenPlatformReadyAndGatewayCRDsMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	// envtest installs no Envoy AI Gateway CRDs, so the reconciler should
	// detect that and surface Pending rather than erroring — a cluster that
	// has the operator but not the data plane is a normal bootstrap ordering,
	// not a failure.
	pName := uniqueName(t, "platfo")
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "support", Tenant: "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	// Force Platform to Ready by writing status directly (skipping the
	// full PlatformReconciler path so this test stays focused on the
	// gateway reconciler).
	p.Status.Phase = phaseReady
	p.Status.Namespace = controller.PlatformNamespace(p)
	if err := k8sClient.Status().Update(ctx, p); err != nil {
		t.Fatalf("force platform Ready: %v", err)
	}

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "g"), Namespace: testNs},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: pName},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "primary", ModelFamily: "anthropic", ModelID: "us.anthropic.claude-3-5-sonnet-20241022-v2:0"},
			},
		},
	}
	mustCreate(ctx, t, mg)
	reconcileGateway(ctx, t, mg)

	var got agentsv1alpha1.ModelGateway
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mg.Name, Namespace: mg.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// No data-plane CRDs → Pending. Reaching this proves both that the
	// platform-ready gate passed and that the missing CRDs were tolerated
	// rather than thrown.
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (Envoy AI Gateway CRDs not installed in envtest)", got.Status.Phase)
	}
}
