/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// exprWindowRE pulls the range window back out of a generated query so the fake
// can answer per-window without the test having to mirror the query builder.
var exprWindowRE = regexp.MustCompile(`\[([0-9]+[a-z]+)\]`)

// fakePrometheus answers instant queries from a per-window table. A window
// absent from the table returns no-data, which is how the reconciler learns the
// series is not there — distinct from a healthy zero.
type fakePrometheus struct {
	byWindow map[string]float64
	err      error
	queries  []string
}

func (f *fakePrometheus) QueryScalar(_ context.Context, expr string) (float64, bool, error) {
	f.queries = append(f.queries, expr)
	if f.err != nil {
		return 0, false, f.err
	}
	m := exprWindowRE.FindStringSubmatch(expr)
	if m == nil {
		return 0, false, nil
	}
	v, ok := f.byWindow[m[1]]
	return v, ok, nil
}

// healthyRatios and burningRatios are full window tables at objective 0.999
// (budget 0.001). 0.02 is 20x the budget — over the 14.4x fast-burn factor.
func healthyRatios() map[string]float64 {
	return map[string]float64{"5m": 0, "30m": 0, "1h": 0, "2h": 0, "6h": 0, "1d": 0, "3d": 0, sloWindow: 0}
}

func burningRatios() map[string]float64 {
	return map[string]float64{"5m": 0.02, "30m": 0.02, "1h": 0.02, "2h": 0.02, "6h": 0.02, "1d": 0.02, "3d": 0.02, sloWindow: 0.02}
}

func sloTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add platform scheme: %v", err)
	}
	if err := governancev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add governance scheme: %v", err)
	}
	// The AppProject is read as unstructured so the argoproj.io Go types stay
	// out of the operator's dependency graph; the fake client still needs the
	// kind registered to serve it.
	s.AddKnownTypeWithName(appProjectGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(appProjectGVK.GroupVersion().WithKind("AppProjectList"), &unstructured.UnstructuredList{})
	return s
}

func newSLOPolicy() *governancev1alpha1.SLOPolicy {
	return &governancev1alpha1.SLOPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-slo", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.SLOPolicySpec{
			PlatformRef:      commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			SLI:              governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: "acme_api"},
			Objective:        "0.999",
			OnPageTierBreach: onBreachHoldRollout,
		},
	}
}

func newReadyPlatform() *platformv1alpha1.Platform {
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: ctrlTestPlatform, Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseReady},
	}
}

// appProjectWithHold builds the AppProject the Platform reconciler would render
// for an engaged hold.
func appProjectWithHold(held bool) *unstructured.Unstructured {
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(appProjectGVK)
	ap.SetName(ctrlTestPlatform)
	ap.SetNamespace(argoCDNamespace)
	spec := map[string]interface{}{"description": "AppProject for Platform acme"}
	if held {
		spec["syncWindows"] = []interface{}{sloDenyWindow()}
	}
	ap.Object["spec"] = spec
	return ap
}

type sloHarness struct {
	r  *SLOReconciler
	eb *fakeEventBridge
	sp *governancev1alpha1.SLOPolicy
}

func newSLOHarness(t *testing.T, ratios map[string]float64, objs ...client.Object) *sloHarness {
	t.Helper()
	sp := newSLOPolicy()
	all := append([]client.Object{newReadyPlatform(), sp}, objs...)
	cl := fake.NewClientBuilder().
		WithScheme(sloTestScheme(t)).
		WithObjects(all...).
		WithStatusSubresource(sp).
		Build()
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{}}
	r := &SLOReconciler{
		Client:                 cl,
		EventBridge:            eb,
		KillSwitchEventBusName: "acme-killswitch",
		RequeueInterval:        5 * time.Minute,
		HoldGraceIntervals:     2,
	}
	if ratios != nil {
		r.Prometheus = &fakePrometheus{byWindow: ratios}
	}
	return &sloHarness{r: r, eb: eb, sp: sp}
}

func condition(sp *governancev1alpha1.SLOPolicy, condType string) *metav1.Condition {
	for i := range sp.Status.Conditions {
		if sp.Status.Conditions[i].Type == condType {
			return &sp.Status.Conditions[i]
		}
	}
	return nil
}

func requireCondition(t *testing.T, sp *governancev1alpha1.SLOPolicy, condType, wantStatus, wantReason string) {
	t.Helper()
	c := condition(sp, condType)
	if c == nil {
		t.Fatalf("condition %s missing; have %+v", condType, sp.Status.Conditions)
	}
	if string(c.Status) != wantStatus || c.Reason != wantReason {
		t.Errorf("condition %s = %s/%s, want %s/%s (message: %s)", condType, c.Status, c.Reason, wantStatus, wantReason, c.Message)
	}
}

// run performs one full tick: evaluate, then write status, the way Reconcile
// does.
func (h *sloHarness) run(t *testing.T) sloReading {
	t.Helper()
	reading, err := h.r.reconcileSLO(context.Background(), h.sp)
	if err != nil {
		t.Fatalf("reconcileSLO: %v", err)
	}
	if err := h.r.applySLOStatus(context.Background(), h.sp, reading); err != nil {
		t.Fatalf("applySLOStatus: %v", err)
	}
	return reading
}

func TestReconcileSLO_HealthyPublishesNothingAndHoldsNothing(t *testing.T) {
	h := newSLOHarness(t, healthyRatios())
	reading := h.run(t)

	if reading.severity != "" {
		t.Errorf("severity = %q, want healthy", reading.severity)
	}
	if len(h.eb.calls) != 0 {
		t.Errorf("a healthy objective must publish nothing, got %d events", len(h.eb.calls))
	}
	if h.sp.Status.HoldEngagedAt != nil {
		t.Error("a healthy objective must not engage a hold")
	}
	requireCondition(t, h.sp, "SLOEvaluated", "True", "Evaluated")
	requireCondition(t, h.sp, "BurnRateBreach", "False", "WithinBudget")
	requireCondition(t, h.sp, "RolloutHeld", "False", "NotHeld")
	if h.sp.Status.ErrorBudgetRemaining != "1.000000" {
		t.Errorf("error budget remaining = %q, want the full budget", h.sp.Status.ErrorBudgetRemaining)
	}
	if got := len(h.sp.Status.ErrorRatios); got != 8 {
		t.Errorf("status carries %d window ratios, want one per queried window", got)
	}
}

func TestReconcileSLO_PageBurnPublishesAndEngagesTheHold(t *testing.T) {
	// Seeded without the hold window: the Platform reconciler renders it after
	// this reconciler engages, so on the engaging tick it is legitimately absent.
	h := newSLOHarness(t, burningRatios(), appProjectWithHold(false))
	reading := h.run(t)

	if reading.severity != severityCritical {
		t.Fatalf("severity = %q, want critical", reading.severity)
	}
	if len(h.eb.calls) != 1 {
		t.Fatalf("want exactly one published breach, got %d", len(h.eb.calls))
	}
	entry := h.eb.calls[0].Entries[0]
	if got := aws.ToString(entry.Source); got != sloEventSource {
		t.Errorf("event source = %q, want %q", got, sloEventSource)
	}
	if got := aws.ToString(entry.DetailType); got != sloEventDetailType {
		t.Errorf("event detail-type = %q, want %q", got, sloEventDetailType)
	}
	detail := aws.ToString(entry.Detail)
	for _, want := range []string{`"severity":"critical"`, `"platformId":"acme"`, `"reason":"slo-burn-rate"`, `"breachedWindow":"1h"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("event detail missing %s: %s", want, detail)
		}
	}
	if h.sp.Status.HoldEngagedAt == nil {
		t.Error("a page-tier breach with OnPageTierBreach=HoldRollout must engage the hold")
	}
	requireCondition(t, h.sp, "BurnRateBreach", "True", "BurningTooFast")
	// The hold is engaged but the Platform reconciler has not rendered the
	// window yet, so it is decided, not effective — and the condition says so.
	requireCondition(t, h.sp, "RolloutHeld", "False", "AwaitingHold")
}

// A steady breach must not re-page every tick: the hold is the action, and it is
// re-verified each tick anyway.
func TestReconcileSLO_SteadyBreachDoesNotRepublish(t *testing.T) {
	h := newSLOHarness(t, burningRatios())
	h.run(t)
	if len(h.eb.calls) != 1 {
		t.Fatalf("first tick should publish once, got %d", len(h.eb.calls))
	}
	h.run(t)
	if len(h.eb.calls) != 1 {
		t.Errorf("a steady-state breach must not re-publish; got %d events", len(h.eb.calls))
	}
}

// An escalation from ticket to page tier is a page that has not happened yet.
func TestReconcileSLO_SeverityEscalationRepublishes(t *testing.T) {
	// 0.0031 is 3.1x the budget: over the 3x ticket factor, under 14.4x.
	ticket := map[string]float64{"5m": 0, "30m": 0, "1h": 0, "2h": 0.0031, "6h": 0, "1d": 0.0031, "3d": 0.0031, sloWindow: 0.0031}
	h := newSLOHarness(t, ticket)
	h.run(t)
	if h.sp.Status.Severity != severityWarning {
		t.Fatalf("first tick severity = %q, want warning", h.sp.Status.Severity)
	}
	if len(h.eb.calls) != 1 {
		t.Fatalf("a ticket-tier breach must publish once, got %d", len(h.eb.calls))
	}

	h.r.Prometheus = &fakePrometheus{byWindow: burningRatios()}
	h.run(t)
	if h.sp.Status.Severity != severityCritical {
		t.Fatalf("second tick severity = %q, want critical", h.sp.Status.Severity)
	}
	if len(h.eb.calls) != 2 {
		t.Errorf("escalating warning→critical must publish again; got %d events", len(h.eb.calls))
	}
}

func TestReconcileSLO_ClearedBurnReleasesTheHold(t *testing.T) {
	h := newSLOHarness(t, burningRatios(), appProjectWithHold(true))
	h.run(t)
	if h.sp.Status.HoldEngagedAt == nil {
		t.Fatal("setup: the hold should be engaged")
	}

	h.r.Prometheus = &fakePrometheus{byWindow: healthyRatios()}
	h.run(t)

	if h.sp.Status.HoldEngagedAt != nil {
		t.Error("a cleared burn must release the hold")
	}
	if h.sp.Status.HoldObservedAt != nil {
		t.Error("releasing the hold must clear the observation too")
	}
	if h.sp.Status.BreachFiredAt != nil {
		t.Error("a cleared burn must drop the publish latch so a later breach pages again")
	}
	requireCondition(t, h.sp, "RolloutHeld", "False", "NotHeld")
}

// Losing telemetry during a bad rollout is exactly when auto-resuming is worst.
// The hold must survive a missing signal, in both of its shapes.
func TestReconcileSLO_MissingSignalNeverReleasesTheHold(t *testing.T) {
	cases := []struct {
		name       string
		ratios     map[string]float64
		wantReason string
	}{
		{"no AMP client at all", nil, "MetricStoreUnavailable"},
		{"AMP up but no series", map[string]float64{}, "NoData"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newSLOHarness(t, burningRatios(), appProjectWithHold(true))
			h.run(t)
			if h.sp.Status.HoldEngagedAt == nil {
				t.Fatal("setup: the hold should be engaged")
			}

			if c.ratios == nil {
				h.r.Prometheus = nil
			} else {
				h.r.Prometheus = &fakePrometheus{byWindow: c.ratios}
			}
			h.run(t)

			if h.sp.Status.HoldEngagedAt == nil {
				t.Error("losing the signal must not release the hold — that would resume a burning rollout during an observability outage")
			}
			requireCondition(t, h.sp, "SLOEvaluated", "False", c.wantReason)
			// Unknown, not False: an absent signal is not evidence of health.
			requireCondition(t, h.sp, "BurnRateBreach", "Unknown", "SignalUnavailable")
		})
	}
}

func TestReconcileSLO_HoldRolloutCanBeDeclined(t *testing.T) {
	h := newSLOHarness(t, burningRatios())
	h.sp.Spec.OnPageTierBreach = "None"
	h.run(t)

	if len(h.eb.calls) != 1 {
		t.Errorf("declining the hold must not suppress the page; got %d events", len(h.eb.calls))
	}
	if h.sp.Status.HoldEngagedAt != nil {
		t.Error("OnPageTierBreach=None must not engage a hold")
	}
}

func TestReconcileSLO_ObservedHoldSettlesTheLatch(t *testing.T) {
	h := newSLOHarness(t, burningRatios(), appProjectWithHold(true))
	h.run(t) // engages
	h.run(t) // observes the window the Platform reconciler rendered

	if h.sp.Status.HoldObservedAt == nil {
		t.Fatal("a present deny window must be recorded as observed")
	}
	requireCondition(t, h.sp, "RolloutHeld", "True", "HoldObserved")
	if got := testutil.ToFloat64(sloHoldActive.WithLabelValues("tenants-acme", "acme-slo", ctrlTestPlatform)); got != 1 {
		t.Errorf("agents_slo_hold_active = %v, want 1 while the window is present", got)
	}
}

// Engaging a hold is a decision; the AppProject carrying the window is the
// effect. If the effect never lands, the tenant's rollout is still advancing and
// the platform must say so loudly rather than record a false success.
func TestReconcileSLO_UnobservedHoldIsReported(t *testing.T) {
	h := newSLOHarness(t, burningRatios(), appProjectWithHold(false))
	h.run(t)

	// Backdate the engagement past the grace window.
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	h.sp.Status.HoldEngagedAt = &past

	before := testutil.ToFloat64(sloHoldUnobservedTotal.WithLabelValues("tenants-acme", "acme-slo", ctrlTestPlatform))
	reading := h.run(t)

	if !reading.holdUnobs {
		t.Fatal("an engaged hold whose window never appeared past the grace window must be reported")
	}
	requireCondition(t, h.sp, "RolloutHeld", "False", "HoldNotObserved")
	after := testutil.ToFloat64(sloHoldUnobservedTotal.WithLabelValues("tenants-acme", "acme-slo", ctrlTestPlatform))
	if after != before+1 {
		t.Errorf("agents_slo_hold_unobserved_total went %v → %v, want +1 so an increase() alert stays lit", before, after)
	}
}

// ArgoCD is not on every cluster. A tenant with no AppProject is a design
// choice, not a broken hold, and must not page as one.
func TestReconcileSLO_AbsentAppProjectIsUnverifiableNotBroken(t *testing.T) {
	h := newSLOHarness(t, burningRatios())
	h.run(t)
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	h.sp.Status.HoldEngagedAt = &past
	reading := h.run(t)

	if reading.holdUnobs {
		t.Error("a missing AppProject must not be reported as a failed hold")
	}
	requireCondition(t, h.sp, "RolloutHeld", "Unknown", "AppProjectAbsent")
}

func TestReconcileSLO_DanglingPlatformRef(t *testing.T) {
	sp := newSLOPolicy()
	cl := fake.NewClientBuilder().WithScheme(sloTestScheme(t)).WithObjects(sp).WithStatusSubresource(sp).Build()
	r := &SLOReconciler{Client: cl, RequeueInterval: 5 * time.Minute}

	reading, err := r.reconcileSLO(context.Background(), sp)
	if err != nil {
		t.Fatalf("a dangling platformRef must not be a hard error: %v", err)
	}
	if err := r.applySLOStatus(context.Background(), sp, reading); err != nil {
		t.Fatalf("applySLOStatus: %v", err)
	}
	requireCondition(t, sp, "SLOEvaluated", "False", "PlatformNotFound")
}

// PutEvents answers 200 even when the entry failed. Treating that as success
// would drop a page silently.
func TestFireBurnRateBreach_PartialFailureIsAnError(t *testing.T) {
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{
		FailedEntryCount: 1,
		Entries: []ebtypes.PutEventsResultEntry{{
			ErrorCode:    aws.String("ThrottlingException"),
			ErrorMessage: aws.String("rate exceeded"),
		}},
	}}
	r := &SLOReconciler{EventBridge: eb, KillSwitchEventBusName: "acme-killswitch"}
	reading := sloReading{eval: burnEvaluation{severity: severityCritical, pageRatio: 2, breachedWindow: "1h"}}

	err := r.fireBurnRateBreach(context.Background(), newSLOPolicy(), "acme", reading)
	if err == nil {
		t.Fatal("FailedEntryCount>0 must surface as an error so the breach is re-published next tick")
	}
	if !strings.Contains(err.Error(), "ThrottlingException") {
		t.Errorf("error must carry the failed entry's ErrorCode, got %q", err)
	}
}

// With no bus configured the control loop still runs — status records the
// breach and the hold still engages. Only paging degrades.
func TestFireBurnRateBreach_NoBusIsLogOnly(t *testing.T) {
	r := &SLOReconciler{}
	if err := r.fireBurnRateBreach(context.Background(), newSLOPolicy(), "acme", sloReading{}); err != nil {
		t.Errorf("an unconfigured bus must degrade paging, not fail the reconcile: %v", err)
	}
}

func TestQueryErrorRatios_SurfacesQueryErrors(t *testing.T) {
	r := &SLOReconciler{Prometheus: &fakePrometheus{err: context.DeadlineExceeded}}
	if _, err := r.queryErrorRatios(context.Background(), newSLOPolicy().Spec.SLI); err == nil {
		t.Fatal("a failing query must surface so the tick retries rather than deciding on partial data")
	}
}

func TestQueryErrorRatios_NilClientIsNotConfigured(t *testing.T) {
	r := &SLOReconciler{}
	_, err := r.queryErrorRatios(context.Background(), newSLOPolicy().Spec.SLI)
	if err == nil || !strings.Contains(err.Error(), "amp query endpoint") {
		t.Fatalf("want errAMPNotConfigured, got %v", err)
	}
}

// A deleted objective must not leave its last burn rate frozen on every
// dashboard that reads the series.
func TestForgetSLOPolicySeriesClearsGauges(t *testing.T) {
	sloBurnRate.WithLabelValues("ns", "gone", "plat", tierPage).Set(7)
	sloHoldActive.WithLabelValues("ns", "gone", "plat").Set(1)
	forgetSLOPolicySeries("ns", "gone")
	// Asserted on this policy's own series, not the vec total — other tests in
	// this package populate the same vectors.
	if testutil.CollectAndCount(sloBurnRate, "agents_slo_burn_rate") > 0 {
		for _, tier := range []string{tierPage, tierTicket} {
			m, err := sloBurnRate.GetMetricWithLabelValues("ns", "gone", "plat", tier)
			if err != nil {
				t.Fatalf("get metric: %v", err)
			}
			if got := testutil.ToFloat64(m); got != 0 {
				t.Errorf("burn-rate series for the deleted policy survived with value %v", got)
			}
		}
	}
}

// Partial signal loss is the subtler version of the same hazard: the ticket tier
// resolves, the page tier does not, so the tick is NOT signalMissing and the
// page tier reads as "not breaching". Releasing the hold on that is releasing it
// on a signal nobody read.
func TestReconcileSLO_PageTierBlackoutNeverReleasesTheHold(t *testing.T) {
	h := newSLOHarness(t, burningRatios(), appProjectWithHold(true))
	h.run(t)
	if h.sp.Status.HoldEngagedAt == nil {
		t.Fatal("setup: the hold should be engaged")
	}

	// Long windows healthy and resolving; both page short-windows absent, as a
	// missed scrape would leave them.
	h.r.Prometheus = &fakePrometheus{byWindow: map[string]float64{
		"1h": 0, "6h": 0, "1d": 0, "2h": 0, "3d": 0, sloWindow: 0,
	}}
	reading := h.run(t)

	if reading.eval.pageHaveData {
		t.Fatal("setup: the page tier should have no data in this fixture")
	}
	if h.sp.Status.HoldEngagedAt == nil {
		t.Error("a page tier that did not resolve must not release the hold — that is auto-resuming a rollout on an unread signal")
	}
	if h.sp.Status.PageTierBreachRatio != "" {
		t.Errorf("page-tier ratio = %q; an unevaluated tier must be absent, not a fabricated 0", h.sp.Status.PageTierBreachRatio)
	}
	if h.sp.Status.TicketTierBreachRatio == "" {
		t.Error("the ticket tier resolved and should still be reported")
	}
}

// LastEvaluated answers "is this objective being measured", which LastReconciled
// cannot: a reconciler that ticks and fails every query keeps LastReconciled
// fresh forever.
func TestReconcileSLO_LastEvaluatedTracksRealReadingsOnly(t *testing.T) {
	h := newSLOHarness(t, healthyRatios())
	h.run(t)
	if h.sp.Status.LastEvaluated == nil {
		t.Fatal("a successful evaluation must set LastEvaluated")
	}
	// Backdate both, so the next tick's effect is visible regardless of how
	// coarse metav1.Now() is — two ticks in one second are otherwise equal.
	// Truncated to the second: a status round-trip drops sub-second precision,
	// so a nanosecond-bearing fixture would fail an equality check for reasons
	// that have nothing to do with the behaviour under test.
	old := metav1.NewTime(time.Now().Add(-2 * time.Hour).Truncate(time.Second))
	h.sp.Status.LastEvaluated = &old
	h.sp.Status.LastReconciled = &old

	h.r.Prometheus = nil
	h.run(t)

	if h.sp.Status.LastReconciled == nil || !h.sp.Status.LastReconciled.After(old.Time) {
		t.Error("LastReconciled must advance on every tick, including one that got no reading")
	}
	if h.sp.Status.LastEvaluated == nil || !h.sp.Status.LastEvaluated.Equal(&old) {
		t.Error("LastEvaluated must NOT advance on a tick that obtained no reading — that is the whole reason it is a separate field")
	}
}

// TestMetricStoreResolvesAfterStartup is the regression for the defect that made
// the whole SLO tier unarmable on a first install.
//
// main.go builds the AMP client from an SSM parameter that managed-monitoring
// publishes, and managed-monitoring is applied AFTER the cluster hosting the
// operator. So the client is nil at boot. The endpoint is not a chart value, so
// ArgoCD sees no manifest change when the parameter lands and nothing restarts
// the pod — and every SLOPolicy reported MetricStoreUnavailable forever on a
// cluster whose AMP workspace was up.
func TestMetricStoreResolvesAfterStartup(t *testing.T) {
	ctx := context.Background()
	var calls int
	want := &fakePrometheus{byWindow: map[string]float64{"5m": 0.01}}

	r := &SLOReconciler{
		ResolvePrometheus: func(context.Context) (awsclients.PrometheusQuery, error) {
			calls++
			if calls == 1 {
				// managed-monitoring has not applied yet: no parameter, no error.
				return nil, nil
			}
			return want, nil
		},
	}

	if got := r.metricStore(ctx); got != nil {
		t.Fatalf("first call: want nil while the parameter is absent, got %#v", got)
	}
	if calls != 1 {
		t.Fatalf("first call: want 1 resolve attempt, got %d", calls)
	}

	// Still inside the backoff — absent is a normal steady state on a cluster
	// without managed-monitoring, so it must not read SSM on every tick.
	if got := r.metricStore(ctx); got != nil {
		t.Fatalf("second call: want nil, got %#v", got)
	}
	if calls != 1 {
		t.Fatalf("second call: want the backoff to suppress a re-read, got %d attempts", calls)
	}

	// The parameter lands. Nothing restarted the process.
	r.promRetryAfter = time.Now().Add(-time.Second)
	got := r.metricStore(ctx)
	if got == nil {
		t.Fatal("after the endpoint appears: want a resolved client, got nil — the tier can never arm")
	}
	if got != want {
		t.Fatalf("resolved the wrong client: got %#v, want %#v", got, want)
	}

	// Resolved once and cached: no further SSM reads.
	if got := r.metricStore(ctx); got != want {
		t.Fatalf("after resolving: want the cached client, got %#v", got)
	}
	if calls != 2 {
		t.Fatalf("want the resolved client cached after 2 attempts, got %d", calls)
	}
}

// A reconciler with no resolver (envtest, and any path with no SSM) must answer
// nil rather than panic, and must never be considered resolvable.
func TestMetricStoreWithoutResolver(t *testing.T) {
	r := &SLOReconciler{}
	if got := r.metricStore(context.Background()); got != nil {
		t.Fatalf("want nil with no resolver and no client, got %#v", got)
	}

	direct := &fakePrometheus{}
	r = &SLOReconciler{Prometheus: direct}
	if got := r.metricStore(context.Background()); got != direct {
		t.Fatalf("want the directly-wired client returned unchanged, got %#v", got)
	}
}

// A resolver that errors must not be retried on the next tick, and must not
// poison the client — SSM throttling should degrade to "not yet", not to a
// permanent failure.
func TestMetricStoreResolveErrorBacksOff(t *testing.T) {
	ctx := context.Background()
	var calls int
	r := &SLOReconciler{
		ResolvePrometheus: func(context.Context) (awsclients.PrometheusQuery, error) {
			calls++
			return nil, errors.New("ssm throttled")
		},
	}
	if got := r.metricStore(ctx); got != nil {
		t.Fatalf("want nil on resolve error, got %#v", got)
	}
	if got := r.metricStore(ctx); got != nil {
		t.Fatalf("want nil on resolve error, got %#v", got)
	}
	if calls != 1 {
		t.Fatalf("want the error path to respect the backoff, got %d attempts", calls)
	}
}
