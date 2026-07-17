/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// mgwScheme knows the operator's own kinds but deliberately NOT the agentgateway
// / Gateway-API kinds — so a CreateOrUpdate of those unstructured resources
// surfaces as a NoKindMatch, the "CRDs not installed" path the reconciler treats
// as Pending rather than an error.
func mgwScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := agentsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGatewayLabels(t *testing.T) {
	l := gatewayLabels(ctrlTestPlatform)
	if l[LabelPlatform] != ctrlTestPlatform || l["app.kubernetes.io/managed-by"] != "eks-agent-platform" {
		t.Errorf("gatewayLabels: %v", l)
	}
	r := routeLabels(ctrlTestPlatform, "anthropic")
	if r[LabelModelFamily] != "anthropic" || r[LabelPlatform] != ctrlTestPlatform {
		t.Errorf("routeLabels: %v", r)
	}
}

func TestModelGatewayResolvePlatform(t *testing.T) {
	s := mgwScheme(t)
	p := newPlatform(ctrlTestPlatform, "team")
	p.Status.Phase = phaseReady
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s}

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: p.Namespace},
		Spec:       agentsv1alpha1.ModelGatewaySpec{PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform}},
	}
	got, err := r.resolvePlatform(context.Background(), mg)
	if err != nil || got.Name != ctrlTestPlatform {
		t.Fatalf("resolvePlatform: got (%v, %v)", got, err)
	}

	mg.Spec.PlatformRef.Name = "ghost"
	if _, err := r.resolvePlatform(context.Background(), mg); !errors.Is(err, errPlatformNotFound) {
		t.Fatalf("dangling ref must be errPlatformNotFound, got %v", err)
	}
}

func TestModelGatewayReconcileSelf(t *testing.T) {
	s := mgwScheme(t)

	mg := func(ns string) *agentsv1alpha1.ModelGateway {
		return &agentsv1alpha1.ModelGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns},
			Spec: agentsv1alpha1.ModelGatewaySpec{
				PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
				Routes: []agentsv1alpha1.ModelRouteSpec{
					{Name: "chat", ModelFamily: "anthropic", ModelID: "anthropic.claude-sonnet-4-6-v1:0", RateLimit: 60},
					{Name: "cheap", ModelFamily: "anthropic", ModelID: "anthropic.claude-haiku-4-5-v1:0", CrossRegionProfile: "us.anthropic.claude-haiku-4-5-v1:0"},
				},
			},
		}
	}

	t.Run("platform not found is pending", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &ModelGatewayReconciler{Client: cl, Scheme: s}
		phase, _, err := r.reconcileSelf(context.Background(), mg(ctrlTestNS))
		if err != nil || phase != phasePending {
			t.Fatalf("missing platform: got (%q, %v)", phase, err)
		}
	})

	t.Run("platform not ready is pending", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &ModelGatewayReconciler{Client: cl, Scheme: s}
		phase, _, err := r.reconcileSelf(context.Background(), mg(ctrlTestNS))
		if err != nil || phase != phasePending {
			t.Fatalf("not-ready platform: got (%q, %v)", phase, err)
		}
	})

	t.Run("ready platform renders the gateway data plane", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		p.Status.Phase = phaseReady
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
		phase, endpoint, err := r.reconcileSelf(context.Background(), mg(ctrlTestNS))
		if err != nil {
			t.Fatalf("reconcileSelf (ready): %v", err)
		}
		if phase != phaseReady {
			t.Errorf("phase: got %q want Ready", phase)
		}
		if endpoint == "" {
			t.Error("a ready gateway must return its data-plane endpoint")
		}
	})
}

func TestEnsureRouteRateLimit_RemovesWhenDisabled(t *testing.T) {
	s := mgwScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s}
	// rpm <= 0 deletes any stale policy; a missing policy is tolerated.
	if err := r.ensureRouteRateLimit(context.Background(), ctrlTestPlatform, "acme-chat", 0); err != nil {
		t.Fatalf("disabling a rate limit must be a clean no-op: %v", err)
	}
}

func TestCleanupGatewayResources(t *testing.T) {
	s := mgwScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s}
	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ctrlTestNS},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Routes:      []agentsv1alpha1.ModelRouteSpec{{Name: "chat", ModelFamily: "anthropic", ModelID: "m"}},
		},
	}
	// Nothing was created — cleanup tolerates NotFound at every delete.
	if err := r.cleanupGatewayResources(context.Background(), mg); err != nil {
		t.Fatalf("cleanupGatewayResources on a fresh cluster must be a no-op: %v", err)
	}
}
