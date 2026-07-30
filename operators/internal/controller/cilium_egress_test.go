/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

func ciliumTestClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(ciliumNetworkPolicyGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(ciliumNetworkPolicyGVK.GroupVersion().WithKind("CiliumNetworkPolicyList"), &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func getCNP(t *testing.T, cl client.Client, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		t.Fatalf("get CiliumNetworkPolicy %s/%s: %v", namespace, name, err)
	}
	return u
}

// TestTenantEgressCiliumRules_AllowsPodIdentityCreds is the regression guard for
// the bug this whole path fixes: a tenant-runtime pod under cilium must reach
// the EKS Pod Identity creds endpoint, which is the reserved host entity on TCP
// 80 — a vanilla NetworkPolicy ipBlock cannot match it.
func TestTenantEgressCiliumRules_AllowsPodIdentityCreds(t *testing.T) {
	found := false
	for _, raw := range tenantEgressCiliumRules() {
		rule := raw.(map[string]interface{})
		ents, ok := rule["toEntities"].([]interface{})
		if !ok {
			continue
		}
		hasHost := false
		for _, e := range ents {
			if e == "host" {
				hasHost = true
			}
		}
		if !hasHost {
			continue
		}
		port := rule["toPorts"].([]interface{})[0].(map[string]interface{})["ports"].([]interface{})[0].(map[string]interface{})
		if port["port"] == "80" && port["protocol"] == "TCP" {
			found = true
		}
	}
	if !found {
		t.Fatal("tenant cilium egress must allow toEntities:[host] on TCP 80 (the Pod Identity creds endpoint)")
	}
}

func TestEnsureTenantCiliumEgress_GatedByEngine(t *testing.T) {
	p := attributedPlatform("acme", "reliability", nil, nil)

	// kubernetes engine: no-op, no CiliumNetworkPolicy created.
	cl := ciliumTestClient(t)
	r := &PlatformReconciler{Client: cl, NetworkEngine: "kubernetes"}
	if err := r.ensureTenantCiliumEgress(context.Background(), p); err != nil {
		t.Fatalf("kubernetes-engine ensureTenantCiliumEgress: %v", err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: PlatformNamespace(p), Name: "tenant-egress"}, u); err == nil {
		t.Fatal("kubernetes engine must not create a CiliumNetworkPolicy")
	}

	// cilium engine: creates tenant-egress with the full egress allow-list.
	cl = ciliumTestClient(t)
	r = &PlatformReconciler{Client: cl, NetworkEngine: NetworkEngineCilium}
	if err := r.ensureTenantCiliumEgress(context.Background(), p); err != nil {
		t.Fatalf("cilium-engine ensureTenantCiliumEgress: %v", err)
	}
	cnp := getCNP(t, cl, PlatformNamespace(p), "tenant-egress")
	egress, found, err := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	if err != nil || !found || len(egress) != 4 {
		t.Fatalf("tenant CNP egress: got %d rules (err=%v found=%v) want 4", len(egress), err, found)
	}
	// A per-tenant (not per-fleet) policy must not deny ingress.
	if _, found, _ := unstructured.NestedSlice(cnp.Object, "spec", "ingress"); found {
		t.Error("tenant CNP must not restrict ingress (only the per-fleet policy denies ingress)")
	}
	// Idempotent.
	if err := r.ensureTenantCiliumEgress(context.Background(), p); err != nil {
		t.Fatalf("second ensure (idempotency): %v", err)
	}
}

func TestEnsureFleetCiliumEgress_DeniesIngressAndSelectsFleet(t *testing.T) {
	p := attributedPlatform("acme", "reliability", nil, nil)
	cl := ciliumTestClient(t)
	r := &AgentFleetReconciler{Client: cl, NetworkEngine: NetworkEngineCilium}

	fleet := &agentsv1alpha1.AgentFleet{}
	fleet.Name = "researchers"
	fleet.Namespace = PlatformNamespace(p)

	if err := r.ensureFleetCiliumEgress(context.Background(), fleet, p); err != nil {
		t.Fatalf("ensureFleetCiliumEgress: %v", err)
	}
	cnp := getCNP(t, cl, PlatformNamespace(p), "fleet-researchers")

	// The fleet policy denies all ingress (an empty ingress rule set present).
	if _, found, _ := unstructured.NestedSlice(cnp.Object, "spec", "ingress"); !found {
		t.Error("fleet CNP must set an (empty) ingress rule set so cilium default-denies ingress")
	}
	// And it selects only the fleet's pods, not the whole namespace.
	sel, found, _ := unstructured.NestedStringMap(cnp.Object, "spec", "endpointSelector", "matchLabels")
	if !found || sel[LabelFleet] != "researchers" {
		t.Errorf("fleet CNP endpointSelector must select %s=researchers: got %v", LabelFleet, sel)
	}
}

// TestTenantEgressCiliumRules_ReachesTheModelGateway guards the same class of
// bug as the Pod Identity rule above, for the gateway.
//
// The Platform's gateway runs in the tenant's own namespace, and default-deny
// egress applies to same-namespace traffic too — so a rule scoped to some other
// namespace, or no rule at all, leaves the tenant unable to reach its own
// gateway. Nothing else in the suite would notice: every CR still applies, the
// Platform still reports Ready, and only real inference traffic fails.
func TestTenantEgressCiliumRules_ReachesTheModelGateway(t *testing.T) {
	for _, raw := range tenantEgressCiliumRules() {
		rule, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		eps, ok := rule["toEndpoints"].([]interface{})
		if !ok || len(eps) == 0 {
			continue
		}
		labels, ok := eps[0].(map[string]interface{})["matchLabels"].(map[string]interface{})
		if !ok || labels["k8s:app.kubernetes.io/name"] != "envoy" {
			continue
		}
		// A namespace label here would scope the rule away from the tenant's own
		// namespace, which is exactly where the gateway lives.
		if _, scoped := labels["k8s:io.kubernetes.pod.namespace"]; scoped {
			t.Error("the gateway rule must stay same-namespace: a namespace selector points it at the wrong place")
		}
		port := rule["toPorts"].([]interface{})[0].(map[string]interface{})["ports"].([]interface{})[0].(map[string]interface{})
		if port["port"] != "8080" || port["protocol"] != "TCP" {
			t.Errorf("gateway egress port: got %v/%v want 8080/TCP", port["port"], port["protocol"])
		}
		return
	}
	t.Fatal("tenant cilium egress must allow the Envoy proxy pods on TCP 8080 — without it every model call fails")
}
