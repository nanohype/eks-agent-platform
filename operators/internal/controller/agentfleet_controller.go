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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

// AgentFleetReconciler reconciles AgentFleet CRs into one Deployment per agent,
// a KEDA ScaledObject (when scaling.enabled), a per-fleet NetworkPolicy locking
// egress to the model gateway + OTel only, and the tenant ServiceAccount that
// EKS Pod Identity binds to the Platform's IAM role.
//
// The agent loop and its tools are the tenant's own image; the platform supplies
// the identity it runs as and the route contract it calls the model through.
//
// KEDA absence is tolerated — the fleet runs at its static replica count, no
// reconcile error.
type AgentFleetReconciler struct {
	// Environment is the operator's deploy stage, stamped on the pods this
	// reconciler builds as the OTel deployment.environment resource attribute
	// (resource-tagging renders the environment dimension there). Empty on a
	// cluster started without --environment, and omitted from the attribute
	// rather than emitted blank.
	Environment string
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int

	// NetworkEngine ("cilium"|"kubernetes") — see PlatformReconciler. Selects a
	// CiliumNetworkPolicy vs a vanilla NetworkPolicy for fleet egress.
	NetworkEngine string

	// Region is the AWS region this cluster runs in, used to name the AWS
	// service endpoints a fleet's egress policy allows on 443 (see
	// datastoreFQDNs). Empty means those hostnames are omitted and the fleet
	// reaches no AWS API — the deliberate fail-closed, since the alternative
	// bound (a bare 443 allow) would also open the model plane.
	Region string

	// VCluster resolves the per-Platform virtual-cluster client for the vcluster
	// isolation tier. nil in the namespace tier and k8s-only test paths. When the
	// owning Platform is vcluster-tier, the fleet's agent Deployments and
	// KEDA ScaledObject land in the virtual cluster's API through this client (the
	// target-client swap); the fleet's host containment (NetworkPolicy) always
	// stays on the host client.
	VCluster VClusterClientFactory
}

// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=agentfleets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=agentfleets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=agentfleets/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.nanohype.dev,resources=modelgateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects;triggerauthentications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives an AgentFleet CR toward its desired state.
func (r *AgentFleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentfleet", req.NamespacedName)

	var fleet agentsv1alpha1.AgentFleet
	if err := r.Get(ctx, req.NamespacedName, &fleet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !fleet.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&fleet, agentFleetFinalizer) {
			platform, perr := r.resolvePlatform(ctx, &fleet)
			if perr != nil && platform == nil {
				logger.Info("platform gone; skipping fleet cleanup")
			} else if perr == nil {
				if err := r.cleanupFleetResources(ctx, r.cleanupTargetClient(ctx, platform), &fleet, platform); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&fleet, agentFleetFinalizer)
			if err := r.Update(ctx, &fleet); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		// Drop the fleet's readiness series so a deleted fleet leaves no stale
		// gauge behind.
		fleetReadyAgents.DeleteLabelValues(fleet.Namespace, fleet.Spec.PlatformRef.Name, fleet.Name)
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&fleet, agentFleetFinalizer) {
		controllerutil.AddFinalizer(&fleet, agentFleetFinalizer)
		if err := r.Update(ctx, &fleet); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	res, err := r.reconcileFleetSelf(ctx, &fleet)
	if err != nil {
		logger.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if err := r.applyFleetStatus(ctx, &fleet, res); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	if res.phase == phasePending {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *AgentFleetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentFleet{}).
		Watches(&agentsv1alpha1.ModelGateway{}, handler.EnqueueRequestsFromMapFunc(r.gatewayToFleets)).
		Named("agentfleet").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}

// gatewayToFleets maps a ModelGateway event onto every fleet on the same
// Platform.
//
// Without it the route contract only reaches a fleet on the 30s Pending
// requeue, and never at all once the fleet is Ready — so a route removed or
// renamed on a running fleet would leave its agents pointed at a base URL the
// gateway no longer serves, with nothing reconciling and the fleet reporting
// Ready. Namespace-scoped, matching how a fleet resolves its own gateway.
func (r *AgentFleetReconciler) gatewayToFleets(ctx context.Context, obj client.Object) []reconcile.Request {
	mg, ok := obj.(*agentsv1alpha1.ModelGateway)
	if !ok || mg.Spec.PlatformRef.Name == "" {
		return nil
	}
	var fleets agentsv1alpha1.AgentFleetList
	if err := r.List(ctx, &fleets, client.InNamespace(mg.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range fleets.Items {
		if fleets.Items[i].Spec.PlatformRef.Name != mg.Spec.PlatformRef.Name {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&fleets.Items[i])})
	}
	return out
}
