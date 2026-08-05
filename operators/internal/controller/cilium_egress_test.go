/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"sort"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// protoTCP is the protocol string these policies carry. A constant because it
// is spelled the same in every rule and every assertion.
const protoTCP = "TCP"

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
		if port["port"] == "80" && port["protocol"] == protoTCP {
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
		if port["port"] != "8080" || port["protocol"] != protoTCP {
			t.Errorf("gateway egress port: got %v/%v want 8080/TCP", port["port"], port["protocol"])
		}
		return
	}
	t.Fatal("tenant cilium egress must allow the Envoy proxy pods on TCP 8080 — without it every model call fails")
}

// TestGatewayEgress_ReachesBedrockAndTenantsDoNot is the regression guard for a
// bug that shipped: moving the gateway into the tenant namespace put it under
// the tenant egress policy, whose allow-list covers DNS, the gateway, the OTel
// collector and the Pod Identity endpoint — and nothing on 443.
//
// The result was invisible in every way that usually catches things: the CRs
// applied, the Gateway came up, the Platform reported Ready, and 100% of model
// calls were denied at the network.
//
// The pair of assertions is the actual contract. The gateway must have outbound
// TLS, and the shared tenant list must NOT — because that is what makes the
// gateway the only route to a model rather than the preferred one.
func TestGatewayEgress_ReachesBedrockAndTenantsDoNot(t *testing.T) {
	tlsFrom := func(rules []interface{}) bool {
		for _, raw := range rules {
			rule, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			ports, ok := rule["toPorts"].([]interface{})
			if !ok {
				continue
			}
			for _, p := range ports {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				for _, e := range pm["ports"].([]interface{}) {
					if e.(map[string]interface{})["port"] == "443" {
						return true
					}
				}
			}
		}
		return false
	}

	if !tlsFrom(gatewayEgressCiliumRules()) {
		t.Error("the gateway must be allowed outbound TLS — without it every model call is denied at the network while the Gateway reports healthy")
	}
	if tlsFrom(tenantEgressCiliumRules()) {
		t.Error("the shared tenant allow-list must not carry 443: that hands every application pod a direct path to Bedrock and demotes the gateway to a convention")
	}
}

// TestEnsureGatewayCiliumEgress_GatedByEngine mirrors the tenant policy's gate —
// the CiliumNetworkPolicy is emitted only on cilium, where the portable
// NetworkPolicy twin does not apply.
func TestEnsureGatewayCiliumEgress_GatedByEngine(t *testing.T) {
	p := newPlatform(ctrlTestPlatform, "team")
	ctx := context.Background()

	cl := ciliumTestClient(t)
	r := &PlatformReconciler{Client: cl, NetworkEngine: "kubernetes"}
	if err := r.ensureGatewayCiliumEgress(ctx, p); err != nil {
		t.Fatalf("kubernetes-engine ensureGatewayCiliumEgress: %v", err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace(p), Name: "gateway-egress"}, u); err == nil {
		t.Error("kubernetes engine must not emit a CiliumNetworkPolicy")
	}

	cl = ciliumTestClient(t)
	r = &PlatformReconciler{Client: cl, NetworkEngine: NetworkEngineCilium}
	if err := r.ensureGatewayCiliumEgress(ctx, p); err != nil {
		t.Fatalf("cilium-engine ensureGatewayCiliumEgress: %v", err)
	}
	cnp := getCNP(t, cl, PlatformNamespace(p), "gateway-egress")
	sel, _, _ := unstructured.NestedMap(cnp.Object, "spec", "endpointSelector", "matchLabels")
	// Selecting anything broader would grant outbound TLS to tenant workloads.
	if sel["k8s:app.kubernetes.io/name"] != "envoy" {
		t.Errorf("gateway-egress must select only the Envoy proxy pods, got %v", sel)
	}
}

// TestGatewayEgressCiliumRules_AllowsXDS is the regression guard for the
// failure this rule exists to prevent. The gateway's Envoy runs in the tenant
// namespace, inside the tenant's default-deny egress, and dials Envoy Gateway's
// control plane on plaintext gRPC 18000 — a port the outbound-TLS rule does not
// cover and a namespace the tenant otherwise cannot reach.
//
// Without it nothing reports a fault: the operator reconciles every resource it
// owns, the ModelGateway CR goes Ready and publishes an endpoint, and the proxy
// behind that endpoint never receives a listener. The assertion is on the rule
// rather than on a healthy cluster because that is the only place the failure
// is visible before traffic.
func TestGatewayEgressCiliumRules_AllowsXDS(t *testing.T) {
	for _, raw := range gatewayEgressCiliumRules() {
		rule := raw.(map[string]interface{})
		eps, ok := rule["toEndpoints"].([]interface{})
		if !ok || len(eps) == 0 {
			continue
		}
		labels := eps[0].(map[string]interface{})["matchLabels"].(map[string]interface{})
		if labels["k8s:io.kubernetes.pod.namespace"] != envoyGatewayNamespace {
			continue
		}
		port := rule["toPorts"].([]interface{})[0].(map[string]interface{})["ports"].([]interface{})[0].(map[string]interface{})
		if port["port"] == strconv.Itoa(envoyGatewayXDSPort) && port["protocol"] == protoTCP {
			return
		}
	}
	t.Fatalf("gateway cilium egress must allow TCP %d to namespace %q — without it the "+
		"data plane never reaches xDS and the Gateway never programs, while the "+
		"ModelGateway CR still reports Ready", envoyGatewayXDSPort, envoyGatewayNamespace)
}

// The vcluster tier's policies are written against Services, but a policy never
// sees a Service. Both cilium and kube-proxy translate a ClusterIP to the
// backend pod and port BEFORE policy evaluation, so a rule naming the published
// port matches nothing and the packet is dropped — silently, because a dropped
// SYN is not a refused one.
//
// That cost this tier its DNS twice over. CoreDNS could not reach the vcluster
// apiserver (Service 443, backend 8443), and once it could, tenant pods still
// could not reach CoreDNS (Service 53, backend 1053). Both were found with
// `cilium monitor --type drop` against a live cluster; neither pod logged
// anything actionable.
func TestVClusterAPIAccess_NamesBackendPortsNotServicePorts(t *testing.T) {
	cl := ciliumTestClient(t)
	r := &PlatformReconciler{Client: cl, NetworkEngine: NetworkEngineCilium}
	p := &platformv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}

	if err := r.ensureVClusterAPIAccess(context.Background(), p); err != nil {
		t.Fatalf("ensureVClusterAPIAccess: %v", err)
	}
	cnp := getCNP(t, cl, PlatformNamespace(p), "vcluster-api-access")

	// Every pod in the namespace, not just the syncer: CoreDNS and every synced
	// tenant workload address the vcluster apiserver as their own.
	sel, found, _ := unstructured.NestedMap(cnp.Object, "spec", "endpointSelector")
	if !found || len(sel) != 0 {
		t.Errorf("endpointSelector = %v, want empty (every pod in the namespace)", sel)
	}

	egress, found, _ := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	if !found || len(egress) == 0 {
		t.Fatal("policy declares no egress; this check would pass vacuously")
	}

	ports := map[string]bool{}
	sawVCluster, sawDNS := false, false
	for _, raw := range egress {
		rule, _ := raw.(map[string]interface{})
		eps, _, _ := unstructured.NestedSlice(rule, "toEndpoints")
		for _, e := range eps {
			m, _ := e.(map[string]interface{})
			labels, _, _ := unstructured.NestedStringMap(m, "matchLabels")
			if labels["app"] == "vcluster" {
				sawVCluster = true
			}
			if labels["k8s:k8s-app"] == "vcluster-kube-dns" {
				sawDNS = true
			}
		}
		tps, _, _ := unstructured.NestedSlice(rule, "toPorts")
		for _, tp := range tps {
			m, _ := tp.(map[string]interface{})
			pl, _, _ := unstructured.NestedSlice(m, "ports")
			for _, pe := range pl {
				pm, _ := pe.(map[string]interface{})
				if s, ok := pm["port"].(string); ok {
					ports[s] = true
				}
			}
		}
	}

	if !sawVCluster {
		t.Error("no rule targets the vcluster control plane; CoreDNS cannot reach the apiserver")
	}
	// Literals, deliberately, not vclusterDNSLabel / vclusterAPIBackendPort /
	// vclusterDNSBackendPorts. Asserting against the same constants the policy is
	// built from is a tautology: change the constant and both sides move
	// together, so the test passes while the cluster breaks. Mutation-testing
	// caught exactly that — reverting the API port to 443, the DNS port to 53,
	// and the DNS label to the host resolver all left this test green.
	//
	// These three values were established against a running cluster with
	// `cilium monitor --type drop`. Changing one should fail here and send the
	// next person back to a cluster to re-establish it, which is the only way
	// any of them were learned.
	if !sawDNS {
		t.Error(`no rule targets k8s-app "vcluster-kube-dns"; tenant pods resolve through the vcluster's own CoreDNS, not the host resolver`)
	}
	if !ports["8443"] {
		t.Errorf("apiserver backend port 8443 absent (ports=%v). The Service publishes 443, but policy is evaluated after ClusterIP translation", keysOf(ports))
	}
	if !ports["1053"] {
		t.Errorf("DNS backend port 1053 absent (ports=%v). The Service publishes 53 and targets 1053, because that CoreDNS runs unprivileged", keysOf(ports))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
