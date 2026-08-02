/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// killSwitchUnroutedTotal counts reconcile ticks that observed a fired
// kill-switch whose suspension never landed: the breach event was published,
// the grace window elapsed, and the platform is still not Suspended. It rises
// every tick the fault persists (not once per breach) so an alert built on
// increase() stays lit for the whole outage — a rising value means the
// EventBridge→StepFunctions path is broken and the tenant is still spending.
// Labelled by the BudgetPolicy identity so the page names the tenant.
var killSwitchUnroutedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agents_killswitch_unrouted_total",
		Help: "Reconcile observations of a fired kill-switch that did not suspend its platform within the grace window (EventBridge→StepFunctions routing failure).",
	},
	[]string{"namespace", "budgetpolicy", "platform"},
)

// fleetReadyAgents is the operator's first-class view of AgentFleet readiness:
// the number of agents the reconciler last observed Ready in a fleet. It is
// the domain metric behind the persona dashboards' fleet-runtime panels — a
// real operator-emitted series, distinct from the KSM projection of
// AgentFleet.status.readyAgents. Set on every fleet status write and cleared
// when the fleet is deleted so no stale series lingers.
var fleetReadyAgents = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "agents_fleet_ready_agents",
		Help: "Agents last observed Ready in an AgentFleet, by fleet.",
	},
	[]string{"namespace", "platform", "fleet"},
)

// evalSuiteScore is the operator's view of an EvalSuite's most recent mean
// score (0..1), the domain metric behind the eval-quality dashboard's score
// panels. Emitted whenever a status write observes a parseable
// EvalSuite.status.lastScore and cleared when the suite is deleted.
var evalSuiteScore = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "agents_eval_suite_score",
		Help: "Most recent mean EvalSuite score (0..1), by suite.",
	},
	[]string{"namespace", "platform", "suite"},
)

// sloBurnRate is the operator's evaluation of an SLOPolicy's error-budget burn,
// normalized per severity tier so one threshold covers windows with different
// factors: 1 means a window pair sits exactly at its factor, above 1 is a
// breach. This is the operator-emitted twin of the KSM projection of
// SLOPolicy.status — the reconciler is the single evaluator, so a dashboard or a
// pager reads the number it computed rather than re-deriving the same PromQL
// against the same data and drifting from the control loop's decision.
var sloBurnRate = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "agents_slo_burn_rate",
		Help: "Normalized error-budget burn per severity tier (>= 1 is a breach of that tier's multi-window burn-rate check).",
	},
	[]string{"namespace", "slopolicy", "platform", "tier"},
)

// sloHoldActive is 1 while a tenant's rollout is held by a deny sync window.
// A gauge, not a counter: the question an operator asks is "is this tenant held
// right now", and a held tenant that nobody notices is the failure mode.
var sloHoldActive = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "agents_slo_hold_active",
		Help: "1 while an SLOPolicy's page-tier breach holds its tenant's ArgoCD auto-sync, 0 otherwise.",
	},
	[]string{"namespace", "slopolicy", "platform"},
)

// sloHoldUnobservedTotal counts reconcile ticks that observed an engaged hold
// whose deny window never appeared on the tenant's AppProject within the grace
// window. It rises every tick the fault persists (not once per breach) so an
// alert built on increase() stays lit for the whole outage. Zero is the only
// healthy value: a rising counter means the hold was decided but never took
// effect, and a burning rollout is still advancing. The exact twin of
// killSwitchUnroutedTotal, for the same reason — publishing a decision is not
// the same as the decision taking effect.
var sloHoldUnobservedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agents_slo_hold_unobserved_total",
		Help: "Reconcile observations of an engaged rollout hold whose deny sync window never reached the tenant's AppProject within the grace window.",
	},
	[]string{"namespace", "slopolicy", "platform"},
)

// budgetSpendUnreadableTotal counts reconciles where the spend query failed, so the
// budget was not evaluated at all.
//
// It exists because the failure is otherwise invisible. Reconcile records the error
// on the CR and returns nil, so it never reaches
// controller_runtime_reconcile_errors_total and the four ReconcileErrorBudget*Burn
// alerts cannot see it. BudgetReconcileLag cannot cover it either: lastReconciled is
// written only on the success path, so a BudgetPolicy that has never once succeeded
// has no series at all and the lag expression yields an empty vector.
//
// A tenant whose query fails on every tick from day one is a tenant with no budget
// enforcement, and before this the only evidence was a condition on one CR. Zero is
// the healthy value.
var budgetSpendUnreadableTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agents_budget_spend_unreadable_total",
		Help: "Reconciles where the tenant's spend could not be read, so the budget was not evaluated and the kill switch could not fire.",
	},
	[]string{"namespace", "budgetpolicy", "platform"},
)

// forgetSLOPolicySeries drops every series an SLOPolicy owns. Called when the CR
// is gone so a deleted objective does not leave a stale burn rate on a dashboard
// forever — the same cleanup fleetReadyAgents and evalSuiteScore do on delete.
func forgetSLOPolicySeries(namespace, name string) {
	labels := prometheus.Labels{"namespace": namespace, "slopolicy": name}
	sloBurnRate.DeletePartialMatch(labels)
	sloHoldActive.DeletePartialMatch(labels)
	sloHoldUnobservedTotal.DeletePartialMatch(labels)
}

func init() {
	ctrlmetrics.Registry.MustRegister(
		killSwitchUnroutedTotal, fleetReadyAgents, evalSuiteScore,
		sloBurnRate, sloHoldActive, sloHoldUnobservedTotal,
		budgetSpendUnreadableTotal,
	)
}
