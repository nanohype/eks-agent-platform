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
)

// SandboxPoolReconciler reconciles SandboxPool CRs into a Deployment of
// Managed Agents self-hosted sandbox workers plus a default-deny
// NetworkPolicy. Worker pods land on the dedicated, tainted sandbox node
// pool. The reconciler is k8s-only — it makes no AWS or Anthropic API
// calls.
type SandboxPoolReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int
	// ShimImage is the operator image run as the KEDA metrics bridge for
	// queue-depth autoscaling. Empty disables autoscaling — workers run at
	// a static replica count.
	ShimImage string
}

// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=sandboxpools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=sandboxpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=sandboxpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a SandboxPool CR toward its desired state.
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("sandboxpool", req.NamespacedName)

	var pool agentsv1alpha1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !pool.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&pool, sandboxPoolFinalizer) {
			platform, perr := r.resolveSandboxPlatform(ctx, &pool)
			if perr != nil && platform == nil {
				logger.Info("platform gone; skipping sandbox pool cleanup")
			} else if perr == nil {
				if err := r.cleanupSandboxResources(ctx, &pool, platform); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&pool, sandboxPoolFinalizer)
			if err := r.Update(ctx, &pool); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&pool, sandboxPoolFinalizer) {
		controllerutil.AddFinalizer(&pool, sandboxPoolFinalizer)
		if err := r.Update(ctx, &pool); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	phase, readyWorkers, err := r.reconcileSandboxPoolSelf(ctx, &pool)
	if err != nil {
		logger.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if err := r.applySandboxPoolStatus(ctx, &pool, phase, readyWorkers); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	if phase == phasePending {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *SandboxPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.SandboxPool{}).
		Named("sandboxpool").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
