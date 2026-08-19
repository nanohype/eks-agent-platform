/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func sandboxPoolScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		platformv1alpha1.AddToScheme,
		agentsv1alpha1.AddToScheme,
		corev1.AddToScheme,
		networkingv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func sandboxPoolFixture() (*agentsv1alpha1.SandboxPool, *platformv1alpha1.Platform) {
	pool := &agentsv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "batch", Namespace: ctrlTestNS},
		Spec: agentsv1alpha1.SandboxPoolSpec{
			EnvironmentID: "env-123",
		},
	}
	p := newPlatform(ctrlTestPlatform, "team")
	return pool, p
}

// TestSandboxScaledObjectReadsTheBridgeKey pins valueLocation on the
// metrics-api trigger.
//
// This is one half of a two-file contract. The other half is
// cmd/metrics-shim/main.go, which serves the queue depth as
// json.Encode(map[string]int64{"depth": depth}). KEDA's metrics-api scaler
// reads valueLocation as the JSON key to pull out of that body. Nothing in Go
// relates the two: the shim writes a map literal, the reconciler writes a
// string, and they are in different packages.
//
// Spelled differently on either side, KEDA finds no such key and the scaler
// reports no metric. The pool holds at minReplicas and never scales with load —
// no error, no event, and a work queue that grows while the ScaledObject sits
// there looking configured.
//
// Asserted here rather than against a shared constant because there is no
// shared constant to assert against: the shim's handler is an inline closure in
// main(), so the key is not reachable from a test. Extracting that handler
// would let both sides be pinned to one symbol and is the version that makes
// this unreachable rather than detected — recorded as a follow-up rather than
// done here, since it is a production change.
func TestSandboxScaledObjectReadsTheBridgeKey(t *testing.T) {
	ctx := context.Background()
	pool, p := sandboxPoolFixture()
	s := sandboxPoolScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &SandboxPoolReconciler{Client: cl, Scheme: s}

	if err := r.ensureSandboxScaledObject(ctx, pool, p); err != nil {
		t.Fatalf("ensureSandboxScaledObject: %v", err)
	}

	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: sandboxResourceName(pool)}
	if err := cl.Get(ctx, key, so); err != nil {
		t.Fatalf("ScaledObject not created: %v", err)
	}

	triggers, found, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
	if !found || len(triggers) != 1 {
		t.Fatalf("want one trigger, got %d (found=%v)", len(triggers), found)
	}
	trig, _ := triggers[0].(map[string]any)
	if trig["type"] != "metrics-api" {
		t.Fatalf("trigger type = %v, want metrics-api", trig["type"])
	}
	meta, _ := trig["metadata"].(map[string]any)

	// "depth" is the key cmd/metrics-shim/main.go encodes. Changing either side
	// alone breaks the scaler silently.
	if got := meta["valueLocation"]; got != "depth" {
		t.Errorf("valueLocation = %v, want \"depth\" — that is the JSON key cmd/metrics-shim encodes; "+
			"a mismatch leaves KEDA reading no metric, so the pool holds at minReplicas while its "+
			"queue grows", got)
	}
	if got := meta["format"]; got != "json" {
		t.Errorf("format = %v, want json — the shim serves JSON, and the scaler parses per this field", got)
	}
	// The URL has to resolve to the bridge Service this reconciler also creates.
	wantURL := "http://" + metricsBridgeName(pool) + "." + PlatformNamespace(p) + ".svc.cluster.local/"
	if got := meta["url"]; got != wantURL {
		t.Errorf("url = %v, want %q — the trigger must address the bridge Service the same reconciler "+
			"renders, or the scrape 404s and the scaler reports nothing", got, wantURL)
	}
}

// TestMetricsBridgeNetworkPolicyAdmitsKeda pins the ingress rule on the bridge.
//
// The bridge exists to be scraped by KEDA and by nothing else. Its policy sets
// both PolicyTypes, so ingress is default-deny and the single rule below is the
// only way in.
//
// The namespaceSelector is the whole rule. Wrong label, wrong namespace name, or
// a dropped selector and KEDA's scrape is refused: the ScaledObject reports a
// metrics error the pool's own status does not surface, and the pool stops
// scaling. Widened instead — an empty selector admits every namespace — and any
// pod on the cluster can read this tenant's queue depth, which is a workload
// signal rather than data but still a cross-tenant read of something the policy
// was written to confine.
func TestMetricsBridgeNetworkPolicyAdmitsKeda(t *testing.T) {
	ctx := context.Background()
	pool, p := sandboxPoolFixture()
	s := sandboxPoolScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &SandboxPoolReconciler{Client: cl, Scheme: s}

	if err := r.ensureMetricsBridgeNetworkPolicy(ctx, pool, p); err != nil {
		t.Fatalf("ensureMetricsBridgeNetworkPolicy: %v", err)
	}

	np := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: metricsBridgeName(pool)}
	if err := cl.Get(ctx, key, np); err != nil {
		t.Fatalf("metrics bridge NetworkPolicy not created: %v", err)
	}

	// Ingress must be a declared PolicyType, or the rule below is inert and the
	// bridge is reachable from anywhere by default.
	var hasIngress bool
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeIngress {
			hasIngress = true
		}
	}
	if !hasIngress {
		t.Error("PolicyTypes omits Ingress — the ingress rule is then inert and the bridge is open")
	}

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("want exactly one ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.From) != 1 {
		t.Fatalf("want exactly one ingress peer, got %d", len(rule.From))
	}
	sel := rule.From[0].NamespaceSelector
	if sel == nil {
		t.Fatal("ingress peer has no namespaceSelector — a peer with every selector nil admits all sources")
	}
	if len(sel.MatchLabels) == 0 {
		t.Fatal("namespaceSelector is empty — an empty selector matches every namespace, so any pod on " +
			"the cluster could scrape this tenant's queue depth")
	}
	if got := sel.MatchLabels["kubernetes.io/metadata.name"]; got != "keda" {
		t.Errorf("namespaceSelector names %q, want the keda namespace — anything else and KEDA's scrape "+
			"is refused, so the pool silently stops scaling", got)
	}
}
