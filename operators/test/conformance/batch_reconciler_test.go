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
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

func newBatchReconciler() *controller.BatchJobReconciler {
	return &controller.BatchJobReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
		// Bedrock intentionally nil — the degrade-to-Pending path (no AWS
		// client, never submits) is what we're verifying.
	}
}

func reconcileBatch(ctx context.Context, t *testing.T, bj *agentsv1alpha1.BatchJob) {
	t.Helper()
	r := newBatchReconciler()
	// First reconcile adds the finalizer (RequeueAfter), the next runs the
	// substantive body. Loop until the reconciler stops asking to requeue or
	// we hit the cap (Pending requeues on the poll interval, so the cap is
	// the loop's real exit there).
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: bj.Name, Namespace: bj.Namespace}})
		if err != nil {
			t.Fatalf("batch reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func TestBatchReconciler_PendingWhenPlatformMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	bj := &agentsv1alpha1.BatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "bj"), Namespace: testNs},
		Spec: agentsv1alpha1.BatchJobSpec{
			PlatformRef:    commonv1alpha1.LocalRef{Name: "no-such-platform"},
			ModelID:        "anthropic.claude-3-5-sonnet-20241022-v2:0",
			InputS3Uri:     "s3://b/in.jsonl",
			OutputS3Prefix: "s3://b/out/",
			TimeoutHours:   24,
		},
	}
	mustCreate(ctx, t, bj)
	reconcileBatch(ctx, t, bj)

	var got agentsv1alpha1.BatchJob
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bj.Name, Namespace: bj.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (PlatformRef dangles)", got.Status.Phase)
	}
	if got.Status.JobArn != "" {
		t.Errorf("status.jobArn: got %q want empty (never submit without a Platform)", got.Status.JobArn)
	}
}

func TestBatchReconciler_PendingWhenNoBedrock(t *testing.T) {
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

	bj := &agentsv1alpha1.BatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "bj"), Namespace: testNs},
		Spec: agentsv1alpha1.BatchJobSpec{
			PlatformRef:    commonv1alpha1.LocalRef{Name: pName},
			ModelID:        "anthropic.claude-3-5-sonnet-20241022-v2:0",
			InputS3Uri:     "s3://b/in.jsonl",
			OutputS3Prefix: "s3://b/out/",
			TimeoutHours:   24,
		},
	}
	mustCreate(ctx, t, bj)
	reconcileBatch(ctx, t, bj)

	var got agentsv1alpha1.BatchJob
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bj.Name, Namespace: bj.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Platform is Ready but the Bedrock client is nil → the reconciler must
	// not submit; it surfaces Pending and leaves status.jobArn empty.
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (no Bedrock client)", got.Status.Phase)
	}
	if got.Status.JobArn != "" {
		t.Errorf("status.jobArn: got %q want empty (no submit without Bedrock)", got.Status.JobArn)
	}
}
