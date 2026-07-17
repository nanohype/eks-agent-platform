/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

func newBudgetReconciler() *controller.BudgetReconciler {
	return &controller.BudgetReconciler{
		Client:          k8sClient,
		Scheme:          scheme,
		Concurrency:     1,
		RequeueInterval: time.Hour,
		// Athena/CloudWatch/EventBridge intentionally nil — the reconciler's
		// degrade-to-zero path is what we're verifying.
	}
}

func reconcileBudget(ctx context.Context, t *testing.T, bp *governancev1alpha1.BudgetPolicy) {
	t.Helper()
	r := newBudgetReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: bp.Name, Namespace: bp.Namespace}}); err != nil {
		t.Fatalf("budget reconcile: %v", err)
	}
}

func TestBudgetReconciler_NoOpWhenPlatformMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "b"), Namespace: testNs},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef:            commonv1alpha1.LocalRef{Name: "no-such-platform"},
			MonthlyUsd:             "1000",
			AlertThresholdsPercent: []int32{50, 80, 100},
			KillSwitchEnabled:      true,
		},
	}
	mustCreate(ctx, t, bp)
	reconcileBudget(ctx, t, bp)

	// Status should reflect the zero-spend reading: a dangling platformRef
	// is recoverable (Platform may be created later), not a permanent error.
	var got governancev1alpha1.BudgetPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name, Namespace: bp.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.PercentOfBudget != 0 {
		t.Errorf("status.percentOfBudget: got %d want 0 (no platform)", got.Status.PercentOfBudget)
	}
	if got.Status.LastReconciled == nil {
		t.Error("status.lastReconciled: got nil, want timestamp (reconciler still records the tick)")
	}
}

func TestBudgetReconciler_ZeroSpendWithoutCostPipeline(t *testing.T) {
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

	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "b"), Namespace: testNs},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef:            commonv1alpha1.LocalRef{Name: pName},
			MonthlyUsd:             "2500",
			AlertThresholdsPercent: []int32{50, 80, 100},
			KillSwitchEnabled:      true,
		},
	}
	mustCreate(ctx, t, bp)
	reconcileBudget(ctx, t, bp)

	var got governancev1alpha1.BudgetPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name, Namespace: bp.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Athena unconfigured + CloudWatch unconfigured → reading degrades to
	// "0" spend, 0% of budget, no kill-switch event. Status still records
	// the tick + a Reconciled condition.
	if got.Status.CurrentSpendUsd != "0.000000" {
		t.Errorf("status.currentSpendUsd: got %q want %q (no cost pipeline)", got.Status.CurrentSpendUsd, "0.000000")
	}
	if got.Status.PercentOfBudget != 0 {
		t.Errorf("status.percentOfBudget: got %d want 0", got.Status.PercentOfBudget)
	}
	if got.Status.KillSwitchFiredAt != nil {
		t.Error("status.killSwitchFiredAt: should be nil at 0% spend")
	}
	found := false
	for _, c := range got.Status.Conditions {
		if c.Type == "BudgetReconciled" {
			found = true
		}
	}
	if !found {
		t.Error("missing BudgetReconciled condition")
	}
}

// budgetWithFiredKillSwitch creates a Platform in the given phase and a
// BudgetPolicy whose kill-switch already fired well outside the grace window,
// so the reconciler runs its effect-verification path on the next tick.
func budgetWithFiredKillSwitch(ctx context.Context, t *testing.T, phase string) *governancev1alpha1.BudgetPolicy {
	t.Helper()
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
	p.Status.Phase = phase
	p.Status.Namespace = controller.PlatformNamespace(p)
	if err := k8sClient.Status().Update(ctx, p); err != nil {
		t.Fatalf("force platform phase %s: %v", phase, err)
	}

	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "b"), Namespace: testNs},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef:       commonv1alpha1.LocalRef{Name: pName},
			MonthlyUsd:        "2500",
			KillSwitchEnabled: true,
		},
	}
	mustCreate(ctx, t, bp)
	firedAt := metav1.NewTime(time.Now().Add(-10 * time.Hour)) // past the default 3h grace
	bp.Status.KillSwitchFiredAt = &firedAt
	if err := k8sClient.Status().Update(ctx, bp); err != nil {
		t.Fatalf("seed KillSwitchFiredAt: %v", err)
	}
	return bp
}

func killSwitchUnroutedCondition(bp *governancev1alpha1.BudgetPolicy) *metav1.Condition {
	for i := range bp.Status.Conditions {
		if bp.Status.Conditions[i].Type == "KillSwitchUnrouted" {
			return &bp.Status.Conditions[i]
		}
	}
	return nil
}

// TestBudgetReconciler_UnroutedWhenPlatformNeverSuspends proves the switch does
// not record a false success: fired + platform never Suspended + grace elapsed
// ⇒ a KillSwitchUnrouted=True condition and a bounded re-fire.
func TestBudgetReconciler_UnroutedWhenPlatformNeverSuspends(t *testing.T) {
	ctx := context.Background()
	bp := budgetWithFiredKillSwitch(ctx, t, phaseReady)

	reconcileBudget(ctx, t, bp)

	var got governancev1alpha1.BudgetPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name, Namespace: bp.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := killSwitchUnroutedCondition(&got)
	if cond == nil {
		t.Fatal("missing KillSwitchUnrouted condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != "SuspensionNotObserved" {
		t.Errorf("KillSwitchUnrouted = %s/%s; want True/SuspensionNotObserved", cond.Status, cond.Reason)
	}
	if got.Status.KillSwitchRefireCount != 1 {
		t.Errorf("KillSwitchRefireCount = %d; want 1 (first re-fire)", got.Status.KillSwitchRefireCount)
	}
	if got.Status.KillSwitchLastRefireAt == nil {
		t.Error("KillSwitchLastRefireAt = nil; want a timestamp after a re-fire")
	}
}

// TestBudgetReconciler_LatchSettlesWhenSuspended proves the latch: once the
// platform is observed Suspended, the fired switch neither re-fires nor flags
// unrouted.
func TestBudgetReconciler_LatchSettlesWhenSuspended(t *testing.T) {
	ctx := context.Background()
	bp := budgetWithFiredKillSwitch(ctx, t, phaseSuspended)

	reconcileBudget(ctx, t, bp)

	var got governancev1alpha1.BudgetPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name, Namespace: bp.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := killSwitchUnroutedCondition(&got)
	if cond == nil {
		t.Fatal("missing KillSwitchUnrouted condition")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "SuspensionObserved" {
		t.Errorf("KillSwitchUnrouted = %s/%s; want False/SuspensionObserved", cond.Status, cond.Reason)
	}
	if got.Status.KillSwitchRefireCount != 0 {
		t.Errorf("KillSwitchRefireCount = %d; want 0 (no re-fire once suspended)", got.Status.KillSwitchRefireCount)
	}
}
