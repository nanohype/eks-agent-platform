/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// assertNoEvalWorkflow fails the test if the suite's Workflow was submitted.
func assertNoEvalWorkflow(ctx context.Context, t *testing.T, c client.Client, name string) {
	t.Helper()
	wf := &unstructured.Unstructured{}
	wf.SetGroupVersionKind(schema.GroupVersionKind{Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: "Workflow"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: "eval-runner", Name: name}, wf); !apierrors.IsNotFound(err) {
		t.Errorf("a Workflow was submitted with no bound identity to run under (get err=%v)", err)
	}
}

// TestEvalRunWaitsForABoundIdentity: no run is submitted until the Platform
// reports its eval identity bound, and the suite says why while it waits. The
// ServiceAccount exists in every case: a Platform pass that failed after
// creating it leaves an account with no role behind it, and a run under that
// holds no AWS credential.
func TestEvalRunWaitsForABoundIdentity(t *testing.T) {
	cases := []struct {
		name string
		cond *metav1.Condition
		want string
	}{
		{"no condition", nil, "the Platform reports no EvalIdentityReady condition; " +
			"the Platform reconciler writes it on a pass that has the eval-reports bucket configured and both its IAM and EKS clients wired"},
		{
			"failed pass",
			&metav1.Condition{Type: conditionEvalIdentityReady, Status: metav1.ConditionFalse, Reason: "ReconcileFailed", Message: "iam CreateRole x-analytics-eval: AccessDenied"},
			"EvalIdentityReady=False (ReconcileFailed): iam CreateRole x-analytics-eval: AccessDenied",
		},
		{
			"deferred",
			&metav1.Condition{Type: conditionEvalIdentityReady, Status: metav1.ConditionFalse, Reason: "RuntimeNamespaceAbsent", Message: "eval runtime namespace eval-runner does not exist"},
			"EvalIdentityReady=False (RuntimeNamespaceAbsent)",
		},
		{
			"unknown",
			&metav1.Condition{Type: conditionEvalIdentityReady, Status: metav1.ConditionUnknown, Reason: "Unread", Message: "-"},
			"EvalIdentityReady=Unknown (Unread)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			suite, platform, fleet := evalFixtures()
			platform.Status.Phase = phaseReady
			fleet.Status.Phase = phaseReady
			if c.cond != nil {
				platform.Status.Conditions = []metav1.Condition{*c.cond}
			}
			r := &EvalReconciler{RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}
			account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: EvalServiceAccountName(platform), Namespace: "eval-runner"}}
			cl := fake.NewClientBuilder().WithScheme(evalScheme(t)).
				WithObjects(platform, fleet, publishedGateway(platform, "chat"), account).Build()
			r.Client = cl

			err := r.ensureArgoWorkflow(ctx, suite, platform, fleet)
			if !errors.Is(err, errEvalIdentityMissing) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ensureArgoWorkflow = %v, want errEvalIdentityMissing carrying %q", err, c.want)
			}
			assertNoEvalWorkflow(ctx, t, cl, evalWorkflowName(suite))

			phase, reason, rerr := r.reconcileEval(ctx, suite)
			if rerr != nil || phase != phasePending {
				t.Fatalf("reconcileEval = (%q, %q, %v), want Pending", phase, reason, rerr)
			}
			if !strings.Contains(reason, "eval identity is not bound") || !strings.Contains(reason, c.want) {
				t.Errorf("Pending reason %q does not say the identity is unbound and why", reason)
			}
		})
	}
}

// TestEvalRunWaitsForTheAccount: a bound condition does not cover an account
// deleted since the pass that wrote it. A Workflow naming an absent
// ServiceAccount creates no pod, including the exit handler that would report
// it, so the run is held and the account named.
func TestEvalRunWaitsForTheAccount(t *testing.T) {
	ctx := context.Background()
	suite, platform, fleet := evalFixtures()
	markEvalIdentityBound(platform)
	c := fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithObjects(publishedGateway(platform, "chat")).Build()
	r := &EvalReconciler{Client: c, RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}

	err := r.ensureArgoWorkflow(ctx, suite, platform, fleet)
	if !errors.Is(err, errEvalIdentityMissing) || !strings.Contains(err.Error(), "eval ServiceAccount eval-runner/eval-analytics does not exist") {
		t.Fatalf("ensureArgoWorkflow = %v, want errEvalIdentityMissing naming the account", err)
	}
	assertNoEvalWorkflow(ctx, t, c, evalWorkflowName(suite))
}

// TestEvalRunSurfacesAnIdentityLookupFailure: an API error on the lookup is a
// retry, not a Pending — Pending would report the identity missing when it was
// never checked.
func TestEvalRunSurfacesAnIdentityLookupFailure(t *testing.T) {
	suite, platform, fleet := evalFixtures()
	markEvalIdentityBound(platform)
	boom := errors.New("boom")
	c := fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithObjects(publishedGateway(platform, "chat")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ServiceAccount); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &EvalReconciler{Client: c, RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}

	err := r.ensureArgoWorkflow(context.Background(), suite, platform, fleet)
	if !errors.Is(err, boom) || errors.Is(err, errEvalIdentityMissing) {
		t.Fatalf("ensureArgoWorkflow = %v, want the lookup error, not errEvalIdentityMissing", err)
	}
	if !strings.Contains(err.Error(), "get eval ServiceAccount eval-runner/eval-analytics") {
		t.Errorf("error %q does not name the lookup", err)
	}
}

// TestEachPlatformsRunsHoldItsOwnIdentity: two Platforms' suites submit under
// two different ServiceAccounts. One shared account is one shared role, which
// reaches every Platform's prefixes in the reports bucket.
func TestEachPlatformsRunsHoldItsOwnIdentity(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"analytics", "billing"} {
		suite, platform, fleet := evalFixtures()
		platform.Name = name
		suite.Spec.PlatformRef.Name = name
		fleet.Spec.PlatformRef.Name = name
		r := &EvalReconciler{RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}
		c := fake.NewClientBuilder().WithScheme(evalScheme(t)).
			WithObjects(publishedGateway(platform, "chat"), boundEvalIdentity(r, platform), platform).Build()
		r.Client = c
		if err := r.ensureArgoWorkflow(context.Background(), suite, platform, fleet); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wf := &unstructured.Unstructured{}
		wf.SetGroupVersionKind(schema.GroupVersionKind{Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: "Workflow"})
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "eval-runner", Name: evalWorkflowName(suite)}, wf); err != nil {
			t.Fatal(err)
		}
		sa, _, _ := unstructured.NestedString(wf.Object, "spec", "serviceAccountName")
		if other, dup := seen[sa]; dup {
			t.Errorf("Platforms %s and %s both run as %q", other, name, sa)
		}
		seen[sa] = name
		if sa != "eval-"+name {
			t.Errorf("%s runs as %q, want eval-%s", name, sa, name)
		}
	}
}
