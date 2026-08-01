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

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

func newAgentFleetReconciler() *controller.AgentFleetReconciler {
	return &controller.AgentFleetReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
	}
}

// publishFleetGateway gives a Platform a ModelGateway that has already
// published the named routes.
//
// A fleet reads that contract to configure its agents' model client, so without
// one there is no base URL to hand them and the fleet stays Pending by design.
// Stubbed here the same way the Platform's own Ready status is: the gateway
// reconciler has its own conformance coverage, and driving it would make every
// fleet test depend on the Envoy AI Gateway CRDs being installed.
func publishFleetGateway(ctx context.Context, t *testing.T, p *platformv1alpha1.Platform, routes ...string) {
	t.Helper()
	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "gw"), Namespace: p.Namespace},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: p.Name},
			Routes: []agentsv1alpha1.ModelRouteSpec{{
				Name: routes[0], ModelSource: agentsv1alpha1.ModelSourceFoundation,
				ModelFamily: "anthropic", ModelID: "anthropic.claude-sonnet-5",
			}},
		},
	}
	mustCreate(ctx, t, mg)
	for _, name := range routes {
		mg.Status.Routes = append(mg.Status.Routes, agentsv1alpha1.RouteStatus{
			Name:    name,
			API:     agentsv1alpha1.RouteAPIAnthropic,
			BaseURL: controller.RouteBaseURL(p, agentsv1alpha1.RouteAPIAnthropic),
		})
	}
	if err := k8sClient.Status().Update(ctx, mg); err != nil {
		t.Fatalf("publish route contract: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, mg) })
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
			PlatformRef: commonv1alpha1.LocalRef{Name: "no-such-platform"},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "primary", SystemPrompt: "be brief", ModelRoute: "primary", Image: "ghcr.io/acme/agent:v1"},
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

func TestAgentFleetReconciler_ReadyOnceThePlatformIs(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	pName := uniqueName(t, "platfo")
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "ops", Tenant: "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
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
	publishFleetGateway(ctx, t, p, "primary")

	fleet := &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "fleet"), Namespace: testNs},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: pName},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "primary", SystemPrompt: "be brief", ModelRoute: "primary", Image: "ghcr.io/acme/agent:v1"},
			},
		},
	}
	mustCreate(ctx, t, fleet)
	reconcileFleet(ctx, t, fleet)

	var got agentsv1alpha1.AgentFleet
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: fleet.Name, Namespace: fleet.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// An agent is a plain Deployment now, so there is no addon whose absence
	// holds the fleet in Pending — reaching Ready on a bare envtest cluster is
	// the point. KEDA is still absent here and still non-fatal: the fleet runs
	// at its static replica count without it.
	if got.Status.Phase != phaseReady {
		t.Errorf("status.phase: got %q want phaseReady (an agent Deployment needs no addon)", got.Status.Phase)
	}
}
