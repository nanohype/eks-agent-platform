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

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
)

func ptrI32(v int32) *int32 { return &v }

func TestAgentFleet_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	af := &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "f"), Namespace: testNs},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: "conformance-platform"},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "assistant", SystemPrompt: "you help", ModelRoute: "primary"},
			},
		},
	}

	mustCreate(ctx, t, af)

	var got agentsv1alpha1.AgentFleet
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: af.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Scaling defaults applied by CRD.
	if got.Spec.Scaling.Min == nil || *got.Spec.Scaling.Min != 1 {
		t.Errorf("scaling.min default: got %v want 1", got.Spec.Scaling.Min)
	}
	if got.Spec.Scaling.Max == nil || *got.Spec.Scaling.Max != 10 {
		t.Errorf("scaling.max default: got %v want 10", got.Spec.Scaling.Max)
	}
}

// Kill-switch invariant: min=0 must be representable for scale-to-zero.
func TestAgentFleet_ScalingMinZeroRoundTrips(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	af := &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "f"), Namespace: testNs},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: "conformance-platform"},
			Scaling:     agentsv1alpha1.ScalingSpec{Enabled: false, Min: ptrI32(0), Max: ptrI32(1)},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "assistant", SystemPrompt: "you help", ModelRoute: "primary"},
			},
		},
	}

	mustCreate(ctx, t, af)

	var got agentsv1alpha1.AgentFleet
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: af.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Scaling.Min == nil || *got.Spec.Scaling.Min != 0 {
		t.Errorf("scaling.min: got %v want 0 (kill-switch must be representable)", got.Spec.Scaling.Min)
	}
}
