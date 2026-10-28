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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestEvalRunWaitsForThePlatformsEvalIdentity: a run submitted under a
// ServiceAccount that does not exist creates no pod, including the exit handler
// that would report it, so the reconciler holds the run until the Platform's
// eval identity is in place and says why.
func TestEvalRunWaitsForThePlatformsEvalIdentity(t *testing.T) {
	ctx := context.Background()
	suite, platform, fleet := evalFixtures()
	platform.Status.Phase = phaseReady
	fleet.Status.Phase = phaseReady
	c := fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithObjects(platform, fleet, publishedGateway(platform, "chat")).Build()
	r := &EvalReconciler{Client: c, RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}

	err := r.ensureArgoWorkflow(ctx, suite, platform, fleet)
	if !errors.Is(err, errEvalIdentityMissing) {
		t.Fatalf("ensureArgoWorkflow = %v, want errEvalIdentityMissing", err)
	}
	if !strings.Contains(err.Error(), "eval-runner/eval-analytics") {
		t.Errorf("error %q does not name the ServiceAccount it is waiting on", err)
	}

	wf := &unstructured.Unstructured{}
	wf.SetGroupVersionKind(schema.GroupVersionKind{Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: "Workflow"})
	if getErr := c.Get(ctx, client.ObjectKey{Namespace: "eval-runner", Name: evalWorkflowName(suite)}, wf); !apierrors.IsNotFound(getErr) {
		t.Errorf("a Workflow was submitted with no identity to run under (get err=%v)", getErr)
	}

	phase, reason, rerr := r.reconcileEval(ctx, suite)
	if rerr != nil || phase != phasePending {
		t.Fatalf("reconcileEval = (%q, %q, %v), want Pending", phase, reason, rerr)
	}
	if !strings.Contains(reason, "eval ServiceAccount") || !strings.Contains(reason, "eval-analytics") {
		t.Errorf("Pending reason %q does not say which identity is missing", reason)
	}
}

// TestEvalRunSurfacesAnIdentityLookupFailure: an API error on the lookup is a
// retry, not a Pending — Pending would report the identity missing when it was
// never checked.
func TestEvalRunSurfacesAnIdentityLookupFailure(t *testing.T) {
	suite, platform, fleet := evalFixtures()
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
			WithObjects(publishedGateway(platform, "chat"), evalIdentityAccount(r, platform)).Build()
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
