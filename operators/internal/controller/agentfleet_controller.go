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

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
)

// AgentFleetReconciler reconciles AgentFleet CRs into kagent Agent +
// ModelConfig CRs, a KEDA ScaledObject (when scaling.enabled), a per-
// fleet NetworkPolicy locking egress to agentgateway + OTel only, and a
// tenant ServiceAccount with the IRSA annotation pointing at the
// Platform's IAM role minted by PlatformReconciler.
//
// kagent + KEDA absence is tolerated — both Pending out via status, no
// reconcile error.
type AgentFleetReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int
}

// +kubebuilder:rbac:groups=agents.stxkxs.io,resources=agentfleets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.stxkxs.io,resources=agentfleets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.stxkxs.io,resources=agentfleets/finalizers,verbs=update
// +kubebuilder:rbac:groups=kagent.dev,resources=agents;modelconfigs;toolservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects;triggerauthentications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives an AgentFleet CR toward its desired state.
func (r *AgentFleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentfleet", req.NamespacedName)

	var fleet agentsv1alpha1.AgentFleet
	if err := r.Get(ctx, req.NamespacedName, &fleet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !fleet.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&fleet, agentFleetFinalizer) {
			platform, perr := r.resolvePlatform(ctx, &fleet)
			if perr != nil && platform == nil {
				logger.Info("platform gone; skipping fleet cleanup")
			} else if perr == nil {
				if err := r.cleanupFleetResources(ctx, &fleet, platform); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&fleet, agentFleetFinalizer)
			if err := r.Update(ctx, &fleet); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&fleet, agentFleetFinalizer) {
		controllerutil.AddFinalizer(&fleet, agentFleetFinalizer)
		if err := r.Update(ctx, &fleet); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	phase, readyAgents, err := r.reconcileFleetSelf(ctx, &fleet)
	if err != nil {
		logger.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if err := r.applyFleetStatus(ctx, &fleet, phase, readyAgents); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	if phase == phasePending {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *AgentFleetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentFleet{}).
		Named("agentfleet").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
