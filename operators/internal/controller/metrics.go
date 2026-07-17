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

func init() {
	ctrlmetrics.Registry.MustRegister(killSwitchUnroutedTotal)
}
