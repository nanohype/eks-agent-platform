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
// its own ServiceAccount bound to the tenant IAM role, one AIServiceBackend per
// route, and a single Bedrock upstream. It reports Ready only once Envoy
// Gateway reports that Gateway Programmed and every Ready, non-terminating Envoy
// pod of it carries a ready ai-gateway-extproc container — the sidecar that
// signs for Bedrock, which Envoy AI Gateway injects at pod creation and so is
// missing from the first pod of every new Gateway. It attaches Bedrock
// Guardrails per route (per-route ref →
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

	// APIReader lists the gateway's Envoy pods past the manager's cache
	// (envoyPodsCanSign), so the operator runs no cluster-wide Pod informer for
	// a read it makes once per pass. SetupWithManager sets it to the manager's
	// API reader when it is unset; with none set, Client is read.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=aigateway.envoyproxy.io,resources=aigatewayroutes;aiservicebackends;backendsecuritypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=envoyproxies;backends;clienttrafficpolicies;backendtrafficpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;backendtlspolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;update
// Ready reads the gateway's Envoy pods for the extproc (envoyPodsCanSign).
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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
		// A failure that still determined a phase — the Envoy pod read, which
		// leaves the gateway Provisioning with its contract withdrawn — writes
		// it before the retry, so a stale Ready does not outlive the failure.
		// Every other failure returns no phase and leaves the status alone.
		if res.phase != "" {
			if serr := r.modelGatewayApplyStatus(ctx, &gw, res); serr != nil {
				logger.Error(serr, "status update after a failed reconcile")
			}
		}
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
	// Provisioning → the Gateway is applied and its data plane is not serving.
	// Nothing here watches Gateways (their CRDs may be absent) or Envoy pods (no
	// Pod informer is held for this), so poll. A Gateway that is Programmed and
	// waiting only on its pods to carry a ready extproc is mid-rollout and
	// looks again sooner; a fresh data plane usually comes up within a minute.
	if res.phase == phaseProvisioning {
		if awaitingEnvoyPods(res.dataPlane.reason) {
			return ctrl.Result{RequeueAfter: envoyRolloutRequeueInterval}, nil
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	// Ready → keep looking. Ready is a reading of the Gateway's Programmed
	// condition and of its Envoy pods, and nothing watches either, so without a
	// requeue a data plane that stopped serving — or a pod admitted without the
	// extproc — would leave the ModelGateway Ready, publishing its endpoint,
	// until the manager's next resync hours later.
	return ctrl.Result{RequeueAfter: readyRecheckInterval}, nil
}

// readyRecheckInterval is how often a Ready ModelGateway re-reads its Gateway's
// Programmed condition and its Envoy pods, which bounds how long Ready can
// outlive the data plane.
const readyRecheckInterval = 2 * time.Minute

// SetupWithManager registers the reconciler with the controller manager.
func (r *ModelGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.ModelGateway{}).
		Named("modelgateway").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
