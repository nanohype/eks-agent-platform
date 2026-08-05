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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

// workloadAwareVCluster builds an in-memory vcluster client whose scheme knows
// the core workload kinds plus KEDA's custom kinds, so the fleet reconciler's
// Deployments and ScaledObjects can actually land there — proving the
// target-client swap routes workload objects into the virtual cluster rather
// than merely tolerating their absence.
func workloadAwareVCluster() client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	register := func(group, version, kind string) {
		gvk := schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		s.AddKnownTypeWithName(gvk, u)
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind + "List"})
		s.AddKnownTypeWithName(ul.GroupVersionKind(), ul)
	}
	register("keda.sh", "v1alpha1", "ScaledObject")
	register("keda.sh", "v1alpha1", "TriggerAuthentication")
	return fake.NewClientBuilder().WithScheme(s).Build()
}

// TestAgentFleetReconciler_VClusterTier_RoutesWorkloadIntoVCluster proves a
// WORKLOAD reconciler honors the isolation tier: for a vcluster-tier Platform the
// fleet's agent Deployments land in the virtual cluster (through the
// target-client swap), not on the host — while the fleet's NetworkPolicy stays
// host containment.
func TestAgentFleetReconciler_VClusterTier_RoutesWorkloadIntoVCluster(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	pName := uniqueName(t, "p")
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "ops", Tenant: "acme",
			Budget:    platformv1alpha1.BudgetRef{Name: "x"},
			Identity:  platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Isolation: isolationVCluster,
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	// Force the Platform Ready so the fleet reconciles (the platform-side vcluster
	// bring-up is covered by the platform conformance test).
	p.Status.Phase = "Ready"
	p.Status.Namespace = tenantNS
	if err := k8sClient.Status().Update(ctx, p); err != nil {
		t.Fatalf("force platform Ready: %v", err)
	}
	tenantNSObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}
	if err := k8sClient.Create(ctx, tenantNSObj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create tenant ns: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, tenantNSObj) })
	publishFleetGateway(ctx, t, p, "primary")

	vc := workloadAwareVCluster()
	r := &controller.AgentFleetReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
		VCluster:    &fakeVClusterFactory{vc: vc},
	}

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
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: fleet.Name, Namespace: fleet.Namespace}})
		if err != nil {
			t.Fatalf("fleet reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			break
		}
	}

	agentName := fleet.Name + "-primary"

	// The agent Deployment landed in the VIRTUAL cluster.
	var vcDeploy appsv1.Deployment
	if err := vc.Get(ctx, types.NamespacedName{Name: agentName, Namespace: tenantNS}, &vcDeploy); err != nil {
		t.Errorf("agent Deployment should exist in the virtual cluster: %v", err)
	}

	// The tenant SA also landed in the virtual cluster (syncs to host from there).
	var vcSA corev1.ServiceAccount
	if err := vc.Get(ctx, types.NamespacedName{Name: "tenant-runtime", Namespace: tenantNS}, &vcSA); err != nil {
		t.Errorf("tenant-runtime SA should exist in the virtual cluster: %v", err)
	}

	// And must NOT be on the host. A Deployment is a core kind, so unlike the
	// CRD-backed runtime this replaced, a stray host write would have *succeeded*
	// rather than erroring on a missing CRD — which makes this the assertion that
	// actually proves the reconcile targeted the virtual cluster.
	var hostDeploy appsv1.Deployment
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentName, Namespace: tenantNS}, &hostDeploy); err == nil {
		t.Error("agent Deployment must not be created on the host in the vcluster tier")
	}
}
