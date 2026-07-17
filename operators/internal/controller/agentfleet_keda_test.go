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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// scalingFleet builds a scaling-enabled fleet with two agents. The whole point
// of the KEDA fix is that each agent becomes its own kagent Deployment, so the
// two-agent shape exercises the per-agent ScaledObject path.
func scalingFleet(queueURL string) *agentsv1alpha1.AgentFleet {
	return &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: ctrlTestNS, Generation: 1},
		Spec: agentsv1alpha1.AgentFleetSpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: ctrlTestPlatform},
			Agents: []agentsv1alpha1.AgentSpec{
				{Name: "triage", SystemPrompt: "you triage", ModelRoute: "chat"},
				{Name: "responder", SystemPrompt: "you respond", ModelRoute: "chat"},
			},
			Scaling: agentsv1alpha1.ScalingSpec{Enabled: true, QueueURL: queueURL},
		},
	}
}

// TestScaledObjectTargetsKagentDeployment is the regression for the bug where
// the ScaledObject targeted "fleet-<name>" — a Deployment kagent never
// creates. kagent names the Deployment it renders after the Agent CR verbatim,
// and the operator names each Agent <fleet>-<agent>; the ScaledObject's
// scaleTargetRef must resolve to that exact name. The assertion cross-checks
// the ScaledObject target against the kagent Agent the reconcile path actually
// created, so it can never again point at a phantom Deployment.
func TestScaledObjectTargetsKagentDeployment(t *testing.T) {
	ctx := context.Background()
	s := fleetScheme(t)
	p := readyPlatformIn()
	fleet := scalingFleet("")
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &AgentFleetReconciler{Client: cl, Scheme: s}

	phase, _, err := r.reconcileFleetSelf(ctx, fleet)
	if err != nil || phase != phaseReady {
		t.Fatalf("reconcileFleetSelf: phase=%q err=%v", phase, err)
	}

	ns := PlatformNamespace(p)
	agentGVK := schema.GroupVersionKind{Group: "kagent.dev", Version: "v1alpha2", Kind: "Agent"}
	soGVK := schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

	for _, agent := range fleet.Spec.Agents {
		want := kagentAgentName(fleet, agent.Name)

		// The kagent Agent the reconcile path created (kagent turns this into a
		// Deployment of the same name).
		ag := &unstructured.Unstructured{}
		ag.SetGroupVersionKind(agentGVK)
		if err := cl.Get(ctx, types.NamespacedName{Name: want, Namespace: ns}, ag); err != nil {
			t.Fatalf("kagent Agent %s/%s not created: %v", ns, want, err)
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
		if target != ag.GetName() {
			t.Errorf("ScaledObject %s targets Deployment %q, but the only kagent workload is named %q — target must resolve to a real object", want, target, ag.GetName())
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
	if err := cl.Get(ctx, types.NamespacedName{Name: kagentAgentName(fleet, "triage"), Namespace: ns}, so); err != nil {
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
	if err := r.applyFleetStatus(ctx, fleet, phaseReady, 3); err != nil {
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
