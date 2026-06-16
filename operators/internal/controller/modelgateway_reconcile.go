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
// agentgateway Gateway/HTTPRoute/Backend/Policy CRs before the CR is
// deleted. Without it, rapid Create→Delete would leave orphan resources.
const modelGatewayFinalizer = "agents.nanohype.dev/modelgateway-finalizer"

// agentgatewayGV / gatewayAPIGV are the GroupVersions the operator
// generates into. A ModelGateway becomes a Gateway-API Gateway + per-route
// HTTPRoute pointing at an AgentgatewayBackend (the Bedrock LLM backend),
// with an AgentgatewayPolicy carrying the per-route rate limit. Lazy detect
// at reconcile time — clusters without agentgateway / Gateway-API installed
// see a NoKindMatch and the reconciler surfaces phase=Pending, not an error.
var (
	agentgatewayGV = schema.GroupVersion{Group: "agentgateway.dev", Version: "v1alpha1"}
	gatewayAPIGV   = schema.GroupVersion{Group: "gateway.networking.k8s.io", Version: "v1"}
)

// agentgatewayNamespace is where the Gateway, AgentgatewayBackend,
// HTTPRoute, and AgentgatewayPolicy land so the agentgateway controller
// (which watches that namespace) picks them up.
const agentgatewayNamespace = "agentgateway"

// gatewayListenerPort is the HTTP listener port on the per-Platform
// Gateway. agentgateway provisions a data-plane Service named after the
// Gateway exposing this port; tenant ModelConfigs target it.
const gatewayListenerPort = 8080

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

// ensureGatewayResources renders a ModelGateway into the agentgateway data
// plane: one Gateway-API Gateway per Platform, then per ModelRoute an
// AgentgatewayBackend (the Bedrock LLM backend), an HTTPRoute exposing it at
// /<platform>-<route>, and — when the route sets a rate limit — an
// AgentgatewayPolicy. Idempotent (CreateOrUpdate keyed by stable
// Platform+route names). Returns the in-cluster endpoint of the Gateway's
// data-plane Service (the base URL tenant ModelConfigs target).
func (r *ModelGatewayReconciler) ensureGatewayResources(ctx context.Context, mg *agentsv1alpha1.ModelGateway, guardrailID, guardrailVersion string) (string, error) {
	platformName := mg.Spec.PlatformRef.Name
	gwName := platformName + "-gateway"

	// 1. The Gateway. agentgateway provisions a data-plane Service named
	//    after it, exposing the HTTP listener port.
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{Group: gatewayAPIGV.Group, Version: gatewayAPIGV.Version, Kind: "Gateway"})
	gw.SetName(gwName)
	gw.SetNamespace(agentgatewayNamespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, gw, func() error {
		gw.SetLabels(gatewayLabels(platformName))
		spec := map[string]any{
			"gatewayClassName": "agentgateway",
			"listeners": []any{
				map[string]any{
					"name":     "http",
					"protocol": "HTTP",
					"port":     int64(gatewayListenerPort),
					"allowedRoutes": map[string]any{
						"namespaces": map[string]any{"from": "All"},
					},
				},
			},
		}
		return unstructured.SetNestedField(gw.Object, spec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return "", errAgentgatewayNotInstalled
		}
		return "", fmt.Errorf("ensure Gateway %s: %w", gwName, err)
	}

	// 2. Per route: backend + HTTPRoute + (optional) rate-limit policy.
	for _, route := range mg.Spec.Routes {
		routeName := platformName + "-" + route.Name

		// Effective model: cross-region inference profile when set, else
		// the bare model id.
		effectiveModel := route.ModelID
		if route.CrossRegionProfile != "" {
			effectiveModel = route.CrossRegionProfile
		}
		// Guardrail: per-route ref wins; else gateway default; else the SSM
		// baseline (id + version). AgentgatewayBackend requires both
		// identifier + version, so a name-only ref pins the DRAFT version.
		gID, gVer := guardrailID, guardrailVersion
		switch {
		case route.GuardrailRef != nil && route.GuardrailRef.Name != "":
			gID, gVer = route.GuardrailRef.Name, "DRAFT"
		case mg.Spec.DefaultGuardrailRef != nil && mg.Spec.DefaultGuardrailRef.Name != "":
			gID, gVer = mg.Spec.DefaultGuardrailRef.Name, "DRAFT"
		}

		// 2a. AgentgatewayBackend — the Bedrock LLM backend.
		backend := &unstructured.Unstructured{}
		backend.SetGroupVersionKind(schema.GroupVersionKind{Group: agentgatewayGV.Group, Version: agentgatewayGV.Version, Kind: "AgentgatewayBackend"})
		backend.SetName(routeName)
		backend.SetNamespace(agentgatewayNamespace)
		if _, berr := controllerutil.CreateOrUpdate(ctx, r.Client, backend, func() error {
			backend.SetLabels(routeLabels(platformName, route.ModelFamily))
			bedrock := map[string]any{"model": effectiveModel}
			if r.Region != "" {
				bedrock["region"] = r.Region
			}
			if gID != "" && gVer != "" {
				bedrock["guardrail"] = map[string]any{"identifier": gID, "version": gVer}
			}
			spec := map[string]any{
				"ai": map[string]any{
					"provider": map[string]any{"bedrock": bedrock},
				},
			}
			return unstructured.SetNestedField(backend.Object, spec, "spec")
		}); berr != nil {
			if isNoKindMatch(berr) {
				return "", errAgentgatewayNotInstalled
			}
			return "", fmt.Errorf("ensure AgentgatewayBackend %s: %w", routeName, berr)
		}

		// 2b. HTTPRoute — exposes the backend at /<platform>-<route>.
		hr := &unstructured.Unstructured{}
		hr.SetGroupVersionKind(schema.GroupVersionKind{Group: gatewayAPIGV.Group, Version: gatewayAPIGV.Version, Kind: "HTTPRoute"})
		hr.SetName(routeName)
		hr.SetNamespace(agentgatewayNamespace)
		if _, herr := controllerutil.CreateOrUpdate(ctx, r.Client, hr, func() error {
			hr.SetLabels(routeLabels(platformName, route.ModelFamily))
			spec := map[string]any{
				"parentRefs": []any{
					map[string]any{"name": gwName},
				},
				"rules": []any{
					map[string]any{
						"matches": []any{
							map[string]any{
								"path": map[string]any{"type": "PathPrefix", "value": "/" + routeName},
							},
						},
						"backendRefs": []any{
							map[string]any{
								"name":  routeName,
								"group": agentgatewayGV.Group,
								"kind":  "AgentgatewayBackend",
							},
						},
					},
				},
			}
			return unstructured.SetNestedField(hr.Object, spec, "spec")
		}); herr != nil {
			if isNoKindMatch(herr) {
				return "", errAgentgatewayNotInstalled
			}
			return "", fmt.Errorf("ensure HTTPRoute %s: %w", routeName, herr)
		}

		// 2c. AgentgatewayPolicy — per-route local rate limit (req/min).
		if rerr := r.ensureRouteRateLimit(ctx, platformName, routeName, route.RateLimit); rerr != nil {
			return "", rerr
		}
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", gwName, agentgatewayNamespace, gatewayListenerPort), nil
}

// ensureRouteRateLimit attaches an AgentgatewayPolicy carrying the route's
// requests-per-minute local rate limit, targeting the HTTPRoute. When the
// route sets no limit it removes any stale policy so disabling a limit
// takes effect.
func (r *ModelGatewayReconciler) ensureRouteRateLimit(ctx context.Context, platformName, routeName string, rpm int32) error {
	policyName := routeName + "-ratelimit"
	pol := &unstructured.Unstructured{}
	pol.SetGroupVersionKind(schema.GroupVersionKind{Group: agentgatewayGV.Group, Version: agentgatewayGV.Version, Kind: "AgentgatewayPolicy"})
	pol.SetName(policyName)
	pol.SetNamespace(agentgatewayNamespace)
	if rpm <= 0 {
		if err := r.Delete(ctx, pol); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
			return fmt.Errorf("delete AgentgatewayPolicy %s: %w", policyName, err)
		}
		return nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pol, func() error {
		pol.SetLabels(gatewayLabels(platformName))
		spec := map[string]any{
			"targetRefs": []any{
				map[string]any{
					"group": gatewayAPIGV.Group,
					"kind":  "HTTPRoute",
					"name":  routeName,
				},
			},
			"traffic": map[string]any{
				"rateLimit": map[string]any{
					"local": []any{
						map[string]any{"requests": int64(rpm), "unit": "Minutes"},
					},
				},
			},
		}
		return unstructured.SetNestedField(pol.Object, spec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return errAgentgatewayNotInstalled
		}
		return fmt.Errorf("ensure AgentgatewayPolicy %s: %w", policyName, err)
	}
	return nil
}

func gatewayLabels(platformName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "eks-agent-platform",
		LabelPlatform:                  platformName,
	}
}

func routeLabels(platformName, modelFamily string) map[string]string {
	l := gatewayLabels(platformName)
	l[LabelModelFamily] = modelFamily
	return l
}

// cleanupGatewayResources is the finalizer counterpart: deletes the
// per-route AgentgatewayPolicy/HTTPRoute/AgentgatewayBackend and the
// Platform Gateway. Tolerates NoKindMatch (agentgateway / Gateway-API not
// installed) and NotFound.
func (r *ModelGatewayReconciler) cleanupGatewayResources(ctx context.Context, mg *agentsv1alpha1.ModelGateway) error {
	platformName := mg.Spec.PlatformRef.Name
	del := func(group, version, kind, name string) error {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		o.SetName(name)
		o.SetNamespace(agentgatewayNamespace)
		if err := r.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
			return fmt.Errorf("delete %s %s: %w", kind, name, err)
		}
		return nil
	}
	for _, route := range mg.Spec.Routes {
		routeName := platformName + "-" + route.Name
		if err := del(agentgatewayGV.Group, agentgatewayGV.Version, "AgentgatewayPolicy", routeName+"-ratelimit"); err != nil {
			return err
		}
		if err := del(gatewayAPIGV.Group, gatewayAPIGV.Version, "HTTPRoute", routeName); err != nil {
			return err
		}
		if err := del(agentgatewayGV.Group, agentgatewayGV.Version, "AgentgatewayBackend", routeName); err != nil {
			return err
		}
	}
	return del(gatewayAPIGV.Group, gatewayAPIGV.Version, "Gateway", platformName+"-gateway")
}

var errAgentgatewayNotInstalled = errors.New("agentgateway.dev / Gateway-API CRDs not installed on this cluster")

// reconcileSelf does the substantive work of ModelGatewayReconciler.
// Returns (phase, endpoint, error). 'Ready' = gateway resources emitted;
// 'Pending' = waiting on agentgateway/Gateway-API CRDs or Platform
// readiness; error = real failure to retry.
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

	endpoint, err := r.ensureGatewayResources(ctx, mg, r.GuardrailID, r.GuardrailVersion)
	if err != nil {
		if errors.Is(err, errAgentgatewayNotInstalled) {
			return phasePending, "", nil
		}
		return "", "", err
	}
	return phaseReady, endpoint, nil
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
		cond.Message = "waiting on Platform or agentgateway / Gateway-API CRDs"
	}
	upsertCondition(&mg.Status.Conditions, cond)
	return r.Status().Update(ctx, mg)
}
