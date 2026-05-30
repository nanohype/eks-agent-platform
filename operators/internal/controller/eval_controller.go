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

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

// EvalReconciler reconciles EvalSuite CRs into Argo Workflows
// CronWorkflow (when spec.schedule is set) or one-shot Workflows
// (manual trigger). Each Workflow runs the eval cases against the
// referenced AgentFleet, uploads an HTML report to the eval-reports S3
// bucket, writes mean-score back to EvalSuite.status.lastScore, and
// exposes a Prometheus metric the Argo Rollouts AnalysisTemplate reads
// to gate promotion.
type EvalReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int

	// RunnerNamespace overrides the default eval-runner namespace used as
	// the destination for emitted Workflow / CronWorkflow CRs. Resolved
	// from SSM /eks-agent-platform/<env>/eval-runtime/runner_namespace.
	// Empty falls back to the package default.
	RunnerNamespace string
}

// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=evalsuites,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=evalsuites/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=evalsuites/finalizers,verbs=update
// +kubebuilder:rbac:groups=argoproj.io,resources=workflows;workflowtemplates;cronworkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=argoproj.io,resources=analysisruns;analysistemplates,verbs=get;list;watch

// Reconcile drives an EvalSuite CR into a CronWorkflow (scheduled) or Workflow (manual).
func (r *EvalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("evalsuite", req.NamespacedName)

	var suite governancev1alpha1.EvalSuite
	if err := r.Get(ctx, req.NamespacedName, &suite); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !suite.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&suite, evalFinalizer) {
			if err := r.cleanupArgoWorkflow(ctx, &suite); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&suite, evalFinalizer)
			if err := r.Update(ctx, &suite); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&suite, evalFinalizer) {
		controllerutil.AddFinalizer(&suite, evalFinalizer)
		if err := r.Update(ctx, &suite); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	phase, err := r.reconcileEval(ctx, &suite)
	if err != nil {
		logger.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if err := r.applyEvalStatus(ctx, &suite, phase); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	if phase == phasePending {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *EvalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.EvalSuite{}).
		Named("eval").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
