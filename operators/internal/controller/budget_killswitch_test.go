/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// fakeEventBridge is a minimal in-memory awsclients.EventBridge that returns a
// canned PutEvents result so the partial-failure branch can be asserted.
type fakeEventBridge struct {
	out   *eventbridge.PutEventsOutput
	calls []eventbridge.PutEventsInput
}

func (f *fakeEventBridge) PutEvents(_ context.Context, params *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	f.calls = append(f.calls, *params)
	return f.out, nil
}

func newBudgetPolicy() *governancev1alpha1.BudgetPolicy {
	return &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "acme"},
			MonthlyUsd:  "100.00",
		},
	}
}

func TestFireKillSwitch_SuccessReturnsNil(t *testing.T) {
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{FailedEntryCount: 0}}
	r := &BudgetReconciler{EventBridge: eb, KillSwitchEventBusName: "killswitch-bus"}

	if err := r.fireKillSwitch(context.Background(), newBudgetPolicy(), "150.00", 150); err != nil {
		t.Fatalf("fireKillSwitch on a clean PutEvents must return nil, got %v", err)
	}
	if len(eb.calls) != 1 {
		t.Fatalf("want exactly 1 PutEvents call, got %d", len(eb.calls))
	}
	if got := aws.ToString(eb.calls[0].Entries[0].EventBusName); got != "killswitch-bus" {
		t.Errorf("event bus = %q, want killswitch-bus", got)
	}
}

func TestFireKillSwitch_PartialFailureIsRetryableError(t *testing.T) {
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{
		FailedEntryCount: 1,
		Entries: []ebtypes.PutEventsResultEntry{{
			ErrorCode:    aws.String("ThrottlingException"),
			ErrorMessage: aws.String("rate exceeded"),
		}},
	}}
	r := &BudgetReconciler{EventBridge: eb, KillSwitchEventBusName: "killswitch-bus"}

	err := r.fireKillSwitch(context.Background(), newBudgetPolicy(), "150.00", 150)
	if err == nil {
		t.Fatal("FailedEntryCount>0 must surface as an error so the kill-switch is retried, got nil")
	}
	if !strings.Contains(err.Error(), "ThrottlingException") {
		t.Errorf("error must carry the failed entry's ErrorCode, got %q", err.Error())
	}
}

// TestKillSwitchEffect covers the effect-verification decision: publishing an
// event is not success — the platform being Suspended is. The switch re-fires
// on a bounded exponential backoff when the suspension never lands.
func TestKillSwitchEffect(t *testing.T) {
	// grace = 1 × RequeueInterval = 1m; backoff(0)=1m, backoff(1)=2m.
	r := &BudgetReconciler{RequeueInterval: time.Minute, KillSwitchGraceIntervals: 1, KillSwitchMaxRefires: 3}
	now := time.Now()

	firedAt := func(d time.Duration) *governancev1alpha1.BudgetPolicy {
		ts := metav1.NewTime(now.Add(d))
		return &governancev1alpha1.BudgetPolicy{Status: governancev1alpha1.BudgetPolicyStatus{KillSwitchFiredAt: &ts}}
	}

	cases := []struct {
		name         string
		bp           *governancev1alpha1.BudgetPolicy
		suspended    bool
		wantUnrouted bool
		wantRefire   bool
	}{
		{
			name: "never_fired",
			bp:   &governancev1alpha1.BudgetPolicy{},
		},
		{
			name:      "suspension_observed_settles",
			bp:        firedAt(-10 * time.Minute),
			suspended: true,
		},
		{
			name: "within_grace_window_waits",
			bp:   firedAt(-30 * time.Second),
		},
		{
			name:         "grace_elapsed_first_refire",
			bp:           firedAt(-2 * time.Minute),
			wantUnrouted: true,
			wantRefire:   true,
		},
		{
			name: "backoff_not_yet_elapsed",
			bp: func() *governancev1alpha1.BudgetPolicy {
				bp := firedAt(-3 * time.Minute)
				last := metav1.NewTime(now.Add(-30 * time.Second)) // < backoff(1)=2m
				bp.Status.KillSwitchRefireCount = 1
				bp.Status.KillSwitchLastRefireAt = &last
				return bp
			}(),
			wantUnrouted: true,
			wantRefire:   false,
		},
		{
			name: "backoff_elapsed_second_refire",
			bp: func() *governancev1alpha1.BudgetPolicy {
				bp := firedAt(-10 * time.Minute)
				last := metav1.NewTime(now.Add(-3 * time.Minute)) // >= backoff(1)=2m
				bp.Status.KillSwitchRefireCount = 1
				bp.Status.KillSwitchLastRefireAt = &last
				return bp
			}(),
			wantUnrouted: true,
			wantRefire:   true,
		},
		{
			name: "bounded_stops_refiring_but_stays_unrouted",
			bp: func() *governancev1alpha1.BudgetPolicy {
				bp := firedAt(-1 * time.Hour)
				last := metav1.NewTime(now.Add(-30 * time.Minute))
				bp.Status.KillSwitchRefireCount = 3 // == max
				bp.Status.KillSwitchLastRefireAt = &last
				return bp
			}(),
			wantUnrouted: true,
			wantRefire:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.killSwitchEffect(c.bp, c.suspended, now)
			if got.unrouted != c.wantUnrouted {
				t.Errorf("unrouted = %v, want %v", got.unrouted, c.wantUnrouted)
			}
			if got.refire != c.wantRefire {
				t.Errorf("refire = %v, want %v", got.refire, c.wantRefire)
			}
		})
	}
}

func killSwitchTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add platform scheme: %v", err)
	}
	if err := governancev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add governance scheme: %v", err)
	}
	return s
}

// TestReconcileBudget_UnroutedRefiresAndAlerts drives the whole reconcile body:
// a kill-switch fired earlier, the platform never suspended, and the grace
// window has elapsed. The reconciler must re-publish the breach to the bus,
// flag the reading unrouted, and bump the unrouted metric.
func TestReconcileBudget_UnroutedRefiresAndAlerts(t *testing.T) {
	ctx := context.Background()
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "unrouted-acme", Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseReady}, // NOT suspended
	}
	cl := fake.NewClientBuilder().WithScheme(killSwitchTestScheme(t)).WithObjects(platform).Build()

	firedAt := metav1.NewTime(time.Now().Add(-10 * time.Hour)) // well past the 3h grace
	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "unrouted-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "unrouted-acme"}, MonthlyUsd: "100.00", KillSwitchEnabled: true,
		},
		Status: governancev1alpha1.BudgetPolicyStatus{KillSwitchFiredAt: &firedAt},
	}
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{FailedEntryCount: 0}}
	r := &BudgetReconciler{Client: cl, EventBridge: eb, KillSwitchEventBusName: "killswitch-bus", RequeueInterval: time.Hour}

	before := testutil.ToFloat64(killSwitchUnroutedTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name))
	reading, err := r.reconcileBudget(ctx, bp)
	if err != nil {
		t.Fatalf("reconcileBudget: %v", err)
	}
	if !reading.killSwitchUnrouted {
		t.Error("reading.killSwitchUnrouted = false; want true (fired, grace elapsed, platform not suspended)")
	}
	if !reading.killSwitchRefired {
		t.Error("reading.killSwitchRefired = false; want true (should re-publish the breach)")
	}
	if len(eb.calls) != 1 {
		t.Fatalf("want exactly 1 re-fire PutEvents, got %d", len(eb.calls))
	}
	if got := aws.ToString(eb.calls[0].Entries[0].Source); got != budgetEventSource {
		t.Errorf("re-fire source = %q, want %q", got, budgetEventSource)
	}
	after := testutil.ToFloat64(killSwitchUnroutedTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name))
	if after != before+1 {
		t.Errorf("unrouted metric = %v, want %v (+1)", after, before+1)
	}
}

// TestReconcileBudget_SuspensionObservedSettles proves the latch: once the
// platform is observed Suspended, a fired kill-switch neither re-fires nor
// flags unrouted.
func TestReconcileBudget_SuspensionObservedSettles(t *testing.T) {
	ctx := context.Background()
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "settled-acme", Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseSuspended},
	}
	cl := fake.NewClientBuilder().WithScheme(killSwitchTestScheme(t)).WithObjects(platform).Build()

	firedAt := metav1.NewTime(time.Now().Add(-10 * time.Hour))
	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "settled-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "settled-acme"}, MonthlyUsd: "100.00", KillSwitchEnabled: true,
		},
		Status: governancev1alpha1.BudgetPolicyStatus{KillSwitchFiredAt: &firedAt},
	}
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{FailedEntryCount: 0}}
	r := &BudgetReconciler{Client: cl, EventBridge: eb, KillSwitchEventBusName: "killswitch-bus", RequeueInterval: time.Hour}

	reading, err := r.reconcileBudget(ctx, bp)
	if err != nil {
		t.Fatalf("reconcileBudget: %v", err)
	}
	if reading.killSwitchUnrouted {
		t.Error("reading.killSwitchUnrouted = true; want false (platform is Suspended)")
	}
	if reading.killSwitchRefired {
		t.Error("reading.killSwitchRefired = true; want false (effect confirmed, latch settles)")
	}
	if !reading.platformSuspended {
		t.Error("reading.platformSuspended = false; want true")
	}
	if len(eb.calls) != 0 {
		t.Errorf("want 0 PutEvents once suspended, got %d", len(eb.calls))
	}
}
