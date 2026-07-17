/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func fleetScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		platformv1alpha1.AddToScheme, agentsv1alpha1.AddToScheme,
		networkingv1.AddToScheme, corev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func agentFleet() *agentsv1alpha1.AgentFleet {
	return &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: ctrlTestNS, Generation: 1},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "triage", SystemPrompt: "you triage", ModelRoute: "chat"},
			},
		},
	}
}

func readyPlatformIn() *platformv1alpha1.Platform {
	p := newPlatform(ctrlTestPlatform, "team")
	p.Namespace = ctrlTestNS
	p.Status.Phase = phaseReady
	p.Status.Namespace = PlatformNamespace(p)
	return p
}

func TestReconcileFleetSelf(t *testing.T) {
	s := fleetScheme(t)

	t.Run("platform not found is pending", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, ready, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || phase != phasePending || ready != 0 {
			t.Fatalf("missing platform: got (%q, %d, %v)", phase, ready, err)
		}
	})

	t.Run("platform not ready is pending", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, _, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || phase != phasePending {
			t.Fatalf("not-ready platform: got (%q, %v)", phase, err)
		}
	})

	t.Run("suspended platform tears the fleet down", func(t *testing.T) {
		p := readyPlatformIn()
		p.Status.Phase = phaseSuspended
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, ready, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || phase != phaseSuspended || ready != 0 {
			t.Fatalf("suspended platform: got (%q, %d, %v)", phase, ready, err)
		}
	})

	t.Run("ready platform renders the fleet", func(t *testing.T) {
		p := readyPlatformIn()
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, ready, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil {
			t.Fatalf("reconcileFleetSelf (ready): %v", err)
		}
		if phase != phaseReady || ready != 1 {
			t.Errorf("ready fleet: got (%q, %d) want (Ready, 1)", phase, ready)
		}
		// The host containment NetworkPolicy landed on the host.
		var np networkingv1.NetworkPolicy
		if err := cl.Get(context.Background(), types.NamespacedName{Name: "fleet-squad", Namespace: PlatformNamespace(p)}, &np); err != nil {
			t.Errorf("fleet NetworkPolicy not created on the host: %v", err)
		}
	})
}

func TestCleanupTargetClient_NamespaceTierUsesHost(t *testing.T) {
	s := fleetScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}
	if got := r.cleanupTargetClient(context.Background(), newPlatform(ctrlTestPlatform, "team")); got != cl {
		t.Error("namespace tier cleanup must delete through the host client")
	}
}

func TestCleanupFleetResources_ToleratesMissing(t *testing.T) {
	s := fleetScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}
	p := readyPlatformIn()
	// Nothing was created; every delete NotFounds and must be tolerated.
	if err := r.cleanupFleetResources(context.Background(), cl, agentFleet(), p); err != nil {
		t.Fatalf("cleanup on a fresh cluster must be a no-op: %v", err)
	}
}

func TestResolveFleetPlatform_WrapsNonNotFound(t *testing.T) {
	s := fleetScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}
	_, err := r.resolvePlatform(context.Background(), agentFleet())
	if !errors.Is(err, errPlatformNotFound) {
		t.Fatalf("a missing platform must be errPlatformNotFound, got %v", err)
	}
}
