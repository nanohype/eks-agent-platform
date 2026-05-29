/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// BatchJobReconciler reconciles BatchJob CRs into Amazon Bedrock batch
// model-invocation jobs.
//
// Unlike the EvalSuite reconciler — which schedules an in-cluster Argo
// Workflow because the eval cases run as pods — a Bedrock batch job runs
// server-side, so there is nothing to schedule in-cluster. This reconciler
// drives the job directly via the AWS SDK (the BudgetPolicy pattern):
// submit once, then poll on a RequeueAfter tick until the job reaches a
// terminal state, copying the output location + record counts into status.
// It carries a finalizer so an in-flight job is stopped if the CR is
// deleted mid-run.
type BatchJobReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Concurrency  int
	PollInterval time.Duration

	// Bedrock — wired by main.go. Nil in envtest/dev paths; the reconciler
	// surfaces Pending and never submits when nil.
	Bedrock awsclients.Bedrock

	// ServiceRoleARN is the Bedrock batch service role (SSM-resolved from
	// terraform/components/batch-runtime). The job's own data-plane identity
	// — distinct from the operator IRSA that submits it.
	ServiceRoleARN string
}

// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=batchjobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=batchjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=batchjobs/finalizers,verbs=update

// Reconcile submits the Bedrock job once, then polls it to terminal state.
func (r *BatchJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("batchjob", req.NamespacedName)

	var bj agentsv1alpha1.BatchJob
	if err := r.Get(ctx, req.NamespacedName, &bj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !bj.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&bj, batchFinalizer) {
			r.stopBatchJob(ctx, &bj)
			controllerutil.RemoveFinalizer(&bj, batchFinalizer)
			if err := r.Update(ctx, &bj); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&bj, batchFinalizer) {
		controllerutil.AddFinalizer(&bj, batchFinalizer)
		if err := r.Update(ctx, &bj); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	phase, err := r.reconcileBatch(ctx, &bj)
	if err != nil {
		logger.Error(err, "batch reconcile failed; will retry on next tick")
		if statusErr := r.applyBatchStatusError(ctx, &bj, "ReconcileFailed", err); statusErr != nil {
			logger.Error(statusErr, "failed to record reconcile-error condition")
		}
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
	if err := r.applyBatchStatus(ctx, &bj, phase); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	if isTerminalBatchPhase(phase) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
}

func (r *BatchJobReconciler) pollInterval() time.Duration {
	if r.PollInterval <= 0 {
		return 2 * time.Minute
	}
	return r.PollInterval
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *BatchJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.BatchJob{}).
		Named("batch").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
