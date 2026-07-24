/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// SLOReconciler reconciles SLOPolicy CRs. Like the Budget reconciler it runs on
// a timer rather than on CR changes, because the signal it reads lives outside
// Kubernetes. Each tick:
//   - queries Amazon Managed Prometheus for the SLI's error ratio over every
//     burn-rate window the observability-slo standard defines,
//   - computes the multi-window multi-burn-rate breach state per severity tier,
//   - writes the ratios, the normalized breach ratios, and the remaining error
//     budget to status,
//   - publishes a BurnRateBreach event to the kill-switch EventBridge bus when a
//     tier trips,
//   - holds the tenant's ArgoCD rollout on a page-tier breach, and releases the
//     hold once the burn clears.
//
// This reconciler is the platform's single evaluator for the objective. Its
// status is what kube-state-metrics projects, so the paging alert reads the
// number computed here instead of re-deriving the same PromQL against the same
// data — which is how the alert thresholds and the control loop are kept from
// drifting apart.
type SLOReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Concurrency     int
	RequeueInterval time.Duration

	// AWS — wired by main.go. Either may be nil in envtest paths, on a cluster
	// with no AMP workspace, or before managed-monitoring has applied.
	Prometheus  awsclients.PrometheusQuery
	EventBridge awsclients.EventBridge

	// KillSwitchEventBusName is the bus BurnRateBreach events are published to.
	// Empty means log-only: status still records the breach, and the hold still
	// engages, but nothing pages.
	KillSwitchEventBusName string

	// HoldGraceIntervals is how many RequeueIntervals an engaged hold may go
	// unobserved on the tenant's AppProject before the reconciler reports it as
	// not landing (default 2).
	HoldGraceIntervals int
}

// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=slopolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=slopolicies/status,verbs=get;update;patch

// Reconcile evaluates one SLOPolicy's burn rate and applies the resulting
// platform action.
func (r *SLOReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("slopolicy", req.NamespacedName)

	var sp governancev1alpha1.SLOPolicy
	if err := r.Get(ctx, req.NamespacedName, &sp); err != nil {
		if apierrors.IsNotFound(err) {
			// The objective is gone. Drop its series rather than leave a burn
			// rate frozen at its last value on every dashboard that reads it.
			forgetSLOPolicySeries(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	reading, err := r.reconcileSLO(ctx, &sp)
	if err != nil {
		// AMP hiccups and AppProject write failures retry on the next tick
		// rather than burning the workqueue with rapid backoffs — this
		// reconciler is already coarse-grained. errAMPNotConfigured is handled
		// inside reconcileSLO, so only genuine errors reach here.
		logger.Error(err, "slo reconcile failed; will retry on next tick")
		if statusErr := r.applySLOStatusError(ctx, &sp, "ReconcileFailed", err); statusErr != nil {
			logger.Error(statusErr, "failed to record reconcile-error condition")
		}
		return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
	}

	if err := r.applySLOStatus(ctx, &sp, reading); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}

	logger.Info("reconcile complete",
		"severity", reading.severity,
		"breachedWindow", reading.breachedWindow,
		"pageTierBreachRatio", reading.pageRatio,
		"ticketTierBreachRatio", reading.ticketRatio,
		"errorBudgetRemaining", reading.budgetRemaining,
		"breachPublished", reading.breachPublished,
		"holdEngaging", reading.holdEngaging,
		"holdReleasing", reading.holdReleasing,
		"holdObserved", reading.obs.present,
		"holdUnobserved", reading.holdUnobs,
	)
	return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *SLOReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.SLOPolicy{}).
		Named("slo").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
