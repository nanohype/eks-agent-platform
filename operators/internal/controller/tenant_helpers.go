/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"
	"math/big"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
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

// spendWrittenWithReading are the BudgetReconciled reasons applyBudgetStatus
// writes, all of which it writes on the same pass that assigns
// status.currentSpendUsd from that tick's reading. KillSwitchFired is False and
// is one of them: the switch firing is a verdict about the number, not a failure
// to obtain it.
//
// The other spelling of False comes from applyBudgetStatusError, which writes no
// spend at all — that is the one that means the leg did not answer.
var spendWrittenWithReading = map[string]bool{
	"Reconciled":       true,
	"ThresholdCrossed": true,
	"KillSwitchFired":  true,
}

// budgetLegReported reports whether a BudgetPolicy's spend is a reading from
// this tick rather than the last value it managed to write.
//
// HOLDS: a leg whose producer reports a failed reconcile is not counted, and one
// whose producer reports any verdict reached WITH a reading is. False alone does
// not mean unread — the kill-switch verdict is False about a number the same
// pass had just measured.
//
// DOES NOT HOLD: the set above is maintained against applyBudgetStatus by hand.
// A reason added there and not here would make a current leg look unread, which
// is the safe direction; one added to the error path and listed here would count
// a stale number, which is not. Nothing binds the two.
func budgetLegReported(bp *governancev1alpha1.BudgetPolicy) bool {
	for _, c := range bp.Status.Conditions {
		if c.Type != "BudgetReconciled" {
			continue
		}
		return c.Status != metav1.ConditionFalse || spendWrittenWithReading[c.Reason]
	}
	return true
}

// conditionTenantBudget picks which of the three sentences this tick earned.
// "Within cap" is a comparison, and spec.aggregateMonthlyBudgetUsd is optional:
// a tenant that declares no cap has nothing to be within, and saying so is not
// the same as saying the spend is fine.
func conditionTenantBudget(reading tenantReading) metav1.Condition {
	switch {
	case !reading.capCompared:
		// A tenant that declares no cap was read, not unread:
		// spec.aggregateMonthlyBudgetUsd is optional, and its absence is a
		// definite answer the reconciler holds in hand. False with a Reason of
		// its own is what the rest of this controller uses for a genuine
		// nothing-to-report; Unknown is for a question that could not be asked,
		// and this one was.
		return metav1.Condition{
			Type:               "TenantBudgetExceeded",
			Status:             metav1.ConditionFalse,
			Reason:             "NoAggregateCap",
			Message:            "this tenant declares no aggregate monthly cap, so there is none to be within",
			LastTransitionTime: metav1Now(),
		}
	case reading.overSpec:
		// A partial sum that already exceeds the cap is a finding whatever the
		// missing legs would have added, so this arm does not care about
		// completeness.
		return conditionTenantOverBudget(reading)
	case !reading.spendComplete:
		// The comparison ran against a total that is not the total: a Platform's
		// BudgetPolicy reported no spend, or something unparseable, and that leg
		// was skipped. Under-cap computed from an understated sum is the
		// positive claim this condition must not make from a reading that did
		// not finish.
		return metav1.Condition{
			Type:               "TenantBudgetExceeded",
			Status:             metav1.ConditionUnknown,
			Reason:             "SpendIncomplete",
			Message:            "at least one platform's budget reported no readable spend, so the aggregate below understates and was not compared against the cap",
			LastTransitionTime: metav1Now(),
		}
	default:
		return conditionTenantUnderBudget()
	}
}
