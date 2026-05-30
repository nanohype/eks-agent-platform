/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// modelGatewayFinalizer is set on every ModelGateway so we can reap the
// agentgateway Route/Listener CRs before the CR is deleted. Without it,
// rapid Create→Delete would leave orphan agentgateway resources.
const modelGatewayFinalizer = "agents.nanohype.dev/modelgateway-finalizer"

// agentgatewayGV is the GroupVersion the operator generates Route +
// Listener CRs under. Lazy detect at reconcile time — clusters without
// agentgateway installed see a NoKindMatch and the reconciler logs +
// skips emission (still updates status with phase=Pending).
var agentgatewayGV = schema.GroupVersion{Group: "agentgateway.dev", Version: "v1alpha1"}

// resolvePlatform fetches the Platform a ModelGateway references and
// returns the resolved tenant namespace + readiness. Returns nil platform
// + ErrPlatformNotFound when the ref is dangling so the reconciler can
// surface that as a status condition rather than retrying forever.
var errPlatformNotFound = errors.New("platformRef not found")

func (r *ModelGatewayReconciler) resolvePlatform(ctx context.Context, mg *agentsv1alpha1.ModelGateway) (*platformv1alpha1.Platform, error) {
	var p platformv1alpha1.Platform
	key := types.NamespacedName{Namespace: mg.Namespace, Name: mg.Spec.PlatformRef.Name}
	if err := r.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errPlatformNotFound
		}
		return nil, fmt.Errorf("get platform %s: %w", key, err)
	}
	return &p, nil
}

// ensureAgentgatewayRoutes emits one agentgateway.dev/v1alpha1 Route CR
// per ModelRoute. Idempotent: CreateOrUpdate keyed by route name. Routes
// land in the agentgateway namespace so agentgateway itself can pick
// them up; named with a stable Platform+route prefix to avoid collisions
// across Platforms that happen to share route names ("primary").
func (r *ModelGatewayReconciler) ensureAgentgatewayRoutes(ctx context.Context, mg *agentsv1alpha1.ModelGateway, _ *platformv1alpha1.Platform, guardrailID, guardrailVersion string) error {
	for _, route := range mg.Spec.Routes {
		routeName := mg.Spec.PlatformRef.Name + "-" + route.Name
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   agentgatewayGV.Group,
			Version: agentgatewayGV.Version,
			Kind:    "Route",
		})
		obj.SetName(routeName)
		obj.SetNamespace("agentgateway")
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
			labels := map[string]string{
				"app.kubernetes.io/managed-by":    "eks-agent-platform",
				"eks-agent-platform/platform":     mg.Spec.PlatformRef.Name,
				"eks-agent-platform/model-family": route.ModelFamily,
			}
			obj.SetLabels(labels)

			// Resolve effective model: prefer cross-region inference profile
			// when set, otherwise the bare modelId.
			effectiveModel := route.ModelID
			if route.CrossRegionProfile != "" {
				effectiveModel = route.CrossRegionProfile
			}

			spec := map[string]any{
				"backend": map[string]any{
					"bedrock": map[string]any{
						"modelId":     effectiveModel,
						"modelFamily": route.ModelFamily,
					},
				},
			}
			if route.RateLimit > 0 {
				spec["rateLimit"] = map[string]any{
					"requestsPerMinute": int64(route.RateLimit),
				}
			}
			// Guardrail attachment: per-route ref wins; else gateway default;
			// else the cluster baseline ID from SSM.
			gID := guardrailID
			gVer := guardrailVersion
			if route.GuardrailRef != nil && route.GuardrailRef.Name != "" {
				gID = route.GuardrailRef.Name
				gVer = ""
			} else if mg.Spec.DefaultGuardrailRef != nil && mg.Spec.DefaultGuardrailRef.Name != "" {
				gID = mg.Spec.DefaultGuardrailRef.Name
				gVer = ""
			}
			if gID != "" {
				g := map[string]any{"identifier": gID}
				if gVer != "" {
					g["version"] = gVer
				}
				spec["guardrail"] = g
			}
			return unstructured.SetNestedField(obj.Object, spec, "spec")
		})
		if err != nil {
			if isNoKindMatch(err) {
				// agentgateway CRDs not installed; surface via status, not error.
				return errAgentgatewayNotInstalled
			}
			return fmt.Errorf("ensure agentgateway Route %s: %w", routeName, err)
		}
	}
	return nil
}

// cleanupAgentgatewayRoutes removes the Route CRs the reconciler created.
// Called from the finalizer path. Tolerates NoKindMatch (agentgateway not
// installed) and NotFound.
func (r *ModelGatewayReconciler) cleanupAgentgatewayRoutes(ctx context.Context, mg *agentsv1alpha1.ModelGateway) error {
	for _, route := range mg.Spec.Routes {
		routeName := mg.Spec.PlatformRef.Name + "-" + route.Name
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   agentgatewayGV.Group,
			Version: agentgatewayGV.Version,
			Kind:    "Route",
		})
		obj.SetName(routeName)
		obj.SetNamespace("agentgateway")
		if err := r.Delete(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) || isNoKindMatch(err) {
				continue
			}
			return fmt.Errorf("delete agentgateway Route %s: %w", routeName, err)
		}
	}
	return nil
}

var errAgentgatewayNotInstalled = errors.New("agentgateway.dev CRDs not installed on this cluster")

// reconcileSelf does the substantive work of ModelGatewayReconciler.
// Returns (phase, endpoint, error). 'Ready' = routes emitted; 'Pending'
// = waiting on agentgateway or Platform; error = real failure to retry.
func (r *ModelGatewayReconciler) reconcileSelf(ctx context.Context, mg *agentsv1alpha1.ModelGateway) (string, string, error) {
	platform, err := r.resolvePlatform(ctx, mg)
	if err != nil {
		if errors.Is(err, errPlatformNotFound) {
			return phasePending, "", nil
		}
		return "", "", err
	}
	// Don't emit routes until the Platform itself is Ready (status.namespace
	// populated + IRSA role minted). Otherwise agentgateway would route
	// requests to a tenant role that doesn't exist yet → AccessDenied.
	if platform.Status.Phase != phaseReady {
		return phasePending, "", nil
	}

	if err := r.ensureAgentgatewayRoutes(ctx, mg, platform, r.GuardrailID, r.GuardrailVersion); err != nil {
		if errors.Is(err, errAgentgatewayNotInstalled) {
			return phasePending, "", nil
		}
		return "", "", err
	}
	return phaseReady, "http://agentgateway.agentgateway.svc.cluster.local:8080", nil
}

// modelGatewayApplyStatus writes the computed phase + endpoint + conditions.
func (r *ModelGatewayReconciler) modelGatewayApplyStatus(ctx context.Context, mg *agentsv1alpha1.ModelGateway, phase, endpoint string) error {
	mg.Status.Phase = phase
	mg.Status.Endpoint = endpoint
	mg.Status.ObservedGeneration = mg.Generation
	cond := metav1.Condition{
		Type:               "RoutesReconciled",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("%d route(s) reconciled", len(mg.Spec.Routes)),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: mg.Generation,
	}
	if phase != phaseReady {
		cond.Status = metav1.ConditionFalse
		cond.Reason = phasePending
		cond.Message = "waiting on Platform or agentgateway CRDs"
	}
	upsertCondition(&mg.Status.Conditions, cond)
	return r.Status().Update(ctx, mg)
}
