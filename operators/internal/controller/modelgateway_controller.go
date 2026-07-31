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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

// ModelGatewayReconciler reconciles ModelGateway CRs into an Envoy AI Gateway
// data plane in the tenant's own namespace: a Gateway whose Envoy runs under
// the tenant ServiceAccount, one AIServiceBackend per route, and a single
// Bedrock upstream. It attaches Bedrock Guardrails per route (per-route ref →
// gateway default → cluster baseline from SSM) as request headers the caller
// cannot override, and resolves cross-region inference profiles into the
// route's upstream model id. Bedrock-side guardrails are managed by
// terraform/components/bedrock; the operator only references the IDs it gets
// from SSM.
type ModelGatewayReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int

	// SSM-resolved baseline guardrail (from terraform/components/bedrock
	// outputs). Empty when the deployment region doesn't support Bedrock
	// Guardrails or when var.enable_guardrails_baseline=false.
	GuardrailID      string
	GuardrailVersion string

	// Region is the AWS region the gateway signs for and whose Bedrock
	// endpoint it dials. It is required: the Backend address is built from it,
	// so an empty value would point the gateway at a hostname with a hole in
	// it rather than falling back to anything.
	Region string
}

// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=aigateway.envoyproxy.io,resources=aigatewayroutes;aiservicebackends;backendsecuritypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=envoyproxies;backends;clienttrafficpolicies;backendtrafficpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;backendtlspolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a ModelGateway CR toward its desired state.
func (r *ModelGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("modelgateway", req.NamespacedName)

	var gw agentsv1alpha1.ModelGateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Finalizer-driven cleanup. Same pattern as PlatformReconciler.
	if !gw.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&gw, modelGatewayFinalizer) {
			if err := r.cleanupGatewayResources(ctx, &gw); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&gw, modelGatewayFinalizer)
			if err := r.Update(ctx, &gw); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&gw, modelGatewayFinalizer) {
		controllerutil.AddFinalizer(&gw, modelGatewayFinalizer)
		if err := r.Update(ctx, &gw); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	res, err := r.reconcileSelf(ctx, &gw)
	if err != nil {
		logger.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if err := r.modelGatewayApplyStatus(ctx, &gw, res); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	// Pending → re-queue with backoff so we pick up Platform-becoming-Ready
	// or gateway-CRDs-installing without waiting for the next CR write.
	if res.phase == phasePending {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *ModelGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.ModelGateway{}).
		Named("modelgateway").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
