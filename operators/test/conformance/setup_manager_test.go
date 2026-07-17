/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

// TestSetupWithManager_RegistersEveryReconciler proves each reconciler wires
// into a real controller-runtime manager: its watches, ownership edges, and (for
// the Tenant reconciler) its field index register without error. The operator
// binary's cmd/main.go calls these at startup; this exercises the same code on
// the envtest API server without starting the manager's control loop.
func TestSetupWithManager_RegistersEveryReconciler(t *testing.T) {
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme: scheme,
		// No metrics/health servers or leader election — we register controllers
		// and index fields, we never Start the manager.
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	setups := map[string]func(ctrl.Manager) error{
		"platform":     (&controller.PlatformReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"tenant":       (&controller.TenantReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"agentfleet":   (&controller.AgentFleetReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"agentsandbox": (&controller.AgentSandboxReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"batch":        (&controller.BatchJobReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"budget":       (&controller.BudgetReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"eval":         (&controller.EvalReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"modelgateway": (&controller.ModelGatewayReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
		"sandboxpool":  (&controller.SandboxPoolReconciler{Client: mgr.GetClient(), Scheme: scheme, Concurrency: 1}).SetupWithManager,
	}
	for name, setup := range setups {
		if err := setup(mgr); err != nil {
			t.Errorf("SetupWithManager(%s): %v", name, err)
		}
	}
}
