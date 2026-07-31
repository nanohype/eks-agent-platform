/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// TestEffectiveRouteAPI covers the resolution the published contract is built
// from. The derived case is the load-bearing one: there is no static default,
// because any single default is wrong for whichever kind of route it does not
// describe.
func TestEffectiveRouteAPI(t *testing.T) {
	cases := []struct {
		name  string
		route agentsv1alpha1.ModelRouteSpec
		want  agentsv1alpha1.RouteAPI
	}{
		{
			// The only family reachable in its own wire shape.
			name:  "anthropic foundation derives Anthropic",
			route: agentsv1alpha1.ModelRouteSpec{Name: "chat", ModelFamily: "anthropic", ModelID: "anthropic.claude-sonnet-4-6-v1:0"},
			want:  agentsv1alpha1.RouteAPIAnthropic,
		},
		{
			// The case a static `Anthropic` default would have broken: an
			// embeddings route is not reachable as Anthropic at all.
			name:  "titan embeddings derives OpenAI",
			route: agentsv1alpha1.ModelRouteSpec{Name: "embeddings", ModelFamily: "amazon-titan", ModelID: "amazon.titan-embed-text-v2:0"},
			want:  agentsv1alpha1.RouteAPIOpenAI,
		},
		{
			name:  "other foundation families derive OpenAI",
			route: agentsv1alpha1.ModelRouteSpec{Name: "oss", ModelFamily: "meta", ModelID: "meta.llama3-70b"},
			want:  agentsv1alpha1.RouteAPIOpenAI,
		},
		{
			// An imported model carries no family, and reaches callers through
			// the Bedrock translator regardless.
			name:  "imported derives OpenAI",
			route: agentsv1alpha1.ModelRouteSpec{Name: "custom", ModelSource: agentsv1alpha1.ModelSourceImported, ModelID: "arn:aws:bedrock:us-west-2:123456789012:imported-model/abc"},
			want:  agentsv1alpha1.RouteAPIOpenAI,
		},
		{
			// The portability case. A Claude route pinned to OpenAI keeps that
			// contract when it is later repointed at an open-weight model, so
			// the swap is a CR edit and no app changes.
			name:  "explicit OpenAI wins over an anthropic family",
			route: agentsv1alpha1.ModelRouteSpec{Name: "chat", ModelFamily: "anthropic", ModelID: "anthropic.claude-sonnet-4-6-v1:0", API: agentsv1alpha1.RouteAPIOpenAI},
			want:  agentsv1alpha1.RouteAPIOpenAI,
		},
		{
			name:  "explicit Anthropic is honoured",
			route: agentsv1alpha1.ModelRouteSpec{Name: "chat", ModelFamily: "anthropic", ModelID: "anthropic.claude-sonnet-4-6-v1:0", API: agentsv1alpha1.RouteAPIAnthropic},
			want:  agentsv1alpha1.RouteAPIAnthropic,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveRouteAPI(c.route); got != c.want {
				t.Errorf("effectiveRouteAPI = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRouteBaseURL pins the prefixes. These are Envoy AI Gateway's registered
// endpoint paths, not a local convention: a base URL missing its prefix
// produces requests to a path no processor is registered for, which never get
// the model extracted from the body and match no route rule. The gateway stays
// healthy and every call fails, so the exact strings are the contract.
func TestRouteBaseURL(t *testing.T) {
	p := newPlatform(ctrlTestPlatform, "team")
	root := "http://" + ctrlTestPlatform + "-gateway.tenants-" + ctrlTestPlatform + ".svc.cluster.local:8080"

	if got, want := RouteBaseURL(p, agentsv1alpha1.RouteAPIAnthropic), root+"/anthropic"; got != want {
		t.Errorf("Anthropic base: got %q want %q", got, want)
	}
	if got, want := RouteBaseURL(p, agentsv1alpha1.RouteAPIOpenAI), root+"/v1"; got != want {
		t.Errorf("OpenAI base: got %q want %q", got, want)
	}

	// The endpoint alone is never a usable base for either client. This is the
	// regression the tenant apps shipped: pointed at the root, the Anthropic
	// SDK requests /v1/messages, a path the gateway routes nowhere.
	if RouteBaseURL(p, agentsv1alpha1.RouteAPIAnthropic) == ModelGatewayEndpoint(p) {
		t.Error("the Anthropic base URL must not be the bare gateway endpoint")
	}
	if RouteBaseURL(p, agentsv1alpha1.RouteAPIOpenAI) == ModelGatewayEndpoint(p) {
		t.Error("the OpenAI base URL must not be the bare gateway endpoint")
	}
}

// mixedRouteGateway pairs an Anthropic-family chat route with a Titan
// embeddings route — the shape every tenant app actually declares, and the one
// that proves the contract is per-route rather than per-gateway.
func mixedRouteGateway() *agentsv1alpha1.ModelGateway {
	return &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ctrlTestNS},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "default", ModelFamily: "anthropic", ModelID: "anthropic.claude-sonnet-4-6-v1:0"},
				{Name: "embeddings", ModelFamily: "amazon-titan", ModelID: "amazon.titan-embed-text-v2:0"},
			},
		},
	}
}

// TestReconcilePublishesRouteContract is the end-to-end assertion: a ready
// reconcile publishes, per route, the wire format and the base URL a client of
// that format is configured with — two different base URLs on one gateway.
func TestReconcilePublishesRouteContract(t *testing.T) {
	s := mgwScheme(t)
	cl := readyPlatform(t, s)
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}

	res, err := r.reconcileSelf(context.Background(), mixedRouteGateway())
	if err != nil {
		t.Fatalf("reconcileSelf: %v", err)
	}
	if res.phase != phaseReady {
		t.Fatalf("phase: got %q want Ready", res.phase)
	}
	if len(res.routes) != 2 {
		t.Fatalf("want a published contract per route, got %d", len(res.routes))
	}

	byName := map[string]agentsv1alpha1.RouteStatus{}
	for _, rt := range res.routes {
		byName[rt.Name] = rt
	}

	chat, ok := byName["default"]
	if !ok {
		t.Fatal(`no published contract for the "default" route`)
	}
	if chat.API != agentsv1alpha1.RouteAPIAnthropic {
		t.Errorf("default route api: got %q want Anthropic", chat.API)
	}
	if want := res.endpoint + "/anthropic"; chat.BaseURL != want {
		t.Errorf("default route baseURL: got %q want %q", chat.BaseURL, want)
	}

	emb, ok := byName["embeddings"]
	if !ok {
		t.Fatal(`no published contract for the "embeddings" route`)
	}
	if emb.API != agentsv1alpha1.RouteAPIOpenAI {
		t.Errorf("embeddings route api: got %q want OpenAI", emb.API)
	}
	if want := res.endpoint + "/v1"; emb.BaseURL != want {
		t.Errorf("embeddings route baseURL: got %q want %q", emb.BaseURL, want)
	}

	// The whole point of publishing per route: one gateway, two contracts.
	if chat.BaseURL == emb.BaseURL {
		t.Error("routes of different wire formats must not publish the same base URL")
	}
}

// TestApplyStatusClearsRoutesWhenNotReady covers the direction that matters for
// trusting the field. A published base URL is a claim the route is reachable
// there; carrying the last good one through a failed reconcile would make an
// unreachable gateway read as served.
func TestApplyStatusClearsRoutesWhenNotReady(t *testing.T) {
	s := mgwScheme(t)
	mg := mixedRouteGateway()
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(mg).WithStatusSubresource(mg).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s}

	ready := gatewayReconcileResult{
		phase:    phaseReady,
		endpoint: "http://gw",
		routes: []agentsv1alpha1.RouteStatus{
			{Name: "default", API: agentsv1alpha1.RouteAPIAnthropic, BaseURL: "http://gw/anthropic"},
		},
	}
	if err := r.modelGatewayApplyStatus(context.Background(), mg, ready); err != nil {
		t.Fatalf("applyStatus (ready): %v", err)
	}
	if len(mg.Status.Routes) != 1 {
		t.Fatalf("a ready pass must publish the route contract, got %v", mg.Status.Routes)
	}

	if err := r.modelGatewayApplyStatus(context.Background(), mg, gatewayReconcileResult{phase: phasePending}); err != nil {
		t.Fatalf("applyStatus (pending): %v", err)
	}
	if len(mg.Status.Routes) != 0 {
		t.Errorf("a pending pass must clear the route contract, got %v", mg.Status.Routes)
	}
}
