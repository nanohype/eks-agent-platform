/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// modelGatewayFinalizer is set on every ModelGateway so we can reap the data
// plane's Gateway/EnvoyProxy/AIGatewayRoute/AIServiceBackend/Backend/policy
// resources before the CR is deleted. Without it, rapid Create→Delete would
// leave orphans — and an orphaned Gateway keeps an Envoy Deployment running.
const modelGatewayFinalizer = "agents.nanohype.dev/modelgateway-finalizer"

// The GroupVersions the operator generates into. A ModelGateway becomes a
// Gateway-API Gateway backed by an Envoy AI Gateway AIGatewayRoute, one
// AIServiceBackend per route, and a single Bedrock Backend. Lazy detect at
// reconcile time — clusters without Envoy AI Gateway / Gateway-API installed
// see a NoKindMatch and the reconciler surfaces phase=Pending, not an error.
var (
	aiGatewayGV = schema.GroupVersion{Group: "aigateway.envoyproxy.io", Version: "v1beta1"}
	// envoyGatewayGV carries Envoy Gateway's own extensions: EnvoyProxy (data
	// plane shape), Backend (upstream address), ClientTrafficPolicy (buffer
	// limits) and BackendTrafficPolicy (rate limits).
	envoyGatewayGV = schema.GroupVersion{Group: "gateway.envoyproxy.io", Version: "v1alpha1"}
	// Gateway API v1 serves Gateway and BackendTLSPolicy alike. BackendTLSPolicy
	// graduated to v1 in the standard channel; its older v1alpha3 is present in
	// the CRD but `served: false`, so emitting that version would be rejected at
	// apply time even though the kind exists.
	gatewayAPIGV = schema.GroupVersion{Group: "gateway.networking.k8s.io", Version: "v1"}
)

// gatewayClassName is the GatewayClass the Envoy AI Gateway install provides.
const gatewayClassName = "envoy-ai-gateway"

// gatewayListenerPort is the HTTP listener port on the per-Platform Gateway.
// Envoy Gateway provisions a data-plane Service exposing it; tenant workloads
// point their model client's base URL at it.
const gatewayListenerPort = 8080

// bedrockAnthropicVersion is the Anthropic API version Bedrock speaks. The
// AIServiceBackend carries it so the gateway stamps it on translated requests;
// it is Bedrock's own constant, not a model or SDK version.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// bedrockHTTPSPort is the port the Bedrock Backend dials, and the same number
// gatewayEgressCiliumRules binds for the gateway's Envoy pods — where the
// comment notes that the port bound plus the pod selector *is* the model
// boundary, since PrivateLink makes FQDN matching depend on cilium's DNS proxy.
// The two are separate documents that must agree: dial a port egress does not
// allow and the gateway hangs until the connection times out, which surfaces as
// a slow model call rather than as a misconfiguration. Named here, and the
// agreement asserted in TestModelGatewayBedrockBackendEndpoint.
const bedrockHTTPSPort = 443

// clientBufferLimit raises Envoy's request buffer from its 32KiB default.
// Upstream calls this out directly: the default "is not sufficient for AI
// workloads". A long prompt exceeds it, so leaving it unset fails on real
// traffic while passing every smoke test.
const clientBufferLimit = "50Mi"

// Bedrock applies a guardrail to InvokeModel through request headers rather
// than the body, which is what lets the gateway enforce one the caller cannot
// opt out of: headerMutation `set` overwrites whatever the client sent.
//
//	https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html
const (
	guardrailIDHeader      = "X-Amzn-Bedrock-GuardrailIdentifier"
	guardrailVersionHeader = "X-Amzn-Bedrock-GuardrailVersion"
)

// aiGatewayModelHeader is the header Envoy AI Gateway derives from the request
// body's model id, and the only thing tying a route's match rule to that same
// route's rate limit. Both are built here, in separate literals, and nothing in
// Go compares them: the match rule selects the backend, the BackendTrafficPolicy
// clientSelector decides which requests the limit counts. Spelled differently in
// one of the two places, routing still works and the limit silently matches zero
// requests — a tenant's declared rpm cap becomes no cap at all, with the CR still
// reporting Ready. One name, used twice, is what makes that undrifted.
const aiGatewayModelHeader = "x-ai-eg-model"

// resolvePlatform fetches the Platform a ModelGateway references and
// returns the resolved tenant namespace + readiness. Returns nil platform
// + ErrPlatformNotFound when the ref is dangling so the reconciler can
// surface that as a status condition rather than retrying forever.
var errPlatformNotFound = errors.New("platformRef not found")

// getReferencedPlatform fetches the Platform named by a same-namespace
// Spec.PlatformRef. It is the one implementation shared by every workload
// reconciler's resolve*Platform helper. A missing Platform maps to notFound so
// each caller keeps its own sentinel — the budget reconciler treats a dangling
// ref as Pending (re-driven by Platform create events), the agent workloads
// surface it as a status condition — while any other Get error is wrapped
// uniformly.
func getReferencedPlatform(ctx context.Context, c client.Client, namespace, refName string, notFound error) (*platformv1alpha1.Platform, error) {
	var p platformv1alpha1.Platform
	key := types.NamespacedName{Namespace: namespace, Name: refName}
	if err := c.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, notFound
		}
		return nil, fmt.Errorf("get platform %s: %w", key, err)
	}
	return &p, nil
}

func (r *ModelGatewayReconciler) resolvePlatform(ctx context.Context, mg *agentsv1alpha1.ModelGateway) (*platformv1alpha1.Platform, error) {
	return getReferencedPlatform(ctx, r.Client, mg.Namespace, mg.Spec.PlatformRef.Name, errPlatformNotFound)
}

// modelFamilyAnthropic is the one family whose own wire format the gateway
// serves directly. Both the upstream schema and the client-facing contract
// branch on it, for related but distinct reasons — see backendSchemaFor and
// effectiveRouteAPI.
const modelFamilyAnthropic = "anthropic"

// backendSchemaFor maps a route to the Envoy AI Gateway upstream schema that
// serves it.
//
// AWSAnthropic accepts native Anthropic Messages on /v1/messages and OpenAI
// chat completions on /v1/chat/completions, translating either to Bedrock —
// so an Anthropic-family route keeps the model's own wire shape end to end,
// with thinking blocks, cache points and tool use intact.
//
// Everything else — the other foundation families and imported open-weight
// models — goes through AWSBedrock, which translates OpenAI-shaped requests to
// Bedrock's Converse API.
func backendSchemaFor(route agentsv1alpha1.ModelRouteSpec) string {
	if route.ModelSource != agentsv1alpha1.ModelSourceImported && route.ModelFamily == modelFamilyAnthropic {
		return "AWSAnthropic"
	}
	return "AWSBedrock"
}

// endpoint prefixes the gateway serves each client-facing wire format under.
//
// These are Envoy AI Gateway's own defaults, not a choice made here: the
// extproc registers a body-parsing processor per endpoint path, so a request
// to an unregistered path never gets the model name extracted from its body,
// never receives the x-ai-eg-model header, and matches no route rule. The
// caller sees a routing failure from a gateway that reports healthy, which is
// why the base URL is published rather than left for callers to assemble.
const (
	anthropicAPIPrefix = "/anthropic"
	openAIAPIPrefix    = "/v1"
)

// effectiveRouteAPI resolves the wire format callers speak to a route.
//
// An explicit spec.api wins; the CRD already rejects Anthropic on anything but
// an anthropic-family foundation route, so a set value is known to be
// serveable. Unset, it follows the model: only the Anthropic family is
// reachable in its native shape, so everything else resolves to OpenAI.
//
// This deliberately mirrors backendSchemaFor rather than reusing it. They
// answer different questions — one picks the upstream translator, the other
// the client-facing contract — and an anthropic-family route can legitimately
// be served as OpenAI while still using the AWSAnthropic backend.
func effectiveRouteAPI(route agentsv1alpha1.ModelRouteSpec) agentsv1alpha1.RouteAPI {
	if route.API != "" {
		return route.API
	}
	if route.ModelSource != agentsv1alpha1.ModelSourceImported && route.ModelFamily == modelFamilyAnthropic {
		return agentsv1alpha1.RouteAPIAnthropic
	}
	return agentsv1alpha1.RouteAPIOpenAI
}

// RouteBaseURL is the base URL a client of the given wire format is configured
// with — the gateway endpoint plus the prefix that format is served under.
//
// Exported alongside ModelGatewayEndpoint because the endpoint alone is not a
// usable base for any client, and every caller that has assembled one by hand
// has had to know a prefix the platform is in a position to tell it.
func RouteBaseURL(platform *platformv1alpha1.Platform, api agentsv1alpha1.RouteAPI) string {
	prefix := openAIAPIPrefix
	if api == agentsv1alpha1.RouteAPIAnthropic {
		prefix = anthropicAPIPrefix
	}
	return ModelGatewayEndpoint(platform) + prefix
}

// routeStatuses is the published per-route client contract.
func routeStatuses(mg *agentsv1alpha1.ModelGateway, platform *platformv1alpha1.Platform) []agentsv1alpha1.RouteStatus {
	out := make([]agentsv1alpha1.RouteStatus, 0, len(mg.Spec.Routes))
	for _, route := range mg.Spec.Routes {
		api := effectiveRouteAPI(route)
		out = append(out, agentsv1alpha1.RouteStatus{
			Name:    route.Name,
			API:     api,
			BaseURL: RouteBaseURL(platform, api),
		})
	}
	return out
}

// Failures a caller of publishedRoutes has to tell apart. The first two are
// timing — a gateway that has not reconciled yet resolves itself — and the
// third is a spec error that never will.
var (
	errGatewayNotFound     = errors.New("no ModelGateway references this Platform")
	errGatewayNotPublished = errors.New("ModelGateway has not published its route contract yet")
	errGatewayAmbiguous    = errors.New("more than one ModelGateway references this Platform")
)

// publishedRoutes reads a Platform's route contract off ModelGateway status,
// keyed by route name.
//
// Callers configure a model client from this rather than from
// ModelGatewayEndpoint, because the endpoint alone is not a usable base URL for
// any client — the gateway serves each wire format under its own prefix, and a
// request to an unprefixed path reaches no body processor, gets no
// x-ai-eg-model header, and matches no route rule. That failure looks like a
// healthy gateway with a dead data path, which is why the base URL is read from
// what the reconciler published rather than assembled by each caller.
//
// Ambiguity is an error, not a choice. Every rendered object is named from the
// Platform (see namesFor), so two ModelGateways referencing one Platform are
// two writers of one Gateway; picking either would report a contract the tenant
// may not be getting.
func publishedRoutes(ctx context.Context, c client.Client, platform *platformv1alpha1.Platform) (map[string]agentsv1alpha1.RouteStatus, error) {
	var list agentsv1alpha1.ModelGatewayList
	if err := c.List(ctx, &list, client.InNamespace(platform.Namespace)); err != nil {
		return nil, fmt.Errorf("list ModelGateways: %w", err)
	}
	var found *agentsv1alpha1.ModelGateway
	for i := range list.Items {
		if list.Items[i].Spec.PlatformRef.Name != platform.Name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: %s and %s", errGatewayAmbiguous, found.Name, list.Items[i].Name)
		}
		found = &list.Items[i]
	}
	if found == nil {
		return nil, errGatewayNotFound
	}
	if len(found.Status.Routes) == 0 {
		return nil, fmt.Errorf("%w: %s", errGatewayNotPublished, found.Name)
	}
	out := make(map[string]agentsv1alpha1.RouteStatus, len(found.Status.Routes))
	for _, route := range found.Status.Routes {
		out[route.Name] = route
	}
	return out, nil
}

// routeNames lists the published route names in sorted order, for an error
// message that tells the reader what they could have asked for.
func routeNames(routes map[string]agentsv1alpha1.RouteStatus) []string {
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// effectiveModelID is the identifier the gateway sends upstream: an imported
// route's model ARN, or a foundation route's cross-region inference profile
// when set, else its bare model id.
func effectiveModelID(route agentsv1alpha1.ModelRouteSpec) string {
	if route.ModelSource != agentsv1alpha1.ModelSourceImported && route.CrossRegionProfile != "" {
		return route.CrossRegionProfile
	}
	return route.ModelID
}

// resolveGuardrail picks the guardrail for a route: a per-route ref wins, then
// the gateway default, then the cluster baseline resolved from SSM. A named ref
// carries no version, so it pins DRAFT.
//
// Bedrock's inline guardrails are foundation-model-only, so an imported route
// cannot carry one. Rather than serve it silently unguarded, the route is
// returned with no guardrail and reported to the caller, which surfaces it as a
// status condition.
func resolveGuardrail(mg *agentsv1alpha1.ModelGateway, route agentsv1alpha1.ModelRouteSpec, baselineID, baselineVersion string) (id, version string, unenforced bool) {
	id, version = baselineID, baselineVersion
	switch {
	case route.GuardrailRef != nil && route.GuardrailRef.Name != "":
		id, version = route.GuardrailRef.Name, "DRAFT"
	case mg.Spec.DefaultGuardrailRef != nil && mg.Spec.DefaultGuardrailRef.Name != "":
		id, version = mg.Spec.DefaultGuardrailRef.Name, "DRAFT"
	}
	if route.ModelSource == agentsv1alpha1.ModelSourceImported && id != "" {
		return "", "", true
	}
	return id, version, false
}

// routeFamilyLabel is the model-family label value for a route. An imported
// route has no model family, so it carries the source instead.
func routeFamilyLabel(route agentsv1alpha1.ModelRouteSpec) string {
	if route.ModelSource == agentsv1alpha1.ModelSourceImported {
		return string(agentsv1alpha1.ModelSourceImported)
	}
	return route.ModelFamily
}

// gatewayNames derives every generated resource name from the Platform name so
// they stay stable across reconciles and collide with nothing else in the
// tenant namespace.
type gatewayNames struct {
	namespace string
	gateway   string
	backend   string
}

func namesFor(platform *platformv1alpha1.Platform) gatewayNames {
	name := platform.Name + "-gateway"
	return gatewayNames{
		namespace: PlatformNamespace(platform),
		gateway:   name,
		backend:   platform.Name + "-bedrock",
	}
}

// ModelGatewayEndpoint is the in-cluster base URL for a Platform's gateway.
//
// It is derived rather than looked up so callers that need the address without
// reading ModelGateway status — the eval workflow, for one — cannot drift from
// what the reconciler publishes. The EnvoyProxy pins the Service name, so this
// stays true for as long as both come from here.
func ModelGatewayEndpoint(platform *platformv1alpha1.Platform) string {
	n := namesFor(platform)
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", n.gateway, n.namespace, gatewayListenerPort)
}

// ensureGatewayResources renders a ModelGateway into an Envoy AI Gateway data
// plane living in the tenant's own namespace, and returns the in-cluster
// endpoint tenant workloads target.
//
// The Envoy proxy runs under the tenant ServiceAccount, which already carries a
// Pod Identity association to the tenant IAM role. Two things follow: the
// gateway reaches Bedrock with no credential of its own, and Bedrock sees the
// tenant's role ARN on every invocation — so per-tenant cost attribution keeps
// working exactly as it did when the app called Bedrock directly.
//
// Idempotent (CreateOrUpdate keyed by stable Platform+route names).
func (r *ModelGatewayReconciler) ensureGatewayResources(ctx context.Context, mg *agentsv1alpha1.ModelGateway, platform *platformv1alpha1.Platform, guardrailID, guardrailVersion string) (string, []string, error) {
	n := namesFor(platform)
	labels := gatewayLabels(platform.Name)

	// 1. EnvoyProxy — the data-plane shape. Two settings carry weight:
	//
	//    envoyServiceAccount pins the proxy to the tenant ServiceAccount, which
	//    is what makes the gateway inherit the tenant's Bedrock identity.
	//
	//    envoyService forces ClusterIP and a deterministic name. The Service
	//    type defaults to LoadBalancer, so leaving it unset provisions an AWS
	//    load balancer per Platform — billed hourly, idle, for a gateway whose
	//    only consumers are in-cluster. The name pin is what keeps the endpoint
	//    below predictable rather than an Envoy-generated one.
	if err := r.ensureUnstructured(ctx, envoyGatewayGV, "EnvoyProxy", n.namespace, n.gateway, labels, map[string]any{
		"provider": map[string]any{
			"type": "Kubernetes",
			"kubernetes": map[string]any{
				"envoyServiceAccount": map[string]any{"name": tenantSAName},
				"envoyService": map[string]any{
					"name": n.gateway,
					"type": "ClusterIP",
				},
			},
		},
	}); err != nil {
		return "", nil, err
	}

	// 2. The Gateway itself, pointed at that EnvoyProxy.
	if err := r.ensureUnstructured(ctx, gatewayAPIGV, "Gateway", n.namespace, n.gateway, labels, map[string]any{
		"gatewayClassName": gatewayClassName,
		"listeners": []any{
			map[string]any{
				"name":     "http",
				"protocol": "HTTP",
				"port":     int64(gatewayListenerPort),
				// Same, not All. Every object this reconciler renders — the
				// Gateway, its AIGatewayRoute, the AIServiceBackends and the
				// Backend — is created in the tenant's own namespace, so Same
				// admits every route the platform legitimately serves.
				//
				// All would let a Route in any namespace on the cluster attach
				// here: Gateway API allows a cross-namespace parentRef, and
				// ReferenceGrant gates backendRefs, not attachment. Anything that
				// attached would be proxied by this tenant's Envoy, which runs
				// under the tenant ServiceAccount and therefore the tenant's IAM
				// role — inheriting its Bedrock model scoping, its quota and its
				// cost attribution — while carrying neither the guardrail
				// headerMutation nor a rate-limit clientSelector that matches it.
				"allowedRoutes": map[string]any{
					"namespaces": map[string]any{"from": "Same"},
				},
			},
		},
		"infrastructure": map[string]any{
			"parametersRef": map[string]any{
				"group": envoyGatewayGV.Group,
				"kind":  "EnvoyProxy",
				"name":  n.gateway,
			},
		},
	}); err != nil {
		return "", nil, err
	}

	// 3. Raise the client buffer limit off its 32KiB default (see the const).
	if err := r.ensureUnstructured(ctx, envoyGatewayGV, "ClientTrafficPolicy", n.namespace, n.gateway+"-buffer", labels, map[string]any{
		"targetRefs": []any{gatewayTargetRef(n.gateway)},
		"connection": map[string]any{"bufferLimit": clientBufferLimit},
	}); err != nil {
		return "", nil, err
	}

	// 4. One Bedrock upstream for the whole gateway. Routes differ by model,
	//    not by endpoint, so they share it.
	bedrockHost := fmt.Sprintf("bedrock-runtime.%s.amazonaws.com", r.Region)
	if err := r.ensureUnstructured(ctx, envoyGatewayGV, "Backend", n.namespace, n.backend, labels, map[string]any{
		"endpoints": []any{
			map[string]any{
				"fqdn": map[string]any{"hostname": bedrockHost, "port": int64(bedrockHTTPSPort)},
			},
		},
	}); err != nil {
		return "", nil, err
	}
	if err := r.ensureUnstructured(ctx, gatewayAPIGV, "BackendTLSPolicy", n.namespace, n.backend+"-tls", labels, map[string]any{
		"targetRefs": []any{
			map[string]any{"group": envoyGatewayGV.Group, "kind": "Backend", "name": n.backend},
		},
		"validation": map[string]any{
			"wellKnownCACertificates": "System",
			"hostname":                bedrockHost,
		},
	}); err != nil {
		return "", nil, err
	}

	// 5. Per route: an AIServiceBackend carrying the upstream wire schema, and a
	//    BackendSecurityPolicy telling the gateway to sign for Bedrock. The
	//    policy names only a region — the AWS default credential chain resolves
	//    the Pod Identity association bound to the tenant ServiceAccount, so no
	//    static credential is stored anywhere.
	var unenforcedGuardrail []string
	rules := make([]any, 0, len(mg.Spec.Routes))
	rateLimitRules := make([]any, 0, len(mg.Spec.Routes))

	for _, route := range mg.Spec.Routes {
		routeName := platform.Name + "-" + route.Name
		routeLbls := routeLabels(platform.Name, routeFamilyLabel(route))

		if err := r.ensureUnstructured(ctx, aiGatewayGV, "AIServiceBackend", n.namespace, routeName, routeLbls, map[string]any{
			"schema": map[string]any{
				"name":    backendSchemaFor(route),
				"version": bedrockAnthropicVersion,
			},
			"backendRef": map[string]any{
				"group": envoyGatewayGV.Group,
				"kind":  "Backend",
				"name":  n.backend,
			},
		}); err != nil {
			return "", nil, err
		}

		if err := r.ensureUnstructured(ctx, aiGatewayGV, "BackendSecurityPolicy", n.namespace, routeName, routeLbls, map[string]any{
			"targetRefs": []any{
				map[string]any{"group": aiGatewayGV.Group, "kind": "AIServiceBackend", "name": routeName},
			},
			"type":           "AWSCredentials",
			"awsCredentials": map[string]any{"region": r.Region},
		}); err != nil {
			return "", nil, err
		}

		// The route rule. Callers select a route by its name — a stable alias
		// like "default" or "escalation" — and modelNameOverride swaps in the
		// real Bedrock identifier upstream. That keeps model ids inside this CR
		// instead of scattered through tenant application config.
		backendRef := map[string]any{
			"name":              routeName,
			"modelNameOverride": effectiveModelID(route),
		}
		gID, gVer, unenforced := resolveGuardrail(mg, route, guardrailID, guardrailVersion)
		if unenforced {
			unenforcedGuardrail = append(unenforcedGuardrail, route.Name)
		}
		if gID != "" && gVer != "" {
			// `set` overwrites, so a caller that sends its own guardrail headers
			// has them replaced rather than honoured.
			backendRef["headerMutation"] = map[string]any{
				"set": []any{
					map[string]any{"name": guardrailIDHeader, "value": gID},
					map[string]any{"name": guardrailVersionHeader, "value": gVer},
				},
			}
		}
		rules = append(rules, map[string]any{
			"matches": []any{
				map[string]any{
					"headers": []any{
						map[string]any{"type": "Exact", "name": aiGatewayModelHeader, "value": route.Name},
					},
				},
			},
			"backendRefs": []any{backendRef},
		})

		if route.RateLimit > 0 {
			rateLimitRules = append(rateLimitRules, map[string]any{
				"clientSelectors": []any{
					map[string]any{
						"headers": []any{
							map[string]any{"name": aiGatewayModelHeader, "value": route.Name},
						},
					},
				},
				"limit": map[string]any{"requests": int64(route.RateLimit), "unit": "Minute"},
			})
		}
	}

	// 6. One AIGatewayRoute holding every rule. Envoy AI Gateway derives the
	//    x-ai-eg-model header from the request body, so the match above routes
	//    on what the caller asked for.
	if err := r.ensureUnstructured(ctx, aiGatewayGV, "AIGatewayRoute", n.namespace, n.gateway, labels, map[string]any{
		"parentRefs": []any{
			map[string]any{"name": n.gateway, "kind": "Gateway", "group": gatewayAPIGV.Group},
		},
		"rules": rules,
	}); err != nil {
		return "", nil, err
	}

	// 7. Rate limits, as one policy on the Gateway with a per-route rule.
	//    Local rather than global: it needs no Redis, matching what the routes
	//    asked for (requests per minute, not tokens).
	if err := r.ensureRateLimitPolicy(ctx, n, labels, rateLimitRules); err != nil {
		return "", nil, err
	}

	return ModelGatewayEndpoint(platform), unenforcedGuardrail, nil
}

// gatewayTargetRef is the policy targetRef shape shared by the traffic policies.
func gatewayTargetRef(name string) map[string]any {
	return map[string]any{"group": gatewayAPIGV.Group, "kind": "Gateway", "name": name}
}

// ensureRateLimitPolicy attaches the per-route request rate limits to the
// Gateway. With no rate-limited route it removes any stale policy, so clearing
// the last limit actually takes effect rather than leaving the old one serving.
func (r *ModelGatewayReconciler) ensureRateLimitPolicy(ctx context.Context, n gatewayNames, labels map[string]string, rules []any) error {
	name := n.gateway + "-ratelimit"
	if len(rules) == 0 {
		return r.deleteUnstructured(ctx, envoyGatewayGV, "BackendTrafficPolicy", n.namespace, name)
	}
	return r.ensureUnstructured(ctx, envoyGatewayGV, "BackendTrafficPolicy", n.namespace, name, labels, map[string]any{
		"targetRefs": []any{gatewayTargetRef(n.gateway)},
		"rateLimit": map[string]any{
			"local": map[string]any{"rules": rules},
		},
	})
}

// ensureUnstructured creates or updates one generated resource. A NoKindMatch
// means the data-plane CRDs are not installed on this cluster, which is a
// Pending state rather than a failure — so it is mapped to a sentinel the
// caller can recognise.
func (r *ModelGatewayReconciler) ensureUnstructured(ctx context.Context, gv schema.GroupVersion, kind, namespace, name string, labels map[string]string, spec map[string]any) error {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gv.WithKind(kind))
	o.SetName(name)
	o.SetNamespace(namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, o, func() error {
		o.SetLabels(labels)
		return unstructured.SetNestedField(o.Object, spec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return errGatewayCRDsNotInstalled
		}
		return fmt.Errorf("ensure %s %s: %w", kind, name, err)
	}
	return nil
}

// deleteUnstructured removes one generated resource, tolerating both an absent
// object and absent CRDs.
func (r *ModelGatewayReconciler) deleteUnstructured(ctx context.Context, gv schema.GroupVersion, kind, namespace, name string) error {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gv.WithKind(kind))
	o.SetName(name)
	o.SetNamespace(namespace)
	if err := r.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
		return fmt.Errorf("delete %s %s: %w", kind, name, err)
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

// cleanupGatewayResources is the finalizer counterpart: removes everything
// ensureGatewayResources creates. Tolerates NoKindMatch (data-plane CRDs not
// installed) and NotFound.
//
// The Gateway goes last. Deleting it first would tear down the Envoy Deployment
// while routes still pointed at it, so in-flight requests would fail rather than
// drain.
func (r *ModelGatewayReconciler) cleanupGatewayResources(ctx context.Context, mg *agentsv1alpha1.ModelGateway) error {
	platform, err := r.resolvePlatform(ctx, mg)
	if err != nil {
		// A Platform deleted before its ModelGateway takes the whole tenant
		// namespace with it, so there is nothing left to reap.
		if errors.Is(err, errPlatformNotFound) {
			return nil
		}
		return err
	}
	n := namesFor(platform)

	for _, route := range mg.Spec.Routes {
		routeName := platform.Name + "-" + route.Name
		if err := r.deleteUnstructured(ctx, aiGatewayGV, "BackendSecurityPolicy", n.namespace, routeName); err != nil {
			return err
		}
		if err := r.deleteUnstructured(ctx, aiGatewayGV, "AIServiceBackend", n.namespace, routeName); err != nil {
			return err
		}
	}
	for _, res := range []struct {
		gv   schema.GroupVersion
		kind string
		name string
	}{
		{envoyGatewayGV, "BackendTrafficPolicy", n.gateway + "-ratelimit"},
		{aiGatewayGV, "AIGatewayRoute", n.gateway},
		{gatewayAPIGV, "BackendTLSPolicy", n.backend + "-tls"},
		{envoyGatewayGV, "Backend", n.backend},
		{envoyGatewayGV, "ClientTrafficPolicy", n.gateway + "-buffer"},
		{gatewayAPIGV, "Gateway", n.gateway},
		{envoyGatewayGV, "EnvoyProxy", n.gateway},
	} {
		if err := r.deleteUnstructured(ctx, res.gv, res.kind, n.namespace, res.name); err != nil {
			return err
		}
	}
	return nil
}

var (
	errGatewayCRDsNotInstalled = errors.New("envoy AI Gateway / Gateway-API CRDs not installed on this cluster")
	errRegionUnset             = errors.New("ModelGatewayReconciler.Region is unset: the Bedrock upstream address and request signing region are both derived from it")
)

// gatewayReconcileResult is what one reconcile pass computed for status.
// A struct rather than another positional return: the endpoint and the
// per-route base URLs are easy to transpose at a call site, and doing so would
// publish a contract that reads plausibly and routes nowhere.
type gatewayReconcileResult struct {
	phase    string
	endpoint string
	// routes is the published client contract, empty until phase is Ready.
	routes []agentsv1alpha1.RouteStatus
	// unenforcedGuardrail names imported routes served without their guardrail.
	unenforcedGuardrail []string
}

// reconcileSelf does the substantive work of ModelGatewayReconciler.
// 'Ready' = gateway resources emitted; 'Pending' = waiting on the data-plane
// CRDs or Platform readiness; error = real failure to retry.
func (r *ModelGatewayReconciler) reconcileSelf(ctx context.Context, mg *agentsv1alpha1.ModelGateway) (gatewayReconcileResult, error) {
	// The Bedrock upstream address is built from the region, so an empty one
	// would emit a Backend pointing at "bedrock-runtime..amazonaws.com" and a
	// BackendSecurityPolicy signing for nowhere. Both would apply cleanly and
	// fail at request time, so refuse to render instead.
	if r.Region == "" {
		return gatewayReconcileResult{}, errRegionUnset
	}

	platform, err := r.resolvePlatform(ctx, mg)
	if err != nil {
		if errors.Is(err, errPlatformNotFound) {
			return gatewayReconcileResult{phase: phasePending}, nil
		}
		return gatewayReconcileResult{}, err
	}
	// Don't emit routes until the Platform itself is Ready. The gateway runs
	// under the tenant ServiceAccount, so before the Platform has minted that
	// account and its Pod Identity association the Envoy pod would come up with
	// no path to Bedrock and every request would fail AccessDenied.
	if platform.Status.Phase != phaseReady {
		return gatewayReconcileResult{phase: phasePending}, nil
	}

	endpoint, unenforcedGuardrail, err := r.ensureGatewayResources(ctx, mg, platform, r.GuardrailID, r.GuardrailVersion)
	if err != nil {
		if errors.Is(err, errGatewayCRDsNotInstalled) {
			return gatewayReconcileResult{phase: phasePending}, nil
		}
		return gatewayReconcileResult{}, err
	}
	return gatewayReconcileResult{
		phase:               phaseReady,
		endpoint:            endpoint,
		routes:              routeStatuses(mg, platform),
		unenforcedGuardrail: unenforcedGuardrail,
	}, nil
}

// modelGatewayApplyStatus writes the computed phase + endpoint + routes +
// conditions.
func (r *ModelGatewayReconciler) modelGatewayApplyStatus(ctx context.Context, mg *agentsv1alpha1.ModelGateway, res gatewayReconcileResult) error {
	phase, unenforcedGuardrail := res.phase, res.unenforcedGuardrail
	mg.Status.Phase = phase
	mg.Status.Endpoint = res.endpoint
	// Cleared rather than left stale on a non-Ready pass: a published base URL
	// is a claim that the route is reachable there, and holding the last good
	// one through a failed reconcile makes an unreachable gateway look served.
	mg.Status.Routes = res.routes
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
		cond.Message = "waiting on Platform or Envoy AI Gateway / Gateway-API CRDs"
	}
	upsertCondition(&mg.Status.Conditions, cond)

	// Surface imported routes whose configured guardrail can't attach inline, so
	// an unguarded imported route is visible rather than silent. True = at least
	// one imported route is served without its guardrail; enforcement via
	// ApplyGuardrail is a tracked follow-up.
	gcond := metav1.Condition{
		Type:               "ImportedRouteGuardrailUnenforced",
		Status:             metav1.ConditionFalse,
		Reason:             "NotApplicable",
		Message:            "no imported route is missing guardrail enforcement",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: mg.Generation,
	}
	if len(unenforcedGuardrail) > 0 {
		gcond.Status = metav1.ConditionTrue
		gcond.Reason = "InlineGuardrailNotApplicable"
		gcond.Message = fmt.Sprintf("imported route(s) %s served without an inline guardrail (Bedrock inline guardrails are foundation-model-only); enforcement for imported models requires ApplyGuardrail", strings.Join(unenforcedGuardrail, ", "))
	}
	upsertCondition(&mg.Status.Conditions, gcond)

	return r.Status().Update(ctx, mg)
}
