/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

// clockBudget is a reconciler over one Ready Platform and a kill-switch-enabled
// BudgetPolicy whose CUR spend alone is 150% of its cap, reading time from clk.
func clockBudget(t *testing.T, clk *clocktesting.FakeClock, athena *awsfake.Athena) (*BudgetReconciler, client.Client, *awsfake.CloudWatch, *awsfake.EventBridge, ctrl.Request) {
	t.Helper()
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "clock-acme", Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseReady},
	}
	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "clock-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "clock-acme"}, MonthlyUsd: "100.00", KillSwitchEnabled: true,
		},
	}
	cl := fake.NewClientBuilder().WithScheme(killSwitchTestScheme(t)).WithObjects(platform, bp).WithStatusSubresource(bp).Build()
	cw, eb := awsfake.NewCloudWatch(0.5), awsfake.NewEventBridge()
	r := &BudgetReconciler{
		Client:                 cl,
		RequeueInterval:        time.Hour,
		ClusterName:            "staging-platform",
		Athena:                 athena,
		AthenaCfg:              AthenaConfig{Workgroup: "cost_wg", Database: "cost_db", CURTableName: "cur_eks_agent_platform"},
		CloudWatch:             cw,
		EventBridge:            eb,
		KillSwitchEventBusName: "killswitch-bus",
		Clock:                  clk,
	}
	return r, cl, cw, eb, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(bp)}
}

func storedBudget(t *testing.T, cl client.Client, req ctrl.Request) *governancev1alpha1.BudgetPolicy {
	t.Helper()
	var bp governancev1alpha1.BudgetPolicy
	if err := cl.Get(context.Background(), req.NamespacedName, &bp); err != nil {
		t.Fatalf("get BudgetPolicy: %v", err)
	}
	return &bp
}

// TestBudgetReconcilerReadsTheInjectedClock drives whole reconciles on a fake
// clock half an hour into March 2024, the day after a leap day, and asserts
// every time the reconciler derives came from that clock and not the wall: the
// in-flight window's end, and its start clamped to the billing month's first
// instant where a 24-hour lookback would reach 29 February; the breach event's
// timestamp; and the times written to status. It then steps the clock inside
// and past the kill-switch grace window. Read from the wall clock, the firing
// it measures from would be years old, and the breach would re-fire on the
// first step.
func TestBudgetReconcilerReadsTheInjectedClock(t *testing.T) {
	ctx := context.Background()
	monthStart := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	t0 := monthStart.Add(30 * time.Minute)
	clk := clocktesting.NewFakeClock(t0)
	r, cl, cw, eb, req := clockBudget(t, clk, succeededAthena(curRollupResultSet("150.000000")))

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if len(cw.Calls) != 1 {
		t.Fatalf("GetMetricData calls = %d, want 1", len(cw.Calls))
	}
	if got := aws.ToTime(cw.Calls[0].StartTime); !got.Equal(monthStart) {
		t.Errorf("in-flight window starts %s, want the billing month's first instant %s (a 24h lookback from the clock is %s)",
			got, monthStart, t0.Add(-24*time.Hour))
	}
	if got := aws.ToTime(cw.Calls[0].EndTime); !got.Equal(t0) {
		t.Errorf("in-flight window ends %s, want the clock's %s", got, t0)
	}
	if len(eb.Entries) != 1 {
		t.Fatalf("published %d breach events at 150%% of budget, want 1", len(eb.Entries))
	}
	if got := aws.ToTime(eb.Entries[0].Time); !got.Equal(t0) {
		t.Errorf("breach event time %s, want the clock's %s", got, t0)
	}
	bp := storedBudget(t, cl, req)
	if bp.Status.LastReconciled == nil || !bp.Status.LastReconciled.Equal(&metav1.Time{Time: t0}) {
		t.Errorf("status.lastReconciled = %v, want %s", bp.Status.LastReconciled, t0)
	}
	if bp.Status.KillSwitchFiredAt == nil || !bp.Status.KillSwitchFiredAt.Time.Equal(t0) {
		t.Errorf("status.killSwitchFiredAt = %v, want %s", bp.Status.KillSwitchFiredAt, t0)
	}
	if c := apimeta.FindStatusCondition(bp.Status.Conditions, "BudgetReconciled"); c == nil || !c.LastTransitionTime.Time.Equal(t0) {
		t.Errorf("BudgetReconciled condition %+v, want its transition at %s", c, t0)
	}

	// Two hours on, inside the 3h grace window: awaiting suspension, no re-fire.
	clk.Step(2 * time.Hour)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile inside the grace window: %v", err)
	}
	if got := aws.ToTime(cw.Calls[1].EndTime); !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("second in-flight window ends %s, want the stepped clock's %s", got, t0.Add(2*time.Hour))
	}
	bp = storedBudget(t, cl, req)
	if len(eb.Entries) != 1 || bp.Status.KillSwitchRefireCount != 0 {
		t.Errorf("inside the grace window: %d events, refire count %d; want the breach published once and not re-fired",
			len(eb.Entries), bp.Status.KillSwitchRefireCount)
	}
	if c := apimeta.FindStatusCondition(bp.Status.Conditions, "KillSwitchUnrouted"); c == nil || c.Reason != "AwaitingSuspension" {
		t.Errorf("KillSwitchUnrouted condition %+v, want AwaitingSuspension inside the grace window", c)
	}

	// Four hours on, past the grace window: the breach is re-published, stamped
	// with the clock's time, and the re-fire recorded at it.
	clk.Step(2 * time.Hour)
	t4 := t0.Add(4 * time.Hour)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile past the grace window: %v", err)
	}
	bp = storedBudget(t, cl, req)
	if len(eb.Entries) != 2 || !aws.ToTime(eb.Entries[1].Time).Equal(t4) {
		t.Fatalf("past the grace window: events %+v, want a re-fire stamped %s", eb.Entries, t4)
	}
	if bp.Status.KillSwitchRefireCount != 1 || bp.Status.KillSwitchLastRefireAt == nil || !bp.Status.KillSwitchLastRefireAt.Time.Equal(t4) {
		t.Errorf("refire count %d at %v, want 1 at %s", bp.Status.KillSwitchRefireCount, bp.Status.KillSwitchLastRefireAt, t4)
	}
	if c := apimeta.FindStatusCondition(bp.Status.Conditions, "KillSwitchUnrouted"); c == nil || c.Reason != "SuspensionNotObserved" {
		t.Errorf("KillSwitchUnrouted condition %+v, want SuspensionNotObserved past the grace window", c)
	}
}

// TestBudgetReconcilerStampsAFailedReconcileFromTheClock: the condition a
// failed reconcile writes carries the clock's time as its transition.
func TestBudgetReconcilerStampsAFailedReconcileFromTheClock(t *testing.T) {
	t0 := time.Date(2024, 3, 1, 0, 30, 0, 0, time.UTC)
	r, cl, _, _, req := clockBudget(t, clocktesting.NewFakeClock(t0), &awsfake.Athena{StartErr: errors.New("throttled")})
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := apimeta.FindStatusCondition(storedBudget(t, cl, req).Status.Conditions, "BudgetReconciled")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ReconcileFailed" || !c.LastTransitionTime.Time.Equal(t0) {
		t.Errorf("BudgetReconciled condition %+v, want False/ReconcileFailed transitioning at %s", c, t0)
	}
}

// TestBudgetReconcilerDefaultsToTheWallClock: with no Clock the reconciler
// reads the wall clock.
func TestBudgetReconcilerDefaultsToTheWallClock(t *testing.T) {
	before := time.Now()
	got := (&BudgetReconciler{}).now()
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("now() = %s with no Clock, want the wall clock's time", got)
	}
}
