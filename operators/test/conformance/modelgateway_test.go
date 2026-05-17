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

	agentsv1alpha1 "github.com/stxkxs/eks-agent-platform/operators/api/v1alpha1"
)

func TestModelGateway_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "g"), Namespace: testNs},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: "conformance-platform"},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{
					Name:        "primary",
					ModelFamily: "anthropic",
					ModelID:     "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
					RateLimit:   60,
				},
			},
		},
	}

	mustCreate(ctx, t, mg)

	var got agentsv1alpha1.ModelGateway
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mg.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Spec.Routes) != 1 || got.Spec.Routes[0].ModelID != "us.anthropic.claude-3-5-sonnet-20241022-v2:0" {
		t.Errorf("spec.routes round-trip: got %#v", got.Spec.Routes)
	}
}

func TestModelGateway_RejectsEmptyRoutes(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "g"), Namespace: testNs},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: "conformance-platform"},
			Routes:      []agentsv1alpha1.ModelRouteSpec{}, // empty — should violate MinItems=1
		},
	}

	err := k8sClient.Create(ctx, mg)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, mg) })
		t.Fatalf("expected validation error for empty routes, got nil")
	}
}

func TestModelGateway_RejectsInvalidModelFamily(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "g"), Namespace: testNs},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: "conformance-platform"},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "x", ModelFamily: "not-a-family", ModelID: "x"},
			},
		},
	}

	err := k8sClient.Create(ctx, mg)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, mg) })
		t.Fatalf("expected validation error for invalid modelFamily, got nil")
	}
}
