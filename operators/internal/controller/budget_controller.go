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
	"sigs.k8s.io/controller-runtime/pkg/log"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// AthenaConfig carries the SSM-resolved cost-pipeline outputs the
// Budget reconciler needs to run its CUR rollup.
type AthenaConfig struct {
	Workgroup     string
	Database      string
	ResultsBucket string
	// CURTableName is the Glue table the CUR Crawler materialized. The
	// table name is the report-name with hyphens normalized to
	// underscores (e.g. "eks-agent-platform-dev" → "eks_agent_platform_dev").
	// Resolved from SSM /eks-agent-platform/<env>/cost-pipeline/cur_table_name.
	CURTableName string
}

// BudgetReconciler reconciles BudgetPolicy CRs. Its reconcile loop runs
// on a timer (re-queue every 1h in production, 5m in dev) rather than on
// every CR change. Each tick:
//   - queries Athena against the CUR for spend tagged with this Platform,
//   - reads CloudWatch Bedrock invocation metrics for in-flight spend,
//   - writes status.currentSpendUsd + status.percentOfBudget,
//   - emits a BudgetBreach event to the kill-switch EventBridge bus when
//     percentOfBudget >= 120 and KillSwitchEnabled is true,
//   - records an alert condition at the AlertThresholdsPercent breakpoints.
type BudgetReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Concurrency     int
	RequeueInterval time.Duration

	// AWS — wired by main.go. May be nil in envtest paths.
	Athena      awsclients.Athena
	CloudWatch  awsclients.CloudWatch
	EventBridge awsclients.EventBridge

	// SSM-resolved configuration.
	AthenaCfg              AthenaConfig
	KillSwitchEventBusName string
}

// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=budgetpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=budgetpolicies/status,verbs=get;update;patch

// Reconcile reads spend signals and updates the BudgetPolicy status,
// firing the kill-switch on breach.
func (r *BudgetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("budget", req.NamespacedName)

	var bp governancev1alpha1.BudgetPolicy
	if err := r.Get(ctx, req.NamespacedName, &bp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	reading, err := r.reconcileBudget(ctx, &bp)
	if err != nil {
		// Athena/CloudWatch hiccups: log + retry on the next tick rather
		// than burn the workqueue with rapid backoffs (this reconciler is
		// already coarse-grained). errAthenaNotConfigured is handled
		// inside reconcileBudget itself (spendCUR falls back to 0); only
		// genuine errors reach here.
		logger.Error(err, "budget reconcile failed; will retry on next tick")
		if statusErr := r.applyBudgetStatusError(ctx, &bp, "ReconcileFailed", err); statusErr != nil {
			logger.Error(statusErr, "failed to record reconcile-error condition")
		}
		return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
	}

	if err := r.applyBudgetStatus(ctx, &bp, reading); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}

	logger.Info("reconcile complete",
		"spendUsd", reading.spendUsd,
		"pct", reading.pct,
		"alertCrossed", reading.alertThreshold,
		"killSwitchFired", reading.killSwitchFired,
	)
	return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *BudgetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.BudgetPolicy{}).
		Named("budget").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
