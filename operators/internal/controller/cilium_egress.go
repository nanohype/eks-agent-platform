package controller

import (
	"context"
	"strconv"

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

// collectorNamespace is where the OTel collector gateway runs, and therefore
// the only namespace a workload's OTLP egress rule may name.
//
// It is a constant shared by every policy that opens OTLP rather than a
// literal repeated per rule: telemetry failing is silent by construction —
// pods stay healthy, the collector stays healthy, and nothing arrives — so a
// single rule naming the wrong namespace has no symptom to notice. One
// definition is checkable against the catalog; four literals are not.
const collectorNamespace = "monitoring"

// OTLP receiver ports on that collector: gRPC and HTTP.
const (
	otlpGRPCPort = 4317
	otlpHTTPPort = 4318
)

// The wire ports of the datastore kinds that speak their own protocol.
//
// A tenant declares its stateful substrate in spec.datastores, the
// tenant-substrate module provisions it, and the operator grants IAM to reach
// it — and then default-deny egress drops the packets, because the tenant
// boundary was built without reference to the tenant's own declaration. The
// symptom is the worst kind: the Platform is Ready, the datastore is available,
// the credential is valid, and the connection simply times out.
const (
	postgresPort = 5432 // relational — Aurora PostgreSQL
	redisPort    = 6379 // cache — ElastiCache (Valkey/Redis)
	kafkaIAMPort = 9098 // stream — MSK Serverless, IAM auth
)

// datastoreEgressPorts returns the TCP ports this Platform's declared
// datastores need, deduplicated and in ascending order. Empty when a tenant
// declares no datastore that speaks its own protocol, so a tenant without one
// gains no egress it did not previously have.
//
// The ports come from the SAME declaration the substrate and the IAM policy are
// generated from. Deriving all three from spec.datastores is what keeps them
// from disagreeing — a hand-maintained port list is a fourth copy that drifts
// the first time a kind is added.
//
// Covers only the kinds that speak their own protocol. objectStore, keyValue
// and queue reach AWS over 443 — the same port the model plane answers on — so
// a port bound cannot express them safely; those go through datastoreFQDNs,
// which bounds by hostname instead.
func datastoreEgressPorts(p *platformv1alpha1.Platform) []int {
	var pg, redis, kafka bool
	for _, d := range p.Spec.Datastores {
		switch d.Kind {
		case platformv1alpha1.DatastoreRelational:
			pg = true
		case platformv1alpha1.DatastoreCache:
			redis = true
		case platformv1alpha1.DatastoreStream:
			kafka = true
		}
	}
	ports := make([]int, 0, 3)
	if pg {
		ports = append(ports, postgresPort)
	}
	if redis {
		ports = append(ports, redisPort)
	}
	if kafka {
		ports = append(ports, kafkaIAMPort)
	}
	return ports
}

// datastoreFQDNs returns the AWS service hostnames this Platform's declared
// datastores need on 443, for the kinds that speak the AWS API rather than their
// own protocol: objectStore (S3), keyValue (DynamoDB) and queue (SQS).
//
// Hostnames rather than a port bound, because 443 is also how the model plane
// answers. Bedrock is reached over PrivateLink and resolves to an in-VPC
// address, so no CIDR separates it from S3 either — a port-scoped or
// CIDR-scoped rule would hand every application pod a direct route to Bedrock
// and reduce the model gateway (see gatewayEgressCiliumRules) from a
// network-enforced boundary to a convention. An FQDN allow-list is the only
// bound that admits the datastore and still excludes the model.
//
// Exact names, never a *.amazonaws.com pattern: that pattern matches
// bedrock-runtime.<region>.amazonaws.com and gives away the whole boundary.
//
// Empty when the region is unknown, which fails CLOSED. A tenant reaching
// nothing is a visible outage; a tenant reaching everything on 443 is a silent
// hole in the model boundary, and only one of those gets noticed.
func datastoreFQDNs(p *platformv1alpha1.Platform, region string) []string {
	if region == "" {
		return nil
	}
	var s3, ddb, sqs bool
	for _, d := range p.Spec.Datastores {
		switch d.Kind {
		case platformv1alpha1.DatastoreObjectStore:
			s3 = true
		case platformv1alpha1.DatastoreKeyValue:
			ddb = true
		case platformv1alpha1.DatastoreQueue:
			sqs = true
		}
	}
	names := make([]string, 0, 3)
	if s3 {
		names = append(names, "s3."+region+".amazonaws.com")
	}
	if ddb {
		names = append(names, "dynamodb."+region+".amazonaws.com")
	}
	if sqs {
		names = append(names, "sqs."+region+".amazonaws.com")
	}
	return names
}

// Where Envoy Gateway's control plane runs, and the xDS port its data plane
// dials — `envoy-gateway.envoy-gateway-system.svc.cluster.local:18000`, from
// the bootstrap Envoy Gateway writes into every proxy it renders.
//
// The gateway's Envoy runs in the *tenant's* namespace (the provider is
// configured GatewayNamespace, which is what lets the proxy carry the tenant
// ServiceAccount and reach Bedrock as the tenant). So it sits inside the
// tenant's default-deny egress and cannot reach its own control plane without
// being allowed out explicitly.
//
// A proxy that cannot reach xDS never receives a listener, never passes its
// startup probe, and the Gateway never reports Programmed — while the
// ModelGateway CR that asked for it still reports Ready, because the operator
// reconciled every resource it owns successfully. Nothing upstream of the
// data plane looks unhealthy.
const (
	envoyGatewayNamespace = "envoy-gateway-system"
	envoyGatewayXDSPort   = 18000
)

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
//
// datastorePorts opens the tenant's own declared datastores (see
// datastoreEgressPorts). Empty for a tenant that declares none, which is why
// this takes a parameter rather than reading a constant: the allow-list is a
// function of what the Platform asked for.
func tenantEgressCiliumRules(datastorePorts []int, datastoreFQDNs []string) []interface{} {
	rules := []interface{}{
		map[string]interface{}{ // DNS
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
				"k8s:io.kubernetes.pod.namespace": "kube-system",
				"k8s:k8s-app":                     "kube-dns",
			}}},
			"toPorts": []interface{}{map[string]interface{}{
				"ports": []interface{}{
					map[string]interface{}{"port": "53", "protocol": "UDP"},
					map[string]interface{}{"port": "53", "protocol": "TCP"},
				},
				// L7 DNS visibility, and toFQDNs below does not work without it.
				//
				// Cilium resolves an FQDN rule by watching the tenant's DNS
				// answers through its proxy and pinning the returned addresses.
				// With no `rules.dns` on the DNS rule the proxy never sees the
				// response, the FQDN cache stays empty, and the toFQDNs rule
				// matches nothing — the policy is accepted, reports Valid, and
				// silently denies every packet. Failing that way is the reason
				// this is spelled out here rather than assumed.
				//
				// matchPattern "*" observes without restricting: the tenant may
				// resolve any name, and what it may CONNECT to is still bounded
				// by the allow-list. Narrowing this to the datastore names would
				// also break resolution of the cluster-internal Services the
				// rules above depend on.
				"rules": map[string]interface{}{
					"dns": []interface{}{map[string]interface{}{"matchPattern": "*"}},
				},
			}},
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
		map[string]interface{}{ // the OTel collector gateway
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
				"k8s:io.kubernetes.pod.namespace": collectorNamespace,
			}}},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": strconv.Itoa(otlpGRPCPort), "protocol": "TCP"},
				map[string]interface{}{"port": strconv.Itoa(otlpHTTPPort), "protocol": "TCP"},
			}}},
		},
		map[string]interface{}{ // EKS Pod Identity creds endpoint 169.254.170.23:80 (host entity)
			"toEntities": []interface{}{"host"},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": "80", "protocol": "TCP"},
			}}},
		},
	}
	// The tenant's own datastores. Appended rather than declared above because
	// the set is per-Platform: a tenant with no relational/cache/stream store
	// gets no rule and no new reach.
	//
	// toEntities "all" scoped to these ports, not a CIDR: Aurora and
	// ElastiCache resolve to addresses the substrate assigns at provision
	// time, which the operator does not know and must not guess. The port is
	// the bound that holds without that knowledge, and 5432/6379/9098 are
	// datastore protocols — unlike 443, nothing else in the tenant's world
	// answers on them.
	if len(datastorePorts) > 0 {
		ports := make([]interface{}, 0, len(datastorePorts))
		for _, p := range datastorePorts {
			ports = append(ports, map[string]interface{}{"port": strconv.Itoa(p), "protocol": "TCP"})
		}
		rules = append(rules, map[string]interface{}{
			"toEntities": []interface{}{"all"},
			"toPorts":    []interface{}{map[string]interface{}{"ports": ports}},
		})
	}
	// The AWS-API datastores, by hostname on 443. See datastoreFQDNs for why
	// this cannot be a port or CIDR bound: Bedrock answers on the same port from
	// the same VPC, and only the name separates them.
	if len(datastoreFQDNs) > 0 {
		names := make([]interface{}, 0, len(datastoreFQDNs))
		for _, n := range datastoreFQDNs {
			names = append(names, map[string]interface{}{"matchName": n})
		}
		rules = append(rules, map[string]interface{}{
			"toFQDNs": names,
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": "443", "protocol": "TCP"},
			}}},
		})
	}
	return rules
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
		map[string]interface{}{ // xDS, to Envoy Gateway's control plane
			// Not covered by the 443 rule above: xDS is plaintext gRPC on
			// 18000, to a namespace the tenant otherwise has no route to.
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
				"k8s:io.kubernetes.pod.namespace": envoyGatewayNamespace,
			}}},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": strconv.Itoa(envoyGatewayXDSPort), "protocol": "TCP"},
			}}},
		},
		map[string]interface{}{ // the OTel collector gateway
			"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
				"k8s:io.kubernetes.pod.namespace": collectorNamespace,
			}}},
			"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
				map[string]interface{}{"port": strconv.Itoa(otlpGRPCPort), "protocol": "TCP"},
				map[string]interface{}{"port": strconv.Itoa(otlpHTTPPort), "protocol": "TCP"},
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
func ensureCiliumEgress(ctx context.Context, c client.Client, namespace, name string, endpointMatch map[string]interface{}, labels map[string]string, denyIngress bool, datastorePorts []int, datastoreFQDNs []string) error {
	cnp := &unstructured.Unstructured{}
	cnp.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	cnp.SetName(name)
	cnp.SetNamespace(namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, c, cnp, func() error {
		cnp.SetLabels(labels)
		spec := map[string]interface{}{
			"endpointSelector": map[string]interface{}{"matchLabels": endpointMatch},
			"egress":           tenantEgressCiliumRules(datastorePorts, datastoreFQDNs),
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
