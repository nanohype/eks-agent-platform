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

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

func TestBatchJob_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bj := &agentsv1alpha1.BatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "bj"), Namespace: testNs},
		Spec: agentsv1alpha1.BatchJobSpec{
			PlatformRef:    commonv1alpha1.LocalRef{Name: "conformance-platform"},
			ModelID:        "anthropic.claude-3-5-sonnet-20241022-v2:0",
			InputS3Uri:     "s3://bucket/in/records.jsonl",
			OutputS3Prefix: "s3://bucket/out/",
			// TimeoutHours omitted — the +kubebuilder:default=24 should apply.
		},
	}
	mustCreate(ctx, t, bj)

	var got agentsv1alpha1.BatchJob
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bj.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.ModelInvocationType != "InvokeModel" {
		t.Errorf("modelInvocationType default: got %q want InvokeModel", got.Spec.ModelInvocationType)
	}
	if got.Spec.TimeoutHours != 24 {
		t.Errorf("timeoutHours default: got %d want 24", got.Spec.TimeoutHours)
	}
	if got.Spec.ModelID != bj.Spec.ModelID {
		t.Errorf("modelId: got %q want %q", got.Spec.ModelID, bj.Spec.ModelID)
	}
}

func TestBatchJob_RejectsBelowMinTimeout(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bj := &agentsv1alpha1.BatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "bj"), Namespace: testNs},
		Spec: agentsv1alpha1.BatchJobSpec{
			PlatformRef:    commonv1alpha1.LocalRef{Name: "x"},
			ModelID:        "anthropic.claude-3-5-sonnet-20241022-v2:0",
			InputS3Uri:     "s3://b/in.jsonl",
			OutputS3Prefix: "s3://b/out/",
			TimeoutHours:   1, // below Minimum=24 — the field under test
		},
	}
	if err := k8sClient.Create(ctx, bj); err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, bj) })
		t.Fatalf("expected validation error for timeoutHours=1 (min 24), got nil")
	}
}

func TestBatchJob_RejectsNonS3Input(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bj := &agentsv1alpha1.BatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "bj"), Namespace: testNs},
		Spec: agentsv1alpha1.BatchJobSpec{
			PlatformRef:    commonv1alpha1.LocalRef{Name: "x"},
			ModelID:        "anthropic.claude-3-5-sonnet-20241022-v2:0",
			InputS3Uri:     "https://not-s3/in.jsonl", // fails the ^s3:// pattern
			OutputS3Prefix: "s3://b/out/",
			TimeoutHours:   24,
		},
	}
	if err := k8sClient.Create(ctx, bj); err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, bj) })
		t.Fatalf("expected validation error for non-s3 inputS3Uri, got nil")
	}
}
