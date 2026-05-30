/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"math/big"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// tenantSpecField is the indexer key registered on Platform so the
// Tenant reconciler can list only the Platforms owned by a given
// Tenant.Name without paying for a cluster-wide scan + post-filter on
// every reconcile tick.
const tenantSpecField = "spec.tenant"

// TenantReconciler reconciles Tenant CRs by aggregating state across the
// Platforms (and their BudgetPolicies) whose spec.tenant references this
// Tenant.metadata.name. Tenant is cluster-scoped; its owned Platforms can
// be in any namespace.
//
// The reconciler does not provision any resources — it's a roll-up. The
// per-Platform IRSA / KMS / S3 / kill-switch state lives on the Platform
// reconciler.
type TenantReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Concurrency     int
	RequeueInterval time.Duration
}

// +kubebuilder:rbac:groups=platform.nanohype.dev,resources=tenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=platform.nanohype.dev,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.nanohype.dev,resources=platforms,verbs=get;list;watch
// +kubebuilder:rbac:groups=governance.nanohype.dev,resources=budgetpolicies,verbs=get;list;watch

// Reconcile re-aggregates the owned Platforms + BudgetPolicies and
// writes the rolled-up status. Always re-queues on a periodic tick;
// Platform / BudgetPolicy changes don't trigger us directly (would
// require a watch wired in SetupWithManager which is overkill for a
// cosmetic roll-up).
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("tenant", req.NamespacedName)

	var tenant platformv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	reading, err := r.aggregate(ctx, &tenant)
	if err != nil {
		logger.Error(err, "tenant aggregate failed; will retry on next tick")
		return ctrl.Result{RequeueAfter: r.requeue()}, nil
	}
	if err := r.applyStatus(ctx, &tenant, reading); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	logger.Info("reconcile complete",
		"platforms", reading.platformCount,
		"ready", reading.readyCount,
		"suspended", reading.suspendedCount,
		"spend", reading.aggregateSpend,
		"pct", reading.pct,
	)
	return ctrl.Result{RequeueAfter: r.requeue()}, nil
}

// tenantReading is the computed roll-up.
type tenantReading struct {
	platformCount   int32
	readyCount      int32
	suspendedCount  int32
	aggregateSpend  string
	aggregateBudget string
	pct             int32
	overSpec        bool // aggregate spend > spec.aggregateMonthlyBudgetUsd
}

// aggregate walks every Platform whose spec.tenant matches this Tenant
// and rolls up the readiness + budget state. Cluster-scoped list with a
// post-filter; for a cluster with thousands of Platforms an indexer would
// reduce CPU but this stays simple until that's the bottleneck.
func (r *TenantReconciler) aggregate(ctx context.Context, t *platformv1alpha1.Tenant) (tenantReading, error) {
	// Cluster-wide list + post-filter on spec.tenant. The manager
	// registers a field indexer at SetupWithManager which kicks in
	// for cache-backed clients in production; the conformance tests
	// use a direct-to-apiserver client that ignores field selectors
	// for arbitrary CRD fields, so the indexer is a no-op there. Keep
	// the in-memory filter as the load-bearing implementation; the
	// indexer is a future optimization for very-large multi-tenant
	// clusters (1000+ Platforms).
	var platforms platformv1alpha1.PlatformList
	if err := r.List(ctx, &platforms); err != nil {
		return tenantReading{}, fmt.Errorf("list platforms: %w", err)
	}

	reading := tenantReading{}
	totalSpend := new(big.Float).SetPrec(64)
	totalBudget := new(big.Float).SetPrec(64)

	for i := range platforms.Items {
		p := &platforms.Items[i]
		if p.Spec.Tenant != t.Name {
			continue
		}
		reading.platformCount++
		switch p.Status.Phase {
		case phaseReady:
			reading.readyCount++
		case phaseSuspended:
			reading.suspendedCount++
		}
		// Each Platform may reference a BudgetPolicy by name within the
		// same namespace. We aggregate every BudgetPolicy referenced so
		// the tenant sees totals across Platforms even when each has its
		// own budget cap.
		if p.Spec.Budget.Name == "" {
			continue
		}
		var bp governancev1alpha1.BudgetPolicy
		if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.Budget.Name}, &bp); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return tenantReading{}, fmt.Errorf("get budget %s/%s: %w", p.Namespace, p.Spec.Budget.Name, err)
			}
			continue
		}
		if v, ok := parseDecimal(bp.Status.CurrentSpendUsd); ok {
			totalSpend.Add(totalSpend, v)
		}
		if v, ok := parseDecimal(bp.Spec.MonthlyUsd); ok {
			totalBudget.Add(totalBudget, v)
		}
	}

	reading.aggregateSpend = totalSpend.Text('f', 6)
	reading.aggregateBudget = totalBudget.Text('f', 6)

	// pct = aggregateSpend / aggregateBudget * 100, clamped.
	if totalBudget.Sign() > 0 {
		ratio := new(big.Float).SetPrec(64).Quo(totalSpend, totalBudget)
		ratio.Mul(ratio, big.NewFloat(100))
		f, _ := ratio.Float64()
		if f < 0 {
			f = 0
		}
		const maxPct = float64(2_000_000_000)
		if f > maxPct {
			f = maxPct
		}
		reading.pct = int32(f + 0.5)
	}

	// Compare aggregate to the tenant-spec cap (if set).
	if cap, ok := parseDecimal(t.Spec.AggregateMonthlyBudgetUsd); ok && cap.Sign() > 0 {
		if totalSpend.Cmp(cap) > 0 {
			reading.overSpec = true
		}
	}
	return reading, nil
}

// applyStatus writes the roll-up + the standard conditions, re-fetching
// the Tenant on conflict so concurrent reconciles don't fight over a
// stale ResourceVersion.
func (r *TenantReconciler) applyStatus(ctx context.Context, t *platformv1alpha1.Tenant, reading tenantReading) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch on each attempt so we never write against a stale
		// ResourceVersion. The first attempt could use the passed-in
		// object, but the indirection cost is negligible and the
		// invariant ('always write against the latest object') is
		// easier to reason about.
		var fresh platformv1alpha1.Tenant
		if err := r.Get(ctx, types.NamespacedName{Name: t.Name}, &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // deleted mid-reconcile; nothing to write
			}
			return err
		}
		fresh.Status.PlatformCount = reading.platformCount
		fresh.Status.ReadyPlatformCount = reading.readyCount
		fresh.Status.SuspendedPlatformCount = reading.suspendedCount
		fresh.Status.AggregateSpendUsd = reading.aggregateSpend
		fresh.Status.AggregateBudgetUsd = reading.aggregateBudget
		fresh.Status.PercentOfBudget = reading.pct
		now := metav1Now()
		fresh.Status.LastReconciled = &now

		switch {
		case reading.platformCount == 0:
			fresh.Status.Phase = phasePending
		case reading.suspendedCount > 0:
			fresh.Status.Phase = phaseSuspended
		case reading.readyCount == reading.platformCount:
			fresh.Status.Phase = "Active"
		default:
			fresh.Status.Phase = phaseProvisioning
		}

		upsertCondition(&fresh.Status.Conditions, conditionForReading(reading))
		if reading.overSpec {
			upsertCondition(&fresh.Status.Conditions, conditionTenantOverBudget(reading))
		} else {
			upsertCondition(&fresh.Status.Conditions, conditionTenantUnderBudget())
		}
		return r.Status().Update(ctx, &fresh)
	})
}

func (r *TenantReconciler) requeue() time.Duration {
	if r.RequeueInterval <= 0 {
		return 5 * time.Minute
	}
	return r.RequeueInterval
}

// SetupWithManager registers:
//   - the field indexer on Platform.spec.tenant (so MatchingFields lists
//     stay O(owned platforms) instead of O(cluster platforms)),
//   - Watches on Platform + BudgetPolicy that map back to the owning
//     Tenant via spec.tenant / spec.platformRef → spec.tenant. Without
//     these the tenant roll-up could lag up to RequeueInterval (5m)
//     behind a kill-switch fire or a Platform-becoming-Ready event,
//     which makes the persona dashboards visibly stale on incident.
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &platformv1alpha1.Platform{}, tenantSpecField, func(obj client.Object) []string {
		p, ok := obj.(*platformv1alpha1.Platform)
		if !ok || p.Spec.Tenant == "" {
			return nil
		}
		return []string{p.Spec.Tenant}
	}); err != nil {
		return fmt.Errorf("index platforms by spec.tenant: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Tenant{}).
		Watches(&platformv1alpha1.Platform{}, handler.EnqueueRequestsFromMapFunc(r.platformToTenant), builder.WithPredicates()).
		Watches(&governancev1alpha1.BudgetPolicy{}, handler.EnqueueRequestsFromMapFunc(r.budgetToTenant), builder.WithPredicates()).
		Named("tenant").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}

// platformToTenant maps a Platform change to a Tenant reconcile request
// on its owning Tenant (Platform.spec.tenant). Empty tenant → no enqueue.
func (r *TenantReconciler) platformToTenant(_ context.Context, obj client.Object) []reconcile.Request {
	p, ok := obj.(*platformv1alpha1.Platform)
	if !ok || p.Spec.Tenant == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: p.Spec.Tenant}}}
}

// budgetToTenant maps a BudgetPolicy change to the owning Tenant by
// looking up its referenced Platform first. Two lookups per budget
// change is acceptable — budget reconciles are infrequent (hourly+).
func (r *TenantReconciler) budgetToTenant(ctx context.Context, obj client.Object) []reconcile.Request {
	bp, ok := obj.(*governancev1alpha1.BudgetPolicy)
	if !ok || bp.Spec.PlatformRef.Name == "" {
		return nil
	}
	var p platformv1alpha1.Platform
	if err := r.Get(ctx, types.NamespacedName{Namespace: bp.Namespace, Name: bp.Spec.PlatformRef.Name}, &p); err != nil {
		return nil
	}
	if p.Spec.Tenant == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: p.Spec.Tenant}}}
}
