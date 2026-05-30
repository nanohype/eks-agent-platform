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

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

func TestBudgetPolicy_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "b"), Namespace: testNs},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef:            commonv1alpha1.LocalRef{Name: "conformance-platform"},
			MonthlyUsd:             "500",
			AlertThresholdsPercent: []int32{50, 80, 100},
			KillSwitchEnabled:      true,
		},
	}

	mustCreate(ctx, t, bp)

	var got governancev1alpha1.BudgetPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.MonthlyUsd != "500" {
		t.Errorf("monthlyUsd: got %q want %q", got.Spec.MonthlyUsd, "500")
	}
}

func TestBudgetPolicy_AcceptsFractionalDollars(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "b"), Namespace: testNs},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "x"},
			MonthlyUsd:  "1500.50",
		},
	}

	mustCreate(ctx, t, bp)
}

func TestBudgetPolicy_RejectsMalformedMonthlyUsd(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "b"), Namespace: testNs},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "x"},
			MonthlyUsd:  "not-a-number",
		},
	}

	err := k8sClient.Create(ctx, bp)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, bp) })
		t.Fatalf("expected validation error for non-numeric monthlyUsd, got nil")
	}
}
