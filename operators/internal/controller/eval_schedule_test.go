/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// scheduledSuite is a scheduled suite past its finalizer pass, on a Platform
// and fleet that are Ready, whose Platform reports its eval identity bound.
// The Platform's ServiceAccount and gateway are returned for the client to
// hold.
func scheduledSuite(r *EvalReconciler) (*governancev1alpha1.EvalSuite, *platformv1alpha1.Platform, *agentsv1alpha1.AgentFleet, []client.Object) {
	suite, platform, fleet := evalFixtures()
	suite.Spec.Schedule = "0 3 * * *"
	suite.Finalizers = []string{evalFinalizer}
	platform.Status.Phase = phaseReady
	fleet.Status.Phase = phaseReady
	account := boundEvalIdentity(r, platform)
	return suite, platform, fleet, []client.Object{account, publishedGateway(platform, "chat")}
}

// reconcileSuite runs one EvalSuite Reconcile pass and returns the suite as
// stored afterwards.
func reconcileSuite(t *testing.T, r *EvalReconciler, suite *governancev1alpha1.EvalSuite) (*governancev1alpha1.EvalSuite, error) {
	t.Helper()
	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(suite)})
	var got governancev1alpha1.EvalSuite
	if gerr := r.Get(ctx, client.ObjectKeyFromObject(suite), &got); gerr != nil {
		t.Fatalf("get suite: %v", gerr)
	}
	return &got, err
}

// TestPendingSuiteSuspendsItsSchedule: a suite is Pending when the reconciler
// would not submit a run for it, and a CronWorkflow made on an earlier pass
// would submit that run anyway, on its own clock. Each reason below holds the
// suite Pending and suspends its schedule: for the identity cases, a run would
// be held to a ServiceAccount that is unbound or about to be deleted.
func TestPendingSuiteSuspendsItsSchedule(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*platformv1alpha1.Platform, *agentsv1alpha1.AgentFleet) bool
		reason string
	}{
		{
			name: "identity not bound",
			mutate: func(p *platformv1alpha1.Platform, _ *agentsv1alpha1.AgentFleet) bool {
				apimeta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
					Type: conditionEvalIdentityReady, Status: metav1.ConditionFalse, Reason: "ReconcileFailed", Message: "iam CreateRole: boom",
				})
				return true
			},
			reason: "EvalIdentityReady=False (ReconcileFailed)",
		},
		{
			name: "Platform being deleted",
			mutate: func(p *platformv1alpha1.Platform, _ *agentsv1alpha1.AgentFleet) bool {
				now := metav1.Now()
				p.DeletionTimestamp = &now
				p.Finalizers = []string{finalizerName}
				return true
			},
			reason: "Platform analytics is being deleted",
		},
		{
			name:   "Platform gone",
			mutate: func(*platformv1alpha1.Platform, *agentsv1alpha1.AgentFleet) bool { return false },
			reason: "platformRef analytics not found",
		},
		{
			name: "fleet not Ready",
			mutate: func(_ *platformv1alpha1.Platform, f *agentsv1alpha1.AgentFleet) bool {
				f.Status.Phase = phaseProvisioning
				return true
			},
			reason: "waiting on readiness",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &EvalReconciler{RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}
			suite, platform, fleet, objs := scheduledSuite(r)
			keep := c.mutate(platform, fleet)
			objs = append(objs, suite, fleet, evalSchedule(evalWorkflowName(suite), "eval-runner", platform.Name, false))
			if keep {
				objs = append(objs, platform)
			}
			r.Client = fake.NewClientBuilder().WithScheme(evalScheme(t)).
				WithStatusSubresource(&governancev1alpha1.EvalSuite{}).WithObjects(objs...).Build()

			got, err := reconcileSuite(t, r, suite)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got.Status.Phase != phasePending {
				t.Errorf("phase = %q, want Pending", got.Status.Phase)
			}
			cond := apimeta.FindStatusCondition(got.Status.Conditions, "EvalReconciled")
			if cond == nil || !strings.Contains(cond.Message, c.reason) {
				t.Errorf("EvalReconciled = %+v, want a message carrying %q", cond, c.reason)
			}
			if !suspended(t, r.Client, "eval-runner", evalWorkflowName(suite)) {
				t.Error("the schedule of a Pending suite was left running")
			}
		})
	}
}

// TestBoundSuiteResumesItsSchedule: the pass that finds every precondition
// met writes the schedule's spec whole, which lifts a suspension an earlier
// Pending pass set.
func TestBoundSuiteResumesItsSchedule(t *testing.T) {
	r := &EvalReconciler{RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}
	suite, platform, fleet, objs := scheduledSuite(r)
	objs = append(objs, suite, platform, fleet, evalSchedule(evalWorkflowName(suite), "eval-runner", platform.Name, true))
	r.Client = fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithStatusSubresource(&governancev1alpha1.EvalSuite{}).WithObjects(objs...).Build()

	got, err := reconcileSuite(t, r, suite)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Status.Phase != phaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
	if suspended(t, r.Client, "eval-runner", evalWorkflowName(suite)) {
		t.Error("the schedule stayed suspended after a pass that admits its runs")
	}
}

// TestSuspendSchedule covers the schedule lookup and write on their own: a
// suite with no CronWorkflow, a cluster without the kind, and a schedule
// already suspended make no write; a failed lookup or write is retried rather
// than reported as Pending.
func TestSuspendSchedule(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	cronGVK := argoWorkflowsGV.WithKind("CronWorkflow")
	cases := []struct {
		name      string
		existing  *unstructured.Unstructured
		getErr    error
		patchErr  error
		want      string
		wantPatch int
	}{
		{name: "no schedule"},
		{name: "kind not installed", getErr: &apimeta.NoKindMatchError{GroupKind: cronGVK.GroupKind()}},
		{name: "already suspended", existing: evalSchedule("analytics-nightly", "eval-runner", "analytics", true)},
		{name: "running", existing: evalSchedule("analytics-nightly", "eval-runner", "analytics", false), wantPatch: 1},
		{name: "lookup fails", getErr: boom, want: "get argo CronWorkflow eval-runner/analytics-nightly"},
		{
			name: "write fails", existing: evalSchedule("analytics-nightly", "eval-runner", "analytics", false),
			patchErr: boom, want: "suspend argo CronWorkflow eval-runner/analytics-nightly", wantPatch: 1,
		},
		{
			name: "deleted since it was read", existing: evalSchedule("analytics-nightly", "eval-runner", "analytics", false),
			patchErr: apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "cronworkflows"}, "analytics-nightly"), wantPatch: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			patches := 0
			b := fake.NewClientBuilder().WithScheme(evalScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if c.getErr != nil {
						return c.getErr
					}
					return cl.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if c.patchErr != nil {
						return c.patchErr
					}
					return cl.Patch(ctx, obj, patch, opts...)
				},
			})
			if c.existing != nil {
				b = b.WithObjects(c.existing)
			}
			r := &EvalReconciler{Client: b.Build(), RunnerNamespace: "eval-runner"}
			suite := &governancev1alpha1.EvalSuite{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: testSuiteNamespace}}
			suite.Spec.PlatformRef.Name = "analytics"

			err := r.suspendSchedule(ctx, suite)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("suspendSchedule = %v, want nil", err)
			case c.want != "" && (!errors.Is(err, boom) || !strings.Contains(err.Error(), c.want)):
				t.Errorf("suspendSchedule = %v, want an error wrapping boom that names %q", err, c.want)
			}
			if patches != c.wantPatch {
				t.Errorf("patches = %d, want %d", patches, c.wantPatch)
			}
			if c.name == "running" && !suspended(t, r.Client, "eval-runner", "analytics-nightly") {
				t.Error("the running schedule was not suspended")
			}
		})
	}
}

// TestPendingSuiteRetriesAFailedSuspension: a schedule that could not be
// suspended is retried, and the suite's status is not rewritten as though it
// had been.
func TestPendingSuiteRetriesAFailedSuspension(t *testing.T) {
	r := &EvalReconciler{RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}
	suite, _, fleet, objs := scheduledSuite(r)
	boom := errors.New("boom")
	objs = append(objs, suite, fleet, evalSchedule(evalWorkflowName(suite), "eval-runner", "analytics", false))
	r.Client = fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithStatusSubresource(&governancev1alpha1.EvalSuite{}).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return boom
		}}).Build()

	got, err := reconcileSuite(t, r, suite)
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile = %v, want the failed suspension returned for a retry", err)
	}
	if got.Status.Phase != "" {
		t.Errorf("phase = %q written over a schedule left running", got.Status.Phase)
	}
}

// TestEvalRunRefusesADeletingPlatform: a Platform being deleted still carries
// the EvalIdentityReady=True its last pass wrote, and the account still exists
// until its finalizer reaches it. Neither admits a run.
func TestEvalRunRefusesADeletingPlatform(t *testing.T) {
	ctx := context.Background()
	suite, platform, fleet := evalFixtures()
	r := &EvalReconciler{RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}
	account := boundEvalIdentity(r, platform)
	now := metav1.Now()
	platform.DeletionTimestamp = &now
	cl := fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithObjects(publishedGateway(platform, "chat"), account).Build()
	r.Client = cl

	err := r.ensureArgoWorkflow(ctx, suite, platform, fleet)
	if !errors.Is(err, errEvalIdentityMissing) || !strings.Contains(err.Error(), "Platform analytics is being deleted") {
		t.Fatalf("ensureArgoWorkflow = %v, want errEvalIdentityMissing naming the deletion", err)
	}
	assertNoEvalWorkflow(ctx, t, cl, evalWorkflowName(suite))
}

// TestPlatformToSuites: a Platform event reaches every suite in its namespace
// that references it, and no other.
func TestPlatformToSuites(t *testing.T) {
	ctx := context.Background()
	mk := func(name, namespace, platform string) *governancev1alpha1.EvalSuite {
		s := &governancev1alpha1.EvalSuite{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		s.Spec.PlatformRef.Name = platform
		return s
	}
	cl := fake.NewClientBuilder().WithScheme(evalScheme(t)).WithObjects(
		mk("nightly", testSuiteNamespace, "analytics"),
		mk("hourly", testSuiteNamespace, "analytics"),
		mk("billing-nightly", testSuiteNamespace, "billing"),
		mk("elsewhere", "other-namespace", "analytics"),
	).Build()
	r := &EvalReconciler{Client: cl}
	p := &platformv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: testSuiteNamespace}}

	got := r.platformToSuites(ctx, p)
	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: testSuiteNamespace, Name: "hourly"}},
		{NamespacedName: types.NamespacedName{Namespace: testSuiteNamespace, Name: "nightly"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("platformToSuites = %v, want %v", got, want)
	}
	if got := r.platformToSuites(ctx, mk("not-a-platform", testSuiteNamespace, "analytics")); got != nil {
		t.Errorf("a non-Platform object mapped to %v", got)
	}

	failing := &EvalReconciler{Client: fake.NewClientBuilder().WithScheme(evalScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	}).Build()}
	if got := failing.platformToSuites(ctx, p); got != nil {
		t.Errorf("a failed list mapped to %v", got)
	}
}
