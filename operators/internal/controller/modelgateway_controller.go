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

// ModelGatewayReconciler reconciles ModelGateway CRs into upstream
// agentgateway Route CRs in the agentgateway namespace, attaches Bedrock
// Guardrails per route (per-route ref → gateway default → cluster
// baseline from SSM), and wires cross-region inference profiles when
// set. Bedrock-side guardrails are managed by terraform/components/
// bedrock; the operator only references the IDs it gets from SSM.
type ModelGatewayReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int

	// SSM-resolved baseline guardrail (from terraform/components/bedrock
	// outputs). Empty when the deployment region doesn't support Bedrock
	// Guardrails or when var.enable_guardrails_baseline=false.
	GuardrailID      string
	GuardrailVersion string

	// Region is the AWS region stamped onto each AgentgatewayBackend's
	// Bedrock provider. Empty falls back to the agentgateway pod's ambient
	// region (its IRSA/credential-chain region).
	Region string
}

// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentgateway.dev,resources=agentgatewaybackends;agentgatewaypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;httproutes,verbs=get;list;watch;create;update;patch;delete

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

	phase, endpoint, err := r.reconcileSelf(ctx, &gw)
	if err != nil {
		logger.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if err := r.modelGatewayApplyStatus(ctx, &gw, phase, endpoint); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	// Pending → re-queue with backoff so we pick up Platform-becoming-Ready
	// or agentgateway-CRDs-installing without waiting for the next CR write.
	if phase == phasePending {
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
