/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// mgwScheme knows the operator's own kinds. The generated Gateway-API and Envoy
// AI Gateway resources are written as unstructured, which the fake client
// accepts without their types being registered — so these tests exercise the
// rendered shapes directly.
func mgwScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := agentsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// readyPlatform builds a Platform in the phase the gateway reconciler requires,
// plus a client holding it.
func readyPlatform(t *testing.T, s *runtime.Scheme) (*platformv1alpha1.Platform, client.Client) {
	t.Helper()
	p := newPlatform(ctrlTestPlatform, "team")
	p.Namespace = ctrlTestNS
	p.Status.Phase = phaseReady
	return p, fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
}

// getRendered fetches one generated resource by kind and name from the tenant
// namespace, failing the test if it was never written.
func getRendered(t *testing.T, cl client.Client, gv schema.GroupVersion, kind, name string) *unstructured.Unstructured {
	t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gv.WithKind(kind))
	key := types.NamespacedName{Namespace: "tenants-" + ctrlTestPlatform, Name: name}
	if err := cl.Get(context.Background(), key, o); err != nil {
		t.Fatalf("get %s %s: %v", kind, name, err)
	}
	return o
}

func TestGatewayLabels(t *testing.T) {
	l := gatewayLabels(ctrlTestPlatform)
	if l[LabelPlatform] != ctrlTestPlatform || l["app.kubernetes.io/managed-by"] != "eks-agent-platform" {
		t.Errorf("gatewayLabels: %v", l)
	}
	r := routeLabels(ctrlTestPlatform, "anthropic")
	if r[LabelModelFamily] != "anthropic" || r[LabelPlatform] != ctrlTestPlatform {
		t.Errorf("routeLabels: %v", r)
	}
}

func TestModelGatewayResolvePlatform(t *testing.T) {
	s := mgwScheme(t)
	p := newPlatform(ctrlTestPlatform, "team")
	p.Status.Phase = phaseReady
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s}

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: p.Namespace},
		Spec:       agentsv1alpha1.ModelGatewaySpec{PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform}},
	}
	got, err := r.resolvePlatform(context.Background(), mg)
	if err != nil || got.Name != ctrlTestPlatform {
		t.Fatalf("resolvePlatform: got (%v, %v)", got, err)
	}

	mg.Spec.PlatformRef.Name = "ghost"
	if _, err := r.resolvePlatform(context.Background(), mg); !errors.Is(err, errPlatformNotFound) {
		t.Fatalf("dangling ref must be errPlatformNotFound, got %v", err)
	}
}

// twoRouteGateway carries one plain foundation route with a rate limit and one
// using a cross-region inference profile.
func twoRouteGateway(ns string) *agentsv1alpha1.ModelGateway {
	return &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "chat", ModelFamily: "anthropic", ModelID: "anthropic.claude-sonnet-4-6-v1:0", RateLimit: 60},
				{Name: "cheap", ModelFamily: "anthropic", ModelID: "anthropic.claude-haiku-4-5-v1:0", CrossRegionProfile: "us.anthropic.claude-haiku-4-5-v1:0"},
			},
		},
	}
}

func TestModelGatewayReconcileSelf(t *testing.T) {
	s := mgwScheme(t)

	t.Run("platform not found is pending", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
		phase, _, _, err := r.reconcileSelf(context.Background(), twoRouteGateway(ctrlTestNS))
		if err != nil || phase != phasePending {
			t.Fatalf("missing platform: got (%q, %v)", phase, err)
		}
	})

	t.Run("platform not ready is pending", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
		phase, _, _, err := r.reconcileSelf(context.Background(), twoRouteGateway(ctrlTestNS))
		if err != nil || phase != phasePending {
			t.Fatalf("not-ready platform: got (%q, %v)", phase, err)
		}
	})

	// The Bedrock host and the signing region are both built from Region. An
	// empty one renders a Backend addressed to "bedrock-runtime..amazonaws.com",
	// which applies cleanly and fails only once traffic arrives — so it has to
	// fail here instead.
	t.Run("unset region refuses to render", func(t *testing.T) {
		_, cl := readyPlatform(t, s)
		r := &ModelGatewayReconciler{Client: cl, Scheme: s}
		if _, _, _, err := r.reconcileSelf(context.Background(), twoRouteGateway(ctrlTestNS)); !errors.Is(err, errRegionUnset) {
			t.Fatalf("an unset region must be refused, got %v", err)
		}
	})

	t.Run("ready platform renders the gateway data plane", func(t *testing.T) {
		_, cl := readyPlatform(t, s)
		r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
		phase, endpoint, unenforced, err := r.reconcileSelf(context.Background(), twoRouteGateway(ctrlTestNS))
		if err != nil {
			t.Fatalf("reconcileSelf (ready): %v", err)
		}
		if phase != phaseReady {
			t.Errorf("phase: got %q want Ready", phase)
		}
		// Tenant workloads use this verbatim as their model client's base URL,
		// so the namespace and port are part of the contract, not cosmetics.
		want := "http://" + ctrlTestPlatform + "-gateway.tenants-" + ctrlTestPlatform + ".svc.cluster.local:8080"
		if endpoint != want {
			t.Errorf("endpoint: got %q want %q", endpoint, want)
		}
		if len(unenforced) != 0 {
			t.Errorf("foundation-only routes must not report unenforced guardrails, got %v", unenforced)
		}
	})
}

// TestModelGatewayEnvoyProxyIdentityAndService pins the two EnvoyProxy settings
// that carry the most weight.
//
// The ServiceAccount is what makes the gateway inherit the tenant's Bedrock
// identity: it already holds a Pod Identity association to the tenant role, so
// the gateway needs no credential of its own and Bedrock still attributes every
// invocation to the tenant. Pointing it anywhere else silently collapses
// per-tenant cost attribution onto one shared role.
//
// The Service type defaults to LoadBalancer, so leaving it unset provisions an
// AWS load balancer per Platform — billed hourly, idle, for a gateway only
// reachable in-cluster. The name pin is what keeps the endpoint predictable.
func TestModelGatewayEnvoyProxyIdentityAndService(t *testing.T) {
	s := mgwScheme(t)
	_, cl := readyPlatform(t, s)
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
	if _, _, _, err := r.reconcileSelf(context.Background(), twoRouteGateway(ctrlTestNS)); err != nil {
		t.Fatalf("reconcileSelf: %v", err)
	}

	proxy := getRendered(t, cl, envoyGatewayGV, "EnvoyProxy", ctrlTestPlatform+"-gateway")
	sa, _, _ := unstructured.NestedString(proxy.Object, "spec", "provider", "kubernetes", "envoyServiceAccount", "name")
	if sa != tenantSAName {
		t.Errorf("envoyServiceAccount: got %q want the tenant ServiceAccount %q", sa, tenantSAName)
	}
	svcType, _, _ := unstructured.NestedString(proxy.Object, "spec", "provider", "kubernetes", "envoyService", "type")
	if svcType != "ClusterIP" {
		t.Errorf("envoyService type: got %q want ClusterIP — the default is LoadBalancer", svcType)
	}
	svcName, _, _ := unstructured.NestedString(proxy.Object, "spec", "provider", "kubernetes", "envoyService", "name")
	if svcName != ctrlTestPlatform+"-gateway" {
		t.Errorf("envoyService name: got %q — the endpoint is built from it", svcName)
	}

	// Envoy's default request buffer is 32KiB, which a long prompt exceeds.
	buf := getRendered(t, cl, envoyGatewayGV, "ClientTrafficPolicy", ctrlTestPlatform+"-gateway-buffer")
	limit, _, _ := unstructured.NestedString(buf.Object, "spec", "connection", "bufferLimit")
	if limit != clientBufferLimit {
		t.Errorf("bufferLimit: got %q want %q", limit, clientBufferLimit)
	}
}

// TestModelGatewayAnthropicRoute covers the foundation path: an Anthropic-family
// route gets the schema that preserves the model's native wire format, the
// cross-region inference profile becomes the upstream model id, and the
// baseline guardrail is attached as headers.
func TestModelGatewayAnthropicRoute(t *testing.T) {
	s := mgwScheme(t)
	_, cl := readyPlatform(t, s)
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2", GuardrailID: "baseline-gr", GuardrailVersion: "1"}
	if _, _, _, err := r.reconcileSelf(context.Background(), twoRouteGateway(ctrlTestNS)); err != nil {
		t.Fatalf("reconcileSelf: %v", err)
	}

	// AWSAnthropic accepts native Anthropic Messages on /v1/messages; AWSBedrock
	// would force every caller through an OpenAI translation instead.
	be := getRendered(t, cl, aiGatewayGV, "AIServiceBackend", ctrlTestPlatform+"-chat")
	if name, _, _ := unstructured.NestedString(be.Object, "spec", "schema", "name"); name != "AWSAnthropic" {
		t.Errorf("anthropic route schema: got %q want AWSAnthropic", name)
	}

	// The gateway signs with the tenant's ambient Pod Identity credentials, so
	// the policy names a region and nothing else. A credentialsFile here would
	// mean a static key had been written somewhere.
	pol := getRendered(t, cl, aiGatewayGV, "BackendSecurityPolicy", ctrlTestPlatform+"-chat")
	if typ, _, _ := unstructured.NestedString(pol.Object, "spec", "type"); typ != "AWSCredentials" {
		t.Errorf("security policy type: got %q want AWSCredentials", typ)
	}
	if _, found, _ := unstructured.NestedMap(pol.Object, "spec", "awsCredentials", "credentialsFile"); found {
		t.Error("the gateway must use the ambient credential chain, not a stored credentials file")
	}

	rules := renderedRouteRules(t, cl)
	if len(rules) != 2 {
		t.Fatalf("want a rule per route, got %d", len(rules))
	}

	chat := ruleFor(t, rules, "chat")
	// modelNameOverride is what keeps real Bedrock model ids inside this CR
	// rather than scattered through tenant application config.
	if got := backendRefField(t, chat, "modelNameOverride"); got != "anthropic.claude-sonnet-4-6-v1:0" {
		t.Errorf("chat modelNameOverride: got %q", got)
	}

	cheap := ruleFor(t, rules, "cheap")
	if got := backendRefField(t, cheap, "modelNameOverride"); got != "us.anthropic.claude-haiku-4-5-v1:0" {
		t.Errorf("a route with a cross-region profile must send the profile upstream, got %q", got)
	}

	// Bedrock takes the guardrail as request headers, and `set` overwrites — so
	// a caller sending its own guardrail headers has them replaced. Dropping
	// this mutation would serve every request unguarded while still reporting
	// the route as reconciled.
	headers := backendRefHeaders(t, chat)
	if headers[guardrailIDHeader] != "baseline-gr" || headers[guardrailVersionHeader] != "1" {
		t.Errorf("guardrail headers: got %v", headers)
	}
}

// TestModelGatewayImportedRoute covers the imported-source path: the ARN is the
// upstream model, the schema falls back off the Anthropic wire format, no
// guardrail is attached (Bedrock inline guardrails are foundation-only), the
// route carries the source as its family label, and a configured guardrail is
// reported as unenforced rather than dropped silently.
func TestModelGatewayImportedRoute(t *testing.T) {
	s := mgwScheme(t)
	_, cl := readyPlatform(t, s)
	// A baseline guardrail is configured cluster-wide; an imported route cannot
	// carry it, so it must surface as unenforced.
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2", GuardrailID: "baseline-gr", GuardrailVersion: "1"}

	const arn = "arn:aws:bedrock:us-west-2:123456789012:imported-model/abc123"
	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ctrlTestNS},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "oss", ModelSource: agentsv1alpha1.ModelSourceImported, ModelID: arn},
			},
		},
	}

	phase, _, unenforced, err := r.reconcileSelf(context.Background(), mg)
	if err != nil {
		t.Fatalf("reconcileSelf (imported): %v", err)
	}
	if phase != phaseReady {
		t.Errorf("phase: got %q want Ready", phase)
	}
	if len(unenforced) != 1 || unenforced[0] != "oss" {
		t.Fatalf("imported route with a baseline guardrail must report unenforced [oss], got %v", unenforced)
	}

	be := getRendered(t, cl, aiGatewayGV, "AIServiceBackend", ctrlTestPlatform+"-oss")
	// An imported open-weight model is not Anthropic-shaped.
	if name, _, _ := unstructured.NestedString(be.Object, "spec", "schema", "name"); name != "AWSBedrock" {
		t.Errorf("imported route schema: got %q want AWSBedrock", name)
	}
	if be.GetLabels()[LabelModelFamily] != string(agentsv1alpha1.ModelSourceImported) {
		t.Errorf("imported route family label: got %q want imported", be.GetLabels()[LabelModelFamily])
	}

	rule := ruleFor(t, renderedRouteRules(t, cl), "oss")
	if got := backendRefField(t, rule, "modelNameOverride"); got != arn {
		t.Errorf("imported modelNameOverride: got %q want the ARN", got)
	}
	if len(backendRefHeaders(t, rule)) != 0 {
		t.Error("an imported route must not carry guardrail headers")
	}
}

// TestModelGatewayRateLimit covers that a limit reaches the policy as a
// per-route rule, and that clearing every limit removes the policy rather than
// leaving the old one enforcing.
func TestModelGatewayRateLimit(t *testing.T) {
	s := mgwScheme(t)
	_, cl := readyPlatform(t, s)
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
	ctx := context.Background()

	if _, _, _, err := r.reconcileSelf(ctx, twoRouteGateway(ctrlTestNS)); err != nil {
		t.Fatalf("reconcileSelf: %v", err)
	}
	pol := getRendered(t, cl, envoyGatewayGV, "BackendTrafficPolicy", ctrlTestPlatform+"-gateway-ratelimit")
	rules, _, _ := unstructured.NestedSlice(pol.Object, "spec", "rateLimit", "local", "rules")
	// Only "chat" sets a limit; "cheap" must not get an unrequested one.
	if len(rules) != 1 {
		t.Fatalf("want one rule for the single rate-limited route, got %d", len(rules))
	}
	rule, _ := rules[0].(map[string]any)
	limit, _, _ := unstructured.NestedInt64(rule, "limit", "requests")
	if limit != 60 {
		t.Errorf("limit requests: got %d want 60", limit)
	}
	if unit, _, _ := unstructured.NestedString(rule, "limit", "unit"); unit != "Minute" {
		t.Errorf("limit unit: got %q want Minute — the CRD field is requests per minute", unit)
	}

	// Clearing the limit must retract the policy.
	cleared := twoRouteGateway(ctrlTestNS)
	cleared.Spec.Routes[0].RateLimit = 0
	if _, _, _, err := r.reconcileSelf(ctx, cleared); err != nil {
		t.Fatalf("reconcileSelf (cleared): %v", err)
	}
	stale := &unstructured.Unstructured{}
	stale.SetGroupVersionKind(envoyGatewayGV.WithKind("BackendTrafficPolicy"))
	key := types.NamespacedName{Namespace: "tenants-" + ctrlTestPlatform, Name: ctrlTestPlatform + "-gateway-ratelimit"}
	if err := cl.Get(ctx, key, stale); err == nil {
		t.Error("clearing every rate limit must delete the policy, not leave it enforcing")
	}
}

// TestModelGatewayApplyStatus_UnenforcedGuardrailCondition covers the status
// surfacing: a non-empty unenforced list flips the ImportedRouteGuardrailUnenforced
// condition True, an empty one keeps it False.
func TestModelGatewayApplyStatus_UnenforcedGuardrailCondition(t *testing.T) {
	s := mgwScheme(t)
	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ctrlTestNS},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Routes:      []agentsv1alpha1.ModelRouteSpec{{Name: "oss", ModelSource: agentsv1alpha1.ModelSourceImported, ModelID: "arn:x"}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(mg).WithStatusSubresource(mg).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s}

	find := func() *metav1.Condition {
		for i := range mg.Status.Conditions {
			if mg.Status.Conditions[i].Type == "ImportedRouteGuardrailUnenforced" {
				return &mg.Status.Conditions[i]
			}
		}
		return nil
	}

	if err := r.modelGatewayApplyStatus(context.Background(), mg, phaseReady, "http://gw", []string{"oss"}); err != nil {
		t.Fatalf("applyStatus (unenforced): %v", err)
	}
	if c := find(); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("unenforced guardrail must set the condition True, got %v", c)
	}

	if err := r.modelGatewayApplyStatus(context.Background(), mg, phaseReady, "http://gw", nil); err != nil {
		t.Fatalf("applyStatus (clear): %v", err)
	}
	if c := find(); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("no unenforced guardrail must set the condition False, got %v", c)
	}
}

// TestCleanupGatewayResources covers the finalizer path. The Gateway is what
// owns the Envoy Deployment, so an incomplete cleanup leaves a pod running and
// a Service answering for a tenant that no longer exists.
func TestCleanupGatewayResources(t *testing.T) {
	s := mgwScheme(t)
	ctx := context.Background()

	t.Run("removes everything it rendered", func(t *testing.T) {
		_, cl := readyPlatform(t, s)
		r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
		mg := twoRouteGateway(ctrlTestNS)
		if _, _, _, err := r.reconcileSelf(ctx, mg); err != nil {
			t.Fatalf("reconcileSelf: %v", err)
		}
		if err := r.cleanupGatewayResources(ctx, mg); err != nil {
			t.Fatalf("cleanupGatewayResources: %v", err)
		}
		for _, res := range []struct {
			gv   schema.GroupVersion
			kind string
			name string
		}{
			{gatewayAPIGV, "Gateway", ctrlTestPlatform + "-gateway"},
			{envoyGatewayGV, "EnvoyProxy", ctrlTestPlatform + "-gateway"},
			{envoyGatewayGV, "ClientTrafficPolicy", ctrlTestPlatform + "-gateway-buffer"},
			{envoyGatewayGV, "Backend", ctrlTestPlatform + "-bedrock"},
			{gatewayAPIPolicyGV, "BackendTLSPolicy", ctrlTestPlatform + "-bedrock-tls"},
			{aiGatewayGV, "AIGatewayRoute", ctrlTestPlatform + "-gateway"},
			{aiGatewayGV, "AIServiceBackend", ctrlTestPlatform + "-chat"},
			{aiGatewayGV, "BackendSecurityPolicy", ctrlTestPlatform + "-chat"},
			{envoyGatewayGV, "BackendTrafficPolicy", ctrlTestPlatform + "-gateway-ratelimit"},
		} {
			o := &unstructured.Unstructured{}
			o.SetGroupVersionKind(res.gv.WithKind(res.kind))
			key := types.NamespacedName{Namespace: "tenants-" + ctrlTestPlatform, Name: res.name}
			if err := cl.Get(ctx, key, o); err == nil {
				t.Errorf("%s %s survived cleanup", res.kind, res.name)
			}
		}
	})

	t.Run("tolerates a platform already gone", func(t *testing.T) {
		// Deleting the Platform takes the tenant namespace with it, so there is
		// nothing left to reap and the finalizer must not wedge.
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
		if err := r.cleanupGatewayResources(ctx, twoRouteGateway(ctrlTestNS)); err != nil {
			t.Fatalf("cleanup with no Platform must be a no-op: %v", err)
		}
	})
}

// ── helpers for reading back the rendered AIGatewayRoute ──────────

func renderedRouteRules(t *testing.T, cl client.Client) []any {
	t.Helper()
	route := getRendered(t, cl, aiGatewayGV, "AIGatewayRoute", ctrlTestPlatform+"-gateway")
	rules, found, err := unstructured.NestedSlice(route.Object, "spec", "rules")
	if err != nil || !found {
		t.Fatalf("AIGatewayRoute has no rules: found=%v err=%v", found, err)
	}
	return rules
}

// ruleFor returns the rule matching the given route name on the x-ai-eg-model
// header — the header Envoy AI Gateway derives from the request body.
func ruleFor(t *testing.T, rules []any, routeName string) map[string]any {
	t.Helper()
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		matches, _, _ := unstructured.NestedSlice(rule, "matches")
		for _, m := range matches {
			match, ok := m.(map[string]any)
			if !ok {
				continue
			}
			headers, _, _ := unstructured.NestedSlice(match, "headers")
			for _, h := range headers {
				header, ok := h.(map[string]any)
				if !ok {
					continue
				}
				if header["name"] == "x-ai-eg-model" && header["value"] == routeName {
					return rule
				}
			}
		}
	}
	t.Fatalf("no rule matches x-ai-eg-model=%q", routeName)
	return nil
}

func firstBackendRef(t *testing.T, rule map[string]any) map[string]any {
	t.Helper()
	refs, found, _ := unstructured.NestedSlice(rule, "backendRefs")
	if !found || len(refs) == 0 {
		t.Fatal("rule has no backendRefs")
	}
	ref, ok := refs[0].(map[string]any)
	if !ok {
		t.Fatal("backendRef is not an object")
	}
	return ref
}

func backendRefField(t *testing.T, rule map[string]any, field string) string {
	t.Helper()
	v, _ := firstBackendRef(t, rule)[field].(string)
	return v
}

// backendRefHeaders flattens the backendRef's headerMutation into name→value.
func backendRefHeaders(t *testing.T, rule map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	set, found, _ := unstructured.NestedSlice(firstBackendRef(t, rule), "headerMutation", "set")
	if !found {
		return out
	}
	for _, h := range set {
		header, ok := h.(map[string]any)
		if !ok {
			continue
		}
		name, _ := header["name"].(string)
		value, _ := header["value"].(string)
		out[name] = value
	}
	return out
}
