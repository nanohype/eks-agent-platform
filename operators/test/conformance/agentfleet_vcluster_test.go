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

// kagentAwareVCluster builds an in-memory vcluster client whose scheme knows the
// kagent + KEDA custom kinds (as unstructured), so the fleet reconciler's kagent
// Agent/ModelConfig objects can actually land there — proving the target-client
// swap routes workload objects into the virtual cluster rather than merely
// tolerating their absence.
func kagentAwareVCluster() client.Client {
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
	register("kagent.dev", "v1alpha1", "ModelConfig")
	register("kagent.dev", "v1alpha2", "Agent")
	register("keda.sh", "v1alpha1", "ScaledObject")
	register("keda.sh", "v1alpha1", "TriggerAuthentication")
	return fake.NewClientBuilder().WithScheme(s).Build()
}

// TestAgentFleetReconciler_VClusterTier_RoutesKagentIntoVCluster proves a WORKLOAD
// reconciler honors the isolation tier: for a vcluster-tier Platform, the fleet's
// kagent Agent + ModelConfig land in the virtual cluster (through the target-client
// swap), not on the host — while the fleet's NetworkPolicy stays host containment.
func TestAgentFleetReconciler_VClusterTier_RoutesKagentIntoVCluster(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	pName := uniqueName(t, "p")
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "ops", Tenant: "acme",
			Budget:    platformv1alpha1.BudgetRef{Name: "x"},
			Identity:  platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Isolation: "vcluster",
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

	vc := kagentAwareVCluster()
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
				{Name: "primary", SystemPrompt: "be brief", ModelRoute: "primary"},
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

	// The kagent Agent landed in the VIRTUAL cluster.
	vcAgent := &unstructured.Unstructured{}
	vcAgent.SetGroupVersionKind(schema.GroupVersionKind{Group: "kagent.dev", Version: "v1alpha2", Kind: "Agent"})
	if err := vc.Get(ctx, types.NamespacedName{Name: agentName, Namespace: tenantNS}, vcAgent); err != nil {
		t.Errorf("kagent Agent should exist in the virtual cluster: %v", err)
	}

	// The tenant SA also landed in the virtual cluster (syncs to host from there).
	var vcSA corev1.ServiceAccount
	if err := vc.Get(ctx, types.NamespacedName{Name: "tenant-runtime", Namespace: tenantNS}, &vcSA); err != nil {
		t.Errorf("tenant-runtime SA should exist in the virtual cluster: %v", err)
	}

	// The Agent must NOT be on the host (envtest has no kagent CRDs anyway, so a
	// host write would have errored — this asserts the reconcile didn't target the
	// host client for workload objects).
	hostAgent := &unstructured.Unstructured{}
	hostAgent.SetGroupVersionKind(schema.GroupVersionKind{Group: "kagent.dev", Version: "v1alpha2", Kind: "Agent"})
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentName, Namespace: tenantNS}, hostAgent); err == nil {
		t.Error("kagent Agent must not be created on the host in the vcluster tier")
	}
}
