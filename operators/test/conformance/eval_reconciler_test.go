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
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

func newEvalReconciler() *controller.EvalReconciler {
	return &controller.EvalReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
	}
}

func reconcileEval(ctx context.Context, t *testing.T, suite *governancev1alpha1.EvalSuite) {
	t.Helper()
	r := newEvalReconciler()
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: suite.Name, Namespace: suite.Namespace}})
		if err != nil {
			t.Fatalf("eval reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func TestEvalReconciler_PendingWhenPlatformMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	suite := &governancev1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: governancev1alpha1.EvalSuiteSpec{
			PlatformRef:   commonv1alpha1.LocalRef{Name: "no-such-platform"},
			AgentFleetRef: commonv1alpha1.LocalRef{Name: "no-such-fleet"},
			PassThreshold: "0.85",
			Schedule:      "0 6 * * *",
			Cases: []governancev1alpha1.EvalCase{
				{Name: "smoke", Input: "ping", ExpectContains: []string{"pong"}},
			},
		},
	}
	mustCreate(ctx, t, suite)
	reconcileEval(ctx, t, suite)

	var got governancev1alpha1.EvalSuite
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: suite.Name, Namespace: suite.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (PlatformRef dangles)", got.Status.Phase)
	}
}

func TestEvalReconciler_PendingWhenArgoMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	// Force a Ready Platform + Ready AgentFleet so the only thing the
	// reconciler can be Pending on is Argo Workflows CRDs not installed.
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

	fName := uniqueName(t, "fleet")
	fleet := &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: fName, Namespace: testNs},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: pName},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "primary", ModelRoute: "primary", SystemPrompt: "be brief"},
			},
		},
	}
	mustCreate(ctx, t, fleet)
	fleet.Status.Phase = phaseReady
	if err := k8sClient.Status().Update(ctx, fleet); err != nil {
		t.Fatalf("force fleet Ready: %v", err)
	}

	suite := &governancev1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: governancev1alpha1.EvalSuiteSpec{
			PlatformRef:   commonv1alpha1.LocalRef{Name: pName},
			AgentFleetRef: commonv1alpha1.LocalRef{Name: fName},
			PassThreshold: "0.85",
			Schedule:      "0 6 * * *",
			Cases: []governancev1alpha1.EvalCase{
				{Name: "smoke", Input: "ping", ExpectContains: []string{"pong"}},
			},
		},
	}
	mustCreate(ctx, t, suite)
	reconcileEval(ctx, t, suite)

	var got governancev1alpha1.EvalSuite
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: suite.Name, Namespace: suite.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// envtest has no argoproj.io CRDs installed → reconciler surfaces
	// Pending and tolerates the NoKindMatch.
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (Argo CRD not installed in envtest)", got.Status.Phase)
	}
}
