/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func fleetScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		platformv1alpha1.AddToScheme, agentsv1alpha1.AddToScheme,
		networkingv1.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func agentFleet() *agentsv1alpha1.AgentFleet {
	return &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: ctrlTestNS, Generation: 1},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "triage", SystemPrompt: "you triage", ModelRoute: "chat", Image: "ghcr.io/acme/agent:v1"},
			},
		},
	}
}

func readyPlatformIn() *platformv1alpha1.Platform {
	p := newPlatform(ctrlTestPlatform, "team")
	p.Namespace = ctrlTestNS
	p.Status.Phase = phaseReady
	p.Status.Namespace = PlatformNamespace(p)
	return p
}

// publishedGateway is a ModelGateway that has already published its route
// contract, which is what a fleet reads to configure its agents' model client.
func publishedGateway(p *platformv1alpha1.Platform, names ...string) *agentsv1alpha1.ModelGateway {
	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: p.Namespace},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: p.Name},
		},
	}
	for _, name := range names {
		mg.Status.Routes = append(mg.Status.Routes, agentsv1alpha1.RouteStatus{
			Name:    name,
			API:     agentsv1alpha1.RouteAPIAnthropic,
			BaseURL: RouteBaseURL(p, agentsv1alpha1.RouteAPIAnthropic),
		})
	}
	return mg
}

func TestReconcileFleetSelf(t *testing.T) {
	s := fleetScheme(t)

	t.Run("platform not found is pending", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		res, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || res.phase != phasePending || res.readyAgents != 0 {
			t.Fatalf("missing platform: got (%q, %d, %v)", res.phase, res.readyAgents, err)
		}
	})

	t.Run("platform not ready is pending", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		res, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || res.phase != phasePending {
			t.Fatalf("not-ready platform: got (%q, %v)", res.phase, err)
		}
	})

	t.Run("suspended platform tears the fleet down", func(t *testing.T) {
		p := readyPlatformIn()
		p.Status.Phase = phaseSuspended
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		res, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || res.phase != phaseSuspended || res.readyAgents != 0 {
			t.Fatalf("suspended platform: got (%q, %d, %v)", res.phase, res.readyAgents, err)
		}
	})

	t.Run("ready platform renders the fleet", func(t *testing.T) {
		p := readyPlatformIn()
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, publishedGateway(p, "chat")).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		res, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil {
			t.Fatalf("reconcileFleetSelf (ready): %v", err)
		}
		if res.phase != phaseReady || res.readyAgents != 1 {
			t.Errorf("ready fleet: got (%q, %d) want (Ready, 1)", res.phase, res.readyAgents)
		}
		// The host containment NetworkPolicy landed on the host.
		var np networkingv1.NetworkPolicy
		if err := cl.Get(context.Background(), types.NamespacedName{Name: "fleet-squad", Namespace: PlatformNamespace(p)}, &np); err != nil {
			t.Errorf("fleet NetworkPolicy not created on the host: %v", err)
		}
	})
}

func TestCleanupTargetClient_NamespaceTierUsesHost(t *testing.T) {
	s := fleetScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}
	if got := r.cleanupTargetClient(context.Background(), newPlatform(ctrlTestPlatform, "team")); got != cl {
		t.Error("namespace tier cleanup must delete through the host client")
	}
}

func TestCleanupFleetResources_ToleratesMissing(t *testing.T) {
	s := fleetScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}
	p := readyPlatformIn()
	// Nothing was created; every delete NotFounds and must be tolerated.
	if err := r.cleanupFleetResources(context.Background(), cl, agentFleet(), p); err != nil {
		t.Fatalf("cleanup on a fresh cluster must be a no-op: %v", err)
	}
}

func TestResolveFleetPlatform_WrapsNonNotFound(t *testing.T) {
	s := fleetScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}
	_, err := r.resolvePlatform(context.Background(), agentFleet())
	if !errors.Is(err, errPlatformNotFound) {
		t.Fatalf("a missing platform must be errPlatformNotFound, got %v", err)
	}
}

// TestAgentDeploymentRunsAsTheTenant pins the property the whole design rests
// on: an agent executes under the tenant ServiceAccount.
//
// That account carries the Pod Identity association to the tenant IAM role, so
// an action the agent takes is taken as the tenant and lands in the Kubernetes
// audit log under the tenant's identity. Run it under any other account and the
// audit record names something other than the agent that asked — which is
// precisely the gap that makes an agent's account of its own actions
// impossible to confirm or refute.
func TestAgentDeploymentRunsAsTheTenant(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	fleet := scalingFleet("")
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, publishedGateway(p, "chat")).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	if _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
		t.Fatalf("reconcileFleetSelf: %v", err)
	}

	deploy := &appsv1.Deployment{}
	key := types.NamespacedName{
		Name:      agentDeploymentName(fleet, fleet.Spec.Agents[0].Name),
		Namespace: PlatformNamespace(p),
	}
	if err := cl.Get(ctx, key, deploy); err != nil {
		t.Fatalf("get agent Deployment: %v", err)
	}

	pod := deploy.Spec.Template.Spec
	if pod.ServiceAccountName != tenantSAName {
		t.Errorf("ServiceAccountName = %q, want the tenant account %q", pod.ServiceAccountName, tenantSAName)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("want exactly one container, got %d", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != fleet.Spec.Agents[0].Image {
		t.Errorf("image = %q, want the tenant's own image %q", c.Image, fleet.Spec.Agents[0].Image)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	// An endpoint and a route name, never a credential and never a model id.
	// A model id here would mean the app, not the CR, decides what runs.
	if env["MODEL_GATEWAY_ENDPOINT"] != ModelGatewayEndpoint(p) {
		t.Errorf("MODEL_GATEWAY_ENDPOINT = %q, want the Platform's gateway", env["MODEL_GATEWAY_ENDPOINT"])
	}
	if env["MODEL_ROUTE"] != fleet.Spec.Agents[0].ModelRoute {
		t.Errorf("MODEL_ROUTE = %q, want the route the AgentSpec names", env["MODEL_ROUTE"])
	}
	for _, banned := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "ANTHROPIC_API_KEY"} {
		if _, found := env[banned]; found {
			t.Errorf("%s is set on the agent — the gateway holds the credential, the agent holds none", banned)
		}
	}

	// The claim stream has to say which agent made a claim. The agent SDK's own
	// agent id defaults to a constant, so without these every agent in the fleet
	// emits indistinguishable spans and no claim can be matched to a record.
	attrs := env[otelResourceAttrsEnvName]
	for _, want := range []string{
		"agents.tenant=" + p.Spec.Tenant,
		"agents.platform=" + p.Name,
		"agents.fleet=" + fleet.Name,
		"agents.agent=" + fleet.Spec.Agents[0].Name,
	} {
		if !strings.Contains(attrs, want) {
			t.Errorf("%s missing from %s=%q", want, otelResourceAttrsEnvName, attrs)
		}
	}
}

// TestAgentDeploymentLeavesReplicasToKEDA covers the handoff between the two
// controllers that both have an opinion about replica count.
//
// The operator sets the floor once, at creation. If it kept setting it, it
// would undo every scale-up moments after KEDA made it, and the two would flap
// against each other for as long as the fleet existed — with the Deployment
// reporting healthy throughout.
func TestAgentDeploymentLeavesReplicasToKEDA(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	fleet := scalingFleet("")
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, publishedGateway(p, "chat")).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	if _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	key := types.NamespacedName{
		Name:      agentDeploymentName(fleet, fleet.Spec.Agents[0].Name),
		Namespace: PlatformNamespace(p),
	}
	deploy := &appsv1.Deployment{}
	if err := cl.Get(ctx, key, deploy); err != nil {
		t.Fatalf("get after create: %v", err)
	}

	// Stand in for KEDA having scaled the fleet up under load.
	scaled := int32(7)
	deploy.Spec.Replicas = &scaled
	if err := cl.Update(ctx, deploy); err != nil {
		t.Fatalf("simulate KEDA scale-up: %v", err)
	}

	if _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	if err := cl.Get(ctx, key, deploy); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != scaled {
		got := "nil"
		if deploy.Spec.Replicas != nil {
			got = fmt.Sprint(*deploy.Spec.Replicas)
		}
		t.Errorf("replicas = %s after reconcile, want KEDA's %d left alone", got, scaled)
	}
}

// TestAgentGetsTheBaseURLNotTheGatewayRoot is the regression for the failure
// that had already been fixed in the tenant apps and was still live here.
//
// The gateway serves each wire format under its own prefix. Envoy AI Gateway's
// extproc registers a body-parsing processor per endpoint path, so a request to
// the bare root never has its model name read out of the body, never gets the
// x-ai-eg-model header, and matches no route rule. An SDK handed the root
// appends /v1/messages and lands exactly there — the gateway reports healthy
// and every model call fails.
//
// So the assertion is not "an endpoint is set" but "the endpoint set is the one
// the gateway published", checked against RouteBaseURL rather than a string
// spelled out here, which would agree with a wrong value just as readily.
func TestAgentGetsTheBaseURLNotTheGatewayRoot(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	fleet := agentFleet()
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, publishedGateway(p, "chat")).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	if _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
		t.Fatalf("reconcileFleetSelf: %v", err)
	}

	deploy := &appsv1.Deployment{}
	key := types.NamespacedName{
		Name:      agentDeploymentName(fleet, fleet.Spec.Agents[0].Name),
		Namespace: PlatformNamespace(p),
	}
	if err := cl.Get(ctx, key, deploy); err != nil {
		t.Fatalf("get agent Deployment: %v", err)
	}
	env := map[string]string{}
	for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}

	want := RouteBaseURL(p, agentsv1alpha1.RouteAPIAnthropic)
	if env["MODEL_ROUTE_BASE_URL"] != want {
		t.Errorf("MODEL_ROUTE_BASE_URL = %q, want the published base %q", env["MODEL_ROUTE_BASE_URL"], want)
	}
	if env["MODEL_ROUTE_BASE_URL"] == ModelGatewayEndpoint(p) {
		t.Error("MODEL_ROUTE_BASE_URL is the bare gateway root; an SDK given that reaches no body processor")
	}
	if env["MODEL_ROUTE_API"] != string(agentsv1alpha1.RouteAPIAnthropic) {
		t.Errorf("MODEL_ROUTE_API = %q, want the published wire format", env["MODEL_ROUTE_API"])
	}
	// The gateway's own address stays available, and stays distinct from the
	// client's base URL — an agent may want one without the other.
	if env["MODEL_GATEWAY_ENDPOINT"] != ModelGatewayEndpoint(p) {
		t.Errorf("MODEL_GATEWAY_ENDPOINT = %q, want the gateway address", env["MODEL_GATEWAY_ENDPOINT"])
	}
}

// TestFleetRefusesAnUnpublishedRoute covers the other half: a route name that
// resolves to nothing.
//
// Emitting the Deployment anyway would produce a pod that starts, passes its
// probes and fails every model call — worse than no pod, because the fleet
// reports Ready. The status has to name the route and say what was available,
// or whoever wrote the typo has nothing to go on.
func TestFleetRefusesAnUnpublishedRoute(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	fleet := agentFleet() // wants "chat"
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(p, publishedGateway(p, "analysis", "embeddings")).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	res, err := r.reconcileFleetSelf(ctx, fleet)
	if err != nil {
		t.Fatalf("a spec error must be reported as status, not returned: %v", err)
	}
	if res.phase != phaseFailed {
		t.Errorf("phase = %q, want %q", res.phase, phaseFailed)
	}
	if res.reason != "RouteNotPublished" {
		t.Errorf("reason = %q, want RouteNotPublished", res.reason)
	}
	for _, want := range []string{"chat", "analysis", "embeddings"} {
		if !strings.Contains(res.message, want) {
			t.Errorf("message %q does not mention %q", res.message, want)
		}
	}

	// And no pod was written for it.
	deploy := &appsv1.Deployment{}
	key := types.NamespacedName{
		Name:      agentDeploymentName(fleet, fleet.Spec.Agents[0].Name),
		Namespace: PlatformNamespace(p),
	}
	if err := cl.Get(ctx, key, deploy); err == nil {
		t.Error("a Deployment was created for an agent whose route does not exist")
	}
}

// TestFleetWaitsForAnUnpublishedGateway separates timing from misconfiguration.
// A gateway that has not reconciled yet resolves itself; reporting Failed there
// would make an ordering artefact look like a broken manifest.
func TestFleetWaitsForAnUnpublishedGateway(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()

	for _, tc := range []struct {
		name    string
		objects []client.Object
	}{
		{"no gateway at all", []client.Object{p}},
		{"gateway with no published routes", []client.Object{p, publishedGateway(p)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objects...).Build()
			r := &AgentFleetReconciler{Client: cl, Scheme: s}
			res, err := r.reconcileFleetSelf(ctx, agentFleet())
			if err != nil {
				t.Fatalf("reconcileFleetSelf: %v", err)
			}
			if res.phase != phasePending {
				t.Errorf("phase = %q, want %q — the gateway resolves itself", res.phase, phasePending)
			}
			if res.reason != "" {
				t.Errorf("reason = %q, want none: this is ordering, not a spec error", res.reason)
			}
		})
	}
}

// TestTwoGatewaysForOnePlatformIsRefused. Every rendered object is named from
// the Platform, so two ModelGateways referencing one Platform are two writers of
// one Gateway. Picking either would publish a contract the tenant may not be
// getting — the failure would surface as intermittent routing, not as config.
func TestTwoGatewaysForOnePlatformIsRefused(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	second := publishedGateway(p, "chat")
	second.Name = "gw-2"
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(p, publishedGateway(p, "chat"), second).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	res, err := r.reconcileFleetSelf(ctx, agentFleet())
	if err != nil {
		t.Fatalf("reconcileFleetSelf: %v", err)
	}
	if res.phase != phaseFailed || res.reason != "GatewayAmbiguous" {
		t.Errorf("got (%q, %q), want (Failed, GatewayAmbiguous)", res.phase, res.reason)
	}
}

// TestPublishedRoutesIgnoresAnotherPlatformsGateway. Gateways for every tenant
// can share a namespace; matching on namespace alone would hand one tenant
// another's routes.
func TestPublishedRoutesIgnoresAnotherPlatformsGateway(t *testing.T) {
	s := fleetScheme(t)
	p := readyPlatformIn()
	other := readyPlatformIn()
	other.Name = "someone-else"
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(p, publishedGateway(other, "chat")).Build()

	_, err := publishedRoutes(context.Background(), cl, p)
	if !errors.Is(err, errGatewayNotFound) {
		t.Fatalf("got %v, want errGatewayNotFound", err)
	}
}

// TestGatewayToFleetsEnqueuesOnlyItsOwn. Without the watch, a route contract
// published after a fleet went Ready never reaches it. Enqueueing every fleet
// in the namespace instead would reconcile other tenants' fleets on one
// tenant's gateway change.
func TestGatewayToFleetsEnqueuesOnlyItsOwn(t *testing.T) {
	s := fleetScheme(t)
	p := readyPlatformIn()
	mine := agentFleet()
	theirs := agentFleet()
	theirs.Name = "other-squad"
	theirs.Spec.PlatformRef.Name = "someone-else"
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(mine, theirs).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	got := r.gatewayToFleets(context.Background(), publishedGateway(p, "chat"))
	if len(got) != 1 || got[0].Name != mine.Name {
		t.Fatalf("enqueued %v, want just %q", got, mine.Name)
	}
}
