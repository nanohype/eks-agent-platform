/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// submitEvalRun runs ensureArgoWorkflow against a fake client holding the given
// gateway and returns the parameters on the object the reconciler created.
func submitEvalRun(t *testing.T, p *platformv1alpha1.Platform, mg *agentsv1alpha1.ModelGateway) map[string]string {
	t.Helper()
	ctx := context.Background()
	s := evalScheme(t)
	fleet := agentFleet() // its one agent is configured for route "chat"
	fleet.Status.Phase = phaseReady
	suite := evalSuite()
	suite.Spec.Cases = []governancev1alpha1.EvalCase{{Name: "greet", Input: "hi"}}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, fleet, mg).Build()
	r := &EvalReconciler{Client: cl, Scheme: s, ReportsBucket: testReportsBucket}

	if err := r.ensureArgoWorkflow(ctx, suite, p, fleet); err != nil {
		t.Fatalf("ensureArgoWorkflow: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: "Workflow",
	})
	key := client.ObjectKey{Namespace: r.evalRunnerNamespace(), Name: evalWorkflowName(suite)}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatalf("get submitted Workflow %s: %v", key, err)
	}
	params := workflowParams(t, obj)
	if len(params) == 0 {
		t.Fatal("the submitted Workflow carries no parameters; this check would pass vacuously")
	}
	return params
}

// TestEvalRunGetsThePublishedRouteContract is the eval-path counterpart of
// TestAgentGetsTheBaseURLNotTheGatewayRoot.
//
// The gateway serves each wire format under its own prefix. Envoy AI Gateway's
// extproc registers a body-parsing processor per endpoint path, so a request to
// the bare root never has its model name read out of the body, never gets the
// x-ai-eg-model header, and matches no route rule. A run handed the root
// executes every case against a gateway that reports healthy and fails all of
// them — after the tenant has been billed for the inference.
//
// Two arms, deliberately. One arm over an Anthropic route cannot tell "reads
// the published contract" apart from "hardcodes Anthropic": both produce the
// same three values. The OpenAI arm is what makes the assertion about status
// rather than about a format the test itself named.
func TestEvalRunGetsThePublishedRouteContract(t *testing.T) {
	t.Run("anthropic route", func(t *testing.T) {
		p := readyPlatformIn()
		params := submitEvalRun(t, p, publishedGateway(p, "chat"))

		want := RouteBaseURL(p, agentsv1alpha1.RouteAPIAnthropic)
		if got := params["model-route-base-url"]; got != want {
			t.Errorf("model-route-base-url = %q, want the published base %q", got, want)
		}
		if got := params["model-route"]; got != "chat" {
			t.Errorf("model-route = %q, want the fleet's route %q — the gateway derives x-ai-eg-model from this", got, "chat")
		}
		if got, want := params["model-route-api"], string(agentsv1alpha1.RouteAPIAnthropic); got != want {
			t.Errorf("model-route-api = %q, want the published wire format %q", got, want)
		}
		// And no parameter at all may carry the bare root. Checked by value
		// rather than by naming the parameter that used to: a rename would
		// satisfy a name-based check while the value stayed wrong, which is the
		// same shape of pass this test exists to refuse.
		for name, value := range params {
			if value == ModelGatewayEndpoint(p) {
				t.Errorf("parameter %q is the bare gateway root %q — the eval run assembles a client base from it and reaches no body processor", name, value)
			}
		}
	})

	// The discriminating arm. A reconciler that derives the contract instead of
	// reading it passes the arm above and fails this one.
	t.Run("openai route", func(t *testing.T) {
		p := readyPlatformIn()
		mg := publishedGateway(p, "chat")
		mg.Status.Routes[0].API = agentsv1alpha1.RouteAPIOpenAI
		mg.Status.Routes[0].BaseURL = RouteBaseURL(p, agentsv1alpha1.RouteAPIOpenAI)
		params := submitEvalRun(t, p, mg)

		if got, want := params["model-route-api"], string(agentsv1alpha1.RouteAPIOpenAI); got != want {
			t.Errorf("model-route-api = %q, want the PUBLISHED wire format %q — the runner picks its request body and its response parser from this", got, want)
		}
		if got, want := params["model-route-base-url"], RouteBaseURL(p, agentsv1alpha1.RouteAPIOpenAI); got != want {
			t.Errorf("model-route-base-url = %q, want the PUBLISHED base %q — an Anthropic body posted at the OpenAI prefix fails every case against a healthy gateway", got, want)
		}
	})
}
