/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	for _, rule := range tenantEgressCiliumRules(nil, nil) {
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

// TestGatewayEgressPolicySelectsOnlyTheProxy pins the podSelector on the
// gateway-egress NetworkPolicy.
//
// That policy grants what the Envoy proxy needs and nothing else needs: TCP/443
// to any destination, xDS to Envoy Gateway's namespace, and OTLP to the
// collector. The ports are asserted next door; the selector deciding WHO gets
// them was not.
//
// An empty LabelSelector is not "no pods" in Kubernetes — it selects every pod
// in the namespace. So dropping the matchLabels turns a proxy-scoped allowance
// into a namespace-wide one: every tenant agent pod gains unrestricted TCP/443
// egress, which is the tenant's whole outbound story and is meant to be
// constrained by the tenant egress policy rather than opened here. Nothing
// errors — the policy is valid, the proxy still works, and the blast radius is
// every workload beside it.
func TestGatewayEgressPolicySelectsOnlyTheProxy(t *testing.T) {
	ctx := context.Background()
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

	sel := np.Spec.PodSelector
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		t.Fatal("podSelector is empty — an empty selector matches EVERY pod in the tenant namespace, " +
			"so every agent workload would inherit the proxy's TCP/443-to-anywhere egress")
	}
	for k, want := range map[string]string{
		"app.kubernetes.io/name":       "envoy",
		"app.kubernetes.io/component":  "proxy",
		"app.kubernetes.io/managed-by": "envoy-gateway",
	} {
		if got := sel.MatchLabels[k]; got != want {
			t.Errorf("podSelector[%s] = %q, want %q — the selector has to name the Envoy proxy, since "+
				"anything broader hands these egress rules to workloads that should not have them", k, got, want)
		}
	}

	// Egress-only. Adding PolicyTypeIngress with no ingress rules would make this
	// a default-deny for inbound on the pods it selects, silently cutting the
	// gateway off from the tenant workloads that call it.
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("policyTypes = %v, want [Egress] only — an Ingress type here with no ingress rules "+
			"default-denies inbound to the proxy", np.Spec.PolicyTypes)
	}
}

// TestEnsureQuotaBoundsEveryDimension pins the ResourceQuota's hard limits.
//
// The conformance suite asserts Pods() is non-zero. That is one of eight keys,
// and the check it performs — non-zero — is satisfied by any value, so the CPU
// and memory ceilings that actually bound a tenant's spend were unobserved.
//
// A ResourceQuota only constrains the dimensions it names. Dropping a key does
// not fail, does not warn, and does not show up anywhere except as a tenant with
// no ceiling on that resource: no limits.memory means one agent pod can request
// the whole node's memory, and on a cluster with Karpenter that provisions
// capacity to satisfy it. The failure is a bill, not an error.
//
// Values are asserted exactly rather than as "non-zero", because the realistic
// mistake is a wrong unit or a stray zero — 16Gi becoming 16G, or 8 becoming
// 80 — and every one of those passes a non-zero check.
func TestEnsureQuotaBoundsEveryDimension(t *testing.T) {
	ctx := context.Background()
	p := newPlatform(ctrlTestPlatform, "team")
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register core/v1: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PlatformReconciler{Client: cl}

	if err := r.ensureQuota(ctx, p); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}
	q := &corev1.ResourceQuota{}
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: "tenant-default"}
	if err := cl.Get(ctx, key, q); err != nil {
		t.Fatalf("ResourceQuota not created: %v", err)
	}

	want := map[corev1.ResourceName]string{
		corev1.ResourceRequestsCPU:    "4",
		corev1.ResourceRequestsMemory: "16Gi",
		corev1.ResourceLimitsCPU:      "8",
		corev1.ResourceLimitsMemory:   "32Gi",
		corev1.ResourcePods:           "50",
		corev1.ResourceServices:       "20",
		corev1.ResourceSecrets:        "50",
		corev1.ResourceConfigMaps:     "50",
	}
	for name, wantStr := range want {
		got, ok := q.Spec.Hard[name]
		if !ok {
			t.Errorf("quota does not bound %s — a ResourceQuota constrains only the dimensions it names, "+
				"so an absent key is no ceiling at all on that resource", name)
			continue
		}
		if got.String() != wantStr {
			t.Errorf("quota %s = %s, want %s", name, got.String(), wantStr)
		}
	}
	if len(q.Spec.Hard) != len(want) {
		t.Errorf("quota bounds %d dimensions, expected %d — an added key is not wrong, but it is a "+
			"tenant-visible ceiling that nothing else describes", len(q.Spec.Hard), len(want))
	}

	// limits must not sit below requests, or the quota admits a pod its own
	// request ceiling already allows and rejects it at the limit — a
	// contradiction that surfaces as unschedulable pods rather than as a
	// configuration error.
	reqCPU, limCPU := q.Spec.Hard[corev1.ResourceRequestsCPU], q.Spec.Hard[corev1.ResourceLimitsCPU]
	if limCPU.Cmp(reqCPU) < 0 {
		t.Errorf("limits.cpu %s < requests.cpu %s", limCPU.String(), reqCPU.String())
	}
	reqMem, limMem := q.Spec.Hard[corev1.ResourceRequestsMemory], q.Spec.Hard[corev1.ResourceLimitsMemory]
	if limMem.Cmp(reqMem) < 0 {
		t.Errorf("limits.memory %s < requests.memory %s", limMem.String(), reqMem.String())
	}
}
