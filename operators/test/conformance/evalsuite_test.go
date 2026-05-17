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

func TestEvalSuite_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: agentsv1alpha1.EvalSuiteSpec{
			PlatformRef:   agentsv1alpha1.LocalRef{Name: "conformance-platform"},
			AgentFleetRef: agentsv1alpha1.LocalRef{Name: "conformance-fleet"},
			Schedule:      "0 6 * * *",
			PassThreshold: "0.85",
			Cases: []agentsv1alpha1.EvalCase{
				{Name: "smoke", Input: "pong?", ExpectContains: []string{"pong"}, MaxLatencyMs: 5000},
			},
		},
	}

	mustCreate(ctx, t, es)

	var got agentsv1alpha1.EvalSuite
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: es.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.PassThreshold != "0.85" {
		t.Errorf("passThreshold: got %q want 0.85", got.Spec.PassThreshold)
	}
}

func TestEvalSuite_RejectsInvalidPassThreshold(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	// Fill every other constraint so the only thing the API server can be
	// rejecting on is the passThreshold pattern. Otherwise this test would
	// pass for the wrong reason if a future required-field is added to the
	// EvalSuite schema.
	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: agentsv1alpha1.EvalSuiteSpec{
			PlatformRef:   agentsv1alpha1.LocalRef{Name: "x"},
			AgentFleetRef: agentsv1alpha1.LocalRef{Name: "y"},
			PassThreshold: "1.5", // out of 0..1 range — the field under test
			Cases: []agentsv1alpha1.EvalCase{
				{Name: "smoke", Input: "x", ExpectContains: []string{"x"}},
			},
		},
	}

	err := k8sClient.Create(ctx, es)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, es) })
		t.Fatalf("expected validation error for passThreshold=1.5, got nil")
	}
}

func TestEvalSuite_RejectsCasesPlusCasesFromManifest(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: agentsv1alpha1.EvalSuiteSpec{
			PlatformRef:       agentsv1alpha1.LocalRef{Name: "x"},
			AgentFleetRef:     agentsv1alpha1.LocalRef{Name: "y"},
			PassThreshold:     "0.85",
			CasesFromManifest: "s3://bucket/manifest.json",
			Cases: []agentsv1alpha1.EvalCase{
				{Name: "inline", Input: "x", ExpectContains: []string{"x"}},
			},
		},
	}

	err := k8sClient.Create(ctx, es)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, es) })
		t.Fatalf("expected validation error for both casesFromManifest AND cases set, got nil (CEL XValidation should fire)")
	}
}
