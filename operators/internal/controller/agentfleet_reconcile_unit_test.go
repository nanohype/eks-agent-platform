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

func TestReconcileFleetSelf(t *testing.T) {
	s := fleetScheme(t)

	t.Run("platform not found is pending", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, ready, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || phase != phasePending || ready != 0 {
			t.Fatalf("missing platform: got (%q, %d, %v)", phase, ready, err)
		}
	})

	t.Run("platform not ready is pending", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		p.Namespace = ctrlTestNS
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, _, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || phase != phasePending {
			t.Fatalf("not-ready platform: got (%q, %v)", phase, err)
		}
	})

	t.Run("suspended platform tears the fleet down", func(t *testing.T) {
		p := readyPlatformIn()
		p.Status.Phase = phaseSuspended
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, ready, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil || phase != phaseSuspended || ready != 0 {
			t.Fatalf("suspended platform: got (%q, %d, %v)", phase, ready, err)
		}
	})

	t.Run("ready platform renders the fleet", func(t *testing.T) {
		p := readyPlatformIn()
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		r := &AgentFleetReconciler{Client: cl, Scheme: s}
		phase, ready, err := r.reconcileFleetSelf(context.Background(), agentFleet())
		if err != nil {
			t.Fatalf("reconcileFleetSelf (ready): %v", err)
		}
		if phase != phaseReady || ready != 1 {
			t.Errorf("ready fleet: got (%q, %d) want (Ready, 1)", phase, ready)
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
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	if _, _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
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
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	if _, _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
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

	if _, _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
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
