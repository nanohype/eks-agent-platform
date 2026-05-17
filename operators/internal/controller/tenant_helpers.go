/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"
	"math/big"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// parseDecimal accepts a CRD decimal-string field and returns a big.Float.
// Used by the tenant reconciler to roll up per-Platform CurrentSpendUsd +
// MonthlyUsd without precision loss. Empty / malformed inputs return
// (nil, false) so callers can skip them.
func parseDecimal(s string) (*big.Float, bool) {
	if s == "" {
		return nil, false
	}
	v, _, err := big.ParseFloat(s, 10, 64, big.ToNearestEven)
	if err != nil {
		return nil, false
	}
	return v, true
}

// metav1Now returns the current time wrapped in metav1.Time. Trivial
// wrapper so tests can stub if they ever need to.
func metav1Now() metav1.Time {
	return metav1.Now()
}

// conditionForReading reports the tenant's overall aggregation health.
// Type=Aggregated, status mirrors whether the tenant has at least one
// Platform AND none are suspended.
func conditionForReading(reading tenantReading) metav1.Condition {
	cond := metav1.Condition{
		Type:               "Aggregated",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("%d platform(s) — %d ready, %d suspended", reading.platformCount, reading.readyCount, reading.suspendedCount),
		LastTransitionTime: metav1Now(),
	}
	if reading.platformCount == 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NoPlatforms"
		cond.Message = "no Platforms found for this tenant"
	} else if reading.suspendedCount > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "SomeSuspended"
		cond.Message = fmt.Sprintf("%d/%d platform(s) suspended by kill-switch", reading.suspendedCount, reading.platformCount)
	}
	return cond
}

// conditionTenantOverBudget is the soft-violation indicator. The tenant
// reconciler does not enforce — each Platform's BudgetPolicy is the
// per-Platform kill-switch. This condition lights up persona dashboards.
func conditionTenantOverBudget(reading tenantReading) metav1.Condition {
	return metav1.Condition{
		Type:               "TenantBudgetExceeded",
		Status:             metav1.ConditionTrue,
		Reason:             "AggregateOverCap",
		Message:            fmt.Sprintf("aggregate spend %s usd exceeds tenant cap; %d%% of per-platform budget sum", reading.aggregateSpend, reading.pct),
		LastTransitionTime: metav1Now(),
	}
}

func conditionTenantUnderBudget() metav1.Condition {
	return metav1.Condition{
		Type:               "TenantBudgetExceeded",
		Status:             metav1.ConditionFalse,
		Reason:             "WithinCap",
		Message:            "aggregate spend within tenant cap",
		LastTransitionTime: metav1Now(),
	}
}
