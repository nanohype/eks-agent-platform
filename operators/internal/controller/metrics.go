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

func init() {
	ctrlmetrics.Registry.MustRegister(killSwitchUnroutedTotal, fleetReadyAgents, evalSuiteScore)
}
