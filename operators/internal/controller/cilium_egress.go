package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// NetworkEngineCilium is the network-engine value that makes the operator emit
// CiliumNetworkPolicies for tenant/fleet egress instead of vanilla k8s
// NetworkPolicies. It mirrors the chart's networkPolicy.engine value (default
// "cilium" — the CNI on every cluster this operator runs on).
const NetworkEngineCilium = "cilium"

// ciliumNetworkPolicyGVK is the cilium.io/v2 CiliumNetworkPolicy kind. The
// operator manipulates it as unstructured to avoid pulling the cilium Go types
// into its dependency graph — the same approach ensureAppProject uses for the
// ArgoCD AppProject.
var ciliumNetworkPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}

// tenantEgressCiliumRules is the egress allow-list shared by the per-tenant and
// per-fleet CiliumNetworkPolicies: kube-dns, agentgateway, the OTel collector,
// and — the reason this whole path exists — the EKS Pod Identity credential
// endpoint at 169.254.170.23:80. Under cilium that endpoint is the reserved
// `host` entity, which a vanilla k8s NetworkPolicy ipBlock CANNOT match, so a
// tenant-runtime pod bound by a Pod Identity association gets NO AWS
// credentials without this rule. Mirrors the operator's own
// charts/operator/templates/networkpolicy.yaml cilium idiom.
func tenantEgressCiliumRules() []interface{} {
	return []interface{}{
		map[string]interface{}{ // DNS
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
				"k8s:io.kubernetes.pod.namespace": "kube-system",
				"k8s:k8s-app":                     "kube-dns",
			}}},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": "53", "protocol": "UDP"},
				map[string]interface{}{"port": "53", "protocol": "TCP"},
			}}},
		},
		map[string]interface{}{ // agentgateway
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
				"k8s:io.kubernetes.pod.namespace": "agentgateway",
			}}},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": "8080", "protocol": "TCP"},
			}}},
		},
		map[string]interface{}{ // observability OTel collector
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
				"k8s:io.kubernetes.pod.namespace": "observability",
			}}},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": "4317", "protocol": "TCP"},
				map[string]interface{}{"port": "4318", "protocol": "TCP"},
			}}},
		},
		map[string]interface{}{ // EKS Pod Identity creds endpoint 169.254.170.23:80 (host entity)
			"toEntities": []interface{}{"host"},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": "80", "protocol": "TCP"},
			}}},
		},
	}
}

// ensureCiliumEgress creates/updates a CiliumNetworkPolicy named `name` in
// `namespace` selecting the endpoints in `endpointMatch` (empty = all pods in
// the namespace), carrying tenantEgressCiliumRules. When denyIngress is true an
// empty ingress rule set is added so cilium default-denies ingress to the
// selected pods (the per-fleet policy denies all ingress, matching its k8s NP
// twin). Returns nil on a non-cilium cluster (the CRD is absent →
// isNoKindMatch) so a kubernetes-engine deployment is unaffected.
func ensureCiliumEgress(ctx context.Context, c client.Client, namespace, name string, endpointMatch map[string]interface{}, labels map[string]string, denyIngress bool) error {
	cnp := &unstructured.Unstructured{}
	cnp.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	cnp.SetName(name)
	cnp.SetNamespace(namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, c, cnp, func() error {
		cnp.SetLabels(labels)
		spec := map[string]interface{}{
			"endpointSelector": map[string]interface{}{"matchLabels": endpointMatch},
			"egress":           tenantEgressCiliumRules(),
		}
		if denyIngress {
			spec["ingress"] = []interface{}{}
		}
		return unstructured.SetNestedField(cnp.Object, spec, "spec")
	})
	if isNoKindMatch(err) {
		return nil
	}
	return err
}
