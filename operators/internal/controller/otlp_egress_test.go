/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"strconv"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// collectorNamespaceFromCatalog is the namespace the eks-gitops catalog
// deploys the OTel collector gateway into, and where the stable
// `telemetry.monitoring.svc` Service every tenant chart wires against lives.
//
// Written out rather than referenced so this test fails when the operator's
// constant changes: `collectorNamespace` on both sides would agree with itself
// no matter what either said.
const collectorNamespaceFromCatalog = "monitoring"

// TestOTLPEgressNamesTheCollectorNamespace is the regression for egress rules
// that pointed at a namespace holding no collector.
//
// This failure has no symptom. Pods stay Running, the collector stays healthy,
// and telemetry silently never arrives — nothing goes unready and no error is
// logged, so the only way to catch it is to assert the namespace.
func TestOTLPEgressNamesTheCollectorNamespace(t *testing.T) {
	if collectorNamespace != collectorNamespaceFromCatalog {
		t.Fatalf("collectorNamespace = %q, but the catalog deploys the collector into %q — "+
			"every OTLP egress rule the operator emits would allow traffic to a namespace with no "+
			"collector in it, and telemetry would be dropped with nothing reporting unhealthy",
			collectorNamespace, collectorNamespaceFromCatalog)
	}
}

// TestGatewayEgressReachesTheCollector covers the gateway's own telemetry leg.
//
// The gateway's egress policy is separate from the tenant allow-list on
// purpose — it is what makes the gateway the only pod with outbound TLS — so
// it does not inherit the tenant OTLP rule and needs its own. Without it the
// extproc exports into a closed egress and the routing ledger is empty.
func TestGatewayEgressReachesTheCollector(t *testing.T) {
	ctx := context.Background()

	t.Run("cilium", func(t *testing.T) {
		var otlp map[string]interface{}
		for _, rule := range gatewayEgressCiliumRules() {
			r, _ := rule.(map[string]interface{})
			eps, found, _ := unstructured.NestedSlice(r, "toEndpoints")
			if !found || len(eps) == 0 {
				continue
			}
			ep, _ := eps[0].(map[string]interface{})
			labels, _, _ := unstructured.NestedStringMap(ep, "matchLabels")
			if labels["k8s:io.kubernetes.pod.namespace"] == collectorNamespace {
				otlp = r
			}
		}
		if otlp == nil {
			t.Fatalf("the gateway egress policy opens no path to the collector in %q — "+
				"the extproc would export into a closed egress and emit no ledger", collectorNamespace)
		}
		ports, _, _ := unstructured.NestedSlice(otlp, "toPorts")
		found := map[string]bool{}
		for _, p := range ports {
			pm, _ := p.(map[string]interface{})
			list, _, _ := unstructured.NestedSlice(pm, "ports")
			for _, entry := range list {
				e, _ := entry.(map[string]interface{})
				port, _ := e["port"].(string)
				found[port] = true
			}
		}
		for _, want := range []int{otlpGRPCPort, otlpHTTPPort} {
			if !found[strconv.Itoa(want)] {
				t.Errorf("gateway OTLP egress does not open port %d; opened %v", want, found)
			}
		}
	})

	t.Run("portable NetworkPolicy", func(t *testing.T) {
		p := newPlatform(ctrlTestPlatform, "team")
		scheme := runtime.NewScheme()
		if err := networkingv1.AddToScheme(scheme); err != nil {
			t.Fatalf("register networking/v1: %v", err)
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &PlatformReconciler{Client: cl, NetworkEngine: "kubernetes"}

		if err := r.ensureGatewayEgressPolicy(ctx, p); err != nil {
			t.Fatalf("ensureGatewayEgressPolicy: %v", err)
		}
		np := &networkingv1.NetworkPolicy{}
		key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: "gateway-egress"}
		if err := cl.Get(ctx, key, np); err != nil {
			t.Fatalf("gateway-egress NetworkPolicy not created: %v", err)
		}

		var otlpPorts []int32
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.NamespaceSelector == nil {
					continue
				}
				if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != collectorNamespace {
					continue
				}
				for _, port := range rule.Ports {
					otlpPorts = append(otlpPorts, port.Port.IntVal)
				}
			}
		}
		if len(otlpPorts) == 0 {
			t.Fatalf("the gateway egress policy opens no path to the collector in %q", collectorNamespace)
		}
		for _, want := range []int32{otlpGRPCPort, otlpHTTPPort} {
			var ok bool
			for _, got := range otlpPorts {
				if got == want {
					ok = true
				}
			}
			if !ok {
				t.Errorf("gateway OTLP egress does not open port %d; opened %v", want, otlpPorts)
			}
		}

		// The gateway keeps outbound TLS. Adding the telemetry rule must not
		// have replaced the rule that lets it reach Bedrock at all.
		var hasTLS bool
		for _, rule := range np.Spec.Egress {
			if len(rule.To) != 0 {
				continue
			}
			for _, port := range rule.Ports {
				if port.Port.IntVal == 443 {
					hasTLS = true
				}
			}
		}
		if !hasTLS {
			t.Error("gateway-egress no longer opens outbound 443 — the gateway cannot reach Bedrock")
		}
	})
}

// TestTenantEgressReachesTheCollector covers the tenant and fleet legs. Tenant
// workloads export their own traces and metrics; the rule naming a namespace
// with no collector is what this regression is for.
func TestTenantEgressReachesTheCollector(t *testing.T) {
	var found bool
	for _, rule := range tenantEgressCiliumRules() {
		r, _ := rule.(map[string]interface{})
		eps, ok, _ := unstructured.NestedSlice(r, "toEndpoints")
		if !ok || len(eps) == 0 {
			continue
		}
		ep, _ := eps[0].(map[string]interface{})
		labels, _, _ := unstructured.NestedStringMap(ep, "matchLabels")
		if labels["k8s:io.kubernetes.pod.namespace"] == collectorNamespace {
			found = true
		}
	}
	if !found {
		t.Errorf("the tenant egress allow-list opens no path to the collector in %q — "+
			"every tenant's traces and metrics would be dropped by default-deny", collectorNamespace)
	}
}

// TestGatewayEgressPolicy_AllowsXDS is the portable-engine twin of
// TestGatewayEgressCiliumRules_AllowsXDS. Same failure, same silence: the
// gateway's Envoy runs inside the tenant's default-deny egress and dials Envoy
// Gateway's control plane on plaintext gRPC 18000, which the outbound-TLS rule
// does not cover.
func TestGatewayEgressPolicy_AllowsXDS(t *testing.T) {
	ctx := context.Background()
	p := attributedPlatform("acme", "reliability", nil, nil)

	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register networking/v1: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PlatformReconciler{Client: cl, NetworkEngine: "kubernetes"}
	if err := r.ensureGatewayEgressPolicy(ctx, p); err != nil {
		t.Fatalf("ensureGatewayEgressPolicy: %v", err)
	}

	np := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: "gateway-egress"}
	if err := cl.Get(ctx, key, np); err != nil {
		t.Fatalf("gateway-egress NetworkPolicy not created: %v", err)
	}

	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector == nil ||
				peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != envoyGatewayNamespace {
				continue
			}
			for _, port := range rule.Ports {
				if port.Port.IntVal == int32(envoyGatewayXDSPort) {
					return
				}
			}
		}
	}
	t.Fatalf("gateway egress opens no path to xDS (TCP %d in %q) — the proxy would never "+
		"program, while the ModelGateway CR still reported Ready",
		envoyGatewayXDSPort, envoyGatewayNamespace)
}
