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

const agentSandboxFinalizer = "agents.nanohype.dev/agentsandbox-finalizer"

// AgentSandboxReconciler reconciles AgentSandbox CRs into one hardened,
// single-use session pod plus a default-deny NetworkPolicy in the
// Platform's tenant namespace. Worker code is pushed in via the spec (image,
// command, env); the reconciler is k8s-only — it makes no AWS or Anthropic
// API calls.
type AgentSandboxReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int
}

// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=agentsandboxes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=agentsandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=agentsandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives an AgentSandbox CR toward its desired state.
func (r *AgentSandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentsandbox", req.NamespacedName)

	var box agentsv1alpha1.AgentSandbox
	if err := r.Get(ctx, req.NamespacedName, &box); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !box.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&box, agentSandboxFinalizer) {
			platform, perr := r.resolveAgentSandboxPlatform(ctx, &box)
			if perr != nil && platform == nil {
				logger.Info("platform gone; skipping agent sandbox cleanup")
			} else if perr == nil {
				if err := r.cleanupAgentSandbox(ctx, &box, platform); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&box, agentSandboxFinalizer)
			if err := r.Update(ctx, &box); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&box, agentSandboxFinalizer) {
		controllerutil.AddFinalizer(&box, agentSandboxFinalizer)
		if err := r.Update(ctx, &box); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	phase, pod, err := r.reconcileAgentSandboxSelf(ctx, &box)
	if err != nil {
		logger.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if err := r.applyAgentSandboxStatus(ctx, &box, phase, pod); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	// Terminal: garbage-collect the AgentSandbox after the TTL.
	if phase == phaseSucceeded || phase == phaseFailed {
		requeue, err := r.reconcileTTL(ctx, &box)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	// In-flight or suspended: poll for the pod / Platform state to change.
	if phase == phasePending || phase == phaseRunning || phase == phaseSuspended {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *AgentSandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentSandbox{}).
		Named("agentsandbox").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
