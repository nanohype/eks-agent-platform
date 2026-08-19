/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// scalingFleet builds a scaling-enabled fleet with two agents. The whole point
// of the KEDA fix is that each agent becomes its own Deployment, so the
// two-agent shape exercises the per-agent ScaledObject path.
func scalingFleet(queueURL string) *agentsv1alpha1.AgentFleet {
	return &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: ctrlTestNS, Generation: 1},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "triage", SystemPrompt: "you triage", ModelRoute: "chat", Image: "ghcr.io/acme/agent:v1"},
				{Name: "responder", SystemPrompt: "you respond", ModelRoute: "chat", Image: "ghcr.io/acme/agent:v1"},
			},
			Scaling: agentsv1alpha1.ScalingSpec{Enabled: true, QueueURL: queueURL},
		},
	}
}

// TestScaledObjectTargetsAgentDeployment is the regression for the bug where
// the ScaledObject targeted "fleet-<name>" — a Deployment nothing creates. The
// operator names each agent's Deployment "<fleet>-<agent>", and the
// ScaledObject's scaleTargetRef must resolve to that exact name. The assertion
// cross-checks the target against the Deployment the reconcile path actually
// created, so it can never again point at a phantom Deployment.
func TestScaledObjectTargetsAgentDeployment(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	fleet := scalingFleet("")
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, publishedGateway(p, "chat")).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	res, err := r.reconcileFleetSelf(ctx, fleet)
	if err != nil || res.phase != phaseReady {
		t.Fatalf("reconcileFleetSelf: phase=%q err=%v", res.phase, err)
	}

	ns := PlatformNamespace(p)
	soGVK := schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

	for _, agent := range fleet.Spec.Agents {
		want := agentDeploymentName(fleet, agent.Name)

		// The Deployment the reconcile path created for this agent.
		deploy := &appsv1.Deployment{}
		if err := cl.Get(ctx, types.NamespacedName{Name: want, Namespace: ns}, deploy); err != nil {
			t.Fatalf("agent Deployment %s/%s not created: %v", ns, want, err)
		}

		// A per-agent ScaledObject exists, targeting that Deployment name.
		so := &unstructured.Unstructured{}
		so.SetGroupVersionKind(soGVK)
		if err := cl.Get(ctx, types.NamespacedName{Name: want, Namespace: ns}, so); err != nil {
			t.Fatalf("ScaledObject %s/%s not created: %v", ns, want, err)
		}
		target, found, err := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "name")
		if err != nil || !found {
			t.Fatalf("scaleTargetRef.name missing on ScaledObject %s: found=%v err=%v", want, found, err)
		}
		if target != deploy.Name {
			t.Errorf("ScaledObject %s targets Deployment %q, but the agent's Deployment is named %q — the target must resolve to a real object or autoscaling silently never fires", want, target, deploy.Name)
		}
		kind, _, _ := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "kind")
		if kind != "Deployment" {
			t.Errorf("scaleTargetRef.kind = %q; want Deployment", kind)
		}
		// The stale "fleet-<name>" name must be gone.
		if target == "fleet-"+fleet.Name {
			t.Errorf("ScaledObject still targets the phantom Deployment %q", target)
		}
	}

	// No fleet-wide ScaledObject named "fleet-<name>" lingers.
	stale := &unstructured.Unstructured{}
	stale.SetGroupVersionKind(soGVK)
	if err := cl.Get(ctx, types.NamespacedName{Name: "fleet-" + fleet.Name, Namespace: ns}, stale); err == nil {
		t.Errorf("a fleet-wide ScaledObject %q still exists; scaling is per-agent now", "fleet-"+fleet.Name)
	}
}

// TestScaledObjectSQSTrigger covers the production SQS path: the shared
// TriggerAuthentication is emitted once and each agent's ScaledObject carries
// an aws-sqs-queue trigger referencing it.
func TestScaledObjectSQSTrigger(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	fleet := scalingFleet("https://sqs.us-west-2.amazonaws.com/123456789012/work")
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	if err := r.ensureKEDAScaledObject(ctx, cl, fleet, p); err != nil {
		t.Fatalf("ensureKEDAScaledObject: %v", err)
	}
	ns := PlatformNamespace(p)

	// Shared per-fleet TriggerAuthentication.
	ta := &unstructured.Unstructured{}
	ta.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "TriggerAuthentication"})
	if err := cl.Get(ctx, types.NamespacedName{Name: "fleet-" + fleet.Name + "-aws", Namespace: ns}, ta); err != nil {
		t.Fatalf("TriggerAuthentication not created: %v", err)
	}

	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	if err := cl.Get(ctx, types.NamespacedName{Name: agentDeploymentName(fleet, "triage"), Namespace: ns}, so); err != nil {
		t.Fatalf("ScaledObject not created: %v", err)
	}
	triggers, _, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
	if len(triggers) != 1 {
		t.Fatalf("want one trigger, got %d", len(triggers))
	}
	trig, _ := triggers[0].(map[string]any)
	if trig["type"] != "aws-sqs-queue" {
		t.Errorf("trigger type = %v; want aws-sqs-queue", trig["type"])
	}
	authRef, _ := trig["authenticationRef"].(map[string]any)
	if authRef["name"] != "fleet-"+fleet.Name+"-aws" {
		t.Errorf("authenticationRef.name = %v; want fleet-%s-aws", authRef["name"], fleet.Name)
	}
}

func TestFleetScalingMinMax(t *testing.T) {
	i := func(v int32) *int32 { return &v }
	cases := []struct {
		name             string
		min, max         *int32
		replicas         *int32
		wantMin, wantMax int32
	}{
		{"defaults", nil, nil, nil, 1, 10},
		{"fleet bounds", i(3), i(5), nil, 3, 5},
		{"agent floor overrides min", i(1), i(10), i(4), 4, 10},
		{"agent floor above max clamps max", i(1), i(10), i(12), 12, 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fleet := &agentsv1alpha1.AgentFleet{
				Spec: agentsv1alpha1.AgentFleetSpec{
					Scaling: agentsv1alpha1.ScalingSpec{Min: c.min, Max: c.max},
				},
			}
			agent := &agentsv1alpha1.AgentSpec{Name: "a", Replicas: c.replicas}
			gotMin, gotMax := fleetScalingMinMax(fleet, agent)
			if gotMin != c.wantMin || gotMax != c.wantMax {
				t.Errorf("got (%d,%d) want (%d,%d)", gotMin, gotMax, c.wantMin, c.wantMax)
			}
		})
	}
}

func TestApplyFleetStatusEmitsReadyGauge(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	fleet := agentFleet()
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(fleet).WithStatusSubresource(fleet).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}
	if err := r.applyFleetStatus(ctx, fleet, fleetResult{phase: phaseReady, readyAgents: 3}); err != nil {
		t.Fatalf("applyFleetStatus: %v", err)
	}
	g := fleetReadyAgents.WithLabelValues(fleet.Namespace, fleet.Spec.PlatformRef.Name, fleet.Name)
	if got := testutil.ToFloat64(g); got != 3 {
		t.Errorf("agents_fleet_ready_agents = %v; want 3", got)
	}
	fleetReadyAgents.DeleteLabelValues(fleet.Namespace, fleet.Spec.PlatformRef.Name, fleet.Name)
}

func TestRequeueJitter(t *testing.T) {
	base := 60 * time.Second
	upper := base + base/5 // +20%
	for i := 0; i < 200; i++ {
		d := requeueJitter(base)
		if d < base || d > upper {
			t.Fatalf("requeueJitter(%s) = %s; want within [%s, %s]", base, d, base, upper)
		}
	}
}

// TestScaledObjectTriggerUsesPodIdentity pins how the SQS scaler gets its
// credentials.
//
// What actually binds the scaler to the tenant's identity is the
// TriggerAuthentication's podIdentity.provider. The existing SQS test fetches
// that object and asserts only that it exists; its provider — the field the
// whole mechanism rests on — was unread. Dropped or changed, KEDA falls back to
// its own credentials and either cannot see the tenant's queue or reads it
// under a shared identity.
//
// identityOwner is asserted too, but deliberately NOT as the control, because
// it is not one:
//
//   - "pod" is KEDA's own default, so setting it changes nothing by itself
//   - it is deprecated as of KEDA v2.13 and slated for removal in v3; this repo
//     pins the KEDA chart at 2.20.2 (charts/operator/values.yaml), so the
//     deprecation is already live
//   - KEDA documents it as applying only under aws-eks authentication, and the
//     TriggerAuthentication above uses provider "aws"
//
// So the assertion holds the value against an upstream default change and
// nothing more. It is left in place with this note so that whoever bumps KEDA
// past v3 finds the reason here rather than a green test pinning a field the
// API no longer has.
func TestScaledObjectTriggerUsesPodIdentity(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	// A real queue URL: with an empty one the reconciler renders a CPU trigger
	// instead, and this test would assert nothing.
	fleet := scalingFleet("https://sqs.us-west-2.amazonaws.com/123456789012/acme-work")
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, publishedGateway(p, "chat")).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	if _, err := r.reconcileFleetSelf(ctx, fleet); err != nil {
		t.Fatalf("reconcileFleetSelf: %v", err)
	}
	ns := PlatformNamespace(p)

	// The live mechanism.
	ta := &unstructured.Unstructured{}
	ta.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "TriggerAuthentication"})
	taName := "fleet-" + fleet.Name + "-aws"
	if err := cl.Get(ctx, types.NamespacedName{Name: taName, Namespace: ns}, ta); err != nil {
		t.Fatalf("TriggerAuthentication not created: %v", err)
	}
	if got, _, _ := unstructured.NestedString(ta.Object, "spec", "podIdentity", "provider"); got != "aws" {
		t.Errorf("TriggerAuthentication podIdentity.provider = %q, want \"aws\" — this is what makes KEDA "+
			"poll as the tenant ServiceAccount rather than with its own credentials", got)
	}

	soGVK := schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}
	checked := 0

	for _, agent := range fleet.Spec.Agents {
		so := &unstructured.Unstructured{}
		so.SetGroupVersionKind(soGVK)
		name := agentDeploymentName(fleet, agent.Name)
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, so); err != nil {
			continue // agents without a queue trigger render no ScaledObject
		}
		triggers, found, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
		if !found {
			continue
		}
		for _, raw := range triggers {
			trig, _ := raw.(map[string]any)
			if trig["type"] != "aws-sqs-queue" {
				continue
			}
			checked++
			meta, _ := trig["metadata"].(map[string]any)
			// Pins the current default; see the note above for why this is not
			// the control.
			if got := meta["identityOwner"]; got != "pod" {
				t.Errorf("ScaledObject %s trigger identityOwner = %v, want \"pod\"", name, got)
			}
			// Without a resolvable authenticationRef the podIdentity provider
			// above never reaches the trigger, and the scaler has no credential.
			ref, _ := trig["authenticationRef"].(map[string]any)
			if ref == nil || ref["name"] != taName {
				t.Errorf("ScaledObject %s authenticationRef = %v, want name %q — the trigger reaches its "+
					"podIdentity only through this reference", name, ref, taName)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no aws-sqs-queue trigger was rendered — this test asserted nothing, which is not the " +
			"same as passing")
	}
}
