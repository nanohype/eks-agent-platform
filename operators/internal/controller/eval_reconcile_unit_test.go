/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func evalScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		platformv1alpha1.AddToScheme, agentsv1alpha1.AddToScheme, governancev1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func evalSuite() *governancev1alpha1.EvalSuite {
	return &governancev1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly-eval", Namespace: ctrlTestNS},
		Spec: governancev1alpha1.EvalSuiteSpec{
			PlatformRef:   commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			AgentFleetRef: commonv1alpha1.LocalRef{Name: "squad"},
			PassThreshold: "0.85",
		},
	}
}

func TestEvalRunnerNamespaceAndWorkflowName(t *testing.T) {
	if got := (&EvalReconciler{}).evalRunnerNamespace(); got != defaultEvalRunnerNamespace {
		t.Errorf("default runner namespace: got %q", got)
	}
	if got := (&EvalReconciler{RunnerNamespace: "evals"}).evalRunnerNamespace(); got != "evals" {
		t.Errorf("override runner namespace: got %q", got)
	}
	if got := evalWorkflowName(evalSuite()); got != "acme-nightly-eval" {
		t.Errorf("workflow name: got %q want acme-nightly-eval", got)
	}
}

func TestResolveEvalRefs(t *testing.T) {
	s := evalScheme(t)

	t.Run("missing platform", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &EvalReconciler{Client: cl, Scheme: s}
		if _, _, err := r.resolveEvalRefs(context.Background(), evalSuite()); !errors.Is(err, errEvalPlatformNotFound) {
			t.Fatalf("missing platform: got %v", err)
		}
	})
	t.Run("missing fleet", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &EvalReconciler{Client: cl, Scheme: s}
		if _, _, err := r.resolveEvalRefs(context.Background(), evalSuite()); !errors.Is(err, errEvalFleetNotFound) {
			t.Fatalf("missing fleet: got %v", err)
		}
	})
	t.Run("both present", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		fleet := agentFleet()
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, fleet).Build()
		r := &EvalReconciler{Client: cl, Scheme: s}
		gotP, gotF, err := r.resolveEvalRefs(context.Background(), evalSuite())
		if err != nil || gotP.Name != ctrlTestPlatform || gotF.Name != "squad" {
			t.Fatalf("both present: got (%v, %v, %v)", gotP, gotF, err)
		}
	})
}

func TestApplyEvalStatusEmitsScoreGauge(t *testing.T) {
	ctx := context.Background()
	s := evalScheme(t)
	suite := evalSuite()
	suite.Status.LastScore = "0.91"
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(suite).WithStatusSubresource(suite).Build()
	r := &EvalReconciler{Client: cl, Scheme: s}
	if err := r.applyEvalStatus(ctx, suite, phaseReady); err != nil {
		t.Fatalf("applyEvalStatus: %v", err)
	}
	g := evalSuiteScore.WithLabelValues(suite.Namespace, suite.Spec.PlatformRef.Name, suite.Name)
	if got := testutil.ToFloat64(g); got < 0.9099 || got > 0.9101 {
		t.Errorf("agents_eval_suite_score = %v; want ~0.91", got)
	}
	evalSuiteScore.DeleteLabelValues(suite.Namespace, suite.Spec.PlatformRef.Name, suite.Name)
}

func TestReconcileEval_PendingUntilBothReady(t *testing.T) {
	s := evalScheme(t)

	// Missing refs → pending.
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &EvalReconciler{Client: cl, Scheme: s}
	if phase, err := r.reconcileEval(context.Background(), evalSuite()); err != nil || phase != phasePending {
		t.Fatalf("missing refs: got (%q, %v)", phase, err)
	}

	// Platform Ready but fleet not Ready → still pending.
	p := readyPlatformIn()
	fleet := agentFleet() // no Ready status
	cl2 := fake.NewClientBuilder().WithScheme(s).WithObjects(p, fleet).Build()
	r2 := &EvalReconciler{Client: cl2, Scheme: s}
	if phase, err := r2.reconcileEval(context.Background(), evalSuite()); err != nil || phase != phasePending {
		t.Fatalf("fleet not ready: got (%q, %v)", phase, err)
	}
}
