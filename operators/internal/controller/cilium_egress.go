package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
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

// envoyProxyPodLabels selects the Envoy pods Envoy Gateway renders for a
// Gateway. These are upstream's own labels for every proxy resource
// (internal/infrastructure/kubernetes/proxy: EnvoyAppLabel), not ones the
// operator sets, so they are the stable handle for reaching the data plane.
func envoyProxyPodLabels() map[string]interface{} {
	return map[string]interface{}{
		"k8s:app.kubernetes.io/name":       "envoy",
		"k8s:app.kubernetes.io/component":  "proxy",
		"k8s:app.kubernetes.io/managed-by": "envoy-gateway",
	}
}

// tenantEgressCiliumRules is the egress allow-list shared by the per-tenant and
// per-fleet CiliumNetworkPolicies: kube-dns, the model gateway, the OTel collector,
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
		map[string]interface{}{ // the Platform's model gateway
			// The gateway's Envoy runs in this same namespace, so the selector
			// carries no namespace label — cilium scopes a bare toEndpoints to
			// the policy's own namespace. Default-deny egress covers
			// same-namespace traffic too, so without this rule the tenant cannot
			// reach its own gateway and every model call fails.
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": envoyProxyPodLabels()}},
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

// gatewayEgressCiliumRules is the extra egress the model gateway's Envoy needs
// and no other tenant pod may have: outbound TLS, which is how it reaches
// Bedrock.
//
// It is a separate policy rather than another entry in the tenant allow-list
// because network policies are additive. Selecting only the Envoy pods gives the
// gateway outbound TLS while leaving every application pod without it — so the
// gateway is the *only* path to a model, enforced by the network rather than by
// asking applications to use it. Adding this rule to the shared tenant list
// instead would hand every pod a direct route to Bedrock and reduce the gateway
// to a convention.
//
// Scoped to 443 rather than a Bedrock FQDN: with the model plane on PrivateLink
// the endpoint resolves to an in-VPC address, and FQDN matching would depend on
// cilium's DNS proxy being enabled. The port bound plus the pod selector is the
// boundary that holds without that dependency.
func gatewayEgressCiliumRules() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"toEntities": []interface{}{"all"},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": "443", "protocol": "TCP"},
			}}},
		},
	}
}

// ensureGatewayCiliumEgress emits the gateway's outbound-TLS CiliumNetworkPolicy
// on cilium clusters. No-op elsewhere, where the portable NetworkPolicy in
// ensureGatewayEgressPolicy carries the same rule.
func (r *PlatformReconciler) ensureGatewayCiliumEgress(ctx context.Context, p *platformv1alpha1.Platform) error {
	if r.NetworkEngine != NetworkEngineCilium {
		return nil
	}
	cnp := &unstructured.Unstructured{}
	cnp.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	cnp.SetName("gateway-egress")
	cnp.SetNamespace(PlatformNamespace(p))
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cnp, func() error {
		cnp.SetLabels(labelsForPlatform(p))
		return unstructured.SetNestedField(cnp.Object, map[string]interface{}{
			"endpointSelector": map[string]interface{}{"matchLabels": envoyProxyPodLabels()},
			"egress":           gatewayEgressCiliumRules(),
		}, "spec")
	})
	if isNoKindMatch(err) {
		return nil
	}
	return err
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
