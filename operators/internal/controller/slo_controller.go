/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"sync"
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
	//
	// Prometheus is read through metricStore() rather than directly, because it
	// can arrive after startup: main.go builds it from an SSM parameter that
	// managed-monitoring writes, and managed-monitoring is applied AFTER the
	// cluster that hosts this operator. A nil client at boot was therefore
	// permanent — the endpoint is not a chart value, so no GitOps change
	// restarts the pod, and every SLOPolicy reported MetricStoreUnavailable
	// forever on a cluster whose AMP workspace existed.
	Prometheus  awsclients.PrometheusQuery
	EventBridge awsclients.EventBridge

	// ResolvePrometheus re-reads the AMP endpoint and builds a query client.
	// Set by main.go when the operator has AWS clients; nil in envtest and in
	// any path that has no SSM to read. metricStore() calls it at most once per
	// promResolveBackoff while Prometheus is nil.
	ResolvePrometheus func(ctx context.Context) (awsclients.PrometheusQuery, error)

	// promMu guards Prometheus and promRetryAfter. Reconciles run concurrently
	// (see Concurrency), so the late assignment is a write several goroutines
	// can race on.
	promMu         sync.RWMutex
	promRetryAfter time.Time

	// KillSwitchEventBusName is the bus BurnRateBreach events are published to.
	// Empty means log-only: status still records the breach, and the hold still
	// engages, but nothing pages.
	KillSwitchEventBusName string

	// HoldGraceIntervals is how many RequeueIntervals an engaged hold may go
	// unobserved on the tenant's AppProject before the reconciler reports it as
	// not landing (default 2).
	HoldGraceIntervals int
}

// promResolveBackoff is the shortest interval between two attempts to resolve a
// missing AMP client. Without it every tick of every SLOPolicy would read SSM on
// a cluster that legitimately has no AMP workspace — enable_managed_monitoring
// is opt-in, so "absent" is a normal steady state and must stay cheap.
const promResolveBackoff = 5 * time.Minute

// metricStore returns the AMP query client, resolving it if it is still nil and
// the backoff has elapsed.
//
// The late resolve exists because the endpoint arrives after this process
// starts. main.go reads it from an SSM parameter written by managed-monitoring,
// which is applied after the cluster that hosts the operator, so on a first
// install the client is nil at boot. Nothing later restarts the pod — the
// endpoint is not a chart value, so ArgoCD sees no manifest change when the
// parameter appears — and the reconciler reported MetricStoreUnavailable
// forever on a cluster whose workspace was up.
//
// Returning nil is still a normal answer, not an error: a cluster without
// managed-monitoring has no metric store, and the caller reports that as a
// condition rather than failing the reconcile.
func (r *SLOReconciler) metricStore(ctx context.Context) awsclients.PrometheusQuery {
	r.promMu.RLock()
	p := r.Prometheus
	r.promMu.RUnlock()
	if p != nil || r.ResolvePrometheus == nil {
		return p
	}

	r.promMu.Lock()
	defer r.promMu.Unlock()
	// Re-check: another goroutine may have resolved it while this one waited.
	if r.Prometheus != nil {
		return r.Prometheus
	}
	if time.Now().Before(r.promRetryAfter) {
		return nil
	}
	r.promRetryAfter = time.Now().Add(promResolveBackoff)

	resolved, err := r.ResolvePrometheus(ctx)
	if err != nil {
		log.FromContext(ctx).V(1).Info("could not resolve the AMP endpoint; SLO evaluation stays disabled until the next attempt",
			"error", err, "retryAfter", promResolveBackoff)
		return nil
	}
	if resolved == nil {
		return nil
	}
	log.FromContext(ctx).Info("resolved the AMP endpoint after startup; SLO burn-rate evaluation is now live")
	r.Prometheus = resolved
	return resolved
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
